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

/// Must run before GTK/WebKit initialization. Software rendering is enabled
/// proactively for Wayland: an EGL initialization failure can abort the
/// process, so there is no safe in-process opportunity to detect and retry it.
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

    if wayland {
        std::env::set_var("WEBKIT_DISABLE_COMPOSITING_MODE", "1");
        std::env::set_var("LIBGL_ALWAYS_SOFTWARE", "1");
        logging::info(format!(
            "Wayland renderer fallback enabled (KDE={kde}, WEBKIT_DISABLE_COMPOSITING_MODE=1, LIBGL_ALWAYS_SOFTWARE=1)"
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
    use super::{filter_incompatible_gtk_modules, is_kde, is_wayland};

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

}
