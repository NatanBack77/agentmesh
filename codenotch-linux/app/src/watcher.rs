use crate::provider::{now_ms, Activity};
use crate::AppState;
use notify::{RecursiveMode, Watcher};
use std::io::{Read, Seek, SeekFrom};
use std::path::{Path, PathBuf};
use std::sync::mpsc::{channel, RecvTimeoutError};
use std::time::Duration;
use tauri::{AppHandle, Manager};

const TAIL_BYTES: u64 = 256 * 1024;
const QUIET_DONE_MS: u64 = 2_500;
const QUIET_WAITING_MS: u64 = 20_000;

struct Track {
    activity: Activity,
    last_append: u64,
    last_kind: Kind,
}

#[derive(Clone, Copy, PartialEq)]
enum Kind {
    User,
    AssistantText,
    AssistantTool,
    Other,
}

pub fn roots() -> Vec<PathBuf> {
    let Some(home) = dirs::home_dir() else {
        return Vec::new();
    };
    vec![home.join(".claude").join("projects")]
}

pub fn start(app: AppHandle) {
    std::thread::spawn(move || {
        let (tx, rx) = channel();
        let Ok(mut watcher) = notify::recommended_watcher(move |event| {
            let _ = tx.send(event);
        }) else {
            return;
        };
        for root in roots().into_iter().filter(|root| root.exists()) {
            let _ = watcher.watch(&root, RecursiveMode::Recursive);
        }
        let mut tracks: std::collections::HashMap<PathBuf, Track> = Default::default();
        loop {
            match rx.recv_timeout(Duration::from_millis(500)) {
                Ok(Ok(event)) => {
                    for path in event.paths.into_iter().filter(|p| is_session_jsonl(p)) {
                        ingest(&app, &mut tracks, &path);
                    }
                }
                Ok(Err(_)) => {}
                Err(RecvTimeoutError::Timeout) => {}
                Err(RecvTimeoutError::Disconnected) => return,
            }
            evaluate(&app, &mut tracks);
        }
    });
}

fn is_session_jsonl(path: &Path) -> bool {
    path.extension().map(|ext| ext == "jsonl").unwrap_or(false)
        && path
            .components()
            .any(|part| part.as_os_str().to_string_lossy() == ".claude")
        && path.file_name().and_then(|n| n.to_str()) != Some("audit.jsonl")
}

fn ingest(app: &AppHandle, tracks: &mut std::collections::HashMap<PathBuf, Track>, path: &Path) {
    let Some((kind, prompt)) = tail_kind(path) else {
        return;
    };
    let id = path
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or("claude-session")
        .to_string();
    let activity = Activity {
        id,
        provider: "claude".into(),
        state: "busy".into(),
        cwd: path.parent().map(|p| p.display().to_string()).unwrap_or_default(),
        prompt,
        ppid: 0,
        updated_at: now_ms(),
    };
    tracks.insert(
        path.to_path_buf(),
        Track {
            activity: activity.clone(),
            last_append: now_ms(),
            last_kind: kind,
        },
    );
    push(app, activity);
}

fn evaluate(app: &AppHandle, tracks: &mut std::collections::HashMap<PathBuf, Track>) {
    let now = now_ms();
    for track in tracks.values_mut() {
        let quiet = now.saturating_sub(track.last_append);
        let next = match track.last_kind {
            Kind::AssistantTool if quiet > QUIET_WAITING_MS => "waiting",
            Kind::AssistantText if quiet > QUIET_DONE_MS => "done",
            Kind::User if quiet > 75_000 => "done",
            _ => "busy",
        };
        if track.activity.state != next {
            track.activity.state = next.into();
            track.activity.updated_at = now;
            push(app, track.activity.clone());
            if next == "done" {
                // A turn just finished, so the account's token/limit usage
                // likely moved — refresh it now instead of waiting for the
                // next scheduled poll (see request_usage_refresh).
                crate::request_usage_refresh(app);
            }
        }
    }
}

fn push(app: &AppHandle, activity: Activity) {
    {
        let state = app.state::<AppState>();
        let mut store = state.store.lock().unwrap();
        store.apply_activity(activity);
    }
    crate::broadcast(app);
}

fn tail_kind(path: &Path) -> Option<(Kind, String)> {
    let mut file = std::fs::File::open(path).ok()?;
    let len = file.metadata().ok()?.len();
    let start = len.saturating_sub(TAIL_BYTES);
    file.seek(SeekFrom::Start(start)).ok()?;
    let mut text = String::new();
    file.read_to_string(&mut text).ok()?;
    let mut prompt = String::new();
    for line in text.lines().rev() {
        let Ok(value) = serde_json::from_str::<serde_json::Value>(line) else {
            continue;
        };
        if prompt.is_empty() {
            prompt = user_text(&value).unwrap_or_default();
        }
        if let Some(kind) = entry_kind(&value) {
            return Some((kind, prompt));
        }
    }
    None
}

fn entry_kind(value: &serde_json::Value) -> Option<Kind> {
    let role = value.pointer("/message/role").and_then(|v| v.as_str())?;
    match role {
        "user" => Some(Kind::User),
        "assistant" => {
            let content = value.pointer("/message/content")?;
            if content
                .as_array()
                .map(|items| items.iter().any(|item| item.get("type").and_then(|t| t.as_str()) == Some("tool_use")))
                .unwrap_or(false)
            {
                Some(Kind::AssistantTool)
            } else {
                Some(Kind::AssistantText)
            }
        }
        _ => Some(Kind::Other),
    }
}

fn user_text(value: &serde_json::Value) -> Option<String> {
    if value.pointer("/message/role").and_then(|v| v.as_str()) != Some("user") {
        return None;
    }
    let content = value.pointer("/message/content")?;
    if let Some(text) = content.as_str() {
        return Some(text.trim().chars().take(160).collect());
    }
    for item in content.as_array()? {
        if item.get("type").and_then(|t| t.as_str()) == Some("text") {
            return item
                .get("text")
                .and_then(|t| t.as_str())
                .map(|text| text.trim().chars().take(160).collect());
        }
    }
    None
}
