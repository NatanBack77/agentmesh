use std::io;
use std::path::PathBuf;

#[cfg(target_os = "linux")]
pub fn init() -> io::Result<PathBuf> {
    use std::fs::{self, OpenOptions, Permissions};
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};

    let config_dir = dirs::config_dir().unwrap_or_else(|| PathBuf::from("."));
    let app_dir = config_dir.join("meshnotch");
    fs::create_dir_all(&app_dir)?;

    let log_path = app_dir.join("app.log");
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .mode(0o600)
        .open(&log_path)?;
    fs::set_permissions(&log_path, Permissions::from_mode(0o600))?;

    // Tee GTK/WebKit diagnostics to both the original stdout/stderr and the
    // file; GUI launches usually have no terminal, but development runs keep
    // their normal console output.
    tee_stream(libc::STDOUT_FILENO, &file, "stdout")?;
    tee_stream(libc::STDERR_FILENO, &file, "stderr")?;

    Ok(log_path)
}

#[cfg(target_os = "linux")]
fn tee_stream(target_fd: libc::c_int, log_file: &std::fs::File, name: &str) -> io::Result<()> {
    use std::fs::File;
    use std::os::fd::FromRawFd;
    use std::io::{Read, Write};

    let mut pipe_fds = [-1; 2];
    // SAFETY: pipe_fds points to two valid c_int slots for libc::pipe to fill.
    if unsafe { libc::pipe(pipe_fds.as_mut_ptr()) } == -1 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: target_fd is stdout or stderr and dup creates an owned descriptor.
    let original_fd = unsafe { libc::dup(target_fd) };
    if original_fd == -1 {
        unsafe {
            libc::close(pipe_fds[0]);
            libc::close(pipe_fds[1]);
        }
        return Err(io::Error::last_os_error());
    }
    // SAFETY: pipe_fds[1] is the valid write end of the newly created pipe.
    if unsafe { libc::dup2(pipe_fds[1], target_fd) } == -1 {
        let error = io::Error::last_os_error();
        unsafe {
            libc::close(pipe_fds[0]);
            libc::close(pipe_fds[1]);
            libc::close(original_fd);
        }
        return Err(error);
    }
    // SAFETY: target_fd now owns a duplicate of the pipe write end.
    unsafe { libc::close(pipe_fds[1]) };

    // SAFETY: both descriptors are uniquely owned by the returned File values.
    let mut reader = unsafe { File::from_raw_fd(pipe_fds[0]) };
    let mut console = unsafe { File::from_raw_fd(original_fd) };
    let mut log = log_file.try_clone()?;
    std::thread::Builder::new()
        .name(format!("meshnotch-{name}-tee"))
        .spawn(move || {
            let mut buffer = [0_u8; 8192];
            loop {
                match reader.read(&mut buffer) {
                    Ok(0) | Err(_) => break,
                    Ok(length) => {
                        let _ = log.write_all(&buffer[..length]);
                        let _ = console.write_all(&buffer[..length]);
                    }
                }
            }
            let _ = log.flush();
            let _ = console.flush();
        })?;
    Ok(())
}

#[cfg(not(target_os = "linux"))]
pub fn init() -> io::Result<PathBuf> {
    Ok(PathBuf::new())
}

pub fn info(message: impl AsRef<str>) {
    eprintln!("[MeshNotch][{}] {}", chrono::Utc::now().to_rfc3339(), message.as_ref());
}
