# Changelog

All notable changes are documented here. This project follows semantic versioning.

## [Unreleased]

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

[Unreleased]: https://github.com/codotcodes/co/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/codotcodes/co/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/codotcodes/co/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/codotcodes/co/releases/tag/v0.1.0
