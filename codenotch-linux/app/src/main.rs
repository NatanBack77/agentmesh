#![cfg_attr(all(not(debug_assertions), target_os = "windows"), windows_subsystem = "windows")]

mod agentmesh;
mod autostart;
mod claude;
mod codex;
mod config;
mod cursor;
mod local_runtime;
mod platform;
mod provider;
mod server;
mod state;
mod tray;
mod watcher;

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use tauri::{AppHandle, Emitter, Manager, WindowEvent};

const NOTCH_W_VERTICAL: f64 = 360.0;
const NOTCH_H_VERTICAL: f64 = 420.0;
const NOTCH_W_HORIZONTAL: f64 = 460.0;
const NOTCH_H_HORIZONTAL: f64 = 360.0;

pub struct AppState {
    store: Mutex<state::Store>,
    cfg: Mutex<config::Config>,
    programmatic_move: AtomicBool,
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
    let app = app.clone();
    std::thread::spawn(move || {
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
        std::thread::sleep(std::time::Duration::from_secs(300));
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
    let window = app
        .get_webview_window("settings")
        .ok_or_else(|| "settings window missing".to_string())?;
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

fn main() {
    let cfg = config::load();
    let port = cfg.port;
    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            if let Some(window) = app.get_webview_window("notch") {
                let _ = window.show();
            }
        }))
        .manage(AppState {
            store: Mutex::new(Default::default()),
            cfg: Mutex::new(cfg),
            programmatic_move: AtomicBool::new(false),
        })
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
            open_agentmesh_dashboard
        ])
        .setup(move |app| {
            let handle = app.handle().clone();
            if let Err(error) = tray::setup(&handle) {
                eprintln!("MeshNotch: tray unavailable; continuing without tray: {error}");
            }
            server::start(handle.clone(), port);
            watcher::start(handle.clone());
            place_notch(&handle);
            if let Some(window) = handle.get_webview_window("notch") {
                let visible = {
                    let state = handle.state::<AppState>();
                    let cfg = state.cfg.lock().unwrap();
                    cfg.notch_visible
                };
                if visible {
                    let _ = window.show();
                }
                let move_handle = handle.clone();
                window.on_window_event(move |event| {
                    if let WindowEvent::Moved(position) = event {
                        let state = move_handle.state::<AppState>();
                        if state.programmatic_move.swap(false, Ordering::SeqCst) {
                            return;
                        }
                        persist_notch_move(&move_handle, *position);
                    }
                });
            }
            if let Some(window) = handle.get_webview_window("settings") {
                let settings_window = window.clone();
                window.on_window_event(move |event| {
                    if let WindowEvent::CloseRequested { api, .. } = event {
                        api.prevent_close();
                        let _ = settings_window.hide();
                    }
                });
            }
            refresh_all(&handle);
            poll_usage(handle);
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running MeshNotch");
}
