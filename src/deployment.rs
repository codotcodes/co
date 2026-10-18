use crate::{api_url, load_config, network_error, repo_spec, secret, session_token};
use reqwest::blocking::{Client, Response};
use serde_json::{Value, json};
use std::io::Read;
use std::path::PathBuf;

const USAGE: &str = "usage: co deployment list OWNER/REPO\n       co deployment configure OWNER/REPO ENV --file POLICY.json [--reauth-file PATH]\n       co deployment set-secret OWNER/REPO NAME [--environment ENV] [--value-file PATH] [--reauth-file PATH]\n       co deployment approve OWNER/REPO ID [--reauth-file PATH]\n       co deployment rollback OWNER/REPO ID [--reauth-file PATH]";

fn decode(response: Response) -> Result<Value, String> {
    let status = response.status();
    if !status.is_success() {
        // A reflected credential in an error body must never reach the terminal.
        return Err(format!("deployment request failed (HTTP {status})"));
    }
    response
        .json()
        .map_err(|_| "invalid deployment API response".into())
}

fn valid_environment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 63
        && value.as_bytes()[0].is_ascii_lowercase()
        && value
            .bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
}

fn valid_id(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(i, c)| {
            if [8, 13, 18, 23].contains(&i) {
                c == b'-'
            } else {
                c.is_ascii_hexdigit()
            }
        })
}

fn proof(
    client: &Client,
    token: &str,
    url: &str,
    body: Value,
    method: &str,
    path: Option<&std::path::Path>,
) -> Result<Value, String> {
    let challenge = decode(
        client
            .post(url)
            .bearer_auth(token)
            .json(&body)
            .send()
            .map_err(network_error)?,
    )?;
    let id = challenge["challengeId"]
        .as_str()
        .filter(|id| !id.is_empty())
        .ok_or("invalid authentication challenge")?;
    let mut proof = secret::prompt_reauth_proof(method, path)?;
    proof
        .as_object_mut()
        .ok_or("invalid authentication proof")?
        .insert("challengeId".into(), json!(id));
    Ok(proof)
}

pub(super) fn run(args: &[String]) -> Result<(), String> {
    if args.len() == 1 && ["help", "-h", "--help"].contains(&args[0].as_str()) {
        println!("{USAGE}");
        return Ok(());
    }
    let action = args.first().map(String::as_str).ok_or(USAGE)?;
    if !["list", "configure", "set-secret", "approve", "rollback"].contains(&action) {
        return Err(USAGE.into());
    }
    let (owner, repo) = repo_spec(args.get(1).ok_or(USAGE)?)?;
    let subject = if action == "list" {
        ""
    } else {
        args.get(2).ok_or(USAGE)?
    };
    let mut policy_file = None;
    let mut value_file = None;
    let mut reauth_file = None;
    let mut environment = None;
    let mut index = if action == "list" { 2 } else { 3 };
    while index < args.len() {
        let slot = match args[index].as_str() {
            "--file" if action == "configure" => &mut policy_file,
            "--value-file" if action == "set-secret" => &mut value_file,
            "--reauth-file" if action != "list" => &mut reauth_file,
            "--environment" if action == "set-secret" => &mut environment,
            _ => return Err(USAGE.into()),
        };
        if slot.is_some() {
            return Err("duplicate deployment option".into());
        }
        index += 1;
        *slot = Some(args.get(index).ok_or(USAGE)?.clone());
        index += 1;
    }
    if action == "configure" && (!valid_environment(subject) || policy_file.is_none()) {
        return Err(USAGE.into());
    }
    if action == "set-secret" {
        secret::validate_secret_name(subject)?;
        if environment
            .as_deref()
            .is_some_and(|name| !valid_environment(name))
        {
            return Err("invalid deployment environment".into());
        }
    }
    if ["approve", "rollback"].contains(&action) && !valid_id(subject) {
        return Err("invalid deployment ID".into());
    }
    if std::env::var_os("CO_AGENT_ID").is_some() {
        return Err(
            "deployment management requires an explicit human session, not agent mode".into(),
        );
    }
    let config = load_config()?;
    let token = session_token(&config)?;
    let base = format!("{}/repos/{owner}/{repo}", api_url(&config));
    let client = Client::builder()
        .user_agent(concat!("co/", env!("CARGO_PKG_VERSION")))
        .timeout(std::time::Duration::from_secs(30))
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|_| "unable to initialize deployment client")?;
    if action == "list" {
        let result = decode(
            client
                .get(format!("{base}/deployments"))
                .bearer_auth(token)
                .send()
                .map_err(network_error)?,
        )?;
        println!(
            "{}",
            serde_json::to_string_pretty(&result)
                .map_err(|_| "invalid deployment list")?
                .replace(token, "***")
        );
        return Ok(());
    }
    let current = decode(
        client
            .get(format!("{base}/deployment-config"))
            .bearer_auth(token)
            .send()
            .map_err(network_error)?,
    )?;
    let methods = current["reauthenticationMethods"]
        .as_array()
        .ok_or("invalid deployment configuration response")?
        .iter()
        .filter_map(Value::as_str)
        .map(str::to_owned)
        .collect::<Vec<_>>();
    let method = secret::select_reauth_method(&methods)?;
    let credential = reauth_file.map(PathBuf::from);
    if action == "approve" || action == "rollback" {
        let path = format!("{base}/deployments/{subject}");
        let proof = proof(
            &client,
            token,
            &format!("{path}/reauthentication"),
            json!({"action":action, "method":method}),
            &method,
            credential.as_deref(),
        )?;
        let result = decode(
            client
                .post(format!("{path}/{action}"))
                .bearer_auth(token)
                .json(&json!({"reauthentication":proof}))
                .send()
                .map_err(network_error)?,
        )?;
        let id = result["id"]
            .as_str()
            .filter(|id| valid_id(id))
            .ok_or("invalid deployment receipt")?;
        if action == "approve" && (id != subject || result["approved"] != true) {
            return Err("invalid deployment approval receipt".into());
        }
        println!("{id}");
        return Ok(());
    }
    let entries = current["environments"]
        .as_array()
        .ok_or("invalid deployment configuration response")?;
    let operation;
    let value;
    if action == "configure" {
        let file = std::fs::File::open(policy_file.ok_or(USAGE)?)
            .map_err(|_| "unable to open deployment policy")?;
        let mut content = String::new();
        file.take(100_001)
            .read_to_string(&mut content)
            .map_err(|_| "unable to read deployment policy")?;
        if content.len() > 100_000 {
            return Err("deployment policy exceeds 100000 bytes".into());
        }
        let policy: Value =
            serde_json::from_str(&content).map_err(|_| "invalid deployment policy JSON")?;
        let version = entries
            .iter()
            .find(|entry| entry["name"] == subject)
            .and_then(|entry| entry["version"].as_u64())
            .unwrap_or(0);
        operation = json!({"action":"configure-environment", "environment":subject, "version":version, "config":policy});
        value = None;
    } else {
        let environment_id = match &environment {
            Some(name) => entries
                .iter()
                .find(|entry| entry["name"] == *name)
                .and_then(|entry| entry["id"].as_str())
                .map(|id| json!(id))
                .ok_or("deployment environment does not exist")?,
            None => Value::Null,
        };
        let version = current["secrets"]
            .as_array()
            .ok_or("invalid deployment configuration response")?
            .iter()
            .find(|entry| entry["name"] == subject && entry["environmentId"] == environment_id)
            .and_then(|entry| entry["version"].as_u64())
            .unwrap_or(0);
        operation = json!({"action":"write-secret", "environment":environment, "name":subject, "version":version});
        let input = secret::read_secret_value(value_file.as_deref().map(std::path::Path::new))?;
        secret::validate_secret_value(&input)?;
        value = Some(input);
    }
    let proof = proof(
        &client,
        token,
        &format!("{base}/deployment-config/reauthentication"),
        json!({"operation":operation, "method":method}),
        &method,
        credential.as_deref(),
    )?;
    let mut body = json!({"operation":operation, "reauthentication":proof});
    if let Some(value) = value {
        body["value"] = json!(value);
    }
    let result = decode(
        client
            .post(format!("{base}/deployment-config/apply"))
            .bearer_auth(token)
            .json(&body)
            .send()
            .map_err(network_error)?,
    )?;
    if result["action"] != operation["action"]
        || result["environment"] != operation["environment"]
        || result["version"].as_u64()
            != operation["version"].as_u64().and_then(|v| v.checked_add(1))
    {
        return Err("invalid deployment configuration receipt".into());
    }
    println!("{subject}");
    Ok(())
}
