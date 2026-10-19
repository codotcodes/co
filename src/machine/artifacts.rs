use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::fs::{self, File};
use std::io::Read;
use std::path::Path;

pub(super) const MAX_BYTES: usize = 32 * 1024 * 1024;
const MAX_FILES: usize = 4096;
const ERROR: &str = "Unable to collect artifact: use a nonempty directory of regular files within the artifact limits.";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct Output {
    pub name: String,
    pub path: String,
}

impl Output {
    pub fn valid(&self) -> bool {
        !self.name.is_empty()
            && self.name.len() <= 64
            && self.name.as_bytes()[0].is_ascii_lowercase()
            && self
                .name
                .bytes()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || b"_-".contains(&c))
            && valid_path(&self.path)
    }
}

pub(super) fn valid_path(path: &str) -> bool {
    path.len() <= 1024
        && path.split('/').all(|part| {
            !part.is_empty()
                && ![".", ".."].contains(&part)
                && !part.eq_ignore_ascii_case(".git")
                && part
                    .bytes()
                    .all(|c| c.is_ascii_alphanumeric() || b"._-".contains(&c))
        })
}

#[derive(Serialize)]
struct Entry {
    path: String,
    size: usize,
    sha256: String,
}

pub(super) fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

/// The job containers have stopped before collection; no repository command is run here.
pub(super) fn collect(checkout: &Path, output: &Output) -> Result<Vec<u8>, String> {
    if !output.valid() {
        return Err(ERROR.into());
    }
    let mut directory = checkout.to_owned();
    for part in output.path.split('/') {
        directory.push(part);
        if !fs::symlink_metadata(&directory)
            .map_err(|_| ERROR)?
            .is_dir()
        {
            return Err(ERROR.into());
        }
    }
    let mut files = Vec::new();
    walk(&directory, "", &mut files, &mut 0, &mut 0)?;
    files.sort_by(|a, b| a.0.cmp(&b.0));
    if files.is_empty() {
        return Err(ERROR.into());
    }
    let entries: Vec<_> = files
        .iter()
        .map(|(path, bytes)| Entry {
            path: path.clone(),
            size: bytes.len(),
            sha256: digest(bytes),
        })
        .collect();
    let manifest = serde_json::to_vec(&entries).map_err(|_| ERROR)?;
    let size = 10 + manifest.len() + files.iter().map(|(_, data)| data.len()).sum::<usize>();
    if size > MAX_BYTES || manifest.len() > 1024 * 1024 {
        return Err(ERROR.into());
    }
    let mut result = Vec::with_capacity(size);
    result.extend_from_slice(b"COAR1\0");
    result.extend_from_slice(&(manifest.len() as u32).to_be_bytes());
    result.extend_from_slice(&manifest);
    for (_, bytes) in files {
        result.extend_from_slice(&bytes);
    }
    Ok(result)
}

fn walk(
    directory: &Path,
    prefix: &str,
    files: &mut Vec<(String, Vec<u8>)>,
    total: &mut usize,
    visited: &mut usize,
) -> Result<(), String> {
    // Depth is bounded by the 1024-byte path bound before descending.
    for entry in fs::read_dir(directory).map_err(|_| ERROR)? {
        let entry = entry.map_err(|_| ERROR)?;
        *visited += 1;
        if *visited > MAX_FILES * 2 {
            return Err(ERROR.into());
        }
        let name = entry.file_name().into_string().map_err(|_| ERROR)?;
        let path = if prefix.is_empty() {
            name
        } else {
            format!("{prefix}/{name}")
        };
        if !valid_path(&path) {
            return Err(ERROR.into());
        }
        let metadata = fs::symlink_metadata(entry.path()).map_err(|_| ERROR)?;
        if metadata.is_dir() {
            walk(&entry.path(), &path, files, total, visited)?;
        } else if metadata.is_file() {
            if files.len() >= MAX_FILES || metadata.len() > (MAX_BYTES - *total) as u64 {
                return Err(ERROR.into());
            }
            let mut options = File::options();
            options.read(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt;
                options.custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK);
            }
            let file = options.open(entry.path()).map_err(|_| ERROR)?;
            let opened = file.metadata().map_err(|_| ERROR)?;
            if !opened.is_file() {
                return Err(ERROR.into());
            }
            #[cfg(unix)]
            {
                use std::os::unix::fs::MetadataExt;
                if opened.nlink() != 1
                    || opened.ino() != metadata.ino()
                    || opened.dev() != metadata.dev()
                {
                    return Err(ERROR.into());
                }
            }
            let mut bytes = Vec::new();
            file.take((MAX_BYTES - *total + 1) as u64)
                .read_to_end(&mut bytes)
                .map_err(|_| ERROR)?;
            if bytes.len() > MAX_BYTES - *total || bytes.len() as u64 != opened.len() {
                return Err(ERROR.into());
            }
            *total += bytes.len();
            files.push((path, bytes));
        } else {
            return Err(ERROR.into());
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn output() -> Output {
        Output {
            name: "site".into(),
            path: "dist".into(),
        }
    }

    #[test]
    fn collects_deterministic_bounded_static_files() {
        let root = tempfile::tempdir().unwrap();
        fs::create_dir_all(root.path().join("dist/assets")).unwrap();
        fs::write(root.path().join("dist/index.html"), "<h1>Astro</h1>").unwrap();
        fs::write(root.path().join("dist/assets/empty.css"), "").unwrap();
        let data = collect(root.path(), &output()).unwrap();
        assert_eq!(&data[..6], b"COAR1\0");
        assert_eq!(data, collect(root.path(), &output()).unwrap());
        assert_eq!(
            digest(&data),
            "62661a2f38380178a1c97e41472dcd809cd9ee3964d41d46a4aaf31a34cfc9f7"
        );
        let len = u32::from_be_bytes(data[6..10].try_into().unwrap()) as usize;
        let entries: serde_json::Value = serde_json::from_slice(&data[10..10 + len]).unwrap();
        assert_eq!(entries[0]["path"], "assets/empty.css");
        assert_eq!(entries[1]["sha256"], digest(b"<h1>Astro</h1>"));
    }

    #[test]
    fn rejects_traversal_empty_and_oversized_artifacts() {
        let root = tempfile::tempdir().unwrap();
        fs::create_dir(root.path().join("dist")).unwrap();
        assert!(collect(root.path(), &output()).is_err());
        let invalid = Output {
            name: "site".into(),
            path: "../dist".into(),
        };
        assert!(collect(root.path(), &invalid).is_err());
        let file = File::create(root.path().join("dist/huge")).unwrap();
        file.set_len(MAX_BYTES as u64 + 1).unwrap();
        assert!(collect(root.path(), &output()).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn rejects_symlinked_roots_files_and_hardlinks() {
        use std::os::unix::fs::symlink;
        let root = tempfile::tempdir().unwrap();
        fs::create_dir(root.path().join("real")).unwrap();
        fs::write(root.path().join("real/index.html"), "site").unwrap();
        symlink("real", root.path().join("dist")).unwrap();
        assert!(collect(root.path(), &output()).is_err());
        fs::remove_file(root.path().join("dist")).unwrap();
        fs::create_dir(root.path().join("dist")).unwrap();
        symlink("../real/index.html", root.path().join("dist/link")).unwrap();
        assert!(collect(root.path(), &output()).is_err());
        fs::remove_file(root.path().join("dist/link")).unwrap();
        fs::hard_link(
            root.path().join("real/index.html"),
            root.path().join("dist/link"),
        )
        .unwrap();
        assert!(collect(root.path(), &output()).is_err());
    }
}
