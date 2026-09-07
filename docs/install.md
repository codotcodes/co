# Install the co CLI

`co` runs on Linux (x86_64 and ARM64) and macOS (Intel and Apple Silicon). Windows remains deferred. Choose one installation method so another `co` earlier in `PATH` does not hide upgrades.

## Linux and macOS installer

Requires `curl`, `tar`, `install`, and either `sha256sum` or `shasum`.

```sh
curl -fsSL https://raw.githubusercontent.com/codotcodes/co/main/install.sh | sh
```

The installer resolves the latest stable release once, verifies the archive's SHA-256 checksum, and atomically installs `co` in `~/.local/bin`. Rerun the same command to upgrade. A failed download or checksum check leaves the existing binary intact. Linux defaults to static musl binaries, which also work on older glibc distributions. Set `CO_LIBC=gnu` only if you specifically need the glibc build (built on Ubuntu 24.04).

Pin a version or choose a different directory:

```sh
curl -fsSL https://raw.githubusercontent.com/codotcodes/co/main/install.sh | CO_VERSION=v0.3.0 CO_INSTALL_DIR="$HOME/.local/bin" sh
```

Add this to `~/.profile` (Bash) or `~/.zprofile` (Zsh), then open a new terminal:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

For Fish, run `fish_add_path "$HOME/.local/bin"`. Verify with `command -v co` and `co version`. Uninstall with `rm "$HOME/.local/bin/co"`, or remove `co` from your custom install directory.

## Homebrew on macOS or Linux

The formula lives in this repository, used as a custom tap:

```sh
brew tap codotcodes/co https://github.com/codotcodes/co
brew install codotcodes/co/co
co version
```

Use the `brew shellenv` setup printed by Homebrew's installer to put its binary directory on `PATH`. The formula uses checksum-pinned release binaries for both architectures. To upgrade after the maintained formula is updated:

```sh
brew update
brew upgrade codotcodes/co/co
co version
```

Uninstall with `brew uninstall codotcodes/co/co`; optionally remove the tap with `brew untap codotcodes/co`. If a new release's formula update is still pending, use its `co.rb` release asset to update your local tap's `Formula/co.rb`, or use the general installer in a separate directory.

## Distribution packages

Download your package and its matching `.sha256` file from [the release page](https://github.com/codotcodes/co/releases). Packages install the static-musl binary at `/usr/bin/co`, plus licenses and this guide. `/usr/bin` is normally already on `PATH`.

| Family | Package | Architectures | Smoke-test environments |
|---|---|---|---|
| Debian / Ubuntu | `co-codes-cli_VERSION-1_ARCH.deb` | `amd64`, `arm64` | Debian 12, Ubuntu 24.04 |
| Fedora / RHEL-compatible | `co-codes-cli-VERSION-1.ARCH.rpm` | `x86_64`, `aarch64` | Fedora 43, Rocky Linux 9 |
| Arch Linux | `co-codes-cli-VERSION-1-ARCH.pkg.tar.zst` | `x86_64`, `aarch64` | Arch rolling on x86_64; ARM64 package for Arch Linux ARM |

Use the filenames actually listed on the release page. For each downloaded package:

```sh
sha256sum -c PACKAGE.sha256
```

Replace `PACKAGE` with the filename. Optionally verify the build provenance with `gh attestation verify PACKAGE --repo codotcodes/co`. Checksums detect corruption; provenance binds the artifact to the release build. Packages are release downloads, not packages in the distributions' official repositories, AUR, or an automatically updating apt/dnf repository. New package formats start with the first release containing this packaging workflow; v0.3.0 has archives only.

### Debian and Ubuntu

```sh
sudo apt install ./co-codes-cli_VERSION-1_amd64.deb
co version
```

For ARM64 use the `arm64` package. Upgrade by downloading and verifying the new `.deb`, then running the same `apt install ./...` command. Uninstall with `sudo apt remove co-codes-cli`.

### Fedora and RHEL-compatible distributions

```sh
sudo dnf install ./co-codes-cli-VERSION-1.x86_64.rpm
co version
```

For ARM64 use the `aarch64` package. Upgrade with `sudo dnf upgrade ./NEW_PACKAGE.rpm` after verifying its checksum. Uninstall with `sudo dnf remove co-codes-cli`. Local RPMs use release checksums and provenance; they are not signed with a separate RPM repository key.

### Arch Linux

```sh
sudo pacman -U ./co-codes-cli-VERSION-1-x86_64.pkg.tar.zst
co version
```

Upgrade by downloading and verifying the new package, then running `pacman -U` again. Uninstall with `sudo pacman -R co-codes-cli`. No AUR helper is required.

## Cargo

Requires a Rust toolchain meeting the minimum version in `Cargo.toml`:

```sh
cargo install --locked co-codes-cli
co version
```

Ensure `~/.cargo/bin` is on `PATH`. Rerun `cargo install --locked co-codes-cli` to upgrade, or add `--version 0.3.0` to select a published crate version. Uninstall with `cargo uninstall co-codes-cli`. Crate publication is optional per release, so the newest GitHub release can precede crates.io availability.

## Nix

Run without installing:

```sh
nix run github:codotcodes/co -- version
```

For a persistent profile installation:

```sh
nix profile add github:codotcodes/co
nix profile list
co version
```

Use the entry name shown by `nix profile list` with `nix profile upgrade NAME` or `nix profile remove NAME`. Nix's shell initialization adds its profile's `bin` directory to `PATH`. Pin a tag using `github:codotcodes/co/v0.3.0` when needed.

## Direct downloads and first use

Release `.tar.gz` archives remain available for all six supported targets. Download the archive and its `.sha256`, verify using `sha256sum -c` on Linux or `shasum -a 256 -c` on macOS, extract, and install `co` in a directory on `PATH`. Replace that binary to upgrade; remove it to uninstall.

```sh
co version
co doctor
co login
co whoami
```

Package removal does not remove your login configuration. Run `co logout` before uninstalling if you want to revoke the session. Configuration is under `${XDG_CONFIG_HOME:-$HOME/.config}/co`.

## Release maintenance

The release tag must match `Cargo.toml`. The release workflow builds the existing six target archives, packages both static Linux binaries with nFPM 2.47.0, emits per-file SHA-256 checksums and build attestations, and generates a version-pinned `co.rb` from verified archive checksums. Package-manager smoke tests must pass before release publication.

After publishing a release, download its `co.rb` and update `Formula/co.rb` on the default branch as the tap's version update. Review its four versioned URLs and checksums against that release. Homebrew users receive that version through `brew update` after the change is merged. To regenerate locally from downloaded archives and checksum files:

```sh
python3 scripts/homebrew-formula.py vX.Y.Z /path/to/release-assets > Formula/co.rb
```

Keep this formula at the latest published version, not an unreleased package version. The release also attaches the generated formula so a tap update never requires guessing checksums. Snap, Flatpak, and additional hosted package repositories add publication/signing maintenance without improving these CLI routes; they are not maintained at present.
