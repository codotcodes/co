use serde_json::json;
use std::io::{Read, Write};
use std::net::TcpListener;
use std::process::{Command, Stdio};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

#[test]
fn agent_mode_never_returns_human_credentials_on_invalid_or_missing_grants() {
    let root = std::env::temp_dir().join(format!("co-agent-fail-{}", std::process::id()));
    std::fs::create_dir_all(&root).unwrap();
    std::fs::write(
        root.join("config.json"),
        json!({"session_token":"human-secret"}).to_string(),
    )
    .unwrap();
    for (id, request) in [
        (
            "agent",
            "protocol=https\nhost=git.co.codes\npath=owner/repo.git\n\n",
        ),
        ("agent", "protocol=https\nhost=git.co.codes\n\n"),
        (
            "agent",
            "protocol=https\nhost=evil.example\npath=owner/repo.git\n\n",
        ),
        (
            "",
            "protocol=https\nhost=git.co.codes\npath=owner/repo.git\n\n",
        ),
    ] {
        let mut process = Command::new(env!("CARGO_BIN_EXE_co"))
            .args(["git-credential", "get"])
            .env("CO_CONFIG_DIR", &root)
            .env("CO_AGENT_ID", id)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        process
            .stdin
            .take()
            .unwrap()
            .write_all(request.as_bytes())
            .unwrap();
        let result = process.wait_with_output().unwrap();
        assert!(!result.status.success());
        assert_eq!(String::from_utf8_lossy(&result.stdout), "quit=true\n\n");
        assert!(!String::from_utf8_lossy(&result.stderr).contains("human-secret"));
    }
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn attestation_uses_selected_agent_lineage_and_explicit_commit() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let api = format!("http://{}", listener.local_addr().unwrap());
    let oid = "a".repeat(40);
    let server_oid = oid.clone();
    let server = std::thread::spawn(move || {
        for (path, authorization, body) in [
            (
                "/grants/selected-grant/token".to_string(),
                "Lineage agent-lineage",
                json!({"accessToken":"agent-access"}),
            ),
            (
                format!("/repos/owner/repo/commits/{server_oid}/provenance"),
                "Bearer agent-access",
                json!({"created":true}),
            ),
        ] {
            let deadline = std::time::Instant::now() + Duration::from_secs(10);
            let mut stream = loop {
                match listener.accept() {
                    Ok((stream, _)) => break stream,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(
                            std::time::Instant::now() < deadline,
                            "missing request {path}"
                        );
                        std::thread::sleep(Duration::from_millis(10));
                    }
                    Err(e) => panic!("{e}"),
                }
            };
            stream
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let mut request = Vec::new();
            while !request.windows(4).any(|part| part == b"\r\n\r\n") {
                let mut buffer = [0; 4096];
                let n = stream.read(&mut buffer).unwrap();
                assert!(n > 0);
                request.extend_from_slice(&buffer[..n]);
            }
            let request = String::from_utf8(request).unwrap();
            assert!(request.starts_with(&format!("POST {path} HTTP/1.1\r\n")));
            assert!(
                request
                    .to_lowercase()
                    .contains(&format!("authorization: {}", authorization.to_lowercase()))
            );
            assert!(!request.contains("human-secret"));
            let body = body.to_string();
            write!(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
        }
    });
    let root = std::env::temp_dir().join(format!("co-agent-success-{}", std::process::id()));
    std::fs::create_dir_all(&root).unwrap();
    let expires = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs()
        + 600;
    std::fs::write(root.join("config.json"), json!({
        "session_token":"human-secret",
        "agent_grants":[
            {"agent_id":"other","owner":"owner","repo":"repo","grant_id":"wrong-grant","lineage_token":"wrong-lineage","operations":["push"],"expires_unix":expires},
            {"agent_id":"selected","owner":"owner","repo":"repo","grant_id":"selected-grant","lineage_token":"agent-lineage","operations":["push"],"expires_unix":expires}
        ]
    }).to_string()).unwrap();
    let result = Command::new(env!("CARGO_BIN_EXE_co"))
        .args(["agent", "attest", "owner/repo", &oid])
        .env("CO_CONFIG_DIR", &root)
        .env("CO_AGENT_ID", "selected")
        .env("CO_API_URL", api)
        .env("NO_PROXY", "127.0.0.1")
        .output()
        .unwrap();
    assert!(
        result.status.success(),
        "{}",
        String::from_utf8_lossy(&result.stderr)
    );
    assert!(
        String::from_utf8_lossy(&result.stdout).contains(&format!("Attested {oid} as selected"))
    );
    assert!(!String::from_utf8_lossy(&result.stdout).contains("agent-access"));
    server.join().unwrap();
    std::fs::remove_dir_all(root).unwrap();
}
