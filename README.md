# co

`co` is the human command-line client for [co.codes](https://co.codes).

The CLI supports browser-based device login, account inspection, human-approved agent access, repository creation and metadata, Git clone and push authentication, and production diagnostics.

## Install

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/codotcodes/co/main/install.sh | sh
```

See the [platform installation guide](docs/install.md) for Homebrew, Debian/Ubuntu, Fedora/RHEL, Arch Linux, Cargo, Nix, direct downloads, and upgrade/uninstall instructions. The installer supports Linux glibc/musl and macOS on x86_64 and ARM64, defaults to `~/.local/bin`, and verifies release checksums. Windows remains deferred.

## Usage

```sh
co login
co whoami
co repo view OWNER/REPO
co repo create [--public | --private] [--json] [OWNER/]NAME
co agent register NAME
co agent list
CO_AGENT_ID=AGENT_ID co agent attest OWNER/REPO FULL_COMMIT_OID
co access request --agent NAME OWNER/REPO
co access wait [REQUEST_ID]
co access view OWNER/REPO
co clone [OPTIONS] OWNER/REPO [DIRECTORY]
co link [OPTIONS] OWNER/REPO
co doctor
co logout
```

`co login` opens a browser for explicit device authorization. The local session is stored under the platform configuration directory with owner-only permissions. It is never written to Git configuration or passed as a command argument.

`co repo create NAME` creates an empty private repository in your personal namespace. Use `OWNER/NAME` for an explicit namespace you can write to, and `--public` to make the repository public. Creation is non-interactive and uses the existing `co login` machine session, so a coding agent can run it on your behalf with that session's permissions. Repository-scoped agent grants do not authorize repository creation.

```sh
co repo create my-project
co repo create my-org/my-project --public --json
```

`--json` writes the repository API response to stdout, including its `id`, resolved `owner`, `name`, `visibility`, `defaultBranch`, timestamps, and `created` storage flag. Errors go to stderr with a nonzero exit status. A successful creation with `created: false` still exits successfully: the repository is registered, but Git storage initialization was not confirmed, and a message goes to stderr. If a request times out, check whether the repository exists before retrying.

To connect existing local code after creation, run `co link OWNER/NAME`. To get a new checkout, run `co clone OWNER/NAME`.

`co access request --agent NAME OWNER/REPO` registers the named agent when needed, creates a one-time request capability through the existing machine session, and opens the repository access request in a browser. The command waits while a human owner or maintainer reviews the exact repository, operations, reason, and expiry. Approval requires a passkey; opening the browser grants nothing. Add `--push` to request pull and push instead of pull only, `--ttl SECONDS` for a 5-minute to 24-hour lifetime, and `--reason TEXT` to explain the task.

Pending requests are saved before the browser opens. If the command is interrupted, `co access wait [REQUEST_ID]` resumes polling. An approved lineage grant is stored in the same owner-only config and mints 15-minute repository-scoped tokens on demand. `co access view OWNER/REPO` returns the agent JSON document without printing its token-bearing self URL.

### Publish as an agent

Use a registered agent's random ID for authenticated work on public or private repositories. Its public profile is `https://co.codes/<owner>:<agent-id>` and its avatar stays stable when its name changes.

```sh
co access request --agent AGENT_ID --push OWNER/REPO
# An eligible human approves with a passkey.
co access wait
co link --jj OWNER/REPO
export CO_AGENT_ID=AGENT_ID
# After the owner authorizes publication of this bookmark:
jj git push -b BOOKMARK --remote=origin
co agent attest OWNER/REPO FULL_COMMIT_OID [ANOTHER_FULL_COMMIT_OID]
```

`CO_AGENT_ID` selects only that agent's live push grant for the exact repository. Clone and link enable repository-path credential requests; rerun `co link` for older checkouts. Agent credential failures stop helper lookup instead of using the human session or another helper. Without `CO_AGENT_ID`, the human credential path remains available.

Attest only full 40- or 64-character commit IDs the agent worked on, up to 100 per invocation. Pushing imported ancestors does not make them AI-assisted. Attestations require a live push grant even for public repositories; they record authenticated participation claims, not independently proven authorship or portable Git signatures. Retries are idempotent. If a batch partially succeeds, replay it; a 409 `contribution_index_pending` means to retry after indexing catches up. Attestation limits are 1000 new receipts per repository per hour and 50 agents per commit.

`co clone` uses the canonical `https://git.co.codes/OWNER/REPO.git` remote. `co link` adds that remote to the Git repository containing the current directory, including a bare repository. Both commands name the remote `origin` by default; use `-u NAME` or `--set-upstream-name NAME` to override it. Add `--jj` to run `jj git init --colocate` at the repository root after Git setup succeeds. Use `--no-jj` to override a configured jj default; jj setup requires a working tree.

Private repository authentication uses HTTP Basic with username `co` and the existing human session as its password. Clone and link configure a URL-scoped local helper so ordinary `git fetch` and `git push` work. Git configuration stores the helper command, never the session token. Public repositories remain anonymously cloneable without a session.

The helper is also available directly as `co git-credential get|store|erase`. It follows Git's credential protocol and returns credentials only for HTTPS requests to `git.co.codes` (with an optional default `:443` port).

Configuration is stored in `~/.config/co/config.json`, or `$XDG_CONFIG_HOME/co/config.json` when `XDG_CONFIG_HOME` is set. In addition to the managed `session_token`, you can set command defaults:

```json
{
  "upstream_name": "co",
  "jj": true
}
```

Command-line options override these defaults. Set `CO_API_URL` to override the configured `api_url` or use a non-production API endpoint.

Set `CO_CONFIG_DIR` to a nonempty directory path to store `config.json` and its lock file there instead. This takes precedence over `XDG_CONFIG_HOME` and `HOME` and isolates CLI sessions without changing the desktop or browser configuration inherited during login. An empty `CO_CONFIG_DIR` uses the default configuration location.

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
