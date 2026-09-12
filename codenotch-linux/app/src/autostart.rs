use crate::config;
use crate::platform;
use std::path::PathBuf;

fn autostart_path() -> PathBuf {
    dirs::config_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join("autostart")
        .join("meshnotch.desktop")
}

pub fn is_enabled() -> bool {
    autostart_path().is_file()
}

pub fn enable() -> Result<String, String> {
    let exe = platform::stable_exe_path();
    if exe.as_os_str().is_empty() {
        return Err("could not determine the MeshNotch executable path".into());
    }
    let path = autostart_path();
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    }
    let body = format!(
        "[Desktop Entry]\nType=Application\nName=MeshNotch\nExec={} --silent\nTerminal=false\nX-GNOME-Autostart-enabled=true\n",
        platform::desktop_exec_arg(&exe)
    );
    std::fs::write(&path, body).map_err(|e| e.to_string())?;
    Ok(format!("autostart enabled at {}", path.display()))
}

pub fn disable() -> Result<String, String> {
    let path = autostart_path();
    if path.exists() {
        std::fs::remove_file(&path).map_err(|e| e.to_string())?;
    }
    Ok("autostart disabled".into())
}

pub fn probe() -> String {
    format!(
        "autostart: {} | data: {}",
        if is_enabled() { "enabled" } else { "disabled" },
        config::data_dir().display()
    )
}
