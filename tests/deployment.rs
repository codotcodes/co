use serde_json::{Value, json};
use std::io::{Read, Write};
use std::net::TcpListener;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn probe(
    args: &[&str],
    input: &[u8],
    results: Vec<(u16, Value)>,
) -> (std::process::Output, Vec<(String, Value)>) {
    let directory = tempfile::tempdir().unwrap();
    std::fs::write(
        directory.path().join("config.json"),
        r#"{"session_token":"fixture-session"}"#,
    )
    .unwrap();
    let password = directory.path().join("reauth");
    std::fs::write(&password, "fixture-password\n").unwrap();
    let policy = directory.path().join("policy.json");
    std::fs::write(&policy, r#"{"enabled":false,"branch":"main"}"#).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let api = format!("http://{}", listener.local_addr().unwrap());
    let server = std::thread::spawn(move || {
        let mut requests = Vec::new();
        for (status, result) in results {
            let deadline = Instant::now() + Duration::from_secs(10);
            let mut stream = loop {
                match listener.accept() {
                    Ok((stream, _)) => break stream,
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(
                            Instant::now() < deadline,
                            "CLI did not send expected request"
                        );
                        std::thread::sleep(Duration::from_millis(10));
                    }
                    Err(error) => panic!("{error}"),
                }
            };
            stream
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let mut bytes = Vec::new();
            let end = loop {
                let mut buffer = [0; 4096];
                let read = stream.read(&mut buffer).unwrap();
                assert!(read > 0);
                bytes.extend_from_slice(&buffer[..read]);
                if let Some(index) = bytes.windows(4).position(|bytes| bytes == b"\r\n\r\n") {
                    break index + 4;
                }
            };
            let header = String::from_utf8(bytes[..end].to_vec()).unwrap();
            assert!(
                header
                    .to_lowercase()
                    .contains("authorization: bearer fixture-session")
            );
            let length = header
                .lines()
                .find_map(|line| {
                    line.to_lowercase()
                        .strip_prefix("content-length: ")
                        .and_then(|value| value.parse::<usize>().ok())
                })
                .unwrap_or(0);
            while bytes.len() < end + length {
                let mut buffer = [0; 4096];
                let read = stream.read(&mut buffer).unwrap();
                assert!(read > 0);
                bytes.extend_from_slice(&buffer[..read]);
            }
            let path = header
                .lines()
                .next()
                .unwrap()
                .split_whitespace()
                .nth(1)
                .unwrap()
                .to_string();
            let body = if length == 0 {
                Value::Null
            } else {
                serde_json::from_slice(&bytes[end..end + length]).unwrap()
            };
            requests.push((path, body));
            let body = result.to_string();
            write!(stream, "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
        }
        requests
    });
    let mut command = Command::new(env!("CARGO_BIN_EXE_co"));
    command
        .args(args)
        .env("CO_CONFIG_DIR", directory.path())
        .env("CO_API_URL", api)
        .env_remove("CO_AGENT_ID")
        .env("NO_PROXY", "127.0.0.1")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if args.contains(&"configure") {
        command.arg("--file").arg(policy);
    }
    command.arg("--reauth-file").arg(password);
    let mut child = command.spawn().unwrap();
    child.stdin.take().unwrap().write_all(input).unwrap();
    let output = child.wait_with_output().unwrap();
    let requests = server.join().unwrap();
    (output, requests)
}

fn configuration() -> Value {
    json!({"environments":[{"id":"environment-id","name":"production","version":2}],"secrets":[{"name":"TOKEN","environmentId":"environment-id","version":3}],"reauthenticationMethods":["password"]})
}

#[test]
fn deployment_secret_is_write_only_and_kept_out_of_challenge_and_output() {
    let value = "fixture-production-token\n";
    let (output, requests) = probe(
        &[
            "deployment",
            "set-secret",
            "owner/repo",
            "TOKEN",
            "--environment",
            "production",
        ],
        value.as_bytes(),
        vec![
            (200, configuration()),
            (200, json!({"challengeId":"challenge"})),
            (
                200,
                json!({"action":"write-secret","environment":"production","version":4}),
            ),
        ],
    );
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(!String::from_utf8_lossy(&output.stdout).contains(value));
    assert_eq!(requests[1].1["operation"]["version"], 3);
    assert!(requests[1].1.get("value").is_none());
    assert_eq!(requests[2].1["value"], value);
    assert_eq!(
        requests[2].1["reauthentication"]["password"],
        "fixture-password"
    );
}

#[test]
fn deployment_policy_uses_current_version_and_exact_file_content() {
    let (output, requests) = probe(
        &["deployment", "configure", "owner/repo", "production"],
        &[],
        vec![
            (200, configuration()),
            (200, json!({"challengeId":"challenge"})),
            (
                200,
                json!({"action":"configure-environment","environment":"production","version":3}),
            ),
        ],
    );
    assert!(output.status.success());
    assert_eq!(requests[1].1["operation"]["version"], 2);
    assert_eq!(
        requests[1].1["operation"]["config"],
        json!({"enabled":false,"branch":"main"})
    );
    assert_eq!(requests[1].1["operation"], requests[2].1["operation"]);
}

#[test]
fn approvals_and_rollbacks_use_action_bound_authentication() {
    let id = "11111111-1111-4111-8111-111111111111";
    for action in ["approve", "rollback"] {
        let (output, requests) = probe(
            &["deployment", action, "owner/repo", id],
            &[],
            vec![
                (200, configuration()),
                (200, json!({"challengeId":"challenge"})),
                (200, json!({"id":id,"approved":true})),
            ],
        );
        assert!(output.status.success());
        assert_eq!(requests[1].1["action"], action);
        assert!(requests[1].0.ends_with("/reauthentication"));
        assert!(requests[2].0.ends_with(action));
        assert_eq!(String::from_utf8(output.stdout).unwrap().trim(), id);
    }
}

#[test]
fn reflected_secret_and_password_are_not_printed_on_errors() {
    let secret = "fixture-sensitive-production-token";
    let (output, _) = probe(
        &["deployment", "set-secret", "owner/repo", "TOKEN"],
        secret.as_bytes(),
        vec![
            (200, configuration()),
            (200, json!({"challengeId":"challenge"})),
            (500, json!({"error":secret,"details":"fixture-password"})),
        ],
    );
    assert!(!output.status.success());
    let output = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(!output.contains(secret));
    assert!(!output.contains("fixture-password"));
}
