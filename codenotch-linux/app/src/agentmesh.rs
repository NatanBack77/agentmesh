use serde::{Deserialize, Serialize};
use std::time::Duration;

// agentmesh's `serve` command defaults to 8990 and walks upward when that
// port is taken (see cmd/agentmesh/main.go baseURL/ensureServer), so a few
// candidates above the default cover the common case without a discovery
// file on disk.
const PORT_RANGE: std::ops::RangeInclusive<u16> = 8990..=8999;

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct AgentmeshAgent {
    pub name: String,
    pub provider: String,
    pub status: String,
    pub attention: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ProviderQuota {
    pub provider: String,
    pub available: bool,
    pub error: String,
    pub plan_type: String,
    pub session_pct: f64,
    pub week_pct: f64,
    pub session_resets_at: String,
    pub week_resets_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct AgentmeshSummary {
    pub connected: bool,
    pub port: Option<u16>,
    pub total_agents: i64,
    pub processing_agents: i64,
    pub needs_attention: i64,
    pub agents: Vec<AgentmeshAgent>,
    pub quotas: Vec<ProviderQuota>,
}

pub fn discover_port() -> Option<u16> {
    for port in PORT_RANGE {
        let url = format!("http://127.0.0.1:{port}/agents");
        if ureq::get(&url).timeout(Duration::from_millis(200)).call().is_ok() {
            return Some(port);
        }
    }
    None
}

pub fn fetch() -> AgentmeshSummary {
    let Some(port) = discover_port() else {
        return AgentmeshSummary::default();
    };

    let url = format!("http://127.0.0.1:{port}/dashboard/data");
    let Ok(resp) = ureq::get(&url).timeout(Duration::from_millis(800)).call() else {
        return AgentmeshSummary {
            connected: false,
            port: Some(port),
            ..Default::default()
        };
    };
    let Ok(value) = resp.into_json::<serde_json::Value>() else {
        return AgentmeshSummary {
            connected: false,
            port: Some(port),
            ..Default::default()
        };
    };

    let summary = value.get("summary").cloned().unwrap_or_default();
    let agents = value
        .get("agents")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .take(6)
                .map(|a| AgentmeshAgent {
                    name: a.get("name").and_then(|v| v.as_str()).unwrap_or("").into(),
                    provider: a.get("provider").and_then(|v| v.as_str()).unwrap_or("").into(),
                    status: a.get("status").and_then(|v| v.as_str()).unwrap_or("").into(),
                    attention: a.get("attention").and_then(|v| v.as_bool()).unwrap_or(false),
                })
                .collect()
        })
        .unwrap_or_default();
    let quotas = value
        .get("quotas")
        .and_then(|v| v.get("providers"))
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .map(|q| ProviderQuota {
                    provider: q
                        .get("provider")
                        .and_then(|v| v.as_str())
                        .unwrap_or("")
                        .into(),
                    available: q
                        .get("available")
                        .and_then(|v| v.as_bool())
                        .unwrap_or(false),
                    error: q
                        .get("error")
                        .and_then(|v| v.as_str())
                        .unwrap_or("")
                        .into(),
                    plan_type: q
                        .get("plan_type")
                        .and_then(|v| v.as_str())
                        .unwrap_or("")
                        .into(),
                    session_pct: q
                        .get("session_pct")
                        .and_then(|v| v.as_f64())
                        .unwrap_or(0.0),
                    week_pct: q
                        .get("week_pct")
                        .and_then(|v| v.as_f64())
                        .unwrap_or(0.0),
                    session_resets_at: q
                        .get("session_resets_at")
                        .and_then(|v| v.as_str())
                        .unwrap_or("")
                        .into(),
                    week_resets_at: q
                        .get("week_resets_at")
                        .and_then(|v| v.as_str())
                        .unwrap_or("")
                        .into(),
                })
                .collect()
        })
        .unwrap_or_default();

    AgentmeshSummary {
        connected: true,
        port: Some(port),
        total_agents: summary.get("total_agents").and_then(|v| v.as_i64()).unwrap_or(0),
        processing_agents: summary.get("processing_agents").and_then(|v| v.as_i64()).unwrap_or(0),
        needs_attention: summary.get("needs_attention").and_then(|v| v.as_i64()).unwrap_or(0),
        agents,
        quotas,
    }
}
