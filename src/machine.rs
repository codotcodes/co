use super::{api_url, client, decode, load_config, mutate_config, network_error};
use serde::{Deserialize, Serialize};
use std::io::{self, BufRead, IsTerminal};
#[cfg(target_os = "macos")]
use std::process::Command;
use std::thread;
use std::time::Duration;

#[derive(Clone, Debug, Deserialize, Serialize)]
pub(super) struct MachineIdentity {
    id: String,
    scope: String,
    api_url: String,
    credential: String,
}

#[derive(Deserialize)]
struct Enrollment {
    #[serde(rename = "machineId")]
    machine_id: String,
    scope: String,
    credential: String,
}

fn ram_mib() -> Result<u64, String> {
    #[cfg(target_os = "linux")]
    {
        let meminfo = std::fs::read_to_string("/proc/meminfo")
            .map_err(|error| format!("unable to detect RAM: {error}"))?;
        let kib = meminfo
            .lines()
            .find_map(|line| line.strip_prefix("MemTotal:")?.split_whitespace().next())
            .ok_or("unable to detect total RAM")?
            .parse::<u64>()
            .map_err(|error| format!("invalid total RAM: {error}"))?;
        Ok(kib.div_ceil(1024))
    }
    #[cfg(target_os = "macos")]
    {
        let output = Command::new("sysctl")
            .args(["-n", "hw.memsize"])
            .output()
            .map_err(|error| format!("unable to detect RAM: {error}"))?;
        if !output.status.success() {
            return Err("unable to detect RAM with sysctl".into());
        }
        let bytes = String::from_utf8(output.stdout)
            .map_err(|error| format!("invalid RAM output: {error}"))?
            .trim()
            .parse::<u64>()
            .map_err(|error| format!("invalid total RAM: {error}"))?;
        Ok(bytes.div_ceil(1024 * 1024))
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    Err("machine enrollment requires Linux or macOS".into())
}

fn machine_resources() -> Result<serde_json::Value, String> {
    let vcpus = thread::available_parallelism()
        .map_err(|error| format!("unable to detect CPUs: {error}"))?
        .get();
    let ram = ram_mib()?;
    if vcpus > 1024 || ram > 16_777_216 {
        return Err("detected resources exceed supported limits".into());
    }
    Ok(serde_json::json!({
        "attributes": [std::env::consts::OS, std::env::consts::ARCH],
        "vcpus": vcpus,
        "ramMiB": ram
    }))
}

fn enroll(args: &[String]) -> Result<(), String> {
    let mut setup_key = false;
    let mut owner: Option<&str> = None;
    let mut label: Option<&str> = None;
    let mut index = 0;
    while index < args.len() {
        match args[index].as_str() {
            "--setup-key" if !setup_key => setup_key = true,
            "--owner" if owner.is_none() => {
                index += 1;
                owner = Some(args.get(index).ok_or("--owner requires a slug")?);
            }
            "--label" if label.is_none() => {
                index += 1;
                label = Some(args.get(index).ok_or("--label requires a name")?);
            }
            _ => {
                return Err(
                    "usage: co machine enroll --setup-key | --owner SLUG --label NAME".into(),
                );
            }
        }
        index += 1;
    }
    if setup_key == owner.is_some()
        || (owner.is_none() && label.is_some())
        || (owner.is_some() && label.is_none())
    {
        return Err("usage: co machine enroll --setup-key | --owner SLUG --label NAME".into());
    }
    let config = load_config()?;
    let api = api_url(&config);
    let resources = machine_resources()?;
    let (path, request) = if setup_key {
        let key = if io::stdin().is_terminal() {
            rpassword::prompt_password("Paste the setup key: ")
                .map_err(|error| format!("unable to read setup key: {error}"))?
        } else {
            let mut key = String::new();
            io::stdin()
                .lock()
                .read_line(&mut key)
                .map_err(|error| format!("unable to read setup key: {error}"))?;
            key
        };
        let key = key.trim();
        if !key.starts_with("co_setup_") {
            return Err("invalid setup key".into());
        }
        (
            "/machine-enrollment/redeem".to_owned(),
            serde_json::json!({
                "setupKey": key,
                "attributes": resources["attributes"],
                "vcpus": resources["vcpus"],
                "ramMiB": resources["ramMiB"]
            }),
        )
    } else {
        let owner = owner.expect("validated above");
        if owner.is_empty()
            || !owner
                .bytes()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
        {
            return Err("owner must be a lowercase slug".into());
        }
        let label = label.expect("validated above");
        if label.trim().is_empty() || label.len() > 80 {
            return Err("label must be 1-80 characters".into());
        }
        let token = config
            .session_token
            .as_deref()
            .ok_or("not signed in; run `co login`")?;
        let path = format!("/machines/owners/{owner}/self-register");
        let mut request = resources;
        request["label"] = serde_json::json!(label);
        let response = client()?
            .post(format!("{api}{path}"))
            .bearer_auth(token)
            .json(&request)
            .send()
            .map_err(network_error)?;
        let enrollment: Enrollment = decode(response)?;
        return store_enrollment(enrollment, &api);
    };
    let response = client()?
        .post(format!("{api}{path}"))
        .json(&request)
        .send()
        .map_err(network_error)?;
    store_enrollment(decode(response)?, &api)
}

fn store_enrollment(enrollment: Enrollment, api: &str) -> Result<(), String> {
    if !matches!(enrollment.scope.as_str(), "owner" | "public")
        || !enrollment.credential.starts_with("co_runner_")
        || enrollment.machine_id.is_empty()
    {
        return Err("invalid enrollment response".into());
    }
    let id = enrollment.machine_id;
    mutate_config(|config| {
        config.machines.retain(|machine| machine.id != id);
        config.machines.push(MachineIdentity {
            id: id.clone(),
            scope: enrollment.scope,
            api_url: api.into(),
            credential: enrollment.credential,
        });
    })?;
    println!("Machine {id} enrolled. Run `co machine run {id}` to connect it.");
    Ok(())
}

enum HeartbeatError {
    Permanent(String),
    Temporary(String),
}

fn heartbeat(id: &str) -> Result<(), HeartbeatError> {
    let config = load_config().map_err(HeartbeatError::Permanent)?;
    let machine = config
        .machines
        .iter()
        .find(|machine| machine.id == id)
        .ok_or_else(|| {
            HeartbeatError::Permanent("machine is not enrolled in this CLI configuration".into())
        })?;
    let selected = api_url(&config);
    if selected != machine.api_url {
        return Err(HeartbeatError::Permanent(
            "machine was enrolled against a different API; select that API before running it"
                .into(),
        ));
    }
    let response = client()
        .map_err(HeartbeatError::Permanent)?
        .post(format!("{}/machine-enrollment/heartbeat", machine.api_url))
        .bearer_auth(&machine.credential)
        .send()
        .map_err(|error| HeartbeatError::Temporary(network_error(error)))?;
    if response.status().is_success() {
        Ok(())
    } else if response.status().as_u16() == 401 {
        Err(HeartbeatError::Permanent(
            "machine credential revoked or unavailable; enroll again".into(),
        ))
    } else if response.status().is_server_error() || response.status().as_u16() == 429 {
        Err(HeartbeatError::Temporary(format!(
            "machine heartbeat failed (HTTP {})",
            response.status()
        )))
    } else {
        Err(HeartbeatError::Permanent(format!(
            "machine heartbeat failed (HTTP {})",
            response.status()
        )))
    }
}

pub(super) fn run(args: &[String]) -> Result<(), String> {
    match args {
        [command, rest @ ..] if command == "enroll" => enroll(rest),
        [command, id] if command == "heartbeat" => heartbeat(id).map_err(|error| match error {
            HeartbeatError::Permanent(message) | HeartbeatError::Temporary(message) => message,
        }),
        [command, id] if command == "run" => {
            eprintln!("Machine {id} connected. Stop with Ctrl-C.");
            loop {
                match heartbeat(id) {
                    Ok(()) => {},
                    Err(HeartbeatError::Temporary(message)) => eprintln!("{message}; retrying in 30 seconds"),
                    Err(HeartbeatError::Permanent(message)) => return Err(message),
                }
                thread::sleep(Duration::from_secs(30));
            }
        }
        [command] if command == "list" => {
            for machine in load_config()?.machines {
                println!("{} {} {}", machine.id, machine.scope, machine.api_url);
            }
            Ok(())
        }
        _ => Err("usage: co machine enroll --setup-key | --owner SLUG --label NAME | co machine list|heartbeat ID|run ID".into()),
    }
}
