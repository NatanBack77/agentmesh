use crate::provider::{now_ms, LimitWindow, ProviderSnapshot};
use std::path::{Path, PathBuf};
use std::time::Duration;

const ENDPOINT: &str = "https://chatgpt.com/backend-api/wham/usage";

fn codex_home() -> Option<PathBuf> {
    dirs::home_dir().map(|home| home.join(".codex"))
}

fn auth_path() -> Option<PathBuf> {
    codex_home().map(|home| home.join("auth.json"))
}

struct Credential {
    access_token: String,
    account_id: String,
}

fn load_credential() -> Option<Credential> {
    let text = std::fs::read_to_string(auth_path()?).ok()?;
    let value: serde_json::Value = serde_json::from_str(&text).ok()?;
    let tokens = value.get("tokens")?;
    Some(Credential {
        access_token: tokens.get("access_token")?.as_str()?.trim().into(),
        account_id: tokens.get("account_id")?.as_str()?.trim().into(),
    })
}

fn label_for(seconds: Option<f64>, id: &str) -> String {
    match seconds {
        Some(s) if s < 3600.0 => format!("{}m limit", (s / 60.0).round() as i64),
        Some(s) if s < 86400.0 => format!("{}h limit", (s / 3600.0).round() as i64),
        Some(s) => format!("{}d limit", (s / 86400.0).round() as i64),
        None if id == "primary" => "Current session".into(),
        None => "Longer window".into(),
    }
}

fn windows_from_usage(value: &serde_json::Value) -> Vec<LimitWindow> {
    let now = now_ms();
    let mut out = Vec::new();
    for (id, key) in [("primary", "primary_window"), ("secondary", "secondary_window")] {
        let Some(window) = value.pointer(&format!("/rate_limit/{key}")).filter(|x| x.is_object()) else {
            continue;
        };
        let Some(percent) = window.get("used_percent").and_then(|x| x.as_f64()) else {
            continue;
        };
        let seconds = window.get("limit_window_seconds").and_then(|x| x.as_f64());
        let resets_at = window
            .get("reset_at")
            .and_then(|x| x.as_f64())
            .map(|s| (s * 1000.0) as u64)
            .or_else(|| {
                window
                    .get("reset_after_seconds")
                    .and_then(|x| x.as_f64())
                    .map(|s| now + (s * 1000.0) as u64)
            });
        out.push(LimitWindow {
            id: id.into(),
            label: label_for(seconds, id),
            used: (percent / 100.0).clamp(0.0, 1.0),
            resets_at,
            ..Default::default()
        });
    }
    out
}

fn newest_rollout() -> Option<PathBuf> {
    let root = codex_home()?.join("sessions");
    let mut files = Vec::new();
    collect_rollouts(&root, &mut files);
    files.sort_by_key(|p| std::fs::metadata(p).and_then(|m| m.modified()).ok());
    files.pop()
}

fn collect_rollouts(root: &Path, out: &mut Vec<PathBuf>) {
    let Ok(read_dir) = std::fs::read_dir(root) else {
        return;
    };
    for entry in read_dir.flatten() {
        let path = entry.path();
        if path.is_dir() {
            collect_rollouts(&path, out);
        } else if path
            .file_name()
            .and_then(|n| n.to_str())
            .map(|n| n.starts_with("rollout-") && n.ends_with(".jsonl"))
            .unwrap_or(false)
        {
            out.push(path);
        }
    }
}

fn fallback_snapshot() -> Option<ProviderSnapshot> {
    let path = newest_rollout()?;
    let text = std::fs::read_to_string(path).ok()?;
    for line in text.lines().rev().take(2000) {
        let Ok(value) = serde_json::from_str::<serde_json::Value>(line) else {
            continue;
        };
        let Some(limits) = value.pointer("/payload/rate_limits") else {
            continue;
        };
        let mut synthetic = serde_json::json!({ "rate_limit": {} });
        if let Some(primary) = limits.get("primary") {
            synthetic["rate_limit"]["primary_window"] = primary.clone();
        }
        if let Some(secondary) = limits.get("secondary") {
            synthetic["rate_limit"]["secondary_window"] = secondary.clone();
        }
        let windows = windows_from_usage(&synthetic);
        if !windows.is_empty() {
            return Some(ProviderSnapshot {
                provider: "codex".into(),
                id: "codex".into(),
                kind: "Usage".into(),
                available: true,
                running: false,
                name: "Codex".into(),
                label: "Codex".into(),
                glyph: "Cx".into(),
                status: "stale".into(),
                windows,
                fetched_at: now_ms(),
                note: "From latest Codex rollout snapshot.".into(),
            });
        }
    }
    None
}

pub fn fetch() -> ProviderSnapshot {
    let mut snapshot = ProviderSnapshot::absent("codex", "Codex", "Cx");
    let Some(credential) = load_credential() else {
        return fallback_snapshot().unwrap_or_else(|| {
            snapshot.status = "needsAuth".into();
            snapshot.note = "Sign in with Codex CLI to create ~/.codex/auth.json.".into();
            snapshot
        });
    };
    let response = ureq::get(ENDPOINT)
        .set("Authorization", &format!("Bearer {}", credential.access_token))
        .set("ChatGPT-Account-Id", &credential.account_id)
        .set("Accept", "application/json")
        .set("Cache-Control", "no-cache, no-store")
        .set("User-Agent", "meshnotch/0.1")
        .timeout(Duration::from_secs(15))
        .call();
    match response {
        Ok(resp) => match resp.into_json::<serde_json::Value>() {
            Ok(value) => {
                snapshot.status = "ok".into();
                snapshot.available = true;
                snapshot.windows = windows_from_usage(&value);
                snapshot.fetched_at = now_ms();
            }
            Err(err) => {
                snapshot.status = "error".into();
                snapshot.note = format!("Codex response parse failed: {err}");
            }
        },
        Err(ureq::Error::Status(401 | 403, _)) => {
            snapshot.status = "needsAuth".into();
            snapshot.note = "Codex session rejected; renew Codex CLI login.".into();
        }
        Err(err) => {
            snapshot.status = "error".into();
            snapshot.note = err.to_string();
        }
    }
    snapshot
}
