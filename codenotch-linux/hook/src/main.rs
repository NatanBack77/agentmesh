use std::io::{Read, Write};
use std::net::TcpStream;
use std::time::Duration;

const DEFAULT_PORT: u16 = 48666;
const MAX_STDIN: u64 = 256 * 1024;

fn main() {
    let event = std::env::args().nth(1).unwrap_or_else(|| "ping".into());
    let mut body = String::new();
    let _ = std::io::stdin().take(MAX_STDIN).read_to_string(&mut body);
    let port = read_port();
    let ppid = std::os::unix::process::parent_id();
    if send(port, &event, ppid, &body).is_ok() {
        return;
    }
    spawn_main();
    for _ in 0..20 {
        std::thread::sleep(Duration::from_millis(100));
        if send(port, &event, ppid, &body).is_ok() {
            return;
        }
    }
}

fn read_port() -> u16 {
    let Some(path) = dirs::config_dir().map(|dir| dir.join("meshnotch").join("config.json")) else {
        return DEFAULT_PORT;
    };
    let Ok(text) = std::fs::read_to_string(path) else {
        return DEFAULT_PORT;
    };
    if let Some(index) = text.find("\"port\"") {
        let digits: String = text[index + 6..]
            .chars()
            .skip_while(|ch| !ch.is_ascii_digit())
            .take_while(|ch| ch.is_ascii_digit())
            .collect();
        return digits.parse().unwrap_or(DEFAULT_PORT);
    }
    DEFAULT_PORT
}

fn send(port: u16, event: &str, ppid: u32, body: &str) -> std::io::Result<()> {
    let mut stream = TcpStream::connect_timeout(
        &std::net::SocketAddr::from(([127, 0, 0, 1], port)),
        Duration::from_millis(300),
    )?;
    stream.set_write_timeout(Some(Duration::from_millis(700)))?;
    stream.set_read_timeout(Some(Duration::from_millis(700)))?;
    let request = format!(
        "POST /event?e={event}&ppid={ppid} HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.len(),
        body
    );
    stream.write_all(request.as_bytes())?;
    let mut buf = [0u8; 64];
    let _ = stream.read(&mut buf);
    Ok(())
}

fn spawn_main() {
    let Ok(me) = std::env::current_exe() else {
        return;
    };
    let Some(dir) = me.parent() else {
        return;
    };
    let app = dir.join("meshnotch");
    if !app.is_file() {
        return;
    }
    let _ = std::process::Command::new(app)
        .arg("--silent")
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn();
}
