# Contributing

Contributions are welcome.

## Development

```sh
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
nix flake check
```

Keep changes focused and add tests for behavior. Do not add analytics, background network calls, credential output, or persistent identifiers without an explicit design decision and documentation.

## CI runners

Linux CI and release jobs use Blacksmith Ubuntu 24.04 runners, with native ARM64 runners for ARM64 releases and package checks. Apple Silicon macOS jobs use Blacksmith macOS 15, retaining a macOS 14 deployment target for released ARM64 binaries. Intel macOS builds and Homebrew checks remain on GitHub's native Intel runners because Blacksmith supports only ARM64 macOS. The Blacksmith GitHub App must have access to this repository for jobs to be provisioned.

## Compatibility

Supported release targets are listed in the README. Windows support is deferred and Windows-only changes should wait until maintainers establish a tested release target.

## Security changes

Use the private process in SECURITY.md instead of opening a pull request that demonstrates an exploitable vulnerability.
