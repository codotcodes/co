# Changelog

All notable changes are documented here. This project follows semantic versioning.

## [Unreleased]

- Support explicitly networked actions on public runners as well as owner-scoped runners, while keeping ordinary steps isolated.

## [0.11.0] - 2026-10-19

- Run repository actions with only their selected secrets and verified artifact inputs.
- Support explicitly declared outbound networking for actions on owner-controlled runners while keeping ordinary steps isolated.
- Capture declared structured action outputs through a separate channel and omit values containing delivered secrets.

## [0.10.0] - 2026-10-18

- Configure deployment environments and write-only secrets, inspect deployment receipts, and approve or roll back native deployments.
- Publish declared build artifacts from Unix runners without granting network access to build steps.

## [0.9.0] - 2026-10-17

- Match clone, link and Git credentials to the selected staging environment while preserving repository-scoped agent access.
- Create and update owner or repository workflow secrets with `co secret set` and fresh owner authentication, including private file and piped input.
- Pass declared workflow variables and secrets to jobs while masking secret values in their output.
- Run workflow jobs concurrently within a machine's available CPU capacity, with separate workspaces, logs and cancellation.

## [0.8.0] - 2026-09-30

- Add Windows x64 downloads and local installation with Scoop, WinGet and Chocolatey.
- Default to `%APPDATA%\co` for Windows login credentials, with access limited to the current user.
- Preserve drive-letter and network-share paths when configuring Git authentication on Windows.
- Support requests for human-approved agent repository access for up to 30 days with `co access request --ttl 2592000` where supported by the service. Existing grants keep their original expiry.

## [0.7.0] - 2026-09-28

- Fix machine enrollment on macOS when system utilities are absent from `PATH`.
- Send only new workflow output and verify cumulative log integrity, automatically repairing missing or mismatched logs.
- Show workflow output while steps run, recover log updates after temporary disconnects, and retain more output with explicit truncation notices.

## [0.6.0] - 2026-09-27

- Add explicit agent Git credentials through `CO_AGENT_ID`, scoped to the approved repository and refusing human-credential fallback.
- Add `co agent attest` to record authenticated participation in explicitly selected commits after pushing.
- Add machine enrollment with a setup key or an authorized owner session, saved machine identities, and reconnecting heartbeats.
- Add `co machine run` to execute assigned workflow jobs at their exact commit, with isolated steps, time limits, cancellation, and bounded logs.
- Add `co workflow check` to validate local workflow files and report file and line diagnostics.
- Keep newly configured clone and link credential helpers working across CLI upgrades that replace installation symlinks.
- Keep machine checkout URLs matched to the configured API environment.

## [0.5.0] - 2026-09-13

- Add `CO_CONFIG_DIR` to isolate CLI sessions while preserving the host's browser preferences during login.

## [0.4.0] - 2026-09-07

- Add a Homebrew formula and release packaging for Debian/Ubuntu, Fedora/RHEL, and Arch Linux on x86_64 and ARM64.
- Document installation, upgrades, removal, and PATH setup in one platform-organized guide.
- Pin installer downloads to one release, use portable static Linux binaries by default, and replace binaries atomically after checksum verification.

## [0.3.0] - 2026-09-06

- Add non-interactive `co repo create [OWNER/]NAME` with private defaults, explicit visibility, and JSON output for humans and coding agents using an authorized machine session.
- Add agent registration and listing through `co agent register` and `co agent list`.
- Add human-approved repository access requests, resumable approval polling, and agent-scoped repository views through `co access request`, `co access wait`, and `co access view`.
- Move repository and release links to `codotcodes/co`.

## [0.2.0] - 2026-08-26

- Add authenticated `co clone` over the canonical co.codes smart HTTP URL.
- Add a host-scoped Git credential helper backed by the existing CLI session.
- Configure cloned repositories for authenticated fetch and push without storing tokens in Git configuration.
- Add `co link` for connecting an existing local repository to co.codes.
- Add configurable upstream names and optional colocated jj initialization to clone and link.

## [0.1.0] - 2026-08-25

- Add device authorization login and local session revocation.
- Add `whoami`, `repo view`, `doctor`, and version commands.
- Validate repository access in `clone` while smart HTTP remains unavailable.
- Add Linux and macOS release packaging, Nix support, and the global installer.

[Unreleased]: https://github.com/codotcodes/co/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/codotcodes/co/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/codotcodes/co/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/codotcodes/co/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/codotcodes/co/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/codotcodes/co/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/codotcodes/co/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/codotcodes/co/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/codotcodes/co/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/codotcodes/co/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/codotcodes/co/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/codotcodes/co/releases/tag/v0.1.0
