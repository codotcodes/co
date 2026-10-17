use super::MachineIdentity;
use super::live_logs::{self, Logs};
use crate::{client, decode, network_error};
use base64::{Engine as _, engine::general_purpose::STANDARD};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
#[cfg(test)]
use std::io::Read;
use std::io::Write;
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
    #[serde(default)]
    environment: BTreeMap<String, String>,
    #[serde(default)]
    secret_values: Vec<String>,
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
        serde_json::json!({"capabilities": ["workflow-environment-v1"]}),
    )?;
    if response.status() == reqwest::StatusCode::NO_CONTENT {
        return Ok(());
    }
    let job: NextJob = decode(response)?;
    eprintln!("Running job {} on machine {}", job.job.id, machine.id);
    let (results, success) = execute(machine, &job.job);
    let hash = live_logs::digest(
        results
            .iter()
            .map(|s| (&s.name, s.exit_code, &s.output))
            .collect::<Vec<_>>(),
    );
    let response = post(
        machine,
        &format!("/machine-enrollment/jobs/{}/finish", job.job.id),
        serde_json::json!({ "assignmentToken": job.job.assignment_token,
            "result": if success { "success" } else { "failure" }, "steps": results, "hash": hash }),
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
    let api = reqwest::Url::parse(api_url).ok();
    let local_api = api.as_ref().is_some_and(|api| {
        matches!(api.host_str(), Some("127.0.0.1" | "localhost"))
            || api
                .host_str()
                .is_some_and(|host| host.ends_with(".localhost"))
    });
    let hosted_git = api.as_ref().is_some_and(|api| {
        api.scheme() == "https"
            && matches!(
                (api.host_str(), url.host_str()),
                (Some("api.co.codes"), Some("git.co.codes" | "co.codes"))
                    | (Some("api.codevved.com"), Some("git.codevved.com"))
            )
    });
    let trusted_origin = url.scheme() == "https" && url.port().is_none() && hosted_git
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
        && valid_environment(job)
}

fn valid_environment(job: &Assignment) -> bool {
    job.environment.len() <= 100
        && job.environment.values().map(String::len).sum::<usize>() <= 65_536
        && job.environment.iter().all(|(name, value)| {
            !name.is_empty()
                && name.len() <= 100
                && name.bytes().enumerate().all(|(i, c)| {
                    c == b'_' || c.is_ascii_alphabetic() || i > 0 && c.is_ascii_digit()
                })
                && ![
                    "HOME",
                    "PATH",
                    "ENV",
                    "BASH_ENV",
                    "IFS",
                    "CDPATH",
                    "SHELLOPTS",
                    "BASHOPTS",
                    "__proto__",
                    "constructor",
                    "prototype",
                ]
                .contains(&name.as_str())
                && !["CO_", "LD_", "DYLD_"]
                    .iter()
                    .any(|prefix| name.starts_with(prefix))
                && value.len() <= 16_384
                && !value.contains('\0')
        })
        && job.secret_values.len() <= 100
        && job.secret_values.iter().all(|secret| {
            !secret.is_empty() && job.environment.values().any(|value| value == secret)
        })
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

fn execute_step(
    image: &str,
    directory: &Path,
    step: &Step,
    job: &Assignment,
    cancelled: &AtomicBool,
    deadline: Instant,
    logs: &Logs,
) -> StepResult {
    let index = logs.lock().unwrap().len();
    live_logs::begin(logs, &step.name);
    let environment = match environment_file(job) {
        Ok(file) => file,
        Err(()) => {
            let message = "Unable to prepare the workflow environment.";
            live_logs::append(logs, index, message);
            logs.lock().unwrap()[index].exit_code = Some(1);
            return StepResult {
                name: step.name.clone(),
                exit_code: 1,
                output: message.into(),
            };
        }
    };
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
    // Load values after entering the container. Inheriting them into Podman
    // itself would let CONTAINERS_CONF/CONTAINER_HOST alter the host launcher.
    command.arg(format!(
        "--volume={}:/run/co-environment.sh:ro,Z",
        environment.path().display()
    ));
    command.arg(format!(
        "--volume={}:{}:rw,Z",
        directory.display(),
        "/workspace"
    ));
    command.args([
        image,
        "/bin/sh",
        "-e",
        "-c",
        ". /run/co-environment.sh\nexec /bin/sh -e -c \"$1\"",
        "co-step",
        &step.run,
    ]);
    command.stdout(Stdio::piped()).stderr(Stdio::piped());
    let mut child = match command.spawn() {
        Ok(child) => child,
        Err(error) => {
            live_logs::append(logs, index, &format!("Unable to start container: {error}"));
            let mut live = logs.lock().unwrap();
            live[index].exit_code = Some(1);
            return StepResult {
                name: step.name.clone(),
                exit_code: 1,
                output: live[index].output.clone(),
            };
        }
    };
    let stdout = live_logs::drain_masked(
        child.stdout.take().expect("piped stdout"),
        logs.clone(),
        index,
        job.secret_values.clone(),
    );
    let stderr = live_logs::drain_masked(
        child.stderr.take().expect("piped stderr"),
        logs.clone(),
        index,
        job.secret_values.clone(),
    );
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
    let _ = stdout.join();
    let _ = stderr.join();
    let mut live = logs.lock().unwrap();
    live[index].exit_code = Some(exit_code);
    let output = live[index].output.clone();
    StepResult {
        name: step.name.clone(),
        exit_code,
        output,
    }
}

fn environment_file(job: &Assignment) -> Result<tempfile::NamedTempFile, ()> {
    if !valid_environment(job) {
        return Err(());
    }
    // NamedTempFile is owner-only and unlinks on drop, including failed steps.
    let mut file = tempfile::NamedTempFile::new().map_err(|_| ())?;
    for (name, value) in &job.environment {
        let quoted = value.replace('\'', "'\\''");
        writeln!(file, "export {name}='{quoted}'").map_err(|_| ())?;
    }
    file.flush().map_err(|_| ())?;
    Ok(file)
}

fn execute(machine: &MachineIdentity, job: &Assignment) -> (Vec<StepResult>, bool) {
    let logs = Logs::default();
    let upload_stop = Arc::new(AtomicBool::new(false));
    let uploader = live_logs::upload(
        machine.clone(),
        job.id.clone(),
        job.assignment_token.clone(),
        logs.clone(),
        upload_stop.clone(),
    );
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
        for step in &job.steps {
            if cancelled.load(Ordering::Relaxed) || Instant::now() >= deadline {
                break;
            }
            let result = execute_step(
                &image,
                &root.path().join("source"),
                step,
                job,
                &cancelled,
                deadline,
                &logs,
            );
            let failed = result.exit_code != 0;
            steps.push(result);
            if failed {
                break;
            }
        }
        Ok::<_, String>(steps)
    })();
    upload_stop.store(true, Ordering::Relaxed);
    uploader.thread().unpark();
    let _ = uploader.join();
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
    fn rejects_reserved_environment_names_and_unbounded_values() {
        let mut job: Assignment = serde_json::from_value(serde_json::json!({
            "id": "test", "assignmentToken": "token", "checkoutUrl": "https://git.co.codes/a/b.git",
            "commitOid": "a".repeat(40), "steps": [], "vcpus": 1, "ramMiB": 128,
            "environment": { "TOKEN": "secret" }, "secretValues": ["secret"]
        }))
        .unwrap();
        assert!(valid_environment(&job));
        for name in [
            "PATH",
            "HOME",
            "BASH_ENV",
            "CO_TOKEN",
            "LD_PRELOAD",
            "BAD-NAME",
        ] {
            job.environment.insert(name.into(), "x".into());
            assert!(!valid_environment(&job));
            job.environment.remove(name);
        }
        job.environment.insert("TOKEN".into(), "x".repeat(16_385));
        assert!(!valid_environment(&job));
    }

    #[test]
    fn environment_script_quotes_values_and_is_ephemeral() {
        let job: Assignment = serde_json::from_value(serde_json::json!({
            "id": "test", "assignmentToken": "token", "checkoutUrl": "https://git.co.codes/a/b.git",
            "commitOid": "a".repeat(40), "steps": [], "vcpus": 1, "ramMiB": 128,
            "environment": {"TOKEN": "literal'$(touch /tmp/co-script-injection)\nnext"},
            "secretValues": []
        }))
        .unwrap();
        let file = environment_file(&job).unwrap();
        let path = file.path().to_path_buf();
        let source = std::fs::read_to_string(&path).unwrap();
        assert_eq!(
            source,
            "export TOKEN='literal'\\''$(touch /tmp/co-script-injection)\nnext'\n"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
                0o600
            );
        }
        drop(file);
        assert!(!path.exists());
    }

    #[test]
    #[ignore = "requires rootless Podman and the pinned Alpine image"]
    fn declared_environment_is_injected_and_redacted_before_upload() {
        let root = tempfile::tempdir().unwrap();
        let job: Assignment = serde_json::from_value(serde_json::json!({
            "id": "00000000-0000-4000-8000-000000000099", "assignmentToken": "checkout-only",
            "checkoutUrl": "https://git.co.codes/a/b.git", "commitOid": "a".repeat(40), "steps": [], "vcpus": 1, "ramMiB": 128,
            "environment": { "TOKEN": "fixture-sensitive", "REGION": "canada", "MULTILINE": "line one\nline two", "CONTAINERS_CONF": "/co-test-must-not-affect-host" },
            "secretValues": ["fixture-sensitive"]
        })).unwrap();
        let step = Step { name: "Environment".into(), run: "test \"$REGION\" = canada; test \"$CONTAINERS_CONF\" = /co-test-must-not-affect-host; test \"$MULTILINE\" = \"$(printf 'line one\\nline two')\"; printf '%s' \"$TOKEN\"".into() };
        let logs = Logs::default();
        let result = execute_step(
            IMAGE,
            root.path(),
            &step,
            &job,
            &AtomicBool::new(false),
            Instant::now() + STEP_TIMEOUT,
            &logs,
        );
        assert_eq!(result.exit_code, 0);
        assert_eq!(result.output, "***");
        assert_eq!(logs.lock().unwrap()[0].output, "***");
    }

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
            environment: BTreeMap::new(),
            secret_values: vec![],
        };
        assert!(valid_job(&job, "https://api.co.codes"));
        assert!(!valid_job(&job, "https://api.codevved.com"));
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
            environment: BTreeMap::new(),
            secret_values: vec![],
        };
        assert!(valid_job(&local, "https://api.co.localhost"));
        assert!(!valid_job(&local, "https://api.co.localhost.evil.example"));

        let mut staging = Assignment {
            checkout_url: "https://git.codevved.com/owner/repo.git".into(),
            ..local
        };
        assert!(valid_job(&staging, "https://api.codevved.com"));
        assert!(!valid_job(&staging, "https://api.co.codes"));
        staging.checkout_url = "https://git.codevved.com.evil.example/owner/repo.git".into();
        assert!(!valid_job(&staging, "https://api.codevved.com"));
        staging.checkout_url = "https://git.codevved.com:444/owner/repo.git".into();
        assert!(!valid_job(&staging, "https://api.codevved.com"));
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
            environment: BTreeMap::new(),
            secret_values: vec![],
        };
        let step = Step { name: "Isolation".into(),
            run: "test -z \"$CO_RUNNER_TEST_SECRET\" && echo result > /workspace/output.txt && cat /workspace/output.txt".into() };
        let result = execute_step(
            IMAGE,
            root.path(),
            &step,
            &job,
            &AtomicBool::new(false),
            Instant::now() + STEP_TIMEOUT,
            &Logs::default(),
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
            vcpus: 1, ram_mib: 128, environment: BTreeMap::new(), secret_values: vec![] };
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
            let (path, request) = read_request(&mut stream);
            assert_eq!(path, "/machine-enrollment/next-job");
            assert_eq!(request["capabilities"][0], "workflow-environment-v1");
            let body = serde_json::json!({"job": {"id": "00000000-0000-4000-8000-000000000003",
                "assignmentToken": "test-token", "checkoutUrl": format!("file://{}", source.display()),
                "commitOid": oid, "steps": [{"name": "Read", "run": "cat payload.txt; sleep 5; printf '%s\\n' \"$TOKEN\""}],
                "environment": {"TOKEN": "fixture-secret-token"}, "secretValues": ["fixture-secret-token"],
                "vcpus": 1, "ramMiB": 128 }}).to_string();
            write!(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
            drop(stream);
            let mut saw_live = false;
            let mut retry = true;
            let mut repair_requested = false;
            let mut repaired = false;
            loop {
                let (mut stream, _) = listener.accept().unwrap();
                let (path, body) = read_request(&mut stream);
                assert!(!body.to_string().contains("fixture-secret-token"));
                if path.ends_with("/log-deltas") {
                    assert_eq!(body["assignmentToken"], "test-token");
                    if body["update"]["steps"][0]["output"] == "from exact commit\n" {
                        assert!(body["update"]["steps"][0]["exitCode"].is_null());
                        saw_live = !retry;
                    }
                    if retry {
                        retry = false;
                        write!(stream, "HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                        continue;
                    }
                    if !repair_requested {
                        repair_requested = true;
                        let reply =
                            serde_json::json!({"type":"resync","hash":"a".repeat(64)}).to_string();
                        write!(stream,"HTTP/1.1 409 Conflict\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{reply}",reply.len()).unwrap();
                        continue;
                    }
                    if !repaired {
                        assert_eq!(body["update"]["reset"], true);
                        assert_eq!(body["update"]["baseHash"], "a".repeat(64));
                        assert_eq!(body["update"]["steps"][0]["offset"], 0);
                        repaired = true;
                    }
                    let reply =
                        serde_json::json!({"type":"ack","hash":body["update"]["hash"]}).to_string();
                    write!(stream,"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{reply}",reply.len()).unwrap();
                } else {
                    assert!(path.ends_with("/finish"));
                    assert!(
                        saw_live,
                        "Output must arrive while the step is still running"
                    );
                    assert_eq!(body["result"], "success", "{body}");
                    assert_eq!(body["steps"][0]["output"], "from exact commit\n***\n");
                    assert!(repaired);
                    assert_eq!(
                        body["hash"],
                        live_logs::digest(vec![("Read", 0, "from exact commit\n***\n")])
                    );
                    write!(
                        stream,
                        "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                    )
                    .unwrap();
                    break;
                }
            }
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
