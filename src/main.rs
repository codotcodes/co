use reqwest::StatusCode;
use reqwest::blocking::{Client, Response};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::thread;
use std::time::{Duration, Instant};

const DEFAULT_API_URL: &str = "https://api.co.codes";
const DEVICE_CLIENT_ID: &str = "co-cli";
const DEVICE_GRANT: &str = "urn:ietf:params:oauth:grant-type:device_code";

#[derive(Debug, Default, Deserialize, Serialize)]
struct Config {
    #[serde(default)]
    api_url: Option<String>,
    #[serde(default)]
    session_token: Option<String>,
}

#[derive(Deserialize)]
struct DeviceCode {
    device_code: String,
    user_code: String,
    verification_uri: String,
    verification_uri_complete: Option<String>,
    expires_in: Option<u64>,
    interval: Option<u64>,
}

#[derive(Deserialize)]
struct DeviceToken {
    access_token: String,
}

#[derive(Deserialize)]
struct ApiError {
    error: Option<String>,
    error_description: Option<String>,
    message: Option<String>,
}

fn main() -> ExitCode {
    match run(std::env::args().skip(1).collect()) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("co: {error}");
            ExitCode::from(1)
        }
    }
}

fn run(args: Vec<String>) -> Result<(), String> {
    let command = args.first().map(String::as_str).unwrap_or("help");
    match command {
        "version" | "--version" | "-V" => {
            println!("co {}", env!("CARGO_PKG_VERSION"));
            Ok(())
        }
        "help" | "--help" | "-h" => {
            help();
            Ok(())
        }
        "login" => login(),
        "logout" => logout(),
        "whoami" => whoami(),
        "doctor" => doctor(),
        "repo" if args.get(1).map(String::as_str) == Some("view") => {
            repo_view(required_arg(&args, 2, "usage: co repo view <owner/repo>")?)
        }
        "clone" => clone_repo(required_arg(&args, 1, "usage: co clone <owner/repo>")?),
        "repo" => Err("usage: co repo view <owner/repo>".into()),
        other => Err(format!("unknown command {other:?} (try: co help)")),
    }
}

fn required_arg<'a>(args: &'a [String], index: usize, usage: &str) -> Result<&'a str, String> {
    args.get(index)
        .map(String::as_str)
        .ok_or_else(|| usage.into())
}

fn help() {
    println!("co: the co.codes command line");
    println!();
    println!("  login                    authorize this machine");
    println!("  logout                   revoke the local session");
    println!("  whoami                   show the signed-in account");
    println!("  repo view <owner/repo>   show repository metadata");
    println!("  clone <owner/repo>       validate access (Git transport pending)");
    println!("  doctor                   check API and authentication");
    println!("  version                  print version");
    println!();
    println!("Environment: CO_API_URL overrides https://api.co.codes.");
}

fn login() -> Result<(), String> {
    let mut config = load_config()?;
    let api = api_url(&config);
    let client = client()?;
    let response = client
        .post(format!("{api}/api/auth/device/code"))
        .json(&serde_json::json!({
            "client_id": DEVICE_CLIENT_ID,
            "scope": "openid profile email"
        }))
        .send()
        .map_err(network_error)?;
    let code: DeviceCode = decode(response)?;

    println!("Open this URL in your browser:");
    println!("  {}", code.verification_uri);
    println!("Enter code:");
    println!("  {}", code.user_code);
    println!();
    let browser_url = code
        .verification_uri_complete
        .as_deref()
        .unwrap_or(&code.verification_uri);
    if webbrowser::open(browser_url).is_err() {
        println!("Unable to open a browser automatically.");
    }
    println!("Waiting for approval...");

    let mut interval = code.interval.unwrap_or(5).max(1);
    let deadline = Instant::now() + Duration::from_secs(code.expires_in.unwrap_or(1800));
    loop {
        if Instant::now() >= deadline {
            return Err("device code expired; run `co login` again".into());
        }
        thread::sleep(Duration::from_secs(interval));
        let response = client
            .post(format!("{api}/api/auth/device/token"))
            .json(&serde_json::json!({
                "grant_type": DEVICE_GRANT,
                "device_code": code.device_code,
                "client_id": DEVICE_CLIENT_ID
            }))
            .send()
            .map_err(network_error)?;
        if response.status().is_success() {
            let token: DeviceToken = response
                .json()
                .map_err(|error| format!("invalid token response: {error}"))?;
            config.session_token = Some(token.access_token);
            save_config(&config)?;
            println!("Authorized. Run `co whoami` to verify this machine.");
            return Ok(());
        }
        let error = response.json::<ApiError>().unwrap_or(ApiError {
            error: Some("server_error".into()),
            error_description: None,
            message: None,
        });
        match error.error.as_deref() {
            Some("authorization_pending") => {}
            Some("slow_down") => interval += 5,
            Some("access_denied") => return Err("device authorization was denied".into()),
            Some("expired_token") => return Err("device code expired; run `co login` again".into()),
            _ => return Err(api_error(error)),
        }
    }
}

fn logout() -> Result<(), String> {
    let mut config = load_config()?;
    if let Some(token) = config.session_token.as_deref() {
        let api = api_url(&config);
        let response = client()?
            .post(format!("{api}/api/auth/sign-out"))
            .bearer_auth(token)
            .send();
        if let Ok(response) = response {
            if !response.status().is_success() && response.status() != StatusCode::UNAUTHORIZED {
                eprintln!("co: warning: server did not confirm session revocation");
            }
        } else {
            eprintln!("co: warning: unable to reach the server; removing the local session");
        }
    }
    config.session_token = None;
    save_config(&config)?;
    println!("Signed out.");
    Ok(())
}

fn whoami() -> Result<(), String> {
    let config = load_config()?;
    let response = authenticated_get(&config, "/api/auth/get-session")?;
    let body: Value = decode(response)?;
    let user = body
        .get("user")
        .ok_or("session is no longer valid; run `co login`")?;
    let username = user.get("username").and_then(Value::as_str);
    let name = user
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or("unknown");
    let email = user
        .get("email")
        .and_then(Value::as_str)
        .unwrap_or("unknown");
    println!("{}", username.unwrap_or(name));
    println!("  name   {name}");
    println!("  email  {email}");
    Ok(())
}

fn repo_view(spec: &str) -> Result<(), String> {
    let (owner, name) = repo_spec(spec)?;
    let config = load_config()?;
    let api = api_url(&config);
    let mut request = client()?.get(format!("{api}/repos/{owner}/{name}"));
    if let Some(token) = config.session_token.as_deref() {
        request = request.bearer_auth(token);
    }
    let repo: Value = decode(request.send().map_err(network_error)?)?;
    println!(
        "{}/{}",
        repo["owner"].as_str().unwrap_or(owner),
        repo["name"].as_str().unwrap_or(name)
    );
    println!(
        "  visibility      {}",
        repo["visibility"].as_str().unwrap_or("unknown")
    );
    println!(
        "  default branch  {}",
        repo["defaultBranch"].as_str().unwrap_or("unknown")
    );
    println!("  web             https://co.codes/{owner}/{name}");
    Ok(())
}

fn clone_repo(spec: &str) -> Result<(), String> {
    repo_view(spec)?;
    Err("Git clone is not available yet; smart HTTP must land before `co clone` can transfer repository data".into())
}

fn doctor() -> Result<(), String> {
    let config = load_config()?;
    let api = api_url(&config);
    let response = client()?
        .get(format!("{api}/health"))
        .send()
        .map_err(network_error)?;
    if !response.status().is_success() {
        return Err(format!("API health check returned {}", response.status()));
    }
    println!("api       ok ({api})");
    println!("config    {}", config_path()?.display());
    if config.session_token.is_some() {
        match authenticated_get(&config, "/api/auth/get-session") {
            Ok(response) if response.status().is_success() => println!("session   valid"),
            _ => println!("session   invalid (run `co login`)"),
        }
    } else {
        println!("session   absent (run `co login`)");
    }
    println!("git       unavailable (smart HTTP pending)");
    Ok(())
}

fn authenticated_get(config: &Config, path: &str) -> Result<Response, String> {
    let token = config
        .session_token
        .as_deref()
        .ok_or("not signed in; run `co login`")?;
    client()?
        .get(format!("{}{}", api_url(config), path))
        .bearer_auth(token)
        .send()
        .map_err(network_error)
}

fn decode<T: for<'de> Deserialize<'de>>(response: Response) -> Result<T, String> {
    let status = response.status();
    if status.is_success() {
        return response
            .json()
            .map_err(|error| format!("invalid API response: {error}"));
    }
    let error = response.json::<ApiError>().unwrap_or(ApiError {
        error: Some(format!("HTTP {status}")),
        error_description: None,
        message: None,
    });
    Err(api_error(error))
}

fn api_error(error: ApiError) -> String {
    error
        .error_description
        .or(error.message)
        .or(error.error)
        .unwrap_or_else(|| "API request failed".into())
}

fn network_error(error: reqwest::Error) -> String {
    if error.is_timeout() {
        "request timed out".into()
    } else {
        format!("network request failed: {error}")
    }
}

fn client() -> Result<Client, String> {
    Client::builder()
        .user_agent(concat!("co/", env!("CARGO_PKG_VERSION")))
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|error| format!("unable to initialize HTTP client: {error}"))
}

fn repo_spec(spec: &str) -> Result<(&str, &str), String> {
    let Some((owner, name)) = spec.split_once('/') else {
        return Err("repository must be written as <owner/repo>".into());
    };
    if owner.is_empty() || name.is_empty() || name.contains('/') {
        return Err("repository must be written as <owner/repo>".into());
    }
    Ok((owner, name))
}

fn api_url(config: &Config) -> String {
    std::env::var("CO_API_URL")
        .ok()
        .or_else(|| config.api_url.clone())
        .unwrap_or_else(|| DEFAULT_API_URL.into())
        .trim_end_matches('/')
        .to_string()
}

fn config_path() -> Result<PathBuf, String> {
    if let Some(root) = std::env::var_os("XDG_CONFIG_HOME") {
        return Ok(PathBuf::from(root).join("co/config.json"));
    }
    let home = std::env::var_os("HOME").ok_or("HOME is unset; set XDG_CONFIG_HOME for co")?;
    Ok(PathBuf::from(home).join(".config/co/config.json"))
}

fn load_config() -> Result<Config, String> {
    let path = config_path()?;
    match fs::read(&path) {
        Ok(bytes) => serde_json::from_slice(&bytes)
            .map_err(|error| format!("invalid config {}: {error}", path.display())),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(Config::default()),
        Err(error) => Err(format!("unable to read {}: {error}", path.display())),
    }
}

fn save_config(config: &Config) -> Result<(), String> {
    let path = config_path()?;
    let parent = path.parent().ok_or("invalid config path")?;
    fs::create_dir_all(parent)
        .map_err(|error| format!("unable to create {}: {error}", parent.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(parent, fs::Permissions::from_mode(0o700))
            .map_err(|error| format!("unable to secure {}: {error}", parent.display()))?;
    }
    let temporary = path.with_extension("json.tmp");
    write_private(
        &temporary,
        &serde_json::to_vec_pretty(config).map_err(|error| error.to_string())?,
    )?;
    fs::rename(&temporary, &path)
        .map_err(|error| format!("unable to replace {}: {error}", path.display()))?;
    Ok(())
}

fn write_private(path: &Path, bytes: &[u8]) -> Result<(), String> {
    let mut options = OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options
        .open(path)
        .map_err(|error| format!("unable to write {}: {error}", path.display()))?;
    file.write_all(bytes)
        .map_err(|error| format!("unable to write {}: {error}", path.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_repo_spec() {
        assert_eq!(repo_spec("hackr/co").unwrap(), ("hackr", "co"));
        assert!(repo_spec("co").is_err());
        assert!(repo_spec("a/b/c").is_err());
    }

    #[test]
    fn trims_api_url() {
        let config = Config {
            api_url: Some("https://example.test/".into()),
            session_token: None,
        };
        assert_eq!(api_url(&config), "https://example.test");
    }
}
