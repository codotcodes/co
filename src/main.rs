use reqwest::StatusCode;
use reqwest::blocking::{Client, Response};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fs::{self, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode};
use std::thread;
use std::time::{Duration, Instant};

const DEFAULT_API_URL: &str = "https://api.co.codes";
const GIT_HOST: &str = "git.co.codes";
const DEVICE_CLIENT_ID: &str = "co-cli";
const DEVICE_GRANT: &str = "urn:ietf:params:oauth:grant-type:device_code";
const DEFAULT_UPSTREAM_NAME: &str = "origin";

#[derive(Debug, Default, Deserialize, Serialize)]
struct Config {
    #[serde(default)]
    api_url: Option<String>,
    #[serde(default)]
    session_token: Option<String>,
    #[serde(default)]
    upstream_name: Option<String>,
    #[serde(default)]
    jj: bool,
}

#[derive(Debug, PartialEq, Eq)]
struct RepoCommand {
    spec: String,
    directory: Option<String>,
    upstream_name: String,
    jj: bool,
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
        "git-credential" if args.len() == 2 => git_credential(&args[1]),
        "git-credential" => Err("usage: co git-credential get|store|erase".into()),
        "repo" if args.get(1).map(String::as_str) == Some("view") => {
            repo_view(required_arg(&args, 2, "usage: co repo view <owner/repo>")?)
        }
        "clone" => {
            let config = load_config()?;
            clone_repo(parse_repo_command(&args[1..], true, &config)?)
        }
        "link" => {
            let config = load_config()?;
            link_repo(parse_repo_command(&args[1..], false, &config)?)
        }
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
    println!("  clone [options] <repo>   clone a repository over HTTPS");
    println!("  link [options] <repo>    link the local Git repository");
    println!("  git-credential <op>      serve credentials to Git");
    println!("  doctor                   check API and authentication");
    println!("  version                  print version");
    println!();
    println!("Clone and link options:");
    println!("  -u, --set-upstream-name <name>  set the Git remote name (default: origin)");
    println!("      --jj / --no-jj              enable or disable colocated jj setup");
    println!();
    println!("Configuration: ~/.config/co/config.json (or $XDG_CONFIG_HOME/co/config.json).");
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

fn git_credential(operation: &str) -> Result<(), String> {
    if !matches!(operation, "get" | "store" | "erase") {
        return Err("usage: co git-credential get|store|erase".into());
    }

    let mut input = String::new();
    std::io::stdin()
        .read_to_string(&mut input)
        .map_err(|error| format!("unable to read Git credential request: {error}"))?;
    let credential = parse_credential(&input);

    if operation == "get" && credential.in_scope() {
        if let Some(token) = load_config()?.session_token {
            print!("username=co\npassword={token}\n\n");
            std::io::stdout()
                .flush()
                .map_err(|error| format!("unable to write Git credential response: {error}"))?;
        }
    }
    Ok(())
}

#[derive(Debug, Default, PartialEq, Eq)]
struct GitCredential<'a> {
    protocol: Option<&'a str>,
    host: Option<&'a str>,
}

impl GitCredential<'_> {
    fn in_scope(&self) -> bool {
        self.protocol == Some("https") && matches!(self.host, Some(GIT_HOST | "git.co.codes:443"))
    }
}

fn parse_credential(input: &str) -> GitCredential<'_> {
    let mut credential = GitCredential::default();
    for line in input.lines() {
        let line = line.strip_suffix('\r').unwrap_or(line);
        if line.is_empty() {
            break;
        }
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        match key {
            "protocol" => credential.protocol = Some(value),
            "host" => credential.host = Some(value),
            _ => {}
        }
    }
    credential
}

fn parse_repo_command(
    args: &[String],
    allows_directory: bool,
    config: &Config,
) -> Result<RepoCommand, String> {
    let command = if allows_directory { "clone" } else { "link" };
    let usage = if allows_directory {
        "usage: co clone [-u <name>] [--jj] <owner/repo> [directory]"
    } else {
        "usage: co link [-u <name>] [--jj] <owner/repo>"
    };
    let mut upstream_name = config
        .upstream_name
        .clone()
        .unwrap_or_else(|| DEFAULT_UPSTREAM_NAME.into());
    let mut jj = config.jj;
    let mut positionals = Vec::new();
    let mut options = true;
    let mut index = 0;

    while index < args.len() {
        let argument = &args[index];
        if options && argument == "--" {
            options = false;
        } else if options && matches!(argument.as_str(), "--jj" | "--no-jj") {
            jj = argument == "--jj";
        } else if options && matches!(argument.as_str(), "-u" | "--set-upstream-name") {
            index += 1;
            upstream_name = args
                .get(index)
                .cloned()
                .ok_or_else(|| format!("{argument} requires a remote name\n{usage}"))?;
        } else if options {
            if let Some(value) = argument.strip_prefix("--set-upstream-name=") {
                upstream_name = value.into();
            } else if argument.starts_with('-') {
                return Err(format!("unknown {command} option {argument:?}\n{usage}"));
            } else {
                positionals.push(argument.clone());
            }
        } else {
            positionals.push(argument.clone());
        }
        index += 1;
    }

    let max_positionals = if allows_directory { 2 } else { 1 };
    if positionals.is_empty() || positionals.len() > max_positionals {
        return Err(usage.into());
    }
    repo_spec(&positionals[0])?;
    validate_upstream_name(&upstream_name)?;

    Ok(RepoCommand {
        spec: positionals.remove(0),
        directory: positionals.pop(),
        upstream_name,
        jj,
    })
}

fn validate_upstream_name(name: &str) -> Result<(), String> {
    if name.is_empty()
        || name.starts_with('-')
        || name.ends_with(['.', '/'])
        || ["..", "@{", "//"].iter().any(|part| name.contains(part))
        || name
            .chars()
            .any(|character| character.is_control() || " ~^:?*[\\".contains(character))
        || name.split('/').any(|component| {
            component.is_empty() || component.starts_with('.') || component.ends_with(".lock")
        })
    {
        return Err(format!("{name:?} is not a valid Git remote name"));
    }
    Ok(())
}

fn clone_repo(command: RepoCommand) -> Result<(), String> {
    let (_, name) = repo_spec(&command.spec)?;
    let executable = std::env::current_exe()
        .map_err(|error| format!("unable to locate the co executable: {error}"))?;
    let helper = credential_helper(&executable)?;
    let url = git_url(&command.spec);
    let clone_args = clone_command_args(
        &url,
        command.directory.as_deref(),
        &command.upstream_name,
        &helper,
    );
    run_git(&clone_args, "clone repository")?;

    let destination = command
        .directory
        .map(PathBuf::from)
        .unwrap_or_else(|| name.into());
    configure_repo_helper(&destination, &helper)?;
    if command.jj {
        initialize_jj(&destination)?;
    }
    Ok(())
}

fn link_repo(command: RepoCommand) -> Result<(), String> {
    let executable = std::env::current_exe()
        .map_err(|error| format!("unable to locate the co executable: {error}"))?;
    let helper = credential_helper(&executable)?;
    let root = git_repo_root()?;
    run_git(
        &[
            "-C".into(),
            path_string(&root, "Git repository root")?,
            "remote".into(),
            "add".into(),
            command.upstream_name,
            git_url(&command.spec),
        ],
        "link repository",
    )?;
    configure_repo_helper(&root, &helper)?;
    if command.jj {
        initialize_jj(&root)?;
    }
    Ok(())
}

fn git_url(spec: &str) -> String {
    format!("https://{GIT_HOST}/{spec}.git")
}

fn credential_helper(executable: &Path) -> Result<String, String> {
    let executable = executable
        .to_str()
        .ok_or("the co executable path is not valid UTF-8")?;
    Ok(format!("!{} git-credential", shell_quote(executable)))
}

fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

fn clone_command_args(
    url: &str,
    directory: Option<&str>,
    upstream_name: &str,
    helper: &str,
) -> Vec<String> {
    let mut args = vec![
        "-c".into(),
        "credential.helper=".into(),
        "-c".into(),
        format!("credential.https://{GIT_HOST}.helper={helper}"),
        "clone".into(),
        "--origin".into(),
        upstream_name.into(),
        "--".into(),
        url.into(),
    ];
    if let Some(directory) = directory {
        args.push(directory.into());
    }
    args
}

fn initialize_jj(directory: &Path) -> Result<(), String> {
    if directory.join(".jj").exists() {
        let output = Command::new("jj")
            .arg("--repository")
            .arg(directory)
            .arg("root")
            .output()
            .map_err(|error| format!("unable to inspect existing jj repository: {error}"))?;
        if output.status.success() {
            return Ok(());
        }
    }
    let status = Command::new("jj")
        .args(["git", "init", "--colocate"])
        .current_dir(directory)
        .status()
        .map_err(|error| format!("unable to initialize jj: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!(
            "jj failed to initialize a colocated repository ({status})"
        ))
    }
}

fn git_repo_root() -> Result<PathBuf, String> {
    let bare = Command::new("git")
        .args(["rev-parse", "--is-bare-repository"])
        .output()
        .map_err(|error| format!("unable to locate the Git repository: {error}"))?;
    if !bare.status.success() {
        return Err("the current directory is not inside a Git repository".into());
    }
    let is_bare = bare.stdout.starts_with(b"true");
    let root_arg = if is_bare {
        "--absolute-git-dir"
    } else {
        "--show-toplevel"
    };
    let output = Command::new("git")
        .args(["rev-parse", root_arg])
        .output()
        .map_err(|error| format!("unable to locate the Git repository: {error}"))?;
    if !output.status.success() {
        return Err("the current directory is not inside a Git repository".into());
    }
    let root = String::from_utf8(output.stdout)
        .map_err(|_| "the Git repository path is not valid UTF-8")?;
    let root = root.strip_suffix('\n').unwrap_or(&root);
    let root = root.strip_suffix('\r').unwrap_or(root);
    Ok(PathBuf::from(root))
}

fn path_string(path: &Path, label: &str) -> Result<String, String> {
    path.to_str()
        .map(str::to_owned)
        .ok_or_else(|| format!("the {label} is not valid UTF-8"))
}

fn configure_repo_helper(directory: &Path, helper: &str) -> Result<(), String> {
    let directory = directory
        .to_str()
        .ok_or("the clone directory is not valid UTF-8")?;
    let key = format!("credential.https://{GIT_HOST}.helper");
    run_git(
        &[
            "-C".into(),
            directory.into(),
            "config".into(),
            "--local".into(),
            "--replace-all".into(),
            key.clone(),
            String::new(),
        ],
        "reset repository credential helpers",
    )?;
    run_git(
        &[
            "-C".into(),
            directory.into(),
            "config".into(),
            "--local".into(),
            "--add".into(),
            key,
            helper.into(),
        ],
        "configure repository credential helper",
    )
}

fn run_git(args: &[String], action: &str) -> Result<(), String> {
    let status = Command::new("git")
        .args(args)
        .status()
        .map_err(|error| format!("unable to run Git to {action}: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("Git failed to {action} ({status})"))
    }
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
    let output = Command::new("git")
        .arg("--version")
        .output()
        .map_err(|error| format!("unable to run Git: {error}"))?;
    if !output.status.success() {
        return Err(format!("Git health check failed ({})", output.status));
    }
    println!("git       ok (HTTPS credential helper ready)");
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
    let valid_owner = (2..=64).contains(&owner.len())
        && owner.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || (index > 0 && byte == b'-')
        });
    let valid_name = (1..=128).contains(&name.len())
        && !matches!(name, "." | "..")
        && name.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_alphanumeric() || (index > 0 && matches!(byte, b'.' | b'_' | b'-'))
        });
    if name.contains('/') || !valid_owner || !valid_name {
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
        assert!(repo_spec("Upper/co").is_err());
        assert!(repo_spec("hackr/-co").is_err());
        assert!(repo_spec("hackr/..").is_err());
        assert!(repo_spec("hackr/co?token=x").is_err());
    }

    #[test]
    fn parses_and_scopes_git_credentials() {
        let credential =
            parse_credential("protocol=https\r\nhost=git.co.codes:443\r\npath=a/b.git\r\n\r\n");
        assert_eq!(
            credential,
            GitCredential {
                protocol: Some("https"),
                host: Some("git.co.codes:443")
            }
        );
        assert!(credential.in_scope());
        assert!(parse_credential("protocol=https\nhost=git.co.codes\n\n").in_scope());
        assert!(!parse_credential("protocol=http\nhost=git.co.codes\n\n").in_scope());
        assert!(!parse_credential("protocol=https\nhost=git.co.codes:444\n\n").in_scope());
        assert!(!parse_credential("protocol=https\nhost=evil.example\n\n").in_scope());
    }

    #[test]
    fn constructs_clone_command_without_a_secret() {
        let helper = "!'/usr/local/bin/co' git-credential";
        assert_eq!(
            clone_command_args(
                "https://git.co.codes/hackr/co.git",
                Some("checkout"),
                "upstream",
                helper
            ),
            vec![
                "-c",
                "credential.helper=",
                "-c",
                "credential.https://git.co.codes.helper=!'/usr/local/bin/co' git-credential",
                "clone",
                "--origin",
                "upstream",
                "--",
                "https://git.co.codes/hackr/co.git",
                "checkout",
            ]
        );
    }

    #[test]
    fn parses_clone_and_link_options_with_config_defaults() {
        let config = Config {
            upstream_name: Some("co".into()),
            jj: true,
            ..Config::default()
        };
        assert_eq!(
            parse_repo_command(&["hackr/co".into(), "checkout".into()], true, &config).unwrap(),
            RepoCommand {
                spec: "hackr/co".into(),
                directory: Some("checkout".into()),
                upstream_name: "co".into(),
                jj: true,
            }
        );
        assert!(
            !parse_repo_command(&["--no-jj".into(), "hackr/co".into()], false, &config)
                .unwrap()
                .jj
        );
        assert_eq!(
            parse_repo_command(
                &[
                    "--jj".into(),
                    "-u".into(),
                    "mirror".into(),
                    "hackr/co".into(),
                ],
                false,
                &Config::default(),
            )
            .unwrap(),
            RepoCommand {
                spec: "hackr/co".into(),
                directory: None,
                upstream_name: "mirror".into(),
                jj: true,
            }
        );
    }

    #[test]
    fn rejects_invalid_repo_command_options() {
        let config = Config::default();
        assert!(parse_repo_command(&["--wat".into(), "hackr/co".into()], true, &config).is_err());
        assert!(parse_repo_command(&["-u".into(), "hackr/co".into()], true, &config).is_err());
        assert!(
            parse_repo_command(
                &["--set-upstream-name=-bad".into(), "hackr/co".into()],
                false,
                &config,
            )
            .is_err()
        );
        assert!(
            parse_repo_command(
                &["--set-upstream-name=bad..name".into(), "hackr/co".into()],
                false,
                &config,
            )
            .is_err()
        );
    }

    #[test]
    fn quotes_credential_helper_executable() {
        assert_eq!(
            credential_helper(Path::new("/tmp/co cli's/co")).unwrap(),
            "!'/tmp/co cli'\\''s/co' git-credential"
        );
    }

    #[test]
    fn trims_api_url() {
        let config = Config {
            api_url: Some("https://example.test/".into()),
            session_token: None,
            upstream_name: None,
            jj: false,
        };
        assert_eq!(api_url(&config), "https://example.test");
    }
}
