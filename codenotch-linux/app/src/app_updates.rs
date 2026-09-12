use serde::Serialize;
use std::sync::{Arc, Mutex};
use std::sync::atomic::{AtomicU64, Ordering};
use tauri::{AppHandle, Emitter, State};
use tauri_plugin_updater::{Update, UpdaterExt};

#[derive(Default)]
pub struct AppUpdateState {
    pending: Mutex<Option<Update>>,
    downloaded: Mutex<Option<Vec<u8>>>,
}

#[derive(Debug, Serialize)]
pub struct UpdateCheckResult {
    pub state: String,
    pub current_version: String,
    pub version: Option<String>,
    pub notes: Option<String>,
    pub pub_date: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct UpdateProgress {
    pub state: String,
    pub downloaded_bytes: u64,
    pub content_length: Option<u64>,
}

fn emit_progress(app: &AppHandle, progress: UpdateProgress) {
    let _ = app.emit("update-progress", progress);
}

#[tauri::command]
pub async fn check_update(app: AppHandle, state: State<'_, AppUpdateState>) -> Result<UpdateCheckResult, String> {
    let current_version = app.package_info().version.to_string();
    let update = app.updater().map_err(|error| error.to_string())?
        .check().await.map_err(|error| error.to_string())?;
    let Some(update) = update else {
        return Ok(UpdateCheckResult {
            state: "up_to_date".into(),
            current_version,
            version: None,
            notes: None,
            pub_date: None,
        });
    };

    let result = UpdateCheckResult {
        state: "available".into(),
        current_version,
        version: Some(update.version.clone()),
        notes: update.body.clone(),
        pub_date: update.date.map(|date| date.to_string()),
    };
    *state.pending.lock().map_err(|_| "update state lock poisoned".to_string())? = Some(update);
    *state.downloaded.lock().map_err(|_| "update state lock poisoned".to_string())? = None;
    Ok(result)
}

#[tauri::command]
pub async fn download_update(app: AppHandle, state: State<'_, AppUpdateState>) -> Result<(), String> {
    let update = state.pending.lock().map_err(|_| "update state lock poisoned".to_string())?
        .take().ok_or_else(|| "no update is pending; run check_update first".to_string())?;
    let downloaded = Arc::new(AtomicU64::new(0));
    let downloaded_for_progress = downloaded.clone();
    let downloaded_for_finish = downloaded.clone();
    let app_for_progress = app.clone();
    emit_progress(&app, UpdateProgress { state: "started".into(), downloaded_bytes: 0, content_length: None });
    let result = update.download(
        move |chunk_length, content_length| {
            let total = downloaded_for_progress.fetch_add(chunk_length as u64, Ordering::Relaxed)
                .saturating_add(chunk_length as u64);
            emit_progress(&app_for_progress, UpdateProgress {
                state: "progress".into(),
                downloaded_bytes: total,
                content_length,
            });
        },
        {
            let app = app.clone();
            move || emit_progress(&app, UpdateProgress {
                state: "finished".into(),
                downloaded_bytes: downloaded_for_finish.load(Ordering::Relaxed),
                content_length: None,
            })
        },
    ).await;

    match result {
        Ok(bytes) => {
            *state.downloaded.lock().map_err(|_| "update state lock poisoned".to_string())? = Some(bytes);
            *state.pending.lock().map_err(|_| "update state lock poisoned".to_string())? = Some(update);
            Ok(())
        }
        Err(error) => {
            *state.pending.lock().map_err(|_| "update state lock poisoned".to_string())? = Some(update);
            emit_progress(&app, UpdateProgress {
                state: "error".into(),
                downloaded_bytes: downloaded.load(Ordering::Relaxed),
                content_length: None,
            });
            Err(error.to_string())
        }
    }
}

#[tauri::command]
pub fn install_update(state: State<'_, AppUpdateState>) -> Result<(), String> {
    let update = state.pending.lock().map_err(|_| "update state lock poisoned".to_string())?
        .as_ref().cloned().ok_or_else(|| "no update is pending; run check_update first".to_string())?;
    let bytes = state.downloaded.lock().map_err(|_| "update state lock poisoned".to_string())?
        .take().ok_or_else(|| "download the update before installing it".to_string())?;
    update.install(bytes).map_err(|error| error.to_string())
}

#[tauri::command]
pub fn restart_app(app: AppHandle) {
    app.restart();
}

pub fn install_plugin(app: &AppHandle) -> tauri::Result<()> {
    app.plugin(tauri_plugin_updater::Builder::new().build())?;
    app.plugin(tauri_plugin_process::init())?;
    Ok(())
}
