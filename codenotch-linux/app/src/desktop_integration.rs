use crate::platform;
use std::path::{Path, PathBuf};
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

/// Extracts the path inside `Exec="<path>" %U` from a desktop entry body.
fn exec_target(contents: &str) -> Option<&str> {
    let line = contents.lines().find(|line| line.starts_with("Exec="))?;
    let rest = line.strip_prefix("Exec=\"")?;
    rest.split('"').next()
}

fn entry_needs_refresh(existing_contents: Option<&str>) -> bool {
    match existing_contents {
        Some(contents) => {
            !contents.contains("Keywords=")
                // Self-heal a stale Exec target: e.g. an AppImage first run from
                // a temp/download mount whose path was later cleaned up, or the
                // AppImage moved without using Settings -> Add to application
                // menu. Without this the launcher entry stays permanently
                // broken since nothing else ever rewrites it.
                || exec_target(contents).is_some_and(|path| !Path::new(path).exists())
        }
        None => true,
    }
}

/// Install on first launch, and migrate existing entries written before
/// Keywords/StartupWMClass were added (older MeshNotch versions), or whose
/// Exec target no longer exists on disk, so upgrading via auto-update also
/// fixes search discoverability and dangling launcher entries without a
/// manual step. Otherwise keep the entry unchanged — a user's own edits are
/// left alone.
pub fn install_if_missing() -> Result<(), String> {
    let desktop_file = desktop_file_path()?;
    let existing = std::fs::read_to_string(&desktop_file).ok();
    if !entry_needs_refresh(existing.as_deref()) {
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

    #[test]
    fn refreshes_entries_missing_or_predating_keywords() {
        use super::entry_needs_refresh;
        assert!(entry_needs_refresh(None));
        assert!(entry_needs_refresh(Some(
            "[Desktop Entry]\nName=MeshNotch\nExec=/x %U\nIcon=meshnotch\n"
        )));
        let existing = format!(
            "[Desktop Entry]\nName=MeshNotch\nKeywords=claude;codex;\nExec=\"{}\" %U\n",
            std::env::current_exe().unwrap().display()
        );
        assert!(!entry_needs_refresh(Some(&existing)));
    }

    #[test]
    fn refreshes_entries_whose_exec_target_no_longer_exists() {
        use super::entry_needs_refresh;
        // Regression test: a first launch from a temp AppImage mount/extraction
        // (e.g. `--appimage-extract-and-run` on a FUSE-less system) or a moved
        // AppImage leaves a dangling Exec path — the entry must self-heal
        // instead of staying permanently broken.
        let dangling = format!(
            "[Desktop Entry]\nName=MeshNotch\nKeywords=claude;codex;\nExec=\"/tmp/does-not-exist-{}\" %U\n",
            std::process::id()
        );
        assert!(entry_needs_refresh(Some(&dangling)));

        let existing = format!(
            "[Desktop Entry]\nName=MeshNotch\nKeywords=claude;codex;\nExec=\"{}\" %U\n",
            std::env::current_exe().unwrap().display()
        );
        assert!(!entry_needs_refresh(Some(&existing)));
    }
}
