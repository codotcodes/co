use std::env;
use std::process::Command;

pub(super) fn run(args: &[String]) -> Result<(), String> {
    let file = match args {
        [command] if command == "check" => None,
        [command, file] if command == "check" && !file.starts_with('-') => Some(file),
        _ => return Err("usage: co workflow check [FILE]".into()),
    };
    let root =
        env::current_dir().map_err(|error| format!("unable to read current directory: {error}"))?;
    let checker = root.join("node_modules/@cocodes/workflows/src/check.ts");
    if !checker.is_file() {
        return Err("workflow checker unavailable; install @cocodes/workflows in this repository and run from its root".into());
    }
    let mut command = Command::new("bun");
    command.arg("run").arg(&checker).current_dir(root);
    if let Some(file) = file {
        command.arg(file);
    }
    let status = command
        .status()
        .map_err(|error| format!("unable to launch Bun workflow checker: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("workflow check failed ({status})"))
    }
}
