use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::process::Command;

fn resolve_stable_exe_path(
    appimage: Option<OsString>,
    appdir: Option<OsString>,
    current_exe: PathBuf,
) -> PathBuf {
    let exe = current_exe.to_string_lossy();
    // FUSE-mounted AppImages run from /tmp/.mount_*; the FUSE-less
    // `--appimage-extract-and-run` fallback (documented in the README for
    // systems without libfuse2, e.g. Fedora) instead extracts to
    // $APPDIR=/tmp/appimage_extracted_<hash> and runs straight from there.
    // Both are equally temporary, so both must be redirected to the stable
    // $APPIMAGE path — otherwise the extracted path gets baked into the
    // application menu entry and breaks the launcher once it's cleaned up.
    let mounted_from_appimage = exe.starts_with("/tmp/.mount")
        || exe.contains(".mount_")
        || exe.contains("/appimage_extracted_")
        || appdir
            .filter(|dir| !dir.is_empty())
            .is_some_and(|dir| exe.starts_with(dir.to_string_lossy().as_ref()));

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
        std::env::var_os("APPDIR"),
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
                None,
                PathBuf::from("/tmp/.mount_x/usr/bin/meshnotch"),
            ),
            PathBuf::from("/home/user/MeshNotch.AppImage")
        );
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/Maestri.AppImage")),
                None,
                PathBuf::from("/workspace/target/debug/meshnotch"),
            ),
            PathBuf::from("/workspace/target/debug/meshnotch")
        );
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/MeshNotch.AppImage")),
                None,
                PathBuf::from("/opt/meshnotch/.mount_contents/usr/bin/meshnotch"),
            ),
            PathBuf::from("/home/user/MeshNotch.AppImage")
        );
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::new()),
                None,
                PathBuf::from("/tmp/.mount_x/usr/bin/meshnotch"),
            ),
            PathBuf::from("/tmp/.mount_x/usr/bin/meshnotch")
        );
    }

    #[test]
    fn appimage_path_is_used_for_fuse_less_extract_and_run() {
        // README-documented Fedora workaround: `--appimage-extract-and-run`
        // extracts to $APPDIR=/tmp/appimage_extracted_<hash> and runs the
        // binary straight from there instead of a FUSE mount.
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/MeshNotch.AppImage")),
                Some(OsString::from("/tmp/appimage_extracted_deadbeef")),
                PathBuf::from("/tmp/appimage_extracted_deadbeef/usr/bin/meshnotch"),
            ),
            PathBuf::from("/home/user/MeshNotch.AppImage")
        );
        // Same extraction naming convention still matches even without an
        // exact $APPDIR value (older/unusual runtimes).
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/MeshNotch.AppImage")),
                None,
                PathBuf::from("/tmp/appimage_extracted_deadbeef/usr/bin/meshnotch"),
            ),
            PathBuf::from("/home/user/MeshNotch.AppImage")
        );
        // A stale/foreign $APPIMAGE inherited from a wrapping process (e.g.
        // this app launched as a child of another AppImage) must not be
        // trusted when the current exe isn't actually running from that
        // AppImage's own extraction directory.
        assert_eq!(
            resolve_stable_exe_path(
                Some(OsString::from("/home/user/Other.AppImage")),
                Some(OsString::from("/tmp/appimage_extracted_other")),
                PathBuf::from("/workspace/target/debug/meshnotch"),
            ),
            PathBuf::from("/workspace/target/debug/meshnotch")
        );
    }
}
