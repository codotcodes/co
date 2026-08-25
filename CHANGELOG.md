# Changelog

All notable changes are documented here. This project follows semantic versioning.

## [Unreleased]

- Add authenticated `co clone` over the canonical co.codes smart HTTP URL.
- Add a host-scoped Git credential helper backed by the existing CLI session.
- Configure cloned repositories for authenticated fetch and push without storing tokens in Git configuration.

## [0.1.0] - 2026-08-25

- Add device authorization login and local session revocation.
- Add `whoami`, `repo view`, `doctor`, and version commands.
- Validate repository access in `clone` while smart HTTP remains unavailable.
- Add Linux and macOS release packaging, Nix support, and the global installer.

[Unreleased]: https://github.com/0xhckr/co/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/0xhckr/co/releases/tag/v0.1.0
