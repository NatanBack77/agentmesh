use crate::provider::ProviderSnapshot;

struct RuntimeDefinition {
    id: &'static str,
    label: &'static str,
    glyph: &'static str,
    executables: &'static [&'static str],
    process_names: &'static [&'static str],
}

const RUNTIMES: &[RuntimeDefinition] = &[
    RuntimeDefinition { id: "ollama", label: "Ollama", glyph: "Ol", executables: &["ollama"], process_names: &["ollama"] },
    RuntimeDefinition { id: "lmstudio", label: "LM Studio", glyph: "LM", executables: &["lm-studio", "lmstudio"], process_names: &["lm-studio", "lmstudio", "lm studio"] },
    RuntimeDefinition { id: "gemini-cli", label: "Gemini CLI", glyph: "Ge", executables: &["gemini"], process_names: &["gemini", "gemini-cli"] },
    RuntimeDefinition { id: "opencode", label: "OpenCode", glyph: "OC", executables: &["opencode"], process_names: &["opencode"] },
];

fn executable_available(names: &[&str]) -> bool {
    let Some(path) = std::env::var_os("PATH") else { return false; };
    std::env::split_paths(&path).any(|dir| names.iter().any(|name| dir.join(name).is_file()))
}

#[cfg(target_os = "linux")]
fn process_running(names: &[&str]) -> bool {
    let Ok(entries) = std::fs::read_dir("/proc") else { return false; };
    entries.flatten().any(|entry| {
        let name = entry.file_name();
        if !name.to_string_lossy().chars().all(|c| c.is_ascii_digit()) { return false; }
        let cmdline = std::fs::read_to_string(entry.path().join("cmdline"))
            .unwrap_or_default().replace('\0', " ").to_lowercase();
        let comm = std::fs::read_to_string(entry.path().join("comm"))
            .unwrap_or_default().trim().to_lowercase();
        names.iter().any(|candidate| {
            let candidate = candidate.to_lowercase();
            comm == candidate || cmdline.split_whitespace().any(|part| {
                std::path::Path::new(part).file_name().and_then(|v| v.to_str())
                    .map(|v| v.eq_ignore_ascii_case(&candidate)).unwrap_or(false) || part == candidate
            })
        })
    })
}

#[cfg(not(target_os = "linux"))]
fn process_running(_names: &[&str]) -> bool { false }

fn fetch_one(runtime: &RuntimeDefinition) -> ProviderSnapshot {
    let running = process_running(runtime.process_names);
    let available = executable_available(runtime.executables) || running;
    ProviderSnapshot::local_runtime(runtime.id, runtime.label, runtime.glyph, available, running)
}

pub fn fetch_all() -> Vec<ProviderSnapshot> {
    RUNTIMES.iter().map(fetch_one).collect()
}
