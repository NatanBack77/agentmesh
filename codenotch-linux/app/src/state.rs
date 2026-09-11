use crate::agentmesh::AgentmeshSummary;
use crate::provider::{Activity, ProviderSnapshot};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Snapshot {
    pub providers: Vec<ProviderSnapshot>,
    pub sessions: Vec<Activity>,
    pub theme: String,
    pub notch_visible: bool,
    pub tray_visible: bool,
    pub scale: f64,
    pub session_type: String,
    pub orientation: String,
    pub accent: String,
    pub accent_color: String,
    pub enabled_providers: Vec<String>,
    pub agentmesh_dashboard_url: String,
    pub notch_style: String,
    pub agentmesh: AgentmeshSummary,
}

#[derive(Default)]
pub struct Store {
    providers: BTreeMap<String, ProviderSnapshot>,
    sessions: BTreeMap<String, Activity>,
    agentmesh: AgentmeshSummary,
}

impl Store {
    pub fn set_provider(&mut self, provider: ProviderSnapshot) {
        self.providers.insert(provider.id.clone(), provider);
    }

    pub fn apply_activity(&mut self, activity: Activity) {
        self.sessions.insert(activity.id.clone(), activity);
    }

    pub fn set_agentmesh(&mut self, summary: AgentmeshSummary) {
        self.agentmesh = summary;
    }

    pub fn snapshot(&self, cfg: &crate::config::Config) -> Snapshot {
        Snapshot {
            providers: self.providers.values().cloned().collect(),
            sessions: self.sessions.values().cloned().collect(),
            theme: cfg.theme.clone(),
            notch_visible: cfg.notch_visible,
            tray_visible: cfg.tray_visible,
            scale: cfg.scale,
            session_type: crate::platform::session_type(),
            orientation: cfg.orientation.clone(),
            accent: cfg.accent.clone(),
            accent_color: crate::config::accent_color(&cfg.accent).to_string(),
            enabled_providers: cfg.enabled_providers.clone(),
            agentmesh_dashboard_url: cfg.agentmesh_dashboard_url.clone(),
            notch_style: cfg.notch_style.clone(),
            agentmesh: self.agentmesh.clone(),
        }
    }
}
