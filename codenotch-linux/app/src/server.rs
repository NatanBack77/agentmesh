use crate::provider::{now_ms, Activity};
use crate::AppState;
use std::io::Read;
use tauri::{AppHandle, Emitter, Manager};

pub fn start(app: AppHandle, port: u16) {
    std::thread::spawn(move || {
        let server = match tiny_http::Server::http(("127.0.0.1", port)) {
            Ok(server) => server,
            Err(err) => {
                eprintln!("[meshnotch] hook server bind failed on {port}: {err}");
                return;
            }
        };
        for mut request in server.incoming_requests() {
            let url = request.url().to_string();
            let mut body = String::new();
            let _ = request
                .as_reader()
                .take(256 * 1024)
                .read_to_string(&mut body);
            if url.starts_with("/event") {
                let activity = parse_activity(&url, &body);
                let state = app.state::<AppState>();
                {
                    let mut store = state.store.lock().unwrap();
                    store.apply_activity(activity);
                }
                crate::broadcast(&app);
                let _ = app.emit("activity", ());
            }
            let _ = request.respond(tiny_http::Response::from_string("ok"));
        }
    });
}

fn param(url: &str, key: &str) -> String {
    let query = url.split_once('?').map(|(_, q)| q).unwrap_or("");
    for pair in query.split('&') {
        let (k, v) = pair.split_once('=').unwrap_or((pair, ""));
        if k == key {
            return urlencoding::decode(v).map(|v| v.into_owned()).unwrap_or_default();
        }
    }
    String::new()
}

fn text(value: &serde_json::Value, key: &str) -> String {
    value
        .get(key)
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .chars()
        .take(160)
        .collect()
}

fn parse_activity(url: &str, body: &str) -> Activity {
    let value: serde_json::Value = serde_json::from_str(body).unwrap_or(serde_json::Value::Null);
    let event = param(url, "e");
    let state = match event.as_str() {
        "PreToolUse" | "UserPromptSubmit" => "busy",
        "Notification" | "Stop" | "SubagentStop" => "done",
        "ApprovalRequired" => "waiting",
        _ => "busy",
    };
    let id = text(&value, "session_id");
    Activity {
        id: if id.is_empty() { "claude-unknown".into() } else { id },
        provider: "claude".into(),
        state: state.into(),
        cwd: text(&value, "cwd"),
        prompt: text(&value, "prompt"),
        ppid: param(url, "ppid").parse().unwrap_or(0),
        updated_at: now_ms(),
    }
}
