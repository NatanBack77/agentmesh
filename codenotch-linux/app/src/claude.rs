use crate::provider::{now_ms, parse_reset, LimitWindow, ProviderSnapshot};
use std::time::Duration;

const ENDPOINT: &str = "https://api.anthropic.com/api/oauth/usage";

fn credential_paths() -> Vec<std::path::PathBuf> {
    let Some(home) = dirs::home_dir() else {
        return Vec::new();
    };
    vec![
        home.join(".claude").join(".credentials.json"),
        home.join(".claude").join("credentials.json"),
    ]
}

fn read_token() -> Option<String> {
    for path in credential_paths() {
        let Ok(text) = std::fs::read_to_string(path) else {
            continue;
        };
        let Ok(value) = serde_json::from_str::<serde_json::Value>(&text) else {
            continue;
        };
        let oauth = value.get("claudeAiOauth").unwrap_or(&value);
        let Some(token) = oauth.get("accessToken").and_then(|t| t.as_str()) else {
            continue;
        };
        let token = token.trim();
        if !token.is_empty() {
            return Some(token.to_string());
        }
    }
    None
}

fn label_for(kind: &str) -> String {
    match kind {
        "session" | "five_hour" => "Current session".into(),
        "seven_day" | "weekly_all" | "weekly" => "Weekly limit".into(),
        "weekly_opus" | "seven_day_opus" => "Weekly Opus".into(),
        other => other.replace('_', " "),
    }
}

fn parse_windows(value: &serde_json::Value) -> Vec<LimitWindow> {
    let mut out = Vec::new();
    if let Some(limits) = value.get("limits").and_then(|x| x.as_array()) {
        for limit in limits {
            let Some(kind) = limit.get("kind").and_then(|x| x.as_str()) else {
                continue;
            };
            let Some(percent) = limit.get("percent").and_then(|x| x.as_f64()) else {
                continue;
            };
            out.push(LimitWindow {
                id: kind.into(),
                label: label_for(kind),
                used: (percent / 100.0).clamp(0.0, 1.0),
                resets_at: parse_reset(limit.get("resets_at")),
                ..Default::default()
            });
        }
    }
    for (field, id) in [("five_hour", "session"), ("seven_day", "weekly_all")] {
        let Some(window) = value.get(field) else {
            continue;
        };
        let Some(utilization) = window.get("utilization").and_then(|x| x.as_f64()) else {
            continue;
        };
        if out.iter().any(|w| w.id == id) {
            continue;
        }
        out.push(LimitWindow {
            id: id.into(),
            label: label_for(id),
            used: (utilization / 100.0).clamp(0.0, 1.0),
            resets_at: parse_reset(window.get("resets_at")),
            ..Default::default()
        });
    }
    out.sort_by_key(|w| if w.id == "session" { 0 } else { 1 });
    out
}

pub fn fetch() -> ProviderSnapshot {
    let mut snapshot = ProviderSnapshot::absent("claude", "Claude", "C");
    let Some(token) = read_token() else {
        snapshot.status = "needsAuth".into();
        snapshot.note = "Sign in with Claude Code to create ~/.claude credentials.".into();
        return snapshot;
    };

    let response = ureq::get(ENDPOINT)
        .set("Authorization", &format!("Bearer {token}"))
        .set("anthropic-beta", "oauth-2025-04-20")
        .timeout(Duration::from_secs(15))
        .call();

    match response {
        Ok(resp) => match resp.into_json::<serde_json::Value>() {
            Ok(value) => {
                snapshot.status = "ok".into();
                snapshot.available = true;
                snapshot.windows = parse_windows(&value);
                snapshot.fetched_at = now_ms();
            }
            Err(err) => {
                snapshot.status = "error".into();
                snapshot.note = format!("Claude response parse failed: {err}");
            }
        },
        Err(ureq::Error::Status(401 | 403, _)) => {
            snapshot.status = "needsAuth".into();
            snapshot.note = "Claude credential was rejected; refresh Claude Code login.".into();
        }
        Err(ureq::Error::Status(429, _)) => {
            snapshot.status = "backoff".into();
            snapshot.note = "Claude usage endpoint is rate limited.".into();
        }
        Err(err) => {
            snapshot.status = "error".into();
            snapshot.note = err.to_string();
        }
    }
    snapshot
}
