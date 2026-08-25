# co

`co` is the human command-line client for [co.codes](https://co.codes).

The current release supports browser-based device login, account inspection, repository metadata, Git clone and push authentication, and production diagnostics.

## Install

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/0xhckr/co/main/install.sh | sh
```

Nix:

```sh
nix run github:0xhckr/co -- help
```

Cargo:

```sh
cargo install co-codes-cli
```

The release installer supports:

| OS | Architectures |
|---|---|
| Linux, glibc or musl | x86_64, ARM64 |
| macOS | Intel, Apple Silicon |

Windows is intentionally deferred until there is enough demand and committed testing coverage.

The installer defaults to `~/.local/bin`. Set `CO_INSTALL_DIR` to choose another location, or `CO_VERSION=v0.1.0` to install a specific release.

## Usage

```sh
co login
co whoami
co repo view OWNER/REPO
co clone OWNER/REPO [DIRECTORY]
co doctor
co logout
```

`co login` opens a browser for explicit device authorization. The local session is stored under the platform configuration directory with owner-only permissions. It is never written to Git configuration or passed as a command argument.

`co clone` uses the canonical `https://git.co.codes/OWNER/REPO.git` remote. Private repository authentication uses HTTP Basic with username `co` and the existing session as its password. The clone receives the credential through a temporary helper command, then gets a URL-scoped local helper so ordinary `git fetch` and `git push` work. Git configuration stores the helper command, never the session token. Public repositories remain anonymously cloneable without a session.

The helper is also available directly as `co git-credential get|store|erase`. It follows Git's credential protocol and returns credentials only for HTTPS requests to `git.co.codes` (with an optional default `:443` port).

Set `CO_API_URL` to use a non-production API endpoint.

## Build

```sh
cargo test
cargo build --release
nix build
```

The minimum supported Rust version is 1.85.

## Releases

Releases follow semantic versioning. A `vX.Y.Z` tag must match the package version. GitHub Actions builds all supported targets, publishes checksums and build attestations, and creates the GitHub release. Crates.io publication is enabled when the repository variable `PUBLISH_CRATES_IO` is `true` and `CARGO_REGISTRY_TOKEN` is configured.

## Security

See [SECURITY.md](SECURITY.md). Do not disclose credential-handling vulnerabilities in a public issue.

## License

Licensed under either Apache-2.0 or MIT, at your option.
