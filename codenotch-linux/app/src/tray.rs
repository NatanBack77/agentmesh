use tauri::menu::{MenuBuilder, MenuItemBuilder};
use tauri::tray::TrayIconBuilder;
use tauri::{AppHandle, Manager};

pub fn setup(app: &AppHandle) -> tauri::Result<()> {
    let menu = MenuBuilder::new(app)
        .item(&MenuItemBuilder::with_id("settings", "Settings").build(app)?)
        .item(&MenuItemBuilder::with_id("refresh", "Refresh").build(app)?)
        .item(&MenuItemBuilder::with_id("reset", "Reset position").build(app)?)
        .separator()
        .item(&MenuItemBuilder::with_id("autostart-on", "Start at sign-in").build(app)?)
        .item(&MenuItemBuilder::with_id("autostart-off", "Disable autostart").build(app)?)
        .item(&MenuItemBuilder::with_id("data", "Open data folder").build(app)?)
        .separator()
        .item(&MenuItemBuilder::with_id("quit", "Quit").build(app)?)
        .build()?;
    let icon = tauri::image::Image::from_bytes(include_bytes!("../icons/tray.png"))?;
    TrayIconBuilder::with_id("main")
        .icon(icon)
        .tooltip("MeshNotch")
        .menu(&menu)
        .show_menu_on_left_click(true)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "settings" => {
                if let Some(window) = app.get_webview_window("settings") {
                    let _ = window.show();
                    let _ = window.unminimize();
                    let _ = window.set_focus();
                }
            }
            "refresh" => crate::refresh_all(app),
            "reset" => crate::reset_notch(app),
            "autostart-on" => {
                let _ = crate::autostart::enable();
            }
            "autostart-off" => {
                let _ = crate::autostart::disable();
            }
            "data" => {
                let _ = std::fs::create_dir_all(crate::config::data_dir());
                let _ = crate::platform::open_path(&crate::config::data_dir());
            }
            "quit" => app.exit(0),
            _ => {}
        })
        .build(app)?;
    Ok(())
}
