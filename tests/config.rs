use std::fs;
use std::process::Command;
use std::time::{SystemTime, UNIX_EPOCH};

#[test]
fn config_override_preserves_host_config_and_default_paths() {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos();
    let root = std::env::temp_dir().join(format!("co-config-{}-{nonce}", std::process::id()));
    let host = root.join("host");
    let isolated = root.join("isolated");
    let home = root.join("home");
    fs::create_dir_all(host.join("co")).unwrap();
    let host_config = host.join("co/config.json");
    fs::write(&host_config, "host config must not be read or overwritten").unwrap();

    let run = |override_dir: Option<&std::path::Path>, xdg: bool| {
        let mut command = Command::new(env!("CARGO_BIN_EXE_co"));
        command
            .arg("logout")
            .env("HOME", &home)
            .env_remove("CO_CONFIG_DIR")
            .env_remove("XDG_CONFIG_HOME");
        if let Some(path) = override_dir {
            command.env("CO_CONFIG_DIR", path);
        }
        if xdg {
            command.env("XDG_CONFIG_HOME", &host);
        }
        let output = command.output().unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
    };

    run(Some(&isolated), true);
    assert!(isolated.join("config.json").is_file());
    assert_eq!(
        fs::read_to_string(&host_config).unwrap(),
        "host config must not be read or overwritten"
    );
    fs::remove_file(&host_config).unwrap();
    run(None, true);
    assert!(host_config.is_file());
    run(None, false);
    assert!(home.join(".config/co/config.json").is_file());
    fs::remove_dir_all(root).unwrap();
}
