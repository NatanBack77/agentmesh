use serde::{Deserialize, Serialize};
use std::path::PathBuf;

pub const DEFAULT_PORT: u16 = 48666;
pub const SCALE_MIN: f64 = 0.40;
pub const SCALE_MAX: f64 = 1.00;

pub const ACCENT_PRESETS: &[(&str, &str)] = &[
    ("system", "#0a84ff"),
    ("pink", "#ff375f"),
    ("red", "#ff453a"),
    ("orange", "#ff9f0a"),
    ("yellow", "#ffd60a"),
    ("green", "#32d74b"),
    ("teal", "#64d2ff"),
    ("blue", "#0a84ff"),
    ("indigo", "#5e5ce6"),
    ("purple", "#bf5af2"),
    ("off-white", "#f2f2f7"),
];

pub const PROVIDER_IDS: &[&str] = &["claude", "codex", "cursor", "ollama", "lmstudio", "gemini-cli", "opencode"];

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    #[serde(default = "default_port")]
    pub port: u16,
    #[serde(default = "default_notch_y")]
    pub notch_y: f64,
    #[serde(default = "default_scale")]
    pub scale: f64,
    #[serde(default = "yes")]
    pub notch_visible: bool,
    #[serde(default = "yes")]
    pub tray_visible: bool,
    #[serde(default = "default_theme")]
    pub theme: String,
    #[serde(default = "default_orientation")]
    pub orientation: String,
    #[serde(default = "default_accent")]
    pub accent: String,
    #[serde(default = "default_providers")]
    pub enabled_providers: Vec<String>,
    #[serde(default = "default_notch_x")]
    pub notch_x: Option<f64>,
    #[serde(default = "default_dashboard_url")]
    pub agentmesh_dashboard_url: String,
    #[serde(default = "default_notch_style")]
    pub notch_style: String,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            port: default_port(),
            notch_y: default_notch_y(),
            scale: default_scale(),
            notch_visible: true,
            tray_visible: true,
            theme: default_theme(),
            orientation: default_orientation(),
            accent: default_accent(),
            enabled_providers: default_providers(),
            notch_x: default_notch_x(),
            agentmesh_dashboard_url: default_dashboard_url(),
            notch_style: default_notch_style(),
        }
    }
}

fn default_port() -> u16 {
    DEFAULT_PORT
}

fn default_notch_y() -> f64 {
    0.5
}

fn default_scale() -> f64 {
    1.0
}

fn default_theme() -> String {
    "system".into()
}

fn yes() -> bool {
    true
}

fn default_orientation() -> String {
    "vertical".into()
}

fn default_accent() -> String {
    "system".into()
}

fn default_providers() -> Vec<String> {
    PROVIDER_IDS.iter().map(|s| s.to_string()).collect()
}

fn default_notch_x() -> Option<f64> {
    None
}

fn default_dashboard_url() -> String {
    "http://127.0.0.1:8990/dashboard".into()
}

fn default_notch_style() -> String {
    "solid".into()
}

pub fn accent_color(accent: &str) -> &'static str {
    ACCENT_PRESETS
        .iter()
        .find(|(id, _)| *id == accent)
        .map(|(_, hex)| *hex)
        .unwrap_or("#0a84ff")
}

pub fn data_dir() -> PathBuf {
    dirs::config_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join("meshnotch")
}

pub fn config_path() -> PathBuf {
    data_dir().join("config.json")
}

pub fn load() -> Config {
    let mut cfg = std::fs::read_to_string(config_path())
        .ok()
        .and_then(|t| serde_json::from_str::<Config>(&t).ok())
        .unwrap_or_default();
    cfg.scale = cfg.scale.clamp(SCALE_MIN, SCALE_MAX);
    if !matches!(cfg.theme.as_str(), "system" | "light" | "dark") {
        cfg.theme = "system".into();
    }
    if !matches!(cfg.orientation.as_str(), "vertical" | "horizontal") {
        cfg.orientation = default_orientation();
    }
    if !ACCENT_PRESETS.iter().any(|(id, _)| *id == cfg.accent) {
        cfg.accent = default_accent();
    }
    cfg.enabled_providers
        .retain(|id| PROVIDER_IDS.contains(&id.as_str()));
    if cfg.enabled_providers.is_empty() {
        cfg.enabled_providers = default_providers();
    }
    if let Some(x) = cfg.notch_x {
        cfg.notch_x = Some(x.clamp(0.0, 1.0));
    }
    if !matches!(cfg.notch_style.as_str(), "solid" | "glass") {
        cfg.notch_style = default_notch_style();
    }
    if !cfg.notch_visible && !cfg.tray_visible {
        cfg.tray_visible = true;
    }
    cfg
}

pub fn save(cfg: &Config) {
    let path = config_path();
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    if let Ok(text) = serde_json::to_string_pretty(cfg) {
        let _ = std::fs::write(path, text);
    }
}
