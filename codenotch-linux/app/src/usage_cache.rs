use crate::provider::{now_ms, ProviderSnapshot};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::{Mutex, OnceLock};
use std::time::Duration;

const BASE_BACKOFF_MS: u64 = 5 * 60 * 1000;
const MAX_BACKOFF_MS: u64 = 30 * 60 * 1000;
const SPEND_CAP_PROBE_MS: u64 = 24 * 60 * 60 * 1000;
const DEFAULT_POLL_MS: u64 = 5 * 60 * 1000;

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
struct ProviderUsageState {
    #[serde(default)]
    last_429_at: Option<u64>,
    #[serde(default)]
    retry_at: Option<u64>,
    #[serde(default)]
    consecutive_429: u32,
    #[serde(default)]
    spend_cap: bool,
    #[serde(default)]
    last_good: Option<ProviderSnapshot>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
struct UsageCacheFile {
    #[serde(default)]
    providers: BTreeMap<String, ProviderUsageState>,
}

static CACHE: OnceLock<Mutex<UsageCacheFile>> = OnceLock::new();

fn path() -> PathBuf {
    crate::config::data_dir().join("usage-cache.json")
}

fn cache() -> &'static Mutex<UsageCacheFile> {
    CACHE.get_or_init(|| {
        let loaded = std::fs::read_to_string(path())
            .ok()
            .and_then(|text| serde_json::from_str(&text).ok())
            .unwrap_or_default();
        Mutex::new(loaded)
    })
}

pub fn next_poll_delay() -> Duration {
    let now = now_ms();
    let cache = cache().lock().unwrap();
    let until_retry = cache.providers.values()
        .filter_map(|state| state.retry_at)
        .filter(|retry_at| *retry_at > now)
        .map(|retry_at| retry_at - now)
        .min()
        .unwrap_or(DEFAULT_POLL_MS);
    Duration::from_millis(until_retry.min(DEFAULT_POLL_MS))
}

fn persist(cache: &UsageCacheFile) {
    let path = path();
    let Some(parent) = path.parent() else { return; };
    if std::fs::create_dir_all(parent).is_err() { return; }
    let Ok(text) = serde_json::to_vec_pretty(cache) else { return; };
    let temporary = path.with_extension("json.tmp");
    if std::fs::write(&temporary, text).is_ok() {
        let _ = std::fs::rename(temporary, path);
    }
}

pub fn cached_or_backoff(provider: &str, fallback: ProviderSnapshot) -> Option<ProviderSnapshot> {
    let cache = cache().lock().unwrap();
    let state = cache.providers.get(provider)?;
    let retry_at = state.retry_at?;
    if now_ms() >= retry_at {
        return None;
    }
    let mut snapshot = state.last_good.clone().unwrap_or(fallback);
    snapshot.status = if state.spend_cap { "stale" } else { "backoff" }.into();
    snapshot.next_retry_at = Some(retry_at);
    if state.spend_cap {
        snapshot.note = "Uso pausado por limite; nova verificação agendada mais tarde.".into();
    } else {
        snapshot.note = "Limite temporário; nova verificação agendada.".into();
    }
    Some(snapshot)
}

pub fn record_success(provider: &str, mut snapshot: ProviderSnapshot) -> ProviderSnapshot {
    snapshot.status = "ok".into();
    snapshot.available = true;
    snapshot.next_retry_at = None;
    let mut cache = cache().lock().unwrap();
    cache.providers.insert(provider.into(), ProviderUsageState {
        last_good: Some(snapshot.clone()),
        ..Default::default()
    });
    persist(&cache);
    snapshot
}

pub fn preserve_failure(
    provider: &str,
    fallback: ProviderSnapshot,
    status: &str,
    note: String,
) -> ProviderSnapshot {
    let cache = cache().lock().unwrap();
    let cached = cache.providers.get(provider).and_then(|state| state.last_good.clone());
    let has_cached_data = cached.is_some();
    let mut snapshot = cached.unwrap_or(fallback);
    snapshot.status = if has_cached_data && status == "error" {
        "stale".into()
    } else {
        status.into()
    };
    snapshot.next_retry_at = None;
    snapshot.note = note;
    snapshot
}

fn jittered_backoff(provider: &str, failures: u32, retry_after_secs: Option<u64>) -> u64 {
    if let Some(seconds) = retry_after_secs {
        let base = seconds.saturating_mul(1000);
        // Positive-only jitter keeps the client from retrying before Retry-After.
        let jitter = base.saturating_mul(jitter_percent(provider, failures, 6) as u64) / 100;
        return base.saturating_add(jitter);
    }
    let exponent = failures.saturating_sub(1).min(3);
    let base = BASE_BACKOFF_MS.saturating_mul(1u64 << exponent).min(MAX_BACKOFF_MS);
    let delta = base.saturating_mul(jitter_percent(provider, failures, 21) as u64) / 100;
    if delta == 0 { return base; }
    // Symmetric jitter in approximately [-10%, +10%].
    let seed = jitter_seed(provider, failures);
    if seed & 1 == 0 { base.saturating_sub(delta / 2) } else { base.saturating_add(delta / 2).min(MAX_BACKOFF_MS) }
}

fn jitter_percent(provider: &str, failures: u32, range: u32) -> u32 {
    (jitter_seed(provider, failures) % range as u64) as u32
}

fn jitter_seed(provider: &str, failures: u32) -> u64 {
    let clock = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)
        .map(|duration| duration.as_nanos() as u64).unwrap_or_default();
    provider.bytes().fold(clock ^ u64::from(failures), |seed, byte| seed.rotate_left(5) ^ u64::from(byte))
}

pub fn record_rate_limit(
    provider: &str,
    fallback: ProviderSnapshot,
    retry_after_secs: Option<u64>,
    spend_cap: bool,
) -> ProviderSnapshot {
    let now = now_ms();
    let mut cache = cache().lock().unwrap();
    let state = cache.providers.entry(provider.into()).or_default();
    state.last_429_at = Some(now);
    state.consecutive_429 = state.consecutive_429.saturating_add(1);
    state.spend_cap = spend_cap;
    let delay = if spend_cap {
        SPEND_CAP_PROBE_MS
    } else {
        jittered_backoff(provider, state.consecutive_429, retry_after_secs)
    };
    let retry_at = now.saturating_add(delay);
    state.retry_at = Some(retry_at);

    let mut snapshot = state.last_good.clone().unwrap_or(fallback);
    snapshot.status = if spend_cap { "stale" } else { "backoff" }.into();
    snapshot.next_retry_at = Some(retry_at);
    snapshot.note = if spend_cap {
        "Uso pausado por limite; nova verificação agendada mais tarde.".into()
    } else {
        "Limite temporário; nova verificação agendada.".into()
    };
    persist(&cache);
    snapshot
}

pub fn is_spend_cap_response(body: &str) -> bool {
    let lower = body.to_lowercase();
    if let Ok(value) = serde_json::from_str::<serde_json::Value>(body) {
        if value.pointer("/error/details/error_code").and_then(|v| v.as_str())
            == Some("enforced_spend_limit_reached") {
            return true;
        }
    }
    lower.contains("spend cap")
        || lower.contains("spend limit")
        || lower.contains("usage tier")
        || lower.contains("usage limit")
        || lower.contains("access resumes")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detects_spend_cap_payload() {
        assert!(is_spend_cap_response(r#"{"error":{"details":{"error_code":"enforced_spend_limit_reached"}}}"#));
        assert!(!is_spend_cap_response(r#"{"error":{"type":"rate_limit_error","message":"Too many requests"}}"#));
    }

    #[test]
    fn backoff_grows_and_respects_cap() {
        let a = jittered_backoff("claude", 1, None);
        let b = jittered_backoff("claude", 2, None);
        let c = jittered_backoff("claude", 3, None);
        let d = jittered_backoff("claude", 4, None);
        assert!((270_000..=330_000).contains(&a));
        assert!((540_000..=660_000).contains(&b));
        assert!((1_080_000..=1_320_000).contains(&c));
        assert!(d <= MAX_BACKOFF_MS);
        assert!(jittered_backoff("claude", 1, Some(120)) >= 120_000);
    }
}
