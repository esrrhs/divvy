//! Go sidecar lifecycle: locate, launch, parse the token-bearing URL, and
//! shut down without leaving orphans. Split from `main` so the behaviour is
//! integration-testable headlessly (see the tests below).

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::sync::Mutex;
use std::thread;
use std::time::{Duration, Instant};

use tauri::Manager;

/// Fixed dev port so Vite's static proxy target (VITE_DEV_TARGET) matches.
/// Release builds ask for port 0 (OS-assigned). Overridable for tests.
const DEFAULT_DEV_SIDECAR_PORT: &str = "8799";
const DEFAULT_STARTUP_TIMEOUT_MS: u64 = 30_000;
/// Let the Go server run its SIGINT checkpoint path (its own grace is 10s).
const SHUTDOWN_GRACE: Duration = Duration::from_secs(12);

pub(crate) struct SidecarState {
    proc: Mutex<Option<SidecarProc>>,
}

impl SidecarState {
    pub(crate) fn new() -> Self {
        Self {
            proc: Mutex::new(None),
        }
    }

    pub(crate) fn set(&self, proc: SidecarProc) {
        *self.proc.lock().unwrap() = Some(proc);
    }

    /// Gracefully stop the sidecar on app shutdown: SIGINT first so the Go
    /// server checkpoints running sessions, then force-kill after a bounded
    /// wait. Best effort and idempotent; never blocks longer than the grace.
    pub(crate) fn shutdown(&self) {
        let Some(proc) = self.proc.lock().unwrap().take() else {
            return;
        };
        proc.graceful_shutdown();
    }
}

pub(crate) struct SidecarProc {
    child: Mutex<Child>,
    pid: u32,
}

pub(crate) struct Launched {
    pub(crate) proc: SidecarProc,
    pub(crate) url: String,
    pub(crate) token: String,
}

/// Locate the sidecar binary for this platform.
pub(crate) fn resolve_sidecar(app: &tauri::AppHandle) -> Result<PathBuf, String> {
    if let Ok(p) = std::env::var("DIVVY_SIDECAR") {
        if !p.is_empty() {
            let pb = PathBuf::from(p);
            return if pb.exists() {
                Ok(pb)
            } else {
                Err(format!("DIVVY_SIDECAR 指向的文件不存在：{}", pb.display()))
            };
        }
    }

    // Release candidates, in order:
    // 1. Next to the running executable. Tauri v2 bundles externalBin this
    //    way on macOS (Contents/MacOS/divvy-sidecar, triple stripped) and it
    //    also works for Windows/AppImage layouts.
    // 2. <resources>/binaries/divvy-sidecar-<triple>, the documented fallback
    //    for other bundle layouts.
    // Debug runs (tauri dev) use the locally built triple-suffixed binary.
    let mut candidates: Vec<PathBuf> = Vec::new();
    if cfg!(debug_assertions) {
        candidates.push(
            PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("binaries")
                .join(sidecar_name()),
        );
    } else {
        if let Ok(exe) = std::env::current_exe() {
            if let Some(dir) = exe.parent() {
                candidates.push(dir.join(sidecar_base_name()));
            }
        }
        if let Ok(res) = app.path().resource_dir() {
            candidates.push(res.join("binaries").join(sidecar_name()));
        }
    }

    for c in &candidates {
        if c.exists() {
            return Ok(c.clone());
        }
    }
    Err(format!(
        "未找到 divvy 后端程序，已尝试：{}\n请先运行 desktop/scripts/build-sidecar.sh（tauri dev / tauri build 会自动构建）。",
        candidates
            .iter()
            .map(|c| c.display().to_string())
            .collect::<Vec<_>>()
            .join("、")
    ))
}

/// File name Tauri gives the bundled sidecar inside the bundle (no triple).
fn sidecar_base_name() -> String {
    if std::env::consts::OS == "windows" {
        "divvy-sidecar.exe".to_string()
    } else {
        "divvy-sidecar".to_string()
    }
}

fn sidecar_name() -> String {
    let triple = match (std::env::consts::OS, std::env::consts::ARCH) {
        ("macos", "aarch64") => "aarch64-apple-darwin",
        ("macos", "x86_64") => "x86_64-apple-darwin",
        ("linux", "x86_64") => "x86_64-unknown-linux-gnu",
        ("linux", "aarch64") => "aarch64-unknown-linux-gnu",
        ("windows", "x86_64") => "x86_64-pc-windows-msvc",
        (os, arch) => panic!("unsupported sidecar platform: {os}/{arch}"),
    };
    let mut name = format!("divvy-sidecar-{triple}");
    if std::env::consts::OS == "windows" {
        name.push_str(".exe");
    }
    name
}

fn configured_port() -> String {
    std::env::var("DIVVY_SIDECAR_PORT").unwrap_or_else(|_| {
        if cfg!(debug_assertions) {
            DEFAULT_DEV_SIDECAR_PORT.to_string()
        } else {
            "0".to_string()
        }
    })
}

fn startup_timeout() -> Duration {
    let ms = std::env::var("DIVVY_SIDECAR_STARTUP_TIMEOUT_MS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(DEFAULT_STARTUP_TIMEOUT_MS);
    Duration::from_millis(ms)
}

/// Spawn the sidecar and wait for its `URL http://127.0.0.1:.../?token=...`
/// banner. On any failure the spawned process is killed before returning.
pub(crate) fn launch_sidecar(
    bin: &Path,
    workdir: &Path,
    datadir: &Path,
) -> Result<Launched, String> {
    let mut command = Command::new(bin);
    command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .args([
            "-serve",
            "-no-open",
            "-port",
            &configured_port(),
            "-workdir",
        ])
        .arg(workdir)
        .args(["-datadir"])
        .arg(datadir);
    // Run the sidecar in its OWN process group so shutdown signals target
    // it (negative pid) without ever reaching the Tauri app's group, and a
    // forced kill covers every process the Go server forked into its group.
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        command.process_group(0);
    }
    let mut child = command
        .spawn()
        .map_err(|e| format!("无法启动后端程序 {}：{e}", bin.display()))?;

    let pid = child.id();
    let stdout = child.stdout.take().expect("piped stdout");
    let stderr = child.stderr.take().expect("piped stderr");

    let (tx, rx) = mpsc::channel::<String>();
    thread::spawn(move || {
        // Drain stdout for the whole process lifetime so the pipe buffer
        // can never block the sidecar; hand over the banner URL once.
        let mut sender = Some(tx);
        for line in BufReader::new(stdout).lines().map_while(Result::ok) {
            println!("[sidecar] {line}");
            if let Some(url) = parse_serve_url(&line) {
                if let Some(s) = sender.take() {
                    let _ = s.send(url);
                }
            }
        }
    });
    thread::spawn(move || {
        for line in BufReader::new(stderr).lines().map_while(Result::ok) {
            eprintln!("[sidecar] {line}");
        }
    });

    let proc = SidecarProc {
        child: Mutex::new(child),
        pid,
    };

    match rx.recv_timeout(startup_timeout()) {
        Ok(url) => {
            let token =
                parse_token(&url).ok_or_else(|| format!("后端访问地址缺少 token：{url}"))?;
            Ok(Launched { proc, url, token })
        }
        Err(RecvTimeoutError::Timeout) => {
            proc.kill_now();
            Err(format!(
                "后端在 {:?} 内没有输出访问地址（端口/token 解析超时）。",
                startup_timeout()
            ))
        }
        Err(RecvTimeoutError::Disconnected) => {
            let status = proc.exit_status();
            proc.kill_now();
            Err(format!(
                "后端进程提前退出（{status:?}），未能获取访问地址。"
            ))
        }
    }
}

/// Extract `http://127.0.0.1:<port>/?token=...` from a banner line.
fn parse_serve_url(line: &str) -> Option<String> {
    line.split_whitespace()
        .map(|t| t.trim_end_matches(','))
        .find(|t| t.starts_with("http://127.0.0.1:") && t.contains("token="))
        .map(str::to_string)
}

fn parse_token(url: &str) -> Option<String> {
    url.split_once("token=")
        .and_then(|(_, rest)| rest.split('&').next())
        .filter(|t| !t.is_empty())
        .map(str::to_string)
}

impl SidecarProc {
    fn graceful_shutdown(&self) {
        // Unix: ask for graceful shutdown (whole group) so the Go server
        // checkpoints running sessions.
        #[cfg(unix)]
        unsafe {
            // Negative pid targets the sidecar's own process group.
            libc::kill(-(self.pid as i32), libc::SIGINT);
        }
        // Windows has no SIGINT: std::process::kill is TerminateProcess, so
        // there is no checkpoint path; the grace wait below is skipped by
        // starting the kill immediately (Windows is not a supported target
        // in this release, see spec Non-Goals).
        #[cfg(windows)]
        if let Ok(mut child) = self.child.lock() {
            let _ = child.start_kill();
            let _ = child.wait();
            return;
        }

        let deadline = Instant::now() + SHUTDOWN_GRACE;
        let mut child = self.child.lock().unwrap();
        loop {
            match child.try_wait() {
                Ok(Some(status)) => {
                    println!("[shell] sidecar exited ({status})");
                    return;
                }
                Ok(None) if Instant::now() < deadline => {
                    thread::sleep(Duration::from_millis(100));
                }
                _ => break,
            }
        }

        eprintln!("[shell] sidecar did not exit in grace; force killing group");
        #[cfg(unix)]
        unsafe {
            // Kill the whole group: a leaf tool that missed the Go server's
            // own cleanup must not be reparented to launchd as an orphan.
            libc::kill(-(self.pid as i32), libc::SIGKILL);
        }
        let _ = child.wait();
    }

    /// Force-stop after a failed startup (no window will be shown).
    fn kill_now(&self) {
        if let Ok(mut child) = self.child.lock() {
            #[cfg(unix)]
            unsafe {
                libc::kill(-(self.pid as i32), libc::SIGKILL);
            }
            #[cfg(not(unix))]
            let _ = child.kill();
            let _ = child.wait();
        }
    }

    fn exit_status(&self) -> Option<std::process::ExitStatus> {
        if let Ok(mut child) = self.child.lock() {
            return child.try_wait().ok().flatten();
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn sidecar_bin() -> Option<PathBuf> {
        let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("binaries")
            .join(sidecar_name());
        p.exists().then_some(p)
    }

    fn temp_dirs(tag: &str) -> (PathBuf, PathBuf) {
        let unique = format!(
            "{}-{}-{}",
            tag,
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        );
        let base = std::env::temp_dir().join(format!("divvy-tauri-test-{unique}"));
        let data = base.join("data");
        let work = data.join("workspace");
        std::fs::create_dir_all(&work).unwrap();
        (data, work)
    }

    #[cfg(unix)]
    fn alive(pid: u32) -> bool {
        unsafe { libc::kill(pid as i32, 0) == 0 }
    }

    fn write_script(body: &str) -> PathBuf {
        let path = std::env::temp_dir().join(format!(
            "divvy-fake-sidecar-{}-{}.sh",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let mut f = std::fs::File::create(&path).unwrap();
        writeln!(f, "#!/bin/sh\n{body}").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        path
    }

    /// Real sidecar: banner is parsed, the token authenticates an HTTP call,
    /// and SIGINT shutdown leaves no process behind.
    #[test]
    fn launch_parse_and_graceful_shutdown() {
        let Some(bin) = sidecar_bin() else {
            eprintln!("skip: sidecar binary not built (run desktop/scripts/build-sidecar.sh)");
            return;
        };
        let (data, work) = temp_dirs("ok");
        // Safety: serialise with the dev app (fixed 8799) by using a unique
        // port, and restore the environment afterwards.
        let port = format!("89{}", (std::process::id() % 100));
        std::env::set_var("DIVVY_SIDECAR_PORT", &port);
        let launched = launch_sidecar(&bin, &work, &data).expect("sidecar launches");
        assert!(
            launched.url.contains(&format!("127.0.0.1:{port}/")),
            "unexpected url: {}",
            launched.url
        );
        assert!(!launched.token.is_empty());
        let pid = launched.proc.pid;
        assert!(alive(pid));

        // The parsed token actually authenticates against the live server.
        use std::io::{Read, Write};
        use std::net::TcpStream;
        let mut sock = TcpStream::connect(("127.0.0.1", port.parse().unwrap())).unwrap();
        write!(
            sock,
            "GET /api/health HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {}\r\nConnection: close\r\n\r\n",
            launched.token
        )
        .unwrap();
        let mut resp = String::new();
        sock.read_to_string(&mut resp).unwrap();
        assert!(resp.starts_with("HTTP/1.1 200"), "health response: {resp}");
        assert!(resp.contains("\"status\":\"ok\""));

        // Graceful SIGINT shutdown (Go side checkpoints and exits).
        launched.proc.graceful_shutdown();
        for _ in 0..150 {
            if !alive(pid) {
                break;
            }
            thread::sleep(Duration::from_millis(100));
        }
        assert!(!alive(pid), "sidecar {pid} survived graceful shutdown");
        std::env::remove_var("DIVVY_SIDECAR_PORT");
    }

    /// A sidecar that never prints the banner: startup timeout, error
    /// reported, no leftover process.
    #[test]
    fn startup_timeout_is_reported_and_killed() {
        std::env::set_var("DIVVY_SIDECAR_STARTUP_TIMEOUT_MS", "1200");
        std::env::set_var("DIVVY_SIDECAR_PORT", "8811");
        let (data, work) = temp_dirs("timeout");
        // exec so killing the script kills sleep too.
        let fake = write_script("exec /bin/sleep 30");
        let err = match launch_sidecar(&fake, &work, &data) {
            Err(e) => e,
            Ok(_) => panic!("launch must time out"),
        };
        assert!(err.contains("解析超时"), "got: {err}");

        // Find the fake sidecar's pid indirectly: the script was spawned and
        // kill_now must have reaped it. Wait briefly then assert the script
        // path is no longer running.
        let out = std::process::Command::new("pgrep")
            .arg("-f")
            .arg(fake.to_string_lossy().as_ref())
            .output()
            .unwrap();
        let pids = String::from_utf8_lossy(&out.stdout);
        assert!(pids.trim().is_empty(), "fake sidecar survived: {pids}");
        std::env::remove_var("DIVVY_SIDECAR_STARTUP_TIMEOUT_MS");
        std::env::remove_var("DIVVY_SIDECAR_PORT");
    }

    /// A sidecar that exits before printing the banner: immediate, explicit
    /// error rather than a hang.
    #[test]
    fn early_exit_is_reported() {
        std::env::set_var("DIVVY_SIDECAR_PORT", "8812");
        let (data, work) = temp_dirs("early");
        let fake = write_script("echo some-garbage-line\nexit 3");
        let err = match launch_sidecar(&fake, &work, &data) {
            Err(e) => e,
            Ok(_) => panic!("launch must fail"),
        };
        assert!(err.contains("提前退出"), "got: {err}");
        std::env::remove_var("DIVVY_SIDECAR_PORT");
    }
}
