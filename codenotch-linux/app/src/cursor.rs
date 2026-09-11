use crate::provider::{now_ms, parse_reset, LimitWindow, ProviderSnapshot};
use rusqlite::OpenFlags;
use std::path::PathBuf;
use std::time::Duration;

const ENDPOINT: &str = "https://cursor.com/api/usage-summary";

fn store_url() -> Option<PathBuf> {
    dirs::config_dir().map(|dir| {
        dir.join("Cursor")
            .join("User")
            .join("globalStorage")
            .join("state.vscdb")
    })
}

fn open_ro(path: &std::path::Path) -> Option<rusqlite::Connection> {
    if !path.is_file() {
        return None;
    }
    rusqlite::Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )
    .ok()
}

fn item(conn: &rusqlite::Connection, key: &str) -> Option<String> {
    conn.query_row("SELECT value FROM ItemTable WHERE key = ?1", [key], |row| {
        row.get::<_, String>(0)
    })
    .ok()
    .filter(|value| !value.is_empty())
}

fn read_cookie() -> Option<String> {
    let path = store_url()?;
    let conn = open_ro(&path)?;
    let token = item(&conn, "cursorAuth/accessToken")?;
    let auth_id = item(&conn, "cursorAuth/stripeMembershipAuthId")?;
    Some(format!("WorkosCursorSessionToken={auth_id}::{token}"))
}

fn percent(value: Option<&serde_json::Value>) -> Option<f64> {
    value.and_then(|v| v.as_f64()).map(|p| (p / 100.0).clamp(0.0, 1.0))
}

fn parse_summary(value: &serde_json::Value) -> (Vec<LimitWindow>, String) {
    let resets_at = parse_reset(value.get("billingCycleEnd"));
    let null = serde_json::Value::Null;
    let usage = value.get("individualUsage").unwrap_or(&null);
    let plan = usage.get("plan").unwrap_or(&null);
    let mut out = Vec::new();

    if let Some(total) = percent(plan.get("totalPercentUsed")) {
        out.push(LimitWindow {
            id: "included".into(),
            label: "Included usage".into(),
            used: total,
            resets_at,
            ..Default::default()
        });
    }
    if let Some(api) = percent(plan.get("apiPercentUsed")) {
        if api > 0.0 {
            out.push(LimitWindow {
                id: "api".into(),
                label: "API usage".into(),
                used: api,
                resets_at,
                ..Default::default()
            });
        }
    }
    if !out.is_empty() {
        return (out, String::new());
    }
    let membership = value
        .get("membershipType")
        .and_then(|v| v.as_str())
        .unwrap_or("current");
    (out, format!("The {membership} plan has no metered usage yet."))
}

pub fn fetch() -> ProviderSnapshot {
    let mut snapshot = ProviderSnapshot::absent("cursor", "Cursor", "Cu");
    let Some(cookie) = read_cookie() else {
        snapshot.status = "needsAuth".into();
        snapshot.note = "Sign in to Cursor editor to expose its local session.".into();
        return snapshot;
    };

    let response = ureq::get(ENDPOINT)
        .set("Cookie", &cookie)
        .set("Accept", "application/json")
        .timeout(Duration::from_secs(15))
        .call();

    match response {
        Ok(resp) => match resp.into_json::<serde_json::Value>() {
            Ok(value) => {
                let (windows, note) = parse_summary(&value);
                snapshot.status = if windows.is_empty() { "none" } else { "ok" }.into();
                snapshot.available = true;
                snapshot.windows = windows;
                snapshot.fetched_at = now_ms();
                snapshot.note = note;
            }
            Err(err) => {
                snapshot.status = "error".into();
                snapshot.note = format!("Cursor response parse failed: {err}");
            }
        },
        Err(ureq::Error::Status(401 | 403, _)) => {
            snapshot.status = "needsAuth".into();
            snapshot.note = "Cursor session rejected; sign in again in Cursor.".into();
        }
        Err(err) => {
            snapshot.status = "error".into();
            snapshot.note = err.to_string();
        }
    }
    snapshot
}
