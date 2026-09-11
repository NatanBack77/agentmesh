use serde::{Deserialize, Serialize};
use std::time::{SystemTime, UNIX_EPOCH};

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct LimitWindow {
    pub id: String,
    pub label: String,
    pub used: f64,
    pub resets_at: Option<u64>,
    #[serde(default)]
    pub count: Option<i64>,
    #[serde(default)]
    pub derived: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProviderSnapshot {
    pub provider: String,
    pub id: String,
    pub kind: String,
    pub available: bool,
    pub running: bool,
    pub name: String,
    pub label: String,
    pub glyph: String,
    pub status: String,
    pub windows: Vec<LimitWindow>,
    pub fetched_at: u64,
    pub note: String,
}

impl ProviderSnapshot {
    pub fn absent(id: &str, name: &str, glyph: &str) -> Self {
        Self {
            provider: id.into(),
            id: id.into(),
            kind: "Usage".into(),
            available: false,
            running: false,
            name: name.into(),
            label: name.into(),
            glyph: glyph.into(),
            status: "absent".into(),
            windows: Vec::new(),
            fetched_at: 0,
            note: String::new(),
        }
    }

    pub fn local_runtime(id: &str, label: &str, glyph: &str, available: bool, running: bool) -> Self {
        Self {
            provider: id.into(),
            id: id.into(),
            kind: "LocalRuntime".into(),
            available,
            running,
            name: label.into(),
            label: label.into(),
            glyph: glyph.into(),
            status: if running {
                "running".into()
            } else if available {
                "available".into()
            } else {
                "absent".into()
            },
            windows: Vec::new(),
            fetched_at: now_ms(),
            note: String::new(),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct Activity {
    pub id: String,
    pub provider: String,
    pub state: String,
    pub cwd: String,
    pub prompt: String,
    pub ppid: u32,
    pub updated_at: u64,
}

pub fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

pub fn parse_reset(v: Option<&serde_json::Value>) -> Option<u64> {
    v.and_then(|x| x.as_str())
        .and_then(|s| chrono::DateTime::parse_from_rfc3339(s).ok())
        .map(|d| d.timestamp_millis().max(0) as u64)
        .or_else(|| v.and_then(|x| x.as_f64()).map(|s| (s * 1000.0) as u64))
}
