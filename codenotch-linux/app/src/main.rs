#![cfg_attr(all(not(debug_assertions), target_os = "windows"), windows_subsystem = "windows")]

mod agentmesh;
mod app_updates;
mod autostart;
mod claude;
mod codex;
mod config;
mod cursor;
mod desktop_integration;
mod local_runtime;
mod logging;
mod platform;
mod provider;
mod runtime_safety;
mod server;
mod state;
mod tray;
mod usage_cache;
mod watcher;

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use tauri::{
    AppHandle, Emitter, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder, WindowEvent,
};

const NOTCH_W_VERTICAL: f64 = 360.0;
const NOTCH_H_VERTICAL: f64 = 420.0;
const NOTCH_W_HORIZONTAL: f64 = 460.0;
const NOTCH_H_HORIZONTAL: f64 = 360.0;

// usage_cache::next_poll_delay() otherwise only refreshes provider usage
// (token/limit percentages) every DEFAULT_POLL_MS (5 minutes), so a turn
// that just finished doesn't show up until the next timer tick. watcher.rs
// already tails ~/.claude/projects for activity state at no extra cost;
// piggybacking a refresh on its "done" transition makes usage feel live
// right when it actually changed, instead of adding a second poller. The
// minimum interval keeps a burst of finishing agents from firing one HTTP
// usage request per agent.
static LAST_ACTIVITY_REFRESH_MS: AtomicU64 = AtomicU64::new(0);
const ACTIVITY_REFRESH_MIN_INTERVAL_MS: u64 = 20_000;

pub fn request_usage_refresh(app: &AppHandle) {
    let now = provider::now_ms();
    let last = LAST_ACTIVITY_REFRESH_MS.load(Ordering::SeqCst);
    if now.saturating_sub(last) < ACTIVITY_REFRESH_MIN_INTERVAL_MS {
        return;
    }
    LAST_ACTIVITY_REFRESH_MS.store(now, Ordering::SeqCst);
    refresh_all(app);
}

pub struct AppState {
    store: Mutex<state::Store>,
    cfg: Mutex<config::Config>,
    programmatic_move: AtomicBool,
    refresh_in_progress: Arc<AtomicBool>,
}

pub fn broadcast(app: &AppHandle) {
    let app_state = app.state::<AppState>();
    let snapshot = {
        let store = app_state.store.lock().unwrap();
        let cfg = app_state.cfg.lock().unwrap();
        store.snapshot(&cfg)
    };
    let _ = app.emit("state", snapshot);
}

pub fn refresh_all(app: &AppHandle) {
    let app_state = app.state::<AppState>();
    if app_state.refresh_in_progress.compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst).is_err() {
        return;
    }
    let app = app.clone();
    std::thread::spawn(move || {
        let _refresh_guard = RefreshGuard(app.state::<AppState>().refresh_in_progress.clone());
        let mut snapshots = vec![claude::fetch(), codex::fetch(), cursor::fetch()];
        snapshots.extend(local_runtime::fetch_all());
        let mesh = agentmesh::fetch();
        {
            let app_state = app.state::<AppState>();
            let mut store = app_state.store.lock().unwrap();
            for snapshot in snapshots {
                store.set_provider(snapshot);
            }
            store.set_agentmesh(mesh);
        }
        broadcast(&app);
    });
}

struct RefreshGuard(Arc<AtomicBool>);

impl Drop for RefreshGuard {
    fn drop(&mut self) {
        self.0.store(false, Ordering::SeqCst);
    }
}

pub fn reset_notch(app: &AppHandle) {
    {
        let app_state = app.state::<AppState>();
        let mut cfg = app_state.cfg.lock().unwrap();
        cfg.notch_y = 0.5;
        cfg.notch_x = None;
        config::save(&cfg);
    }
    place_notch(app);
}

fn place_notch(app: &AppHandle) {
    let Some(window) = app.get_webview_window("notch") else {
        return;
    };
    let Ok(Some(monitor)) = window.primary_monitor() else {
        return;
    };

    let (orientation, ratio, free_x) = {
        let app_state = app.state::<AppState>();
        let cfg = app_state.cfg.lock().unwrap();
        (cfg.orientation.clone(), cfg.notch_y.clamp(0.0, 1.0), cfg.notch_x)
    };

    let (base_w, base_h) = if orientation == "horizontal" {
        (NOTCH_W_HORIZONTAL, NOTCH_H_HORIZONTAL)
    } else {
        (NOTCH_W_VERTICAL, NOTCH_H_VERTICAL)
    };

    let scale = monitor.scale_factor();
    let target = tauri::PhysicalSize::new((base_w * scale).round() as u32, (base_h * scale).round() as u32);
    let size = monitor.size();
    let pos = monitor.position();
    let min_x = pos.x;
    let max_x = (pos.x + size.width as i32 - target.width as i32).max(pos.x);
    let min_y = pos.y;
    let max_y = (pos.y + size.height as i32 - target.height as i32).max(pos.y);

    let (x, y) = if let Some(fx) = free_x {
        let cx = pos.x as f64 + size.width as f64 * fx;
        let cy = pos.y as f64 + size.height as f64 * ratio;
        (
            (cx - target.width as f64 / 2.0).round() as i32,
            (cy - target.height as f64 / 2.0).round() as i32,
        )
    } else if orientation == "horizontal" {
        let x = (pos.x as f64 + size.width as f64 * ratio - target.width as f64 / 2.0).round() as i32;
        (x, pos.y)
    } else {
        let x = pos.x + size.width as i32 - target.width as i32;
        let y = (pos.y as f64 + size.height as f64 * ratio - target.height as f64 / 2.0).round() as i32;
        (x, y)
    };

    let app_state = app.state::<AppState>();
    app_state.programmatic_move.store(true, Ordering::SeqCst);
    let _ = window.set_size(target);
    let _ = window.set_position(tauri::PhysicalPosition::new(
        x.clamp(min_x, max_x),
        y.clamp(min_y, max_y),
    ));
}

fn create_notch_window(app: &AppHandle) -> tauri::Result<WebviewWindow> {
    let window = WebviewWindowBuilder::new(app, "notch", WebviewUrl::App("notch.html".into()))
        .title("MeshNotch")
        .inner_size(NOTCH_W_VERTICAL, NOTCH_H_VERTICAL)
        .transparent(true)
        .decorations(false)
        .always_on_top(true)
        // Without this, KDE/GNOME only keep the window above others on the
        // workspace it was created on: switching virtual desktops, or
        // another window grabbing focus in a way that changes stacking
        // order, drops the notch out of view instead of it staying pinned
        // like a real overlay/dock.
        .visible_on_all_workspaces(true)
        .skip_taskbar(true)
        .resizable(false)
        .shadow(false)
        .visible(false)
        .focused(false)
        .build()?;

    let move_handle = app.clone();
    window.on_window_event(move |event| {
        if let WindowEvent::Moved(position) = event {
            let state = move_handle.state::<AppState>();
            if state.programmatic_move.swap(false, Ordering::SeqCst) {
                return;
            }
            persist_notch_move(&move_handle, *position);
        }
    });
    Ok(window)
}

fn create_settings_window(app: &AppHandle) -> tauri::Result<WebviewWindow> {
    let window = WebviewWindowBuilder::new(
        app,
        "settings",
        WebviewUrl::App("settings.html".into()),
    )
    .title("MeshNotch Settings")
    .inner_size(560.0, 640.0)
    .min_inner_size(480.0, 520.0)
    .resizable(true)
    .visible(false)
    .skip_taskbar(false)
    .build()?;

    let settings_window = window.clone();
    window.on_window_event(move |event| {
        if let WindowEvent::CloseRequested { api, .. } = event {
            api.prevent_close();
            let _ = settings_window.hide();
        }
    });
    Ok(window)
}

pub(crate) fn get_or_create_settings_window(
    app: &AppHandle,
) -> Result<WebviewWindow, String> {
    if let Some(window) = app.get_webview_window("settings") {
        return Ok(window);
    }
    create_settings_window(app).map_err(|error| error.to_string())
}

fn create_windows_independently(app: &AppHandle) {
    match create_notch_window(app) {
        Ok(window) => {
            place_notch(app);
            let visible = {
                let state = app.state::<AppState>();
                let visible = state.cfg.lock().unwrap().notch_visible;
                visible
            };
            if visible {
                if let Err(error) = window.show() {
                    logging::info(format!("could not show notch window: {error}"));
                }
            }
        }
        Err(error) => logging::info(format!(
            "notch WebView creation failed; tray/settings startup continues: {error}"
        )),
    }

    // Settings gets its own full WebKitWebProcess (~70-100MB) once created;
    // creating it eagerly here meant every user paid that cost for the
    // entire session even if they never opened Settings. It's created
    // lazily instead, on first use, via get_or_create_settings_window
    // (already the path the tray menu and the open_settings command use).
}

fn persist_notch_move(app: &AppHandle, position: tauri::PhysicalPosition<i32>) {
    let Some(window) = app.get_webview_window("notch") else {
        return;
    };
    let Ok(Some(monitor)) = window.primary_monitor() else {
        return;
    };
    let Ok(size) = window.outer_size() else {
        return;
    };
    let mon_size = monitor.size();
    let mon_pos = monitor.position();
    let cx = position.x as f64 + size.width as f64 / 2.0 - mon_pos.x as f64;
    let cy = position.y as f64 + size.height as f64 / 2.0 - mon_pos.y as f64;
    let x_ratio = (cx / mon_size.width as f64).clamp(0.0, 1.0);
    let y_ratio = (cy / mon_size.height as f64).clamp(0.0, 1.0);

    let app_state = app.state::<AppState>();
    let mut cfg = app_state.cfg.lock().unwrap();
    cfg.notch_x = Some(x_ratio);
    cfg.notch_y = y_ratio;
    config::save(&cfg);
}

fn poll_usage(app: AppHandle) {
    std::thread::spawn(move || loop {
        refresh_all(&app);
        let refreshing = app.state::<AppState>().refresh_in_progress.clone();
        while refreshing.load(Ordering::SeqCst) {
            std::thread::sleep(std::time::Duration::from_millis(50));
        }
        std::thread::sleep(usage_cache::next_poll_delay());
    });
}

#[tauri::command]
fn get_state(state: tauri::State<AppState>) -> state::Snapshot {
    let store = state.store.lock().unwrap();
    let cfg = state.cfg.lock().unwrap();
    store.snapshot(&cfg)
}

#[tauri::command]
fn refresh_all_cmd(app: AppHandle) {
    refresh_all(&app);
}

#[tauri::command]
fn open_data_dir() -> Result<(), String> {
    std::fs::create_dir_all(config::data_dir()).map_err(|e| e.to_string())?;
    platform::open_path(&config::data_dir())
}

#[tauri::command]
fn open_provider_page(provider: String) -> Result<(), String> {
    let url = match provider.as_str() {
        "codex" => "https://chatgpt.com/#settings/Account",
        "cursor" => "https://cursor.com/dashboard",
        _ => "https://claude.ai/settings/usage",
    };
    platform::open_url(url)
}

#[tauri::command]
fn set_theme(app: AppHandle, theme: String) -> Result<(), String> {
    if !matches!(theme.as_str(), "system" | "light" | "dark") {
        return Err("theme must be system, light or dark".into());
    }
    {
        let app_state = app.state::<AppState>();
        let mut cfg = app_state.cfg.lock().unwrap();
        cfg.theme = theme.clone();
        config::save(&cfg);
    }
    let _ = app.emit("theme", theme);
    broadcast(&app);
    Ok(())
}

#[tauri::command]
fn set_scale(app: AppHandle, scale: f64) {
    let value = {
        let app_state = app.state::<AppState>();
        let mut cfg = app_state.cfg.lock().unwrap();
        cfg.scale = scale.clamp(config::SCALE_MIN, config::SCALE_MAX);
        config::save(&cfg);
        cfg.scale
    };
    let _ = app.emit("scale", value);
    broadcast(&app);
}

#[tauri::command]
fn set_autostart(enabled: bool) -> Result<String, String> {
    if enabled {
        autostart::enable()
    } else {
        autostart::disable()
    }
}

#[tauri::command]
fn autostart_status() -> bool {
    autostart::is_enabled()
}

#[tauri::command]
fn linux_probe() -> String {
    format!("{} | session={}", autostart::probe(), platform::session_type())
}

#[tauri::command]
fn set_hot(_rects: Vec<[f64; 4]>, _expanded: bool) {
    // Frontend contract placeholder. The X11 MVP can later use this to toggle
    // click-through outside the visible notch/card rectangles.
}

#[tauri::command]
fn drag_begin(app: AppHandle) {
    if let Some(window) = app.get_webview_window("notch") {
        let _ = window.start_dragging();
    }
}

#[tauri::command]
fn open_settings(app: AppHandle) -> Result<(), String> {
    let window = get_or_create_settings_window(&app)?;
    window.show().map_err(|e| e.to_string())?;
    let _ = window.unminimize();
    window.set_focus().map_err(|e| e.to_string())
}

#[tauri::command]
fn set_orientation(app: AppHandle, orientation: String) -> Result<(), String> {
    if !matches!(orientation.as_str(), "vertical" | "horizontal") {
        return Err("orientation must be vertical or horizontal".into());
    }
    {
        let app_state = app.state::<AppState>();
        let mut cfg = app_state.cfg.lock().unwrap();
        cfg.orientation = orientation;
        cfg.notch_x = None;
        config::save(&cfg);
    }
    place_notch(&app);
    broadcast(&app);
    Ok(())
}

#[tauri::command]
fn set_notch_style(app: AppHandle, style: String) -> Result<(), String> {
    if !matches!(style.as_str(), "solid" | "glass") {
        return Err("style must be solid or glass".into());
    }
    {
        let app_state = app.state::<AppState>();
        let mut cfg = app_state.cfg.lock().unwrap();
        cfg.notch_style = style;
        config::save(&cfg);
    }
    broadcast(&app);
    Ok(())
}

#[tauri::command]
fn set_accent(app: AppHandle, accent: String) -> Result<(), String> {
    if !config::ACCENT_PRESETS.iter().any(|(id, _)| *id == accent) {
        return Err("unknown accent preset".into());
    }
    {
        let app_state = app.state::<AppState>();
        let mut cfg = app_state.cfg.lock().unwrap();
        cfg.accent = accent;
        config::save(&cfg);
    }
    broadcast(&app);
    Ok(())
}

#[tauri::command]
fn set_enabled_providers(app: AppHandle, providers: Vec<String>) -> Result<(), String> {
    let filtered: Vec<String> = providers
        .into_iter()
        .filter(|id| config::PROVIDER_IDS.contains(&id.as_str()))
        .collect();
    if filtered.is_empty() {
        return Err("at least one provider must stay enabled".into());
    }
    {
        let app_state = app.state::<AppState>();
        let mut cfg = app_state.cfg.lock().unwrap();
        cfg.enabled_providers = filtered;
        config::save(&cfg);
    }
    broadcast(&app);
    Ok(())
}

#[tauri::command]
fn reset_notch_position(app: AppHandle) {
    reset_notch(&app);
    broadcast(&app);
}

#[tauri::command]
fn open_agentmesh_dashboard(state: tauri::State<AppState>) -> Result<(), String> {
    let port = agentmesh::discover_port();
    let url = if let Some(port) = port {
        format!("http://127.0.0.1:{port}/dashboard")
    } else {
        state.cfg.lock().unwrap().agentmesh_dashboard_url.clone()
    };
    platform::open_url(&url)
}

#[tauri::command]
fn quit_app(app: AppHandle) {
    app.exit(0);
}

#[tauri::command]
fn add_to_app_menu() -> Result<(), String> {
    desktop_integration::add_to_app_menu()
}

fn main() {
    match logging::init() {
        Ok(path) => logging::info(format!("persistent log: {}", path.display())),
        Err(error) => eprintln!("MeshNotch: could not initialize file logging: {error}"),
    }
    runtime_safety::prepare_before_gtk();
    logging::info("starting Tauri runtime");

    let cfg = config::load();
    let port = cfg.port;
    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            if let Some(window) = app.get_webview_window("notch") {
                let _ = window.show();
            } else if let Ok(window) = get_or_create_settings_window(app) {
                let _ = window.show();
            }
        }))
        .manage(AppState {
            store: Mutex::new(Default::default()),
            cfg: Mutex::new(cfg),
            programmatic_move: AtomicBool::new(false),
            refresh_in_progress: Arc::new(AtomicBool::new(false)),
        })
        .manage(app_updates::AppUpdateState::default())
        .invoke_handler(tauri::generate_handler![
            get_state,
            refresh_all_cmd,
            open_data_dir,
            open_provider_page,
            set_theme,
            set_scale,
            set_autostart,
            autostart_status,
            linux_probe,
            set_hot,
            drag_begin,
            open_settings,
            set_orientation,
            set_notch_style,
            set_accent,
            set_enabled_providers,
            reset_notch_position,
            open_agentmesh_dashboard,
            quit_app,
            add_to_app_menu,
            app_updates::check_update,
            app_updates::download_update,
            app_updates::install_update,
            app_updates::restart_app
        ])
        .setup(move |app| {
            if let Err(error) = app_updates::install_plugin(app.handle()) {
                logging::info(format!("updater plugin unavailable; continuing: {error}"));
            }
            let handle = app.handle().clone();
            if let Err(error) = desktop_integration::install_if_missing() {
                logging::info(format!("could not install application menu entry: {error}"));
            }
            if let Err(error) = tray::setup(&handle) {
                logging::info(format!("tray unavailable; continuing: {error}"));
            }
            logging::info("tray setup attempted before any WebView creation");
            server::start(handle.clone(), port);
            watcher::start(handle.clone());
            create_windows_independently(&handle);
            refresh_all(&handle);
            poll_usage(handle);
            Ok(())
        })
        .run(tauri::generate_context!())
        .unwrap_or_else(|error| logging::info(format!("Tauri runtime stopped with error: {error}")));
}

#[cfg(test)]
mod startup_tests {
    #[test]
    fn tauri_does_not_eagerly_create_webviews_before_setup() {
        let config: serde_json::Value =
            serde_json::from_str(include_str!("../tauri.conf.json")).unwrap();
        assert!(config["app"].get("windows").is_none());
    }
}
