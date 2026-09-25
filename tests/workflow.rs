use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::process::Command;
use std::time::{SystemTime, UNIX_EPOCH};

#[test]
fn workflow_check_passes_one_file_as_an_argument_and_returns_failure_status() {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos();
    let root = std::env::temp_dir().join(format!("co-check-{}-{nonce}", std::process::id()));
    let package = root.join("node_modules/@cocodes/workflows/src");
    fs::create_dir_all(&package).unwrap();
    fs::write(
        package.join("check.ts"),
        "// The CLI must pass this file to Bun.",
    )
    .unwrap();
    let bin = root.join("bin");
    fs::create_dir(&bin).unwrap();
    let bun = bin.join("bun");
    fs::write(
        &bun,
        "#!/bin/sh\n[ \"$1\" = run ] && [ \"$2\" = \"$EXPECTED_CHECKER\" ] && [ \"$3\" = .co/workflows/example.ts ] && [ \"$#\" -eq 3 ] || exit 3\nprintf '%s\\n' 'diagnostic: bad selector'\nexit 1\n",
    ).unwrap();
    fs::set_permissions(&bun, fs::Permissions::from_mode(0o755)).unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_co"))
        .args(["workflow", "check", ".co/workflows/example.ts"])
        .current_dir(&root)
        .env("PATH", &bin)
        .env("EXPECTED_CHECKER", package.join("check.ts"))
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stdout).contains("diagnostic: bad selector"));
    assert!(String::from_utf8_lossy(&output.stderr).contains("workflow check failed"));
    fs::remove_dir_all(&root).unwrap();
}
