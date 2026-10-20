use std::collections::{BTreeMap, BTreeSet};
#[cfg(unix)]
use std::io::Read;
use std::path::Path;

pub(super) const JOB_BYTES: usize = 65_536;

pub(super) fn valid_keys(keys: &[String]) -> bool {
    !keys.is_empty()
        && keys.len() <= 32
        && keys.iter().collect::<BTreeSet<_>>().len() == keys.len()
        && keys.iter().all(|key| {
            key.len() <= 64
                && !["constructor", "prototype"].contains(&key.as_str())
                && key.bytes().enumerate().all(|(index, byte)| {
                    byte.is_ascii_lowercase()
                        || index > 0 && (byte.is_ascii_digit() || byte == b'_' || byte == b'-')
                })
                && !key.is_empty()
        })
}

pub(super) fn size(outputs: &BTreeMap<String, String>) -> usize {
    outputs
        .iter()
        .map(|(key, value)| key.len() + value.len())
        .sum()
}

#[cfg(unix)]
fn encoded_secret(secret: &str) -> String {
    let mut result = String::new();
    for byte in secret.bytes() {
        if byte.is_ascii_alphanumeric() || b"-_.!~*'()".contains(&byte) {
            result.push(char::from(byte));
        } else {
            result.push_str(&format!("%{byte:02X}"));
        }
    }
    result
}

/// Only the fixed file under this step's fresh mount is read, never a container-supplied path.
#[cfg(unix)]
pub(super) fn collect(
    directory: &Path,
    declared: &[String],
    secrets: &[String],
) -> Option<BTreeMap<String, String>> {
    use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
    if !valid_keys(declared) {
        return None;
    }
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(directory.join("outputs.json"))
        .ok()?;
    let metadata = file.metadata().ok()?;
    if !metadata.is_file() || metadata.nlink() != 1 || metadata.len() > 65_536 {
        return None;
    }
    let mut bytes = Vec::new();
    file.take(65_537).read_to_end(&mut bytes).ok()?;
    if bytes.len() > 65_536 {
        return None;
    }
    let mut outputs: BTreeMap<String, String> = serde_json::from_slice(&bytes).ok()?;
    if outputs.len() > 32
        || size(&outputs) > 16_384
        || outputs.iter().any(|(key, value)| {
            !declared.contains(key) || value.len() > 4096 || value.contains('\0')
        })
    {
        return None;
    }
    outputs.retain(|_, value| {
        !secrets
            .iter()
            .filter(|secret| !secret.is_empty())
            .any(|secret| {
                value.contains(secret)
                    || value
                        .to_lowercase()
                        .contains(&encoded_secret(secret).to_lowercase())
            })
    });
    (!outputs.is_empty()).then_some(outputs)
}

#[cfg(not(unix))]
pub(super) fn collect(
    _directory: &Path,
    _declared: &[String],
    _secrets: &[String],
) -> Option<BTreeMap<String, String>> {
    None
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    #[test]
    fn accepts_only_bounded_declared_values_and_omits_secrets() {
        let dir = tempfile::tempdir().unwrap();
        let keys = vec!["url".into(), "version_id".into()];
        std::fs::write(
            dir.path().join("outputs.json"),
            r#"{"url":"https://site.example/?token=secret/value","version_id":"v1"}"#,
        )
        .unwrap();
        let outputs = collect(dir.path(), &keys, &["secret/value".into()]).unwrap();
        assert_eq!(outputs.len(), 1);
        assert_eq!(outputs["version_id"], "v1");
        std::fs::write(
            dir.path().join("outputs.json"),
            r#"{"url":"https://site.example/?token=secret%2fvalue"}"#,
        )
        .unwrap();
        assert!(collect(dir.path(), &keys, &["secret/value".into()]).is_none());
        for source in [
            r#"{"undeclared":"x"}"#.to_owned(),
            r#"{"url":4}"#.to_owned(),
            serde_json::json!({"url":"x".repeat(4097)}).to_string(),
        ] {
            std::fs::write(dir.path().join("outputs.json"), source).unwrap();
            assert!(collect(dir.path(), &keys, &[]).is_none());
        }
    }

    #[cfg(unix)]
    #[test]
    fn rejects_symlinks_and_hardlinks_outside_the_output_mount() {
        let dir = tempfile::tempdir().unwrap();
        let outside = tempfile::NamedTempFile::new().unwrap();
        std::fs::write(outside.path(), r#"{"url":"outside"}"#).unwrap();
        let channel = dir.path().join("outputs.json");
        std::os::unix::fs::symlink(outside.path(), &channel).unwrap();
        assert!(collect(dir.path(), &["url".into()], &[]).is_none());
        std::fs::remove_file(&channel).unwrap();
        std::fs::hard_link(outside.path(), &channel).unwrap();
        assert!(collect(dir.path(), &["url".into()], &[]).is_none());
    }
}
