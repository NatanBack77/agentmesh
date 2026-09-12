use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::process::Command;

fn resolve_stable_exe_path(appimage: Option<OsString>, current_exe: PathBuf) -> PathBuf {
    let mounted_from_appimage = current_exe
        .to_string_lossy()
        .starts_with("/tmp/.mount")
        || current_exe.to_string_lossy().contains(".mount_");

    if mounted_from_appimage {
        if let Some(path) = appimage.filter(|path| !path.is_empty()) {
            return PathBuf::from(path);
        }
    }

    current_exe
}

/// Returns the persistent AppImage path when running from an AppImage, or the
/// native executable path when running from a development/system install.
pub fn stable_exe_path() -> PathBuf {
    resolve_stable_exe_path(
        std::env::var_os("APPIMAGE"),
        std::env::current_exe().unwrap_or_default(),
    )
}

pub fn desktop_exec_arg(path: &Path) -> String {
    let escaped = path
        .to_string_lossy()
        .replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('`', "\\`")
        .replace('$', "\\$")
        .replace('%', "%%");
    format!("\"{escaped}\"")
}

pub fn open_path(path: &Path) -> Result<(), String> {
    Command::new("xdg-open")
        .arg(path)
        .spawn()
        .map(|_| ())
        .map_err(|e| format!("xdg-open failed: {e}"))
}

pub fn open_url(url: &str) -> Result<(), String> {
    Command::new("xdg-open")
        .arg(url)
        .spawn()
        .map(|_| ())
        .map_err(|e| format!("xdg-open failed: {e}"))
}

pub fn session_type() -> String {
    std::env::var("XDG_SESSION_TYPE").unwrap_or_else(|_| "unknown".into())
}

#[cfg(test)]
mod tests {
    use super::resolve_stable_exe_path;
    use std::ffi::OsString;
    use std::path::PathBuf;

    #[test]
    fn appimage_path_is_used_only_for_an_appimage_mount() {
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/MeshNotch.AppImage")),
                PathBuf::from("/tmp/.mount_x/usr/bin/meshnotch"),
            ),
            PathBuf::from("/home/user/MeshNotch.AppImage")
        );
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/Maestri.AppImage")),
                PathBuf::from("/workspace/target/debug/meshnotch"),
            ),
            PathBuf::from("/workspace/target/debug/meshnotch")
        );
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/MeshNotch.AppImage")),
                PathBuf::from("/opt/meshnotch/.mount_contents/usr/bin/meshnotch"),
            ),
            PathBuf::from("/home/user/MeshNotch.AppImage")
        );
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::new()),
                PathBuf::from("/tmp/.mount_x/usr/bin/meshnotch"),
            ),
            PathBuf::from("/tmp/.mount_x/usr/bin/meshnotch")
        );
    }
}
