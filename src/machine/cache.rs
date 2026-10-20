use super::artifacts;
use fs2::FileExt;
use serde::{Deserialize, Serialize};
use std::fs::{self, File};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};

const LIMIT: usize = 32 * 1024 * 1024;
const HOST_LIMIT: u64 = 1024 * 1024 * 1024;

#[derive(Clone)]
pub(super) struct Store {
    root: PathBuf,
    namespace: PathBuf,
}

pub(super) struct Publication {
    store: Store,
    id: String,
    bytes: Vec<u8>,
}

#[derive(Deserialize, Serialize)]
struct Request {
    key: String,
    path: String,
}

#[derive(Serialize)]
struct Entry {
    path: String,
    size: usize,
    sha256: String,
    mode: u32,
}

fn invalid() -> io::Error {
    io::Error::other("cache input is invalid or exceeds its limits")
}

fn directory(path: &Path) -> io::Result<()> {
    fs::create_dir_all(path)?;
    if !fs::symlink_metadata(path)?.is_dir() {
        return Err(invalid());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

fn read(path: &Path, limit: usize) -> io::Result<Vec<u8>> {
    let mut options = File::options();
    options.read(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK);
    }
    let file = options.open(path)?;
    let metadata = file.metadata()?;
    if !metadata.is_file() || metadata.len() > limit as u64 {
        return Err(invalid());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        if metadata.nlink() != 1 {
            return Err(invalid());
        }
    }
    let mut bytes = Vec::new();
    file.take((limit + 1) as u64).read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(invalid());
    }
    Ok(bytes)
}

fn safe_path(value: &str) -> bool {
    value.len() <= 1024
        && value.split('/').all(|part| {
            !part.is_empty()
                && !matches!(part, "." | "..")
                && !part.eq_ignore_ascii_case(".git")
                && !part.chars().any(|c| c.is_control() || c == '\\')
        })
}

fn collect(root: &Path) -> io::Result<Vec<u8>> {
    fn walk(
        root: &Path,
        prefix: &str,
        entries: &mut Vec<Entry>,
        data: &mut Vec<u8>,
        visited: &mut usize,
    ) -> io::Result<()> {
        for entry in fs::read_dir(root)? {
            let entry = entry?;
            *visited += 1;
            if *visited > 8192 {
                return Err(invalid());
            }
            let name = entry.file_name().into_string().map_err(|_| invalid())?;
            let path = if prefix.is_empty() {
                name
            } else {
                format!("{prefix}/{name}")
            };
            if !safe_path(&path) {
                return Err(invalid());
            }
            let metadata = fs::symlink_metadata(entry.path())?;
            if metadata.is_dir() {
                walk(&entry.path(), &path, entries, data, visited)?;
            } else if metadata.is_file() && entries.len() < 4096 {
                let bytes = read(&entry.path(), LIMIT.saturating_sub(data.len()))?;
                #[cfg(unix)]
                let executable = {
                    use std::os::unix::fs::PermissionsExt;
                    metadata.permissions().mode() & 0o111 != 0
                };
                #[cfg(not(unix))]
                let executable = false;
                entries.push(Entry {
                    path,
                    size: bytes.len(),
                    sha256: artifacts::digest(&bytes),
                    mode: if executable { 0o755 } else { 0o644 },
                });
                data.extend(bytes);
            } else {
                return Err(invalid());
            }
        }
        Ok(())
    }
    let mut entries = Vec::new();
    let mut data = Vec::new();
    if !fs::symlink_metadata(root)?.is_dir() {
        return Err(invalid());
    }
    walk(root, "", &mut entries, &mut data, &mut 0)?;
    let manifest = serde_json::to_vec(&entries)?;
    if entries.is_empty()
        || manifest.len() > 1024 * 1024
        || data.len() + manifest.len() + 10 > LIMIT
    {
        return Err(invalid());
    }
    let mut bytes = b"COAR1\0".to_vec();
    bytes.extend((manifest.len() as u32).to_be_bytes());
    bytes.extend(manifest);
    bytes.extend(data);
    Ok(bytes)
}

impl Store {
    pub(super) fn open(root: PathBuf, checkout_url: &str, image: &str) -> io::Result<Self> {
        directory(&root)?;
        let marker = root.join(".co-cache-v1");
        if !marker.exists() {
            if fs::read_dir(&root)?.next().is_some() {
                return Err(invalid());
            }
            match File::options().write(true).create_new(true).open(&marker) {
                Ok(mut file) => file.write_all(b"co-cache-v1")?,
                Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
                Err(error) => return Err(error),
            }
        }
        if read(&marker, 32)? != b"co-cache-v1" {
            return Err(invalid());
        }
        let namespace = root.join(artifacts::digest(
            format!("{checkout_url}\0{image}").as_bytes(),
        ));
        if !namespace.exists() && fs::read_dir(&root)?.take(258).count() > 256 {
            return Err(invalid());
        }
        directory(&namespace)?;
        Ok(Self { root, namespace })
    }

    pub(super) fn path(&self) -> &Path {
        &self.namespace
    }

    pub(super) fn collect(&self, checkout: &Path, runtime: &Path) -> io::Result<Vec<Publication>> {
        let source = match read(&runtime.join("cache-requests.json"), 65_536) {
            Ok(bytes) => bytes,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(vec![]),
            Err(error) => return Err(error),
        };
        let requests: Vec<Request> = serde_json::from_slice(&source)?;
        if requests.len() > 16 {
            return Err(invalid());
        }
        let mut result = Vec::new();
        let mut total = 0;
        for request in requests {
            if request.key.is_empty() || request.key.len() > 512 || request.key.contains('\0') {
                return Err(invalid());
            }
            let (root, relative) = if let Some(path) = request.path.strip_prefix("/workspace/") {
                (checkout, path)
            } else if let Some(path) = request.path.strip_prefix("/run/co-job/home/") {
                (runtime, path)
            } else {
                return Err(invalid());
            };
            let relative = if request.path.starts_with("/run/co-job/home/") {
                format!("home/{relative}")
            } else {
                relative.to_owned()
            };
            if !safe_path(&relative) {
                return Err(invalid());
            }
            let mut target = root.to_owned();
            for part in relative.split('/') {
                target.push(part);
                if !fs::symlink_metadata(&target)?.is_dir() {
                    return Err(invalid());
                }
            }
            let id = artifacts::digest(format!("{}\0{}", request.path, request.key).as_bytes());
            if self.namespace.join(format!("{id}.cache")).exists() {
                continue;
            }
            let bundle = collect(&target)?;
            total += bundle.len();
            if total > LIMIT {
                return Err(invalid());
            }
            let metadata = serde_json::to_vec(
                &serde_json::json!({ "key": request.key, "path": request.path, "digest": artifacts::digest(&bundle) }),
            )?;
            let mut bytes = b"COCA1\0".to_vec();
            bytes.extend((metadata.len() as u32).to_be_bytes());
            bytes.extend(metadata);
            bytes.extend(bundle);
            result.push(Publication {
                store: self.clone(),
                id,
                bytes,
            });
        }
        Ok(result)
    }
}

impl Publication {
    /// Call only after the controller accepts successful completion.
    pub(super) fn publish(self) -> io::Result<()> {
        let lock = File::options()
            .create(true)
            .truncate(false)
            .write(true)
            .open(self.store.root.join(".lock"))?;
        lock.try_lock_exclusive()?;
        let mut entries = Vec::new();
        let mut total = 0;
        for namespace in fs::read_dir(&self.store.root)?.take(258) {
            let namespace = namespace?;
            let name = namespace.file_name();
            if name.to_string_lossy().len() != 64 || !namespace.file_type()?.is_dir() {
                continue;
            }
            for entry in fs::read_dir(namespace.path())?.take(34) {
                let entry = entry?;
                if !entry.file_type()?.is_file()
                    || entry.path().extension().is_none_or(|ext| ext != "cache")
                {
                    continue;
                }
                let metadata = entry.metadata()?;
                total += metadata.len();
                entries.push((metadata.modified()?, entry.path(), metadata.len()));
            }
        }
        entries.sort_by_key(|entry| entry.0);
        let mut namespace_count = entries
            .iter()
            .filter(|entry| entry.1.parent() == Some(self.store.path()))
            .count();
        for (_, path, size) in &entries {
            if total + self.bytes.len() as u64 <= HOST_LIMIT && namespace_count < 32 {
                break;
            }
            if total + self.bytes.len() as u64 <= HOST_LIMIT
                && path.parent() != Some(self.store.path())
            {
                continue;
            }
            fs::remove_file(path)?;
            total -= size;
            if path.parent() == Some(self.store.path()) {
                namespace_count -= 1;
            }
        }
        let mut temp = tempfile::NamedTempFile::new_in(self.store.path())?;
        temp.write_all(&self.bytes)?;
        match temp.persist_noclobber(self.store.namespace.join(format!("{}.cache", self.id))) {
            Ok(_) => Ok(()),
            Err(error) if error.error.kind() == io::ErrorKind::AlreadyExists => Ok(()),
            Err(error) => Err(error.error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cache_is_immutable_and_repository_scoped() {
        let root = tempfile::tempdir().unwrap();
        let checkout = tempfile::tempdir().unwrap();
        let runtime = tempfile::tempdir().unwrap();
        let store =
            Store::open(root.path().into(), "https://git.co.codes/a/b.git", "image").unwrap();
        let other =
            Store::open(root.path().into(), "https://git.co.codes/a/c.git", "image").unwrap();
        assert_ne!(store.path(), other.path());
        let replaced = Store::open(
            root.path().into(),
            "https://git.co.codes/a/b.git\0replacement-repository",
            "image",
        )
        .unwrap();
        assert_ne!(store.path(), replaced.path());
        fs::create_dir_all(checkout.path().join("dependencies/@scope")).unwrap();
        fs::write(checkout.path().join("dependencies/@scope/file"), "cached").unwrap();
        fs::write(
            runtime.path().join("cache-requests.json"),
            r#"[{"key":"lock-key","path":"/workspace/dependencies"}]"#,
        )
        .unwrap();
        let mut pending = store.collect(checkout.path(), runtime.path()).unwrap();
        assert_eq!(fs::read_dir(store.path()).unwrap().count(), 0);
        pending.pop().unwrap().publish().unwrap();
        assert_eq!(fs::read_dir(store.path()).unwrap().count(), 1);
        assert!(
            store
                .collect(checkout.path(), runtime.path())
                .unwrap()
                .is_empty()
        );
        assert_eq!(fs::read_dir(other.path()).unwrap().count(), 0);
    }

    #[cfg(unix)]
    #[test]
    fn cache_rejects_links_and_host_paths() {
        let root = tempfile::tempdir().unwrap();
        let checkout = tempfile::tempdir().unwrap();
        let runtime = tempfile::tempdir().unwrap();
        let store = Store::open(root.path().into(), "repo", "image").unwrap();
        fs::create_dir(checkout.path().join("deps")).unwrap();
        std::os::unix::fs::symlink("/etc/passwd", checkout.path().join("deps/link")).unwrap();
        fs::write(
            runtime.path().join("cache-requests.json"),
            r#"[{"key":"x","path":"/workspace/deps"}]"#,
        )
        .unwrap();
        assert!(store.collect(checkout.path(), runtime.path()).is_err());
        fs::write(
            runtime.path().join("cache-requests.json"),
            r#"[{"key":"x","path":"/etc"}]"#,
        )
        .unwrap();
        assert!(store.collect(checkout.path(), runtime.path()).is_err());
    }

    #[test]
    fn cache_budget_evicts_its_own_old_entries_without_adopting_other_data() {
        let root = tempfile::tempdir().unwrap();
        fs::write(root.path().join("unrelated"), "keep").unwrap();
        assert!(Store::open(root.path().into(), "repo", "image").is_err());
        assert_eq!(
            fs::read_to_string(root.path().join("unrelated")).unwrap(),
            "keep"
        );
        fs::remove_file(root.path().join("unrelated")).unwrap();
        let store = Store::open(root.path().into(), "repo", "image").unwrap();
        let other = Store::open(root.path().into(), "other", "image").unwrap();
        fs::write(
            other.path().join(format!("{}.cache", "a".repeat(64))),
            "keep",
        )
        .unwrap();
        for i in 0..32 {
            fs::write(store.path().join(format!("{i:064x}.cache")), "old").unwrap();
        }
        Publication {
            store: store.clone(),
            id: "f".repeat(64),
            bytes: b"new".to_vec(),
        }
        .publish()
        .unwrap();
        assert_eq!(fs::read_dir(store.path()).unwrap().count(), 32);
        assert_eq!(fs::read_dir(other.path()).unwrap().count(), 1);
    }
}
