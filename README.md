# co

`co` is the human command-line client for [co.codes](https://co.codes).

The CLI supports browser-based device login, account inspection, human-approved agent access, repository creation and metadata, Git clone and push authentication, and production diagnostics.

## Install

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/codotcodes/co/main/install.sh | sh
```

See the [platform installation guide](docs/install.md) for Homebrew, Debian/Ubuntu, Fedora/RHEL, Arch Linux, Cargo, Nix, direct downloads, and upgrade/uninstall instructions. The installer supports Linux glibc/musl and macOS on x86_64 and ARM64, defaults to `~/.local/bin`, and verifies release checksums.

Windows x64 releases provide direct ZIP downloads and local installation with Scoop, WinGet and Chocolatey. Use v0.8.0 or newer for the Windows assets; v0.7.0 has none. Public package catalogs are pending publication. The installation guide covers checksum-verified release downloads and local package installation.

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
co machine enroll --setup-key
co machine enroll --owner OWNER --label LABEL
co machine list
co machine run MACHINE_ID
co workflow check [FILE]
co deployment list OWNER/REPO
co deployment configure OWNER/REPO ENV --file POLICY.json
co deployment set-secret OWNER/REPO NAME --environment ENV
co deployment approve OWNER/REPO DEPLOYMENT_ID
co deployment rollback OWNER/REPO DEPLOYMENT_ID
co secret set NAME --owner OWNER
co secret set NAME --repo OWNER/REPO
co doctor
co logout
```

`co login` opens a browser for explicit device authorization. The local session is stored under the platform configuration directory with owner-only permissions. It is never written to Git configuration or passed as a command argument.

### Machines

`co machine enroll --setup-key` reads a one-time setup key from standard input, then detects the machine's OS, architecture, vCPUs and RAM. An administrator must first label a public machine in the admin dashboard and provide its setup key. The key is never passed as a command argument or printed. An owner or maintainer can instead use `co machine enroll --owner OWNER --label LABEL` after `co login` to register a personal or organization machine without a setup key; public machines cannot self-register. Owner-controlled attributes and resources in the dashboard can override detected values.

Enrollment saves a separate machine credential in the owner-only CLI config for the exact API used at enrollment. `co machine list` shows enrolled machine IDs without credentials. Start `co machine run MACHINE_ID` in the foreground; it sends a heartbeat every 30 seconds and polls for work every second while CPU slots remain. A registered machine is shown as offline after 90 seconds without a heartbeat. Keep the same `CO_API_URL` when using a non-production API.

The runner advertises concurrent CPU-slot support. A 32-vCPU host can run 4, 8, 16, 2 and 2-vCPU jobs together, each with its own workspace, containers, logs, secrets and lease renewal. The controller must support CPU reservations; older controllers continue assigning one job at a time. CPU requests set container quotas, not physical-core pinning. Memory does not affect placement: each job receives a proportional share of an 80% host-memory safety budget, with swap disabled. A profile permits one runner process per enrollment. On restart it stops that enrollment's leftover labeled containers before accepting new work.

Rootless Podman and a locally available image are required for execution. The default pinned Alpine image supports shell steps; set `CO_MACHINE_IMAGE` to another already provisioned image pinned by `@sha256:` for builds needing other tools. Checkout uses a short-lived pull-only token outside the container. Steps have no network or host credentials, a 10-minute per-step and 30-minute total limit. Changed output suffixes are uploaded about every 500 ms while commands run, with at most 16 KiB per step and 128 KiB per job, including visible truncation notices. Each batch carries UTF-8 byte offsets and a cumulative SHA-256 digest. The server acknowledges only persisted, verified content; missing or mismatched logs trigger a full snapshot repair. Temporary log-upload failures retry without stopping execution; final output and its digest are sent with the result. Failed lease renewals stop checkout or execution before the reservation expires; a disconnected or cancelled run cannot report success after its lease expires.

Public and owner-scoped runners support the same job types. An action declaring outbound networking requires rootless Podman and `slirp4netns`; capable runners advertise support regardless of enrollment scope. Only that action step receives networking, with host loopback disabled. Ordinary steps remain network-isolated. This is not a public-Internet-only filter: runner operators must isolate sensitive private networks. Repository authorization, explicitly selected secrets and pull-request restrictions remain enforced by the controller (D155).

`co workflow check` validates `.co/workflows/*.ts` in the current repository, or a single direct file with `co workflow check .co/workflows/verify.ts`. Install `@cocodes/workflows` into the repository first and run the command at the repository root. The checker requires Bun, parses TypeScript without evaluating repository code or contacting the API, and reports file/line diagnostics. Dynamic job generation is unsupported in v1.

Unix runners support `artifact: { name: "site", path: "dist" }` on a job. After all steps succeed, the harness uploads that directory through its live assignment, without enabling container networking. Outputs must contain only regular files: symbolic links, hard links, special files, traversal paths and empty artifacts are rejected. Limits are 4096 files, 8192 inspected entries and 32 MiB per bundle. Artifact-producing jobs cannot reference secrets. The controller must have private artifact storage configured; unsupported runners or missing storage fail closed. The server binds verified bytes to the exact job and commit and requires the receipt before accepting success. This is a build handoff, not deployment support.

### GitHub-compatible jobs

Current controllers can assign GitHub-compatible shell, JavaScript and composite
steps. Provision the platform's `deploy/runner/Containerfile` image and pin its
manifest digest in `CO_MACHINE_IMAGE`. The image label
`io.co.codes.github-actions=1` enables the `github-actions-v1` capability; the
minimal Alpine fallback does not advertise it. Outbound jobs also require
rootless Podman and `slirp4netns` (D156).

Compatible jobs have isolated writable HOME, tool-cache and command-file mounts.
`GITHUB_ENV`, `GITHUB_PATH` and `GITHUB_OUTPUT` persist bounded state between
compatible steps, never between jobs. Node/Bun/OpenTofu setup downloads verify
release checksums and install into that job's tool cache. Ordinary native jobs
keep their existing offline behavior. Public GitHub action refs resolve to exact
commits on the controller; the runner downloads and executes their code only
inside its container. No GitHub token or host credentials are provided.

The platform migration guide records unsupported action hooks, nested composites,
Docker actions, expressions and GitHub service APIs. An unavailable feature must
fail rather than be treated as successful. Existing controller secret, retry,
resource, log and lease rules continue to apply.

### Native deployments

`co deployment configure OWNER/REPO ENV --file POLICY.json` creates or updates an owner-managed deployment environment. The policy names the protected branch, required native jobs, build artifact and Cloudflare Worker target. `co deployment set-secret OWNER/REPO NAME --environment ENV` reads a write-only deployment secret from an echo-disabled prompt, standard input, or `--value-file PATH`. Omit `--environment` for a repository-scoped deployment secret. These secrets are separate from ordinary workflow secrets and are never passed to build jobs.

`co deployment list OWNER/REPO` prints deployment receipts. Use `co deployment approve OWNER/REPO ID` to approve an exact pending build, or `co deployment rollback OWNER/REPO ID` to request a new deployment of a recorded successful provider version. All mutations require an explicit human session and fresh password or authenticator verification; `--reauth-file PATH` supports a separate private credential file. Passkey-only accounts require a browser-based flow. The CLI never accepts secret values in arguments and never prints secret or authentication input. Uncertain promotions remain locked for operator investigation, not force-retried.

### Workflow secrets

`co secret set NAME --owner OWNER` creates or replaces a secret inherited by all of that owner's repositories. Use `--repo OWNER/REPO` for a repository-only value that overrides the inherited secret. Both commands require the actual personal owner or an organization owner, a human `co login` session, and fresh password or authenticator reauthentication. Passkey-only accounts must use the web settings page or enroll an authenticator. Agent grants do not authorize secret management.

The CLI prompts without echoing the value or credential. Piped input and `--value-file PATH` preserve the entire UTF-8 value, including newlines; an empty value is valid. Reauthentication uses a separate terminal prompt, never a line from the secret input. For noninteractive use, `--reauth-file PATH` reads the password or authenticator code from a separate private file, removing one final line ending. Keep both files private and never put secret values or passwords in command arguments.

```sh
co secret set DEPLOY_TOKEN --repo my-org/my-app
co secret set SIGNING_KEY --owner my-org --value-file ./private-key.txt
```

Names use uppercase letters, digits and underscores, start with a letter or underscore, and contain at most 100 characters. Values contain at most 16 KiB and no NUL bytes. Concurrent changes fail without retrying or overwriting the newer version. Successful output contains only the name, kind and version; values and credentials are not printed. Use the same `CO_API_URL` and `CO_CONFIG_DIR` as the corresponding login session when targeting staging.

`co repo create NAME` creates an empty private repository in your personal namespace. Use `OWNER/NAME` for an explicit namespace you can write to, and `--public` to make the repository public. Creation is non-interactive and uses the existing `co login` machine session, so a coding agent can run it on your behalf with that session's permissions. Repository-scoped agent grants do not authorize repository creation.

```sh
co repo create my-project
co repo create my-org/my-project --public --json
```

`--json` writes the repository API response to stdout, including its `id`, resolved `owner`, `name`, `visibility`, `defaultBranch`, timestamps, and `created` storage flag. Errors go to stderr with a nonzero exit status. A successful creation with `created: false` still exits successfully: the repository is registered, but Git storage initialization was not confirmed, and a message goes to stderr. If a request times out, check whether the repository exists before retrying.

To connect existing local code after creation, run `co link OWNER/NAME`. To get a new checkout, run `co clone OWNER/NAME`.

`co access request --agent NAME OWNER/REPO` registers the named agent when needed, creates a one-time request capability through the existing machine session, and opens the repository access request in a browser. The command waits while a human owner or maintainer reviews the exact repository, operations, reason, requested duration, and expiry. Approval requires a passkey, authenticator code, or verified-email code; opening the browser grants nothing. Add `--push` to request pull and push instead of pull only, `--ttl SECONDS` for a 5-minute to 30-day lifetime (300 through 2592000 seconds, default 3600), and `--reason TEXT` to explain the task.

Grant expiry starts when the request is submitted, not approved. Pending approval expires at the earlier of the grant expiry and 24 hours after submission. One-time request capabilities remain valid for at most 24 hours; verification codes remain short-lived. Existing grants keep their expiry; request and approve a new grant for longer access.

Pending requests are saved before the browser opens. If the command is interrupted, `co access wait [REQUEST_ID]` resumes polling, including retrieval of an already-approved month grant after the approval deadline. An approved lineage grant is stored in the same owner-only config and mints repository-scoped tokens on demand throughout its remaining lifetime. Each token lasts at most 15 minutes and never outlives the grant. Expired, revoked, or out-of-scope grants fail without human-credential fallback. `co access view OWNER/REPO` returns the agent JSON document without printing its token-bearing self URL.

### Publish as an agent

Use a registered agent's random ID for authenticated work on public or private repositories. Its public profile is `https://co.codes/<owner>:<agent-id>` and its avatar stays stable when its name changes.

```sh
co access request --agent AGENT_ID --push --ttl 2592000 OWNER/REPO
# An eligible human approves with a passkey, authenticator, or email code.
co access wait
co link --jj OWNER/REPO
export CO_AGENT_ID=AGENT_ID
# After the owner authorizes publication of this bookmark:
jj git push -b BOOKMARK --remote=origin
co agent attest OWNER/REPO FULL_COMMIT_OID [ANOTHER_FULL_COMMIT_OID]
```

`CO_AGENT_ID` selects only that agent's live push grant for the exact repository. Clone and link enable repository-path credential requests; rerun `co link` for older checkouts. Agent credential failures stop helper lookup instead of using the human session or another helper. Without `CO_AGENT_ID`, the human credential path remains available.

Attest only full 40- or 64-character commit IDs the agent worked on, up to 100 per invocation. Pushing imported ancestors does not make them AI-assisted. Attestations require a live push grant even for public repositories; they record authenticated participation claims, not independently proven authorship or portable Git signatures. Retries are idempotent. If a batch partially succeeds, replay it; a 409 `contribution_index_pending` means to retry after indexing catches up. Attestation limits are 1000 new receipts per repository per hour and 50 agents per commit.

`co clone` uses the canonical `https://git.co.codes/OWNER/REPO.git` remote. When the effective API is `https://api.codevved.com` (optionally with the default `:443` port), it uses `https://git.codevved.com/OWNER/REPO.git` instead. Other API overrides retain the production Git target. `co link` adds the selected remote to the Git repository containing the current directory, including a bare repository. Both commands name the remote `origin` by default; use `-u NAME` or `--set-upstream-name NAME` to override it. Add `--jj` to run `jj git init --colocate` at the repository root after Git setup succeeds. Use `--no-jj` to override a configured jj default; jj setup requires a working tree.

Private repository authentication uses HTTP Basic with username `co` and the existing human session as its password. Clone and link configure a URL-scoped local helper so ordinary `git fetch` and `git push` work. Git configuration stores the helper command, never the session token. Public repositories remain anonymously cloneable without a session.

The helper is also available directly as `co git-credential get|store|erase`. It follows Git's credential protocol and returns credentials only for HTTPS requests to the Git host selected by the effective API, with an optional default `:443` port. In agent mode, the requested repository path must match the selected agent's live push grant. Foreign hosts, cross-environment hosts and out-of-scope paths fail with `quit=true` and a nonzero exit status.

Keep the same API, isolated profile and selected agent for staging Git operations:

```sh
export CO_API_URL=https://api.codevved.com
export CO_CONFIG_DIR="$HOME/.config/co.codes/staging/co"
export CO_AGENT_ID=STAGING_AGENT_ID
co link --no-jj -u staging OWNER/REPO
git push staging HEAD:refs/heads/main
```

Use an existing human-approved staging pull/push grant for that exact repository. Every credential lookup refreshes a token under the grant's original expiry; it never extends the grant. The helper command stored by clone/link contains only the executable path, so retain these environment settings when running Git. A production profile or grant does not provide staging authorization.

Configuration defaults to `~/.config/co/config.json` on Linux and macOS, and `%APPDATA%\co\config.json` on Windows. A set `XDG_CONFIG_HOME` selects its `co/config.json` on any platform. Windows config directories, files and locks use protected current-user-only ACLs; the CLI rejects config directories/files owned by another identity or represented by a reparse point. In addition to the managed `session_token`, you can set command defaults:

```json
{
  "upstream_name": "co",
  "jj": true
}
```

Command-line options override these defaults. Set `CO_API_URL` to override the configured `api_url` or use a non-production API endpoint.

Set `CO_CONFIG_DIR` to a nonempty directory path to store `config.json` and its lock file there instead. This takes precedence over `XDG_CONFIG_HOME` and the platform default and isolates CLI sessions without changing the desktop or browser configuration inherited during login. An empty `CO_CONFIG_DIR` uses the default configuration location. In PowerShell, set an isolated directory with `$env:CO_CONFIG_DIR = "$env:LOCALAPPDATA\co-test"`; remove the override with `Remove-Item Env:CO_CONFIG_DIR`.

## Build

```sh
cargo test
cargo build --release
nix build
```

The minimum supported Rust version is 1.85.

## Releases

Releases follow semantic versioning. A `vX.Y.Z` tag must match the package version. GitHub Actions builds the six Linux/macOS targets and Windows x64 (`x86_64-pc-windows-msvc`), publishes checksums and build attestations, and creates the GitHub release after automated checks. Windows manager metadata comes from the verified ZIP and records its exact source and binary hashes. Community catalog publication is a separate maintenance step; see the [release maintenance guide](docs/install.md#release-maintenance). Crates.io publication is enabled when the repository variable `PUBLISH_CRATES_IO` is `true` and `CARGO_REGISTRY_TOKEN` is configured.

## Security

See [SECURITY.md](SECURITY.md). Do not disclose credential-handling vulnerabilities in a public issue.

## License

Licensed under either Apache-2.0 or MIT, at your option.
