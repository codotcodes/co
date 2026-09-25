use serde_json::{Value, json};
use std::fs;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::PathBuf;
use std::process::{Command, Output, Stdio};
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

struct Fixture(PathBuf);

impl Fixture {
    fn new() -> Self {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = std::env::temp_dir().join(format!("co-machine-{}-{nonce}", std::process::id()));
        fs::create_dir_all(&root).unwrap();
        fs::write(
            root.join("config.json"),
            json!({"session_token":"human-session"}).to_string(),
        )
        .unwrap();
        Self(root)
    }

    fn run(&self, api: &str, args: &[&str], stdin: Option<&str>) -> Output {
        let mut command = Command::new(env!("CARGO_BIN_EXE_co"));
        command
            .args(["machine"])
            .args(args)
            .env("CO_CONFIG_DIR", &self.0)
            .env("CO_API_URL", api)
            .env("NO_PROXY", "127.0.0.1")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let mut child = command.spawn().unwrap();
        if let Some(value) = stdin {
            child
                .stdin
                .take()
                .unwrap()
                .write_all(value.as_bytes())
                .unwrap();
        }
        child.wait_with_output().unwrap()
    }

    fn config(&self) -> Value {
        serde_json::from_slice(&fs::read(self.0.join("config.json")).unwrap()).unwrap()
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        fs::remove_dir_all(&self.0).unwrap();
    }
}

fn receive(stream: &mut TcpStream) -> (String, String, Value) {
    let mut bytes = Vec::new();
    let (end, length) = loop {
        let mut chunk = [0; 4096];
        let count = stream.read(&mut chunk).unwrap();
        assert!(count > 0);
        bytes.extend_from_slice(&chunk[..count]);
        if let Some(end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
            let headers = String::from_utf8(bytes[..end].to_vec()).unwrap();
            let length = headers
                .lines()
                .find_map(|line| {
                    line.to_ascii_lowercase()
                        .strip_prefix("content-length: ")
                        .map(str::to_owned)
                })
                .and_then(|value| value.parse::<usize>().ok())
                .unwrap_or(0);
            break (end + 4, length);
        }
    };
    while bytes.len() < end + length {
        let mut chunk = [0; 4096];
        let count = stream.read(&mut chunk).unwrap();
        assert!(count > 0);
        bytes.extend_from_slice(&chunk[..count]);
    }
    let headers = String::from_utf8(bytes[..end - 4].to_vec()).unwrap();
    let path = headers
        .lines()
        .next()
        .unwrap()
        .split_whitespace()
        .nth(1)
        .unwrap()
        .to_owned();
    let body = if length > 0 {
        serde_json::from_slice(&bytes[end..end + length]).unwrap()
    } else {
        json!(null)
    };
    (path, headers, body)
}

fn reply(stream: &mut TcpStream, status: &str, value: Value) {
    let body = value.to_string();
    write!(stream, "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
}

#[test]
fn self_enrollment_persists_identity_and_heartbeat_survives_restart() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let api = format!("http://{}", listener.local_addr().unwrap());
    let server = thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        let (path, headers, body) = receive(&mut stream);
        assert_eq!(path, "/machines/owners/my-org/self-register");
        assert!(
            headers
                .to_ascii_lowercase()
                .contains("authorization: bearer human-session")
        );
        assert_eq!(body["label"], "builder");
        assert!(body["vcpus"].as_u64().unwrap() > 0);
        assert!(body["ramMiB"].as_u64().unwrap() > 0);
        reply(
            &mut stream,
            "201 Created",
            json!({"machineId":"machine-one","scope":"owner","credential":"co_runner_secret"}),
        );
        let (mut stream, _) = listener.accept().unwrap();
        let (path, headers, _) = receive(&mut stream);
        assert_eq!(path, "/machine-enrollment/heartbeat");
        assert!(
            headers
                .to_ascii_lowercase()
                .contains("authorization: bearer co_runner_secret")
        );
        reply(&mut stream, "204 No Content", json!(null));
    });
    let fixture = Fixture::new();
    let enrolled = fixture.run(
        &api,
        &["enroll", "--owner", "my-org", "--label", "builder"],
        None,
    );
    assert!(
        enrolled.status.success(),
        "{}",
        String::from_utf8_lossy(&enrolled.stderr)
    );
    assert!(!String::from_utf8_lossy(&enrolled.stdout).contains("co_runner_secret"));
    assert_eq!(
        fixture.config()["machines"][0]["credential"],
        "co_runner_secret"
    );
    let resumed = fixture.run(&api, &["heartbeat", "machine-one"], None);
    assert!(
        resumed.status.success(),
        "{}",
        String::from_utf8_lossy(&resumed.stderr)
    );
    assert!(resumed.stdout.is_empty());
    server.join().unwrap();
}

#[test]
fn setup_key_is_read_from_stdin_and_never_echoed() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let api = format!("http://{}", listener.local_addr().unwrap());
    let server = thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        let (path, headers, body) = receive(&mut stream);
        assert_eq!(path, "/machine-enrollment/redeem");
        assert!(!headers.to_ascii_lowercase().contains("authorization:"));
        assert_eq!(body["setupKey"], "co_setup_test-secret");
        reply(
            &mut stream,
            "200 OK",
            json!({"machineId":"public-one","scope":"public","credential":"co_runner_public-secret"}),
        );
        let (mut stream, _) = listener.accept().unwrap();
        let (path, _, _) = receive(&mut stream);
        assert_eq!(path, "/machine-enrollment/redeem");
        reply(
            &mut stream,
            "404 Not Found",
            json!({"error":"setup_key_unavailable"}),
        );
    });
    let fixture = Fixture::new();
    let first = fixture.run(
        &api,
        &["enroll", "--setup-key"],
        Some("co_setup_test-secret\n"),
    );
    assert!(
        first.status.success(),
        "{}",
        String::from_utf8_lossy(&first.stderr)
    );
    assert!(!String::from_utf8_lossy(&first.stderr).contains("co_setup_test-secret"));
    assert!(!String::from_utf8_lossy(&first.stdout).contains("co_runner_public-secret"));
    let second = fixture.run(
        &api,
        &["enroll", "--setup-key"],
        Some("co_setup_test-secret\n"),
    );
    assert!(!second.status.success());
    assert!(String::from_utf8_lossy(&second.stderr).contains("setup_key_unavailable"));
    assert_eq!(fixture.config()["machines"].as_array().unwrap().len(), 1);
    server.join().unwrap();
}

#[test]
fn foreground_harness_survives_a_temporary_disconnect() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let api = format!("http://{}", listener.local_addr().unwrap());
    let fixture = Fixture::new();
    fs::write(
        fixture.0.join("config.json"),
        json!({"machines":[{"id":"test-id","scope":"owner","api_url":api,"credential":"co_runner_secret"}]}).to_string(),
    ).unwrap();
    let mut child = Command::new(env!("CARGO_BIN_EXE_co"))
        .args(["machine", "run", "test-id"])
        .env("CO_CONFIG_DIR", &fixture.0)
        .env("CO_API_URL", &api)
        .env("NO_PROXY", "127.0.0.1")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let (mut stream, _) = listener.accept().unwrap();
    let (path, _, _) = receive(&mut stream);
    assert_eq!(path, "/machine-enrollment/heartbeat");
    reply(
        &mut stream,
        "503 Service Unavailable",
        json!({"error":"temporarily_unavailable"}),
    );
    drop(stream);
    thread::sleep(Duration::from_millis(100));
    assert!(
        child.try_wait().unwrap().is_none(),
        "harness exited after a temporary error"
    );
    child.kill().unwrap();
    let output = child.wait_with_output().unwrap();
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert!(stderr.contains("retrying in 30 seconds"));
    assert!(!stderr.contains("co_runner_secret"));
}
