use crate::logging;

fn is_wayland(session_type: Option<&str>, wayland_display: Option<&str>) -> bool {
    session_type.is_some_and(|value| value.eq_ignore_ascii_case("wayland"))
        || wayland_display.is_some_and(|value| !value.is_empty())
}

fn is_kde(desktop: Option<&str>, kde_full_session: Option<&str>, session: Option<&str>) -> bool {
    desktop.is_some_and(|value| {
        let desktop = value.to_ascii_lowercase();
        desktop.contains("kde") || desktop.contains("plasma")
    }) || kde_full_session.is_some_and(|value| value.eq_ignore_ascii_case("true"))
        || session.is_some_and(|value| {
            let session = value.to_ascii_lowercase();
            session.contains("kde") || session.contains("plasma")
        })
}

fn is_gnome(desktop: Option<&str>, gnome_session_id: Option<&str>) -> bool {
    desktop.is_some_and(|value| value.to_ascii_lowercase().contains("gnome"))
        || gnome_session_id.is_some_and(|value| !value.is_empty())
}

fn nvidia_proprietary_driver_present() -> bool {
    std::path::Path::new("/proc/driver/nvidia/version").exists()
}

/// GNOME's Mutter compositor doesn't honor always-on-top for regular
/// toplevel windows on Wayland the way KWin/Sway do (confirmed by upstream
/// reports: tauri-apps/tauri#3117, #13121, tauri-apps/tao#1134) — an
/// always-on-top window like the notch gets buried the moment focus moves
/// to another window. gtk-layer-shell, the usual fix for this class of
/// problem, explicitly doesn't support GNOME either. Running the webview
/// through XWayland instead (GDK_BACKEND=x11) is the one mitigation with
/// real evidence of fixing this exact symptom, at the cost of the window
/// going through the X11 compat layer instead of native Wayland.
fn should_force_x11_backend(explicit: Option<&str>, wayland: bool, gnome: bool) -> bool {
    match explicit.map(|value| value.trim().to_ascii_lowercase()) {
        Some(value) if matches!(value.as_str(), "1" | "true" | "yes") => true,
        Some(value) if matches!(value.as_str(), "0" | "false" | "no") => false,
        _ => wayland && gnome,
    }
}

fn should_force_software_renderer(explicit: Option<&str>, nvidia_proprietary: bool) -> bool {
    match explicit.map(|value| value.trim().to_ascii_lowercase()) {
        Some(value) if matches!(value.as_str(), "1" | "true" | "yes") => true,
        Some(value) if matches!(value.as_str(), "0" | "false" | "no") => false,
        _ => nvidia_proprietary,
    }
}

fn renderer_decision_source(explicit: Option<&str>, nvidia_proprietary: bool) -> &'static str {
    match explicit.map(|value| value.trim().to_ascii_lowercase()) {
        Some(value) if matches!(value.as_str(), "1" | "true" | "yes" | "0" | "false" | "no") => {
            "explicit MESHNOTCH_FORCE_SOFTWARE_RENDERER override"
        }
        _ if nvidia_proprietary => "NVIDIA proprietary driver detected",
        _ => "native renderer default",
    }
}

fn filter_incompatible_gtk_modules(modules: &str) -> (String, Vec<String>) {
    let mut removed = Vec::new();
    let retained = modules
        .split(':')
        .filter(|module| !module.is_empty())
        .filter(|module| {
            let normalized = module.to_ascii_lowercase();
            let incompatible = normalized.contains("appmenu-gtk-module")
                || normalized.contains("window-decorations-gtk-module");
            if incompatible {
                removed.push((*module).to_owned());
            }
            !incompatible
        })
        .collect::<Vec<_>>()
        .join(":");
    (retained, removed)
}

/// Must run before GTK/WebKit initialization. NVIDIA proprietary drivers use
/// the software fallback by default; other Wayland systems use native EGL.
pub fn prepare_before_gtk() {
    let wayland = is_wayland(
        std::env::var("XDG_SESSION_TYPE").ok().as_deref(),
        std::env::var("WAYLAND_DISPLAY").ok().as_deref(),
    );
    let kde = is_kde(
        std::env::var("XDG_CURRENT_DESKTOP").ok().as_deref(),
        std::env::var("KDE_FULL_SESSION").ok().as_deref(),
        std::env::var("DESKTOP_SESSION").ok().as_deref(),
    );

    let gnome = is_gnome(
        std::env::var("XDG_CURRENT_DESKTOP").ok().as_deref(),
        std::env::var("GNOME_DESKTOP_SESSION_ID").ok().as_deref(),
    );
    let x11_backend_override = std::env::var("MESHNOTCH_FORCE_X11").ok();
    let force_x11_backend = should_force_x11_backend(x11_backend_override.as_deref(), wayland, gnome);
    // Only override GDK_BACKEND if the user hasn't already picked one
    // themselves, unless they explicitly asked for this fallback.
    let user_set_backend = std::env::var("GDK_BACKEND").is_ok();
    let explicit_x11_request = x11_backend_override
        .as_deref()
        .is_some_and(|value| matches!(value.trim().to_ascii_lowercase().as_str(), "1" | "true" | "yes"));
    let apply_x11_backend = force_x11_backend && (!user_set_backend || explicit_x11_request);
    if apply_x11_backend {
        std::env::set_var("GDK_BACKEND", "x11");
    }
    // Running through XWayland (GLX) instead of native Wayland (EGL) means
    // the EGL-specific renderer decision below no longer applies the same
    // way; the software-renderer override is still respected verbatim since
    // NVIDIA-proprietary + GLX can also need it.
    let effectively_wayland = wayland && !apply_x11_backend;

    let renderer_override = std::env::var("MESHNOTCH_FORCE_SOFTWARE_RENDERER").ok();
    let nvidia_proprietary = nvidia_proprietary_driver_present();
    let force_software_renderer =
        should_force_software_renderer(renderer_override.as_deref(), nvidia_proprietary);
    let renderer_source =
        renderer_decision_source(renderer_override.as_deref(), nvidia_proprietary);
    if effectively_wayland && force_software_renderer {
        std::env::set_var("WEBKIT_DISABLE_COMPOSITING_MODE", "1");
        std::env::set_var("LIBGL_ALWAYS_SOFTWARE", "1");
        logging::info(format!(
            "Wayland software renderer enabled ({renderer_source}, KDE={kde}, WEBKIT_DISABLE_COMPOSITING_MODE=1, LIBGL_ALWAYS_SOFTWARE=1)"
        ));
    } else if effectively_wayland {
        logging::info(format!("native Wayland WebKit renderer enabled ({renderer_source})"));
    } else if apply_x11_backend {
        // GNOME's Mutter doesn't honor always-on-top for regular toplevels
        // on Wayland (tauri-apps/tauri#3117, #13121, tao#1134), and
        // gtk-layer-shell doesn't support GNOME either. XWayland is the one
        // mitigation with real evidence of fixing it; set
        // MESHNOTCH_FORCE_X11=0 to opt back into native Wayland.
        logging::info(format!(
            "GNOME Wayland detected: forcing GDK_BACKEND=x11 (XWayland) so the notch keeps its \
             always-on-top behavior across focus changes ({renderer_source} still applies under X11/GLX); \
             set MESHNOTCH_FORCE_X11=0 to opt out"
        ));
    }

    if wayland && kde {
        // KDE injects GTK modules built against the host GTK. AppImages may
        // load a different GTK/WebKit stack, so remove only the incompatible
        // modules and retain accessibility and other GTK modules.
        if let Ok(modules) = std::env::var("GTK_MODULES") {
            let (filtered, removed) = filter_incompatible_gtk_modules(&modules);
            if !removed.is_empty() {
                std::env::set_var("GTK_MODULES", filtered);
                logging::info(format!(
                    "filtered incompatible KDE GTK modules: {}",
                    removed.join(", ")
                ));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{
        filter_incompatible_gtk_modules, is_gnome, is_kde, is_wayland,
        should_force_software_renderer, should_force_x11_backend,
    };

    #[test]
    fn detects_kde_wayland_without_affecting_other_sessions() {
        assert!(is_wayland(Some("wayland"), None));
        assert!(is_wayland(None, Some("wayland-0")));
        assert!(!is_wayland(Some("x11"), None));
        assert!(is_kde(Some("KDE"), None, None));
        assert!(is_kde(Some("plasma"), None, None));
        assert!(!is_kde(Some("GNOME"), None, Some("gnome")));
    }

    #[test]
    fn removes_only_the_incompatible_gtk_modules() {
        assert_eq!(
            filter_incompatible_gtk_modules(
                "atk-bridge:appmenu-gtk-module:window-decorations-gtk-module"
            ),
            (
                "atk-bridge".into(),
                vec![
                    "appmenu-gtk-module".to_string(),
                    "window-decorations-gtk-module".to_string()
                ]
            )
        );
    }

    #[test]
    fn selects_renderer_from_nvidia_detection_unless_explicitly_overridden() {
        assert!(should_force_software_renderer(None, true));
        assert!(!should_force_software_renderer(None, false));
        assert!(should_force_software_renderer(Some("1"), false));
        assert!(!should_force_software_renderer(Some("0"), true));
        assert!(should_force_software_renderer(Some(" TRUE "), false));
        assert!(!should_force_software_renderer(Some("No"), true));
        assert!(should_force_software_renderer(Some("invalid"), true));
        assert!(!should_force_software_renderer(Some("invalid"), false));
    }

    #[test]
    fn detects_gnome_without_affecting_other_desktops() {
        assert!(is_gnome(Some("GNOME"), None));
        assert!(is_gnome(Some("ubuntu:GNOME"), None));
        assert!(is_gnome(None, Some("this-is-a-session-id")));
        assert!(!is_gnome(Some("KDE"), None));
        assert!(!is_gnome(None, None));
    }

    #[test]
    fn forces_x11_backend_only_for_gnome_wayland_unless_overridden() {
        // Mutter (GNOME's Wayland compositor) doesn't honor always-on-top
        // for regular toplevels, so this combination needs the XWayland
        // fallback; every other combination should be left alone.
        assert!(should_force_x11_backend(None, true, true));
        assert!(!should_force_x11_backend(None, true, false));
        assert!(!should_force_x11_backend(None, false, true));
        assert!(!should_force_x11_backend(None, false, false));
        // Explicit override wins regardless of detection.
        assert!(should_force_x11_backend(Some("1"), false, false));
        assert!(!should_force_x11_backend(Some("0"), true, true));
    }
}
