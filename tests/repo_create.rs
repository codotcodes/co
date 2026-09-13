use serde_json::{Value, json};
use std::fs;
use std::io::{Read, Write};
use std::net::TcpListener;
use std::path::PathBuf;
use std::process::{Command, Output};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

struct TestConfig(PathBuf);

impl TestConfig {
    fn new(signed_in: bool) -> Self {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = std::env::temp_dir().join(format!("co-create-{}-{nonce}", std::process::id()));
        fs::create_dir_all(root.join("co")).unwrap();
        let config = if signed_in {
            json!({"session_token": "test-session"})
        } else {
            json!({})
        };
        fs::write(root.join("co/config.json"), config.to_string()).unwrap();
        Self(root)
    }

    fn run(&self, api: &str, args: &[&str]) -> Output {
        Command::new(env!("CARGO_BIN_EXE_co"))
            .args(["repo", "create"])
            .args(args)
            .env("XDG_CONFIG_HOME", &self.0)
            .env_remove("CO_CONFIG_DIR")
            .env("CO_API_URL", api)
            .env("NO_PROXY", "127.0.0.1")
            .output()
            .unwrap()
    }
}

impl Drop for TestConfig {
    fn drop(&mut self) {
        fs::remove_dir_all(&self.0).unwrap();
    }
}

fn mock_create(args: &[&str], status: &str, response: Value) -> (Output, Value) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let api = format!("http://{}", listener.local_addr().unwrap());
    let status = status.to_string();
    let server = thread::spawn(move || {
        let deadline = Instant::now() + Duration::from_secs(10);
        let mut stream = loop {
            match listener.accept() {
                Ok((stream, _)) => break stream,
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    assert!(
                        Instant::now() < deadline,
                        "CLI never sent its create request"
                    );
                    thread::sleep(Duration::from_millis(10));
                }
                Err(error) => panic!("{error}"),
            }
        };
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut bytes = Vec::new();
        let (header_end, body_len) = loop {
            let mut buffer = [0; 4096];
            let count = stream.read(&mut buffer).unwrap();
            assert!(count > 0);
            bytes.extend_from_slice(&buffer[..count]);
            if let Some(end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                let headers = String::from_utf8(bytes[..end].to_vec()).unwrap();
                assert!(headers.starts_with("POST /repos HTTP/1.1\r\n"));
                let headers = headers.to_ascii_lowercase();
                assert!(headers.contains("\r\nauthorization: bearer test-session"));
                assert!(headers.contains("\r\ncontent-type: application/json"));
                let len: usize = headers
                    .lines()
                    .find_map(|line| line.strip_prefix("content-length: "))
                    .unwrap()
                    .parse()
                    .unwrap();
                break (end + 4, len);
            }
        };
        while bytes.len() < header_end + body_len {
            let mut buffer = [0; 4096];
            let count = stream.read(&mut buffer).unwrap();
            assert!(count > 0);
            bytes.extend_from_slice(&buffer[..count]);
        }
        let request: Value =
            serde_json::from_slice(&bytes[header_end..header_end + body_len]).unwrap();
        let response = response.to_string();
        write!(stream, "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{response}", response.len()).unwrap();
        request
    });
    let output = TestConfig::new(true).run(&api, args);
    (output, server.join().unwrap())
}

fn repository(created: bool) -> Value {
    json!({
        "id": "test-repo-id", "owner": "resolved-owner", "name": "project",
        "visibility": "private", "defaultBranch": "main", "created": created,
        "createdAt": "2026-09-06T00:00:00Z", "updatedAt": "2026-09-06T00:00:00Z"
    })
}

#[test]
fn creates_private_personal_repo_with_exact_json_output() {
    let response = repository(true);
    let (output, request) = mock_create(&["project", "--json"], "201 Created", response.clone());
    assert!(output.status.success());
    assert_eq!(request, json!({"name": "project", "visibility": "private"}));
    assert_eq!(
        serde_json::from_slice::<Value>(&output.stdout).unwrap(),
        response
    );
    assert!(output.stderr.is_empty());
}

#[test]
fn creates_public_org_repo_and_reports_resolved_identity() {
    let mut response = repository(true);
    response["visibility"] = json!("public");
    let (output, request) = mock_create(&["--public", "my-org/project"], "201 Created", response);
    assert!(output.status.success());
    assert_eq!(
        request,
        json!({"name": "project", "orgSlug": "my-org", "visibility": "public"})
    );
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(stdout.contains("Created resolved-owner/project (public)"));
    assert!(stdout.contains("https://git.co.codes/resolved-owner/project.git"));
}

#[test]
fn deferred_storage_preserves_success_and_json() {
    let response = repository(false);
    let (output, _) = mock_create(
        &["project", "--private", "--json"],
        "201 Created",
        response.clone(),
    );
    assert!(output.status.success());
    assert_eq!(
        serde_json::from_slice::<Value>(&output.stdout).unwrap(),
        response
    );
    assert!(
        String::from_utf8(output.stderr)
            .unwrap()
            .contains("storage initialization was not confirmed")
    );
}

#[test]
fn api_errors_fail_without_success_output_or_retries() {
    for (status, error) in [
        ("401 Unauthorized", "unauthorized"),
        ("403 Forbidden", "org_not_writable"),
        ("403 Forbidden", "preview_access_required"),
        ("409 Conflict", "name_taken"),
    ] {
        let (output, _) = mock_create(&["project", "--json"], status, json!({"error": error}));
        assert!(!output.status.success());
        assert!(output.stdout.is_empty());
        assert!(String::from_utf8(output.stderr).unwrap().contains(error));
    }
}

#[test]
fn rejects_bad_arguments_and_missing_session_before_network_access() {
    let config = TestConfig::new(false);
    for args in [
        vec![],
        vec!["one", "two"],
        vec!["--unknown", "project"],
        vec!["project", "--public", "--private"],
        vec!["project", "--private", "--public"],
        vec!["."],
        vec!["bad..name"],
        vec!["_bad"],
        vec!["my-org/../escape"],
        vec!["Upper/project"],
        vec!["/project"],
        vec!["my-org/"],
        vec!["project?token=x"],
        vec!["project name"],
    ] {
        let output = config.run("http://127.0.0.1:1", &args);
        assert!(!output.status.success(), "accepted {args:?}");
        assert!(output.stdout.is_empty());
        let stderr = String::from_utf8(output.stderr).unwrap();
        assert!(!stderr.contains("network"), "{stderr}");
        assert!(
            !stderr.contains("not signed in"),
            "arguments were not validated first: {args:?}"
        );
    }
    let output = config.run("http://127.0.0.1:1", &["project"]);
    assert!(!output.status.success());
    assert!(
        String::from_utf8(output.stderr)
            .unwrap()
            .contains("co login")
    );
    let output = config.run("http://127.0.0.1:1", &["--help"]);
    assert!(output.status.success());
    assert!(String::from_utf8(output.stdout).unwrap().contains("--json"));
}

#[test]
fn accepts_api_name_boundaries_and_preserves_dot_names() {
    for name in [".config".to_string(), "a".repeat(128)] {
        let (output, request) = mock_create(
            &[&format!("my-org/{name}"), "--json"],
            "201 Created",
            repository(true),
        );
        assert!(output.status.success());
        assert_eq!(request["name"], name);
    }
    let output = TestConfig::new(false).run("http://127.0.0.1:1", &[&"a".repeat(129)]);
    assert!(!output.status.success());
    assert!(String::from_utf8(output.stderr).unwrap().contains("1-128"));
}
