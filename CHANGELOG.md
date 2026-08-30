# Changelog

All notable changes are documented here. This project follows semantic versioning.

## [Unreleased]

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

[Unreleased]: https://github.com/codotcodes/co/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/codotcodes/co/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/codotcodes/co/releases/tag/v0.1.0
