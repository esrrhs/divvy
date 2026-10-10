//! divvy Tauri shell entry point (Task 12).
//!
//! See `sidecar.rs` for sidecar process management. This file wires it into
//! the Tauri lifecycle: launch before the window opens, point the webview at
//! the sidecar URL (release) or the Vite dev server (debug), and shut the
//! sidecar down on exit. Fatal startup errors show a native dialog.

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod sidecar;

use tauri::{Manager, RunEvent, WebviewUrl, WebviewWindowBuilder};

use sidecar::{launch_sidecar, resolve_sidecar, SidecarState};

fn main() {
    tauri::Builder::default()
        .manage(SidecarState::new())
        .setup(|app| {
            // Persistent data (sessions/events/logs) and the default
            // workspace leaves operate on, both inside the app data dir.
            let data_dir = app
                .path()
                .app_data_dir()
                .map_err(|e| fatal(&format!("无法确定应用数据目录：{e}")))?;
            std::fs::create_dir_all(&data_dir)
                .map_err(|e| fatal(&format!("创建数据目录失败：{e}")))?;
            let workdir = data_dir.join("workspace");
            std::fs::create_dir_all(&workdir)
                .map_err(|e| fatal(&format!("创建工作区失败：{e}")))?;

            let bin = resolve_sidecar(app.handle()).map_err(|e| fatal(&e))?;
            let launched = launch_sidecar(&bin, &workdir, &data_dir).map_err(|e| fatal(&e))?;
            app.state::<SidecarState>().set(launched.proc);

            // Debug: Vite serves the shell and proxies /api to the fixed
            // dev port; only the token is delivered via the query string.
            // Release: the sidecar serves the embedded UI directly.
            let win_url = if cfg!(debug_assertions) {
                format!("http://localhost:5173/?token={}", launched.token)
            } else {
                launched.url
            };
            let url = match url::Url::parse(&win_url) {
                Ok(u) => u,
                Err(e) => {
                    // Sidecar already launched: stop it before aborting so
                    // setup failure cannot orphan the process.
                    app.state::<SidecarState>().shutdown();
                    return Err(fatal(&format!("无法解析后端访问地址 {win_url}: {e}")));
                }
            };

            if let Err(e) = WebviewWindowBuilder::new(app, "main", WebviewUrl::External(url))
                .title("divvy")
                .inner_size(1280.0, 820.0)
                .min_inner_size(960.0, 600.0)
                .build()
            {
                app.state::<SidecarState>().shutdown();
                return Err(fatal(&format!("创建窗口失败：{e}")));
            }

            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error while building tauri application")
        .run(|app, event| {
            if let RunEvent::ExitRequested { .. } = event {
                app.state::<SidecarState>().shutdown();
            }
        });
}

/// Show a native error dialog (before any window exists) and return a
/// boxed error so Tauri aborts startup with a non-zero exit code.
fn fatal(message: &str) -> Box<dyn std::error::Error> {
    eprintln!("[shell] fatal: {message}");
    rfd::MessageDialog::new()
        .set_title("divvy 无法启动")
        .set_level(rfd::MessageLevel::Error)
        .set_description(message)
        .show();
    message.to_string().into()
}
