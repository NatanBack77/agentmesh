use crate::platform;
use std::path::PathBuf;
use std::process::Command;

const APP_ICON: &[u8] = include_bytes!("../icons/icon.png");

fn applications_dir() -> Result<PathBuf, String> {
    dirs::data_local_dir()
        .map(|path| path.join("applications"))
        .ok_or_else(|| "could not determine the user's local data directory".into())
}

fn icon_dir() -> Result<PathBuf, String> {
    dirs::data_local_dir()
        .map(|path| path.join("icons/hicolor/32x32/apps"))
        .ok_or_else(|| "could not determine the user's local data directory".into())
}

fn desktop_file_path() -> Result<PathBuf, String> {
    Ok(applications_dir()?.join("meshnotch.desktop"))
}

fn refresh_desktop_caches() {
    if let Ok(path) = applications_dir() {
        let _ = Command::new("update-desktop-database").arg(path).status();
    }
    if let Some(data_dir) = dirs::data_local_dir() {
        let _ = Command::new("gtk-update-icon-cache")
            .args(["-f", "-t"])
            .arg(data_dir.join("icons/hicolor"))
            .status();
    }
}

fn write_entry() -> Result<(), String> {
    let executable = platform::stable_exe_path();
    if executable.as_os_str().is_empty() {
        return Err("could not determine the MeshNotch executable path".into());
    }

    let desktop_file = desktop_file_path()?;
    let icons = icon_dir()?;
    std::fs::create_dir_all(&desktop_file.parent().unwrap())
        .map_err(|error| format!("could not create applications directory: {error}"))?;
    std::fs::create_dir_all(&icons)
        .map_err(|error| format!("could not create icon directory: {error}"))?;

    let exec = platform::desktop_exec_arg(&executable);
    let body = format!(
        "[Desktop Entry]\nVersion=1.0\nType=Application\nName=MeshNotch\nGenericName=AI Usage Monitor\nComment=Track Claude, Codex and Cursor usage from a notch on your screen\nExec={exec} %U\nIcon=meshnotch\nCategories=Utility;\nKeywords=claude;codex;cursor;agentmesh;usage;quota;notch;ai;\nStartupWMClass=MeshNotch\nTerminal=false\n"
    );
    std::fs::write(&desktop_file, body)
        .map_err(|error| format!("could not write application menu entry: {error}"))?;
    std::fs::write(icons.join("meshnotch.png"), APP_ICON)
        .map_err(|error| format!("could not install MeshNotch icon: {error}"))?;

    refresh_desktop_caches();
    Ok(())
}

/// Install on first launch only; keep an existing entry unchanged until the
/// user explicitly asks to refresh it from Settings.
pub fn install_if_missing() -> Result<(), String> {
    let desktop_file = desktop_file_path()?;
    if desktop_file.is_file() {
        return Ok(());
    }
    write_entry()
}

/// Create or refresh the application menu entry and icon using the current
/// stable executable path.
pub fn add_to_app_menu() -> Result<(), String> {
    write_entry()
}

#[cfg(test)]
mod tests {
    #[test]
    fn desktop_entry_has_expected_launcher_contract() {
        let exec = crate::platform::desktop_exec_arg(std::path::Path::new("/home/a user/MeshNotch.AppImage"));
        let entry = format!(
            "[Desktop Entry]\nVersion=1.0\nType=Application\nName=MeshNotch\nGenericName=AI Usage Monitor\nComment=Track Claude, Codex and Cursor usage from a notch on your screen\nExec={exec} %U\nIcon=meshnotch\nCategories=Utility;\nKeywords=claude;codex;cursor;agentmesh;usage;quota;notch;ai;\nStartupWMClass=MeshNotch\nTerminal=false\n"
        );
        assert!(entry.contains("Exec=\"/home/a user/MeshNotch.AppImage\" %U"));
        assert!(entry.contains("Icon=meshnotch"));
        assert!(entry.contains("Categories=Utility;"));
        assert!(entry.contains("Keywords=claude;codex;cursor;agentmesh;usage;quota;notch;ai;"));
        assert!(entry.contains("StartupWMClass=MeshNotch"));
    }
}
