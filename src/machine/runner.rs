use super::MachineIdentity;
use crate::{client, decode, network_error};
use base64::{Engine as _, engine::general_purpose::STANDARD};
use serde::{Deserialize, Serialize};
use std::io::Read;
use std::path::Path;
use std::process::{Command, Stdio};
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::thread;
use std::time::{Duration, Instant};

const STEP_TIMEOUT: Duration = Duration::from_secs(600);
const IMAGE: &str = "docker.io/library/alpine@sha256:1f3591b8a02ea153f41c5bba878ad477f63ab3d19349762cb77504db02a23e15";

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Assignment {
    id: String,
    assignment_token: String,
    checkout_url: String,
    commit_oid: String,
    steps: Vec<Step>,
    vcpus: u32,
    #[serde(rename = "ramMiB")]
    ram_mib: u32,
}

#[derive(Deserialize)]
struct Step {
    name: String,
    run: String,
}

#[derive(Deserialize)]
struct NextJob {
    job: Assignment,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct StepResult {
    name: String,
    exit_code: i32,
    output: String,
}

fn post(
    machine: &MachineIdentity,
    path: &str,
    body: impl Serialize,
) -> Result<reqwest::blocking::Response, String> {
    client()?
        .post(format!("{}{path}", machine.api_url))
        .bearer_auth(&machine.credential)
        .json(&body)
        .send()
        .map_err(network_error)
}

pub(super) fn poll(machine: &MachineIdentity) -> Result<(), String> {
    let response = post(
        machine,
        "/machine-enrollment/next-job",
        serde_json::json!({}),
    )?;
    if response.status() == reqwest::StatusCode::NO_CONTENT {
        return Ok(());
    }
    let job: NextJob = decode(response)?;
    eprintln!("Running job {} on machine {}", job.job.id, machine.id);
    let (results, success) = execute(machine, &job.job);
    let response = post(
        machine,
        &format!("/machine-enrollment/jobs/{}/finish", job.job.id),
        serde_json::json!({ "assignmentToken": job.job.assignment_token,
            "result": if success { "success" } else { "failure" }, "steps": results }),
    )?;
    if response.status() == reqwest::StatusCode::CONFLICT {
        return Err("assignment expired or cancelled; result discarded".into());
    }
    let _: serde_json::Value = if response.status() == reqwest::StatusCode::NO_CONTENT {
        serde_json::Value::Null
    } else {
        decode(response)?
    };
    Ok(())
}

fn valid_job(job: &Assignment, api_url: &str) -> bool {
    let Ok(url) = reqwest::Url::parse(&job.checkout_url) else {
        return false;
    };
    let local_api = reqwest::Url::parse(api_url).ok().is_some_and(|api| {
        matches!(api.host_str(), Some("127.0.0.1" | "localhost"))
            || api
                .host_str()
                .is_some_and(|host| host.ends_with(".localhost"))
    });
    let trusted_origin = url.scheme() == "https"
        && matches!(url.host_str(), Some("git.co.codes" | "co.codes"))
        || local_api
            && url.scheme() == "http"
            && matches!(url.host_str(), Some("127.0.0.1" | "localhost"))
        || cfg!(test) && url.scheme() == "file";
    trusted_origin
        && url.path().ends_with(".git")
        && url.query().is_none()
        && url.fragment().is_none()
        && url.username().is_empty()
        && url.password().is_none()
        && job.commit_oid.len() >= 40
        && job.commit_oid.len() <= 64
        && job
            .commit_oid
            .bytes()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
        && job.vcpus > 0
        && job.ram_mib > 0
        && !job.assignment_token.is_empty()
        && job.steps.len() <= 20
}

fn checkout(job: &Assignment, root: &Path, api_url: &str) -> Result<(), String> {
    if !valid_job(job, api_url) {
        return Err("invalid job assignment".into());
    }
    let auth = STANDARD.encode(format!("co:{}", job.assignment_token));
    let status = Command::new("git")
        .args(["clone", "--no-checkout", "--", &job.checkout_url, "source"])
        .current_dir(root)
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("GIT_CONFIG_COUNT", "1")
        .env("GIT_CONFIG_KEY_0", "http.extraHeader")
        .env("GIT_CONFIG_VALUE_0", format!("Authorization: Basic {auth}"))
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|error| format!("unable to start checkout: {error}"))?;
    if !wait_bounded(status, STEP_TIMEOUT)? {
        return Err("unable to fetch the exact commit".into());
    }
    let status = Command::new("git")
        .args(["-C", "source", "checkout", "--detach", &job.commit_oid])
        .current_dir(root)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|error| format!("unable to check out commit: {error}"))?;
    if !wait_bounded(status, STEP_TIMEOUT)? {
        return Err("exact commit is unavailable".into());
    }
    Ok(())
}

fn wait_bounded(mut child: std::process::Child, timeout: Duration) -> Result<bool, String> {
    let start = Instant::now();
    loop {
        match child.try_wait() {
            Ok(Some(status)) => return Ok(status.success()),
            Err(error) => return Err(format!("process wait failed: {error}")),
            Ok(None) if start.elapsed() >= timeout => {
                let _ = child.kill();
                let _ = child.wait();
                return Ok(false);
            }
            Ok(None) => thread::sleep(Duration::from_millis(200)),
        }
    }
}

fn drain<R: Read + Send + 'static>(mut stream: R) -> thread::JoinHandle<String> {
    thread::spawn(move || {
        let mut kept = Vec::with_capacity(2048);
        let mut buffer = [0u8; 8192];
        while let Ok(count) = stream.read(&mut buffer) {
            if count == 0 {
                break;
            }
            kept.extend_from_slice(&buffer[..count.min(2048 - kept.len())]);
        }
        String::from_utf8_lossy(&kept)
            .chars()
            .filter(|c| !c.is_control() || matches!(c, '\n' | '\t'))
            .collect()
    })
}

fn execute_step(
    image: &str,
    directory: &Path,
    step: &Step,
    job: &Assignment,
    index: usize,
    cancelled: &AtomicBool,
    deadline: Instant,
) -> StepResult {
    let container = format!("co-{}-{index}", job.id);
    let mut command = Command::new("podman");
    command.args([
        "run",
        "--rm",
        "--pull=never",
        "--name",
        &container,
        "--network=none",
        "--read-only",
        "--cap-drop=ALL",
        "--security-opt=no-new-privileges",
        "--pids-limit=256",
        "--userns=keep-id",
        "--tmpfs=/tmp:rw,nosuid,nodev,size=64m",
        "--workdir=/workspace",
        "--env=HOME=/tmp",
        "--env=PATH=/usr/local/bin:/usr/bin:/bin",
    ]);
    command.arg(format!("--memory={}m", job.ram_mib));
    command.arg(format!("--cpus={}", job.vcpus));
    command.arg(format!(
        "--volume={}:{}:rw,Z",
        directory.display(),
        "/workspace"
    ));
    command.args([image, "sh", "-e", "-c", &step.run]);
    command.stdout(Stdio::piped()).stderr(Stdio::piped());
    let mut child = match command.spawn() {
        Ok(child) => child,
        Err(error) => {
            return StepResult {
                name: step.name.clone(),
                exit_code: 1,
                output: format!("Unable to start container: {error}"),
            };
        }
    };
    let stdout = drain(child.stdout.take().expect("piped stdout"));
    let stderr = drain(child.stderr.take().expect("piped stderr"));
    let start = Instant::now();
    let exit_code = loop {
        match child.try_wait() {
            Ok(Some(status)) => break status.code().unwrap_or(1),
            Err(_) => {
                let _ = child.kill();
                let _ = child.wait();
                break 1;
            }
            Ok(None)
                if cancelled.load(Ordering::Relaxed)
                    || start.elapsed() >= STEP_TIMEOUT
                    || Instant::now() >= deadline =>
            {
                let _ = Command::new("podman")
                    .args(["stop", "--time=0", &container])
                    .output();
                let _ = child.kill();
                let _ = child.wait();
                break 1;
            }
            Ok(None) => thread::sleep(Duration::from_millis(200)),
        }
    };
    let mut output = stdout.join().unwrap_or_default();
    let errors = stderr.join().unwrap_or_default();
    let cut = |text: &str, bytes: usize| {
        if text.len() <= bytes {
            return text.len();
        }
        text.char_indices()
            .map(|(index, _)| index)
            .take_while(|index| *index <= bytes)
            .last()
            .unwrap_or(0)
    };
    output.truncate(cut(&output, 512));
    output.push_str(&errors[..cut(&errors, 1024 - output.len())]);
    StepResult {
        name: step.name.clone(),
        exit_code,
        output,
    }
}

fn execute(machine: &MachineIdentity, job: &Assignment) -> (Vec<StepResult>, bool) {
    let deadline = Instant::now() + Duration::from_secs(1800);
    let cancelled = Arc::new(AtomicBool::new(false));
    let renew_stop = Arc::new(AtomicBool::new(false));
    let renew_machine = machine.clone();
    let id = job.id.clone();
    let token = job.assignment_token.clone();
    let stop = Arc::clone(&renew_stop);
    let flag = Arc::clone(&cancelled);
    let renewer = thread::spawn(move || {
        while !stop.load(Ordering::Relaxed) {
            thread::sleep(Duration::from_secs(20));
            if stop.load(Ordering::Relaxed) {
                break;
            }
            let response = post(
                &renew_machine,
                &format!("/machine-enrollment/jobs/{id}/renew"),
                serde_json::json!({"assignmentToken": token}),
            );
            if matches!(response, Ok(ref response) if response.status() == reqwest::StatusCode::CONFLICT || response.status() == reqwest::StatusCode::UNAUTHORIZED)
            {
                flag.store(true, Ordering::Relaxed);
                break;
            }
        }
    });
    let result = (|| {
        let root =
            tempfile::tempdir().map_err(|error| format!("unable to create workspace: {error}"))?;
        checkout(job, root.path(), &machine.api_url)?;
        let image = std::env::var("CO_MACHINE_IMAGE").unwrap_or_else(|_| IMAGE.to_owned());
        if !image.contains("@sha256:") {
            return Err("CO_MACHINE_IMAGE must be pinned by digest".into());
        }
        let mut steps = Vec::new();
        for (index, step) in job.steps.iter().enumerate() {
            if cancelled.load(Ordering::Relaxed) || Instant::now() >= deadline {
                break;
            }
            let result = execute_step(
                &image,
                &root.path().join("source"),
                step,
                job,
                index,
                &cancelled,
                deadline,
            );
            let failed = result.exit_code != 0;
            steps.push(result);
            if failed {
                break;
            }
        }
        Ok::<_, String>(steps)
    })();
    renew_stop.store(true, Ordering::Relaxed);
    // The renewal thread is deliberately detached: waiting for its sleep would
    // delay completion. Its stop flag prevents further requests after wake-up.
    drop(renewer);
    match result {
        Ok(steps) => {
            let success = !cancelled.load(Ordering::Relaxed)
                && steps.len() == job.steps.len()
                && steps.iter().all(|step| step.exit_code == 0);
            (steps, success)
        }
        Err(message) => (
            vec![StepResult {
                name: job.steps.first().map_or("Checkout", |s| &s.name).to_owned(),
                exit_code: 1,
                output: message,
            }],
            false,
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::net::{TcpListener, TcpStream};

    #[test]
    fn checkout_origin_is_bound_to_the_api_environment() {
        let job = Assignment {
            id: "test".into(),
            assignment_token: "token".into(),
            checkout_url: "https://git.co.codes/owner/repo.git".into(),
            commit_oid: "a".repeat(40),
            steps: vec![],
            vcpus: 1,
            ram_mib: 128,
        };
        assert!(valid_job(&job, "https://api.co.codes"));
        assert!(!valid_job(
            &Assignment {
                checkout_url: "http://127.0.0.1:7700/owner/repo.git".into(),
                ..job
            },
            "https://api.co.codes"
        ));
        let local = Assignment {
            checkout_url: "http://127.0.0.1:7700/owner/repo.git".into(),
            commit_oid: "a".repeat(40),
            id: "test".into(),
            assignment_token: "token".into(),
            steps: vec![],
            vcpus: 1,
            ram_mib: 128,
        };
        assert!(valid_job(&local, "https://api.co.localhost"));
        assert!(!valid_job(&local, "https://api.co.localhost.evil.example"));
    }

    #[test]
    #[ignore = "requires rootless Podman and the pinned Alpine image"]
    fn isolated_step_has_workspace_without_runner_environment() {
        let root = tempfile::tempdir().unwrap();
        let job = Assignment {
            id: "00000000-0000-4000-8000-000000000001".into(),
            assignment_token: "secret-token".into(),
            checkout_url: "https://git.co.codes/test/repo.git".into(),
            commit_oid: "a".repeat(40),
            steps: vec![],
            vcpus: 1,
            ram_mib: 128,
        };
        let step = Step { name: "Isolation".into(),
            run: "test -z \"$CO_RUNNER_TEST_SECRET\" && echo result > /workspace/output.txt && cat /workspace/output.txt".into() };
        let result = execute_step(
            IMAGE,
            root.path(),
            &step,
            &job,
            0,
            &AtomicBool::new(false),
            Instant::now() + STEP_TIMEOUT,
        );
        assert_eq!(result.exit_code, 0, "{}", result.output);
        assert_eq!(
            std::fs::read_to_string(root.path().join("output.txt")).unwrap(),
            "result\n"
        );
    }

    #[test]
    #[ignore = "requires Git, rootless Podman and the pinned Alpine image"]
    fn exact_commit_runs_inside_container_without_checkout_token() {
        let root = tempfile::tempdir().unwrap();
        let source = root.path().join("sample.git");
        assert!(
            Command::new("git")
                .args(["init", "--quiet", "--initial-branch=main"])
                .arg(&source)
                .status()
                .unwrap()
                .success()
        );
        std::fs::write(source.join("payload.txt"), "verified commit\n").unwrap();
        assert!(
            Command::new("git")
                .args(["-C"])
                .arg(&source)
                .args(["add", "payload.txt"])
                .status()
                .unwrap()
                .success()
        );
        assert!(
            Command::new("git")
                .args(["-C"])
                .arg(&source)
                .args([
                    "-c",
                    "user.email=test@example.test",
                    "-c",
                    "user.name=CI",
                    "commit",
                    "--quiet",
                    "-m",
                    "Fixture"
                ])
                .status()
                .unwrap()
                .success()
        );
        let oid = String::from_utf8(
            Command::new("git")
                .args(["-C"])
                .arg(&source)
                .args(["rev-parse", "HEAD"])
                .output()
                .unwrap()
                .stdout,
        )
        .unwrap();
        let job = Assignment { id: "00000000-0000-4000-8000-000000000002".into(),
            assignment_token: "not-visible-inside-container".into(),
            checkout_url: format!("file://{}", source.display()), commit_oid: oid.trim().to_owned(),
            steps: vec![Step { name: "Verify".into(),
                run: "test \"$(cat payload.txt)\" = 'verified commit' && test -z \"$GIT_CONFIG_VALUE_0\" && echo passed".into() }],
            vcpus: 1, ram_mib: 128 };
        let machine = MachineIdentity {
            id: "test".into(),
            scope: "owner".into(),
            api_url: "http://127.0.0.1:1".into(),
            credential: "unused".into(),
        };
        let (steps, passed) = execute(&machine, &job);
        assert!(
            passed,
            "{:?}",
            steps.iter().map(|step| &step.output).collect::<Vec<_>>()
        );
        assert!(steps[0].output.contains("passed"));
    }

    fn read_request(stream: &mut TcpStream) -> (String, serde_json::Value) {
        let mut bytes = Vec::new();
        let (end, length, path) = loop {
            let mut buffer = [0u8; 4096];
            let count = stream.read(&mut buffer).unwrap();
            assert!(count > 0);
            bytes.extend_from_slice(&buffer[..count]);
            if let Some(end) = bytes.windows(4).position(|value| value == b"\r\n\r\n") {
                let header = String::from_utf8_lossy(&bytes[..end]);
                let length = header
                    .lines()
                    .find_map(|line| {
                        line.to_ascii_lowercase()
                            .strip_prefix("content-length: ")
                            .and_then(|value| value.parse::<usize>().ok())
                    })
                    .unwrap_or(0);
                let path = header
                    .lines()
                    .next()
                    .unwrap()
                    .split_whitespace()
                    .nth(1)
                    .unwrap()
                    .to_owned();
                break (end + 4, length, path);
            }
        };
        while bytes.len() < end + length {
            let mut buffer = [0u8; 4096];
            let count = stream.read(&mut buffer).unwrap();
            assert!(count > 0);
            bytes.extend_from_slice(&buffer[..count]);
        }
        (
            path,
            serde_json::from_slice(&bytes[end..end + length]).unwrap(),
        )
    }

    #[test]
    #[ignore = "requires Git, rootless Podman and the pinned Alpine image"]
    fn polled_job_returns_exact_commit_step_logs() {
        let root = tempfile::tempdir().unwrap();
        let source = root.path().join("sample.git");
        assert!(
            Command::new("git")
                .args(["init", "--quiet", "--initial-branch=main"])
                .arg(&source)
                .status()
                .unwrap()
                .success()
        );
        std::fs::write(source.join("payload.txt"), "from exact commit\n").unwrap();
        assert!(
            Command::new("git")
                .args(["-C"])
                .arg(&source)
                .args(["add", "payload.txt"])
                .status()
                .unwrap()
                .success()
        );
        assert!(
            Command::new("git")
                .args(["-C"])
                .arg(&source)
                .args([
                    "-c",
                    "user.email=test@example.test",
                    "-c",
                    "user.name=CI",
                    "commit",
                    "--quiet",
                    "-m",
                    "Fixture"
                ])
                .status()
                .unwrap()
                .success()
        );
        let oid = String::from_utf8(
            Command::new("git")
                .args(["-C"])
                .arg(&source)
                .args(["rev-parse", "HEAD"])
                .output()
                .unwrap()
                .stdout,
        )
        .unwrap()
        .trim()
        .to_owned();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let api_url = format!("http://{}", listener.local_addr().unwrap());
        let server = thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let (path, _) = read_request(&mut stream);
            assert_eq!(path, "/machine-enrollment/next-job");
            let body = serde_json::json!({"job": {"id": "00000000-0000-4000-8000-000000000003",
                "assignmentToken": "test-token", "checkoutUrl": format!("file://{}", source.display()),
                "commitOid": oid, "steps": [{"name": "Read", "run": "cat payload.txt"}],
                "vcpus": 1, "ramMiB": 128 }}).to_string();
            write!(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
            drop(stream);
            let (mut stream, _) = listener.accept().unwrap();
            let (path, body) = read_request(&mut stream);
            assert_eq!(
                path,
                "/machine-enrollment/jobs/00000000-0000-4000-8000-000000000003/finish"
            );
            assert_eq!(body["result"], "success", "{body}");
            assert_eq!(body["steps"][0]["output"], "from exact commit\n");
            write!(
                stream,
                "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            )
            .unwrap();
        });
        let machine = MachineIdentity {
            id: "test".into(),
            scope: "public".into(),
            api_url,
            credential: "co_runner_test".into(),
        };
        poll(&machine).unwrap();
        server.join().unwrap();
    }
}
