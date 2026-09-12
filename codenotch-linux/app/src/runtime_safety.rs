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

fn nvidia_proprietary_driver_present() -> bool {
    std::path::Path::new("/proc/driver/nvidia/version").exists()
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

    let renderer_override = std::env::var("MESHNOTCH_FORCE_SOFTWARE_RENDERER").ok();
    let nvidia_proprietary = nvidia_proprietary_driver_present();
    let force_software_renderer =
        should_force_software_renderer(renderer_override.as_deref(), nvidia_proprietary);
    let renderer_source =
        renderer_decision_source(renderer_override.as_deref(), nvidia_proprietary);
    if wayland && force_software_renderer {
        std::env::set_var("WEBKIT_DISABLE_COMPOSITING_MODE", "1");
        std::env::set_var("LIBGL_ALWAYS_SOFTWARE", "1");
        logging::info(format!(
            "Wayland software renderer enabled ({renderer_source}, KDE={kde}, WEBKIT_DISABLE_COMPOSITING_MODE=1, LIBGL_ALWAYS_SOFTWARE=1)"
        ));
    } else if wayland {
        logging::info(format!("native Wayland WebKit renderer enabled ({renderer_source})"));
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
    use super::{filter_incompatible_gtk_modules, is_kde, is_wayland, should_force_software_renderer};

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

}
