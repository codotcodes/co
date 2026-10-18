use super::{api_url, load_config, network_error, repo_spec, session_token};
use reqwest::blocking::{Client, Response};
use serde::Deserialize;
use std::io::{self, IsTerminal, Read};
use std::path::PathBuf;

const SECRET_SET_USAGE: &str = "usage: co secret set <NAME> --owner <OWNER> | --repo <OWNER/REPO> [--value-file <PATH>] [--reauth-file <PATH>]";

/// Workflow secret values are limited to 16 KiB of UTF-8 (no NUL).
const MAX_VALUE_BYTES: usize = 16_384;

/// Workflow secret names: `^[A-Z_][A-Z0-9_]{0,99}$`.
const SECRET_NAME_MAX: usize = 100;

#[derive(Deserialize)]
struct WorkflowConfigurationEntry {
    kind: String,
    name: String,
    version: u64,
}

#[derive(Deserialize)]
struct WorkflowConfigurationList {
    entries: Vec<WorkflowConfigurationEntry>,
    #[serde(rename = "canManageSecrets")]
    can_manage_secrets: bool,
    #[serde(rename = "reauthenticationMethods")]
    reauthentication_methods: Vec<String>,
}

#[derive(Deserialize)]
struct ReauthenticationResult {
    #[serde(rename = "challengeId")]
    challenge_id: String,
}

#[derive(Debug, Deserialize)]
struct SecretWriteResult {
    name: String,
    kind: String,
    version: u64,
}

pub(super) fn run(args: &[String]) -> Result<(), String> {
    if args.len() == 1 && matches!(args[0].as_str(), "--help" | "-h" | "help") {
        print_secret_help();
        return Ok(());
    }

    if args.is_empty() || args[0] != "set" {
        return Err(format!(
            "unknown secret command {:?} (try: `co secret set --help`)",
            args.first()
        ));
    }

    let rest = &args[1..];
    if rest.len() == 1 && matches!(rest[0].as_str(), "--help" | "-h" | "help") {
        print_secret_help();
        return Ok(());
    }

    let mut name: Option<String> = None;
    let mut owner: Option<String> = None;
    let mut repo: Option<String> = None;
    let mut value_file: Option<PathBuf> = None;
    let mut reauth_file: Option<PathBuf> = None;
    let mut index = 0;
    while index < rest.len() {
        match rest[index].as_str() {
            "--owner" => {
                index += 1;
                owner = Some(
                    rest.get(index)
                        .cloned()
                        .ok_or_else(|| format!("--owner requires a value\n{SECRET_SET_USAGE}"))?,
                );
            }
            "--repo" => {
                index += 1;
                repo = Some(
                    rest.get(index)
                        .cloned()
                        .ok_or_else(|| format!("--repo requires a value\n{SECRET_SET_USAGE}"))?,
                );
            }
            "--value-file" => {
                index += 1;
                value_file = Some(
                    rest.get(index)
                        .cloned()
                        .ok_or_else(|| {
                            format!("--value-file requires a value\n{SECRET_SET_USAGE}")
                        })?
                        .into(),
                );
            }
            "--reauth-file" => {
                index += 1;
                reauth_file = Some(
                    rest.get(index)
                        .ok_or("--reauth-file requires a path")?
                        .into(),
                );
            }
            option if option.starts_with('-') => {
                return Err(format!(
                    "unknown secret option {option:?}\n{SECRET_SET_USAGE}"
                ));
            }
            value if name.is_none() => name = Some(value.to_string()),
            _ => return Err(SECRET_SET_USAGE.into()),
        }
        index += 1;
    }

    let name = name.ok_or_else(|| SECRET_SET_USAGE.to_string())?;
    validate_secret_name(&name)?;

    match (owner.is_some(), repo.is_some()) {
        (true, true) => return Err("pass either --owner or --repo, not both".into()),
        (false, false) => {
            return Err(format!(
                "pass --owner <OWNER> or --repo <OWNER/REPO>\n{SECRET_SET_USAGE}"
            ));
        }
        _ => {}
    }

    if let Some(owner) = &owner {
        repo_spec(&format!("{owner}/scope"))?;
    }
    let config = load_config()?;
    let token = session_token(&config)?;
    let api = api_url(&config);
    let client = Client::builder()
        .user_agent(concat!("co/", env!("CARGO_PKG_VERSION")))
        .timeout(std::time::Duration::from_secs(30))
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|_| "unable to initialize secret API client")?;

    let (base, scope_label) = match (&owner, &repo) {
        (Some(o), None) => (
            format!("{api}/workflow-config/owners/{o}"),
            format!("owner {o}"),
        ),
        (None, Some(r)) => {
            let (r_owner, r_name) = repo_spec(r)?;
            (
                format!("{api}/workflow-config/repos/{r_owner}/{r_name}"),
                format!("repository {r}"),
            )
        }
        _ => unreachable!("scope validated above"),
    };

    let list: WorkflowConfigurationList = decode(
        client
            .get(&base)
            .bearer_auth(token)
            .send()
            .map_err(network_error)?,
    )?;

    if !list.can_manage_secrets {
        return Err(format!(
            "secret management on {scope_label} requires the owner role; run `co login` as the owner"
        ));
    }

    let local_version = list
        .entries
        .iter()
        .find(|entry| entry.kind == "secret" && entry.name == name)
        .map(|entry| entry.version)
        .unwrap_or(0);

    let methods = &list.reauthentication_methods;
    let method = select_reauth_method(methods)?;

    let value = read_secret_value(value_file.as_deref())?;
    validate_secret_value(&value)?;

    let challenge: ReauthenticationResult = decode(
        client
            .post(format!("{base}/secret/{name}/reauthentication"))
            .bearer_auth(token)
            .json(&serde_json::json!({
                "action": "write",
                "version": local_version,
                "method": method,
            }))
            .send()
            .map_err(network_error)?,
    )?;

    let proof = prompt_reauth_proof(&method, reauth_file.as_deref())?;

    let mut reauthentication = serde_json::json!({ "challengeId": challenge.challenge_id });
    if let Some(proof_obj) = proof.as_object() {
        if let Some(auth_obj) = reauthentication.as_object_mut() {
            for (key, val) in proof_obj {
                auth_obj.insert(key.clone(), val.clone());
            }
        }
    }
    let result: SecretWriteResult = decode(
        client
            .put(format!("{base}/secret/{name}"))
            .bearer_auth(token)
            .json(&serde_json::json!({
                "value": value,
                "version": local_version,
                "reauthentication": reauthentication,
            }))
            .send()
            .map_err(network_error)?,
    )?;

    if result.name != name || result.kind != "secret" || result.version != local_version + 1 {
        return Err("invalid secret API response".into());
    }
    println!("{name}");
    println!("  kind    secret");
    println!("  version {}", result.version);
    Ok(())
}

fn print_secret_help() {
    println!("{SECRET_SET_USAGE}");
    println!();
    println!("Create or replace a workflow secret in a personal or repository scope.");
    println!("Requires an unambiguous scope: --owner or --repo, never both.");
    println!("Reads the secret value from an echo-disabled prompt, stdin, or --value-file.");
    println!("Never pass a secret value as a command argument.");
    println!();
    println!("Options:");
    println!("  --owner <OWNER>        personal or organization owner scope");
    println!("  --repo <OWNER/REPO>    repository scope");
    println!("  --value-file <PATH>    read the secret value from this file");
    println!("  --reauth-file <PATH>   read password or TOTP from a separate private file");
    println!("Stdin and value files are read in full, preserving every byte, including newlines.");
    println!("Authentication is prompted on the terminal unless --reauth-file is given.");
    println!();
    println!("Examples:");
    println!("  co secret set DATABASE_URL --owner my-org");
    println!("  co secret set DATABASE_URL --repo my-org/my-app");
    println!("  co secret set DATABASE_URL --owner my-org --value-file ./secret.txt");
}

pub(super) fn validate_secret_name(name: &str) -> Result<(), String> {
    if name.is_empty() || name.len() > SECRET_NAME_MAX {
        return Err("secret name must be 1 to 100 characters".into());
    }
    let mut chars = name.chars();
    let first = chars.next().ok_or("secret name is empty")?;
    if !(first.is_ascii_uppercase() || first == '_') {
        return Err("secret name must start with an uppercase letter or underscore".into());
    }
    for character in chars {
        if !(character.is_ascii_uppercase() || character.is_ascii_digit() || character == '_') {
            return Err(
                "secret name must contain only uppercase letters, digits, and underscores".into(),
            );
        }
    }
    Ok(())
}

pub(super) fn validate_secret_value(value: &str) -> Result<(), String> {
    if value.len() > MAX_VALUE_BYTES {
        return Err(format!(
            "secret value exceeds {MAX_VALUE_BYTES} UTF-8 bytes"
        ));
    }
    if value.contains('\0') {
        return Err("secret value must not contain a NUL byte".into());
    }
    Ok(())
}

/// Select a terminal-usable reauthentication method from the backend's list.
///
/// Prefers password, then TOTP. Passkey requires browser-based WebAuthn and is
/// not supported in this terminal-only slice; report an actionable limitation
/// rather than silently downgrading. No available method means the account must
/// enroll a passkey or authenticator first.
pub(super) fn select_reauth_method(methods: &[String]) -> Result<String, String> {
    if methods.iter().any(|m| m == "password") {
        return Ok("password".to_string());
    }
    if methods.iter().any(|m| m == "totp") {
        return Ok("totp".to_string());
    }
    if methods.iter().any(|m| m == "passkey") {
        return Err(
            "this account only supports passkey reauthentication, which requires browser-based WebAuthn and is not available in the terminal CLI. Create the secret in the web UI, or enroll an authenticator to use TOTP."
                .to_string(),
        );
    }
    Err(
        "no reauthentication method is available for this account. Enroll a passkey or authenticator in your account settings, then try again."
            .to_string(),
    )
}

// Never print untrusted API messages: a proxy or server may echo request secrets.
fn decode<T: for<'de> Deserialize<'de>>(response: Response) -> Result<T, String> {
    let status = response.status();
    if status.is_success() {
        return response
            .json()
            .map_err(|_| "invalid secret API response".into());
    }
    let body = response.json::<serde_json::Value>().unwrap_or_default();
    let code = body
        .get("error")
        .and_then(|value| value.as_str())
        .filter(|code| {
            matches!(
                *code,
                "unauthorized"
                    | "forbidden"
                    | "not_found"
                    | "version_conflict"
                    | "invalid_configuration"
                    | "configuration_limit"
                    | "invalid_reauthentication"
                    | "reauthentication_expired"
                    | "reauthentication_rate_limited"
                    | "reauthentication_method_unavailable"
                    | "workflow_secret_unavailable"
            )
        })
        .unwrap_or("request_failed");
    Err(format!("secret request failed: {code} (HTTP {status})"))
}

fn read_bounded(reader: impl Read, limit: usize) -> Result<String, String> {
    let mut value = String::new();
    reader
        .take((limit + 1) as u64)
        .read_to_string(&mut value)
        .map_err(|_| "unable to read UTF-8 secret input")?;
    if value.len() > limit {
        return Err(format!("secret input exceeds {limit} UTF-8 bytes"));
    }
    Ok(value)
}

pub(super) fn read_secret_value(value_file: Option<&std::path::Path>) -> Result<String, String> {
    if let Some(path) = value_file {
        let file = std::fs::File::open(path).map_err(|_| "unable to open secret value file")?;
        return read_bounded(file, MAX_VALUE_BYTES);
    }

    if io::stdin().is_terminal() {
        return rpassword::prompt_password("Enter the secret value: ")
            .map_err(|error| format!("unable to read secret value: {error}"));
    }

    read_bounded(io::stdin().lock(), MAX_VALUE_BYTES)
}

fn read_reauth_credential(prompt: &str, path: Option<&std::path::Path>) -> Result<String, String> {
    if let Some(path) = path {
        let file = std::fs::File::open(path).map_err(|_| "unable to open reauthentication file")?;
        let mut value = read_bounded(file, 4096)?;
        if value.ends_with('\n') {
            value.pop();
            if value.ends_with('\r') {
                value.pop();
            }
        }
        return Ok(value);
    }
    // rpassword opens the controlling terminal, never the secret-value pipe.
    rpassword::prompt_password(prompt)
        .map_err(|_| "unable to read authentication from the terminal; use --reauth-file for noninteractive input".into())
}

/// Prompt for the reauthentication proof matching the chosen method.
pub(super) fn prompt_reauth_proof(
    method: &str,
    path: Option<&std::path::Path>,
) -> Result<serde_json::Value, String> {
    match method {
        "password" => {
            let password = read_reauth_credential("Enter your account password: ", path)?;
            Ok(serde_json::json!({ "password": password }))
        }
        "totp" => {
            let code = read_reauth_credential("Enter your 6-digit authentication code: ", path)?;
            Ok(serde_json::json!({ "code": code }))
        }
        _ => Err(format!("unsupported reauthentication method {method:?}")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validates_secret_names() {
        assert!(validate_secret_name("FOO").is_ok());
        assert!(validate_secret_name("_FOO").is_ok());
        assert!(validate_secret_name("FOO_BAR_123").is_ok());
        assert!(validate_secret_name(&"A".repeat(100)).is_ok());
        assert!(validate_secret_name("").is_err());
        assert!(validate_secret_name("foo").is_err());
        assert!(validate_secret_name("FOO BAR").is_err());
        assert!(validate_secret_name("FOO-bar").is_err());
        assert!(validate_secret_name(&"A".repeat(101)).is_err());
    }

    #[test]
    fn validates_secret_values() {
        assert!(validate_secret_value("").is_ok());
        assert!(validate_secret_value("hello").is_ok());
        assert!(validate_secret_value("  spaced  ").is_ok());
        assert!(validate_secret_value("line1\nline2").is_ok());
        assert!(validate_secret_value(&"a".repeat(MAX_VALUE_BYTES)).is_ok());
        assert!(validate_secret_value(&"a".repeat(MAX_VALUE_BYTES + 1)).is_err());
        assert!(validate_secret_value("a\0b").is_err());
    }

    #[test]
    fn bounds_secret_input_without_changing_whitespace() {
        assert_eq!(
            read_bounded(&b"line one\nline two\n"[..], MAX_VALUE_BYTES).unwrap(),
            "line one\nline two\n"
        );
        assert_eq!(read_bounded(&b""[..], MAX_VALUE_BYTES).unwrap(), "");
        assert!(read_bounded(&vec![b'x'; MAX_VALUE_BYTES + 1][..], MAX_VALUE_BYTES).is_err());
        assert!(read_bounded(&[0xff][..], MAX_VALUE_BYTES).is_err());
    }

    #[test]
    fn selects_password_then_totp_then_reports_passkey() {
        assert_eq!(
            select_reauth_method(&["password".to_string()]).unwrap(),
            "password"
        );
        assert_eq!(
            select_reauth_method(&["totp".to_string(), "passkey".to_string()]).unwrap(),
            "totp"
        );
        assert_eq!(
            select_reauth_method(&["passkey".to_string(), "totp".to_string()]).unwrap(),
            "totp"
        );
        assert!(select_reauth_method(&["passkey".to_string()]).is_err());
        assert!(select_reauth_method(&[]).is_err());
    }
}
