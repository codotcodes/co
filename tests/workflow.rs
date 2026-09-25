use std::fs;
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
        "if (process.argv[2] !== '.co/workflows/example.ts') process.exit(3); console.log('diagnostic: bad selector'); process.exit(1);",
    ).unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_co"))
        .args(["workflow", "check", ".co/workflows/example.ts"])
        .current_dir(&root)
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stdout).contains("diagnostic: bad selector"));
    assert!(String::from_utf8_lossy(&output.stderr).contains("workflow check failed"));
    fs::remove_dir_all(&root).unwrap();
}
