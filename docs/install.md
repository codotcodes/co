# Install the co CLI

`co` runs on Linux (x86_64 and ARM64), macOS (Intel and Apple Silicon) and Windows x64. Choose one installation method so another `co` earlier in `PATH` does not hide upgrades.

## Linux and macOS installer

Requires `curl`, `tar`, `install`, and either `sha256sum` or `shasum`.

```sh
curl -fsSL https://raw.githubusercontent.com/codotcodes/co/main/install.sh | sh
```

The installer resolves the latest stable release once, verifies the archive's SHA-256 checksum, and atomically installs `co` in `~/.local/bin`. Rerun the same command to upgrade. A failed download or checksum check leaves the existing binary intact. Linux defaults to the static `musl` archive name. Go releases also use static binaries under the existing `gnu` archive names; older Rust `gnu` builds require glibc from Ubuntu 24.04 or newer.

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

| Family                   | Package                                   | Architectures       | Smoke-test environments                                  |
| ------------------------ | ----------------------------------------- | ------------------- | -------------------------------------------------------- |
| Debian / Ubuntu          | `co-codes-cli_VERSION-1_ARCH.deb`         | `amd64`, `arm64`    | Debian 12, Ubuntu 24.04                                  |
| Fedora / RHEL-compatible | `co-codes-cli-VERSION-1.ARCH.rpm`         | `x86_64`, `aarch64` | Fedora 43, Rocky Linux 9                                 |
| Arch Linux               | `co-codes-cli-VERSION-1-ARCH.pkg.tar.zst` | `x86_64`, `aarch64` | Arch rolling on x86_64; ARM64 package for Arch Linux ARM |

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

## Windows x64

Windows installation is available from v0.8.0 through `co-x86_64-pc-windows-msvc.zip` and the selected package metadata. v0.7.0 contains no Windows assets. Public Scoop bucket, WinGet catalog and Chocolatey community entries are pending publication; the proposed identifiers below are not reserved. Release downloads and local manager installation do not require those entries.

Use PowerShell and [Git for Windows](https://gitforwindows.org/) for Git commands. Go releases need no MSVC runtime. `install.sh` is the Linux/macOS installer.

### Select and verify release downloads

Choose an actual tag from [the release page](https://github.com/codotcodes/co/releases) that lists the Windows assets. Run this setup once in PowerShell; it downloads each asset and its matching checksum into a retained directory:

```powershell
$ErrorActionPreference = 'Stop'
$Version = Read-Host 'Release tag containing Windows assets (vX.Y.Z)'
if ($Version -notmatch '^v\d+\.\d+\.\d+$') { throw 'Expected a stable release tag' }
$ReleaseUrl = "https://github.com/codotcodes/co/releases/download/$Version"
$DownloadDir = Join-Path $env:LOCALAPPDATA 'co-release-downloads'
New-Item -ItemType Directory -Force $DownloadDir | Out-Null

function Get-CoReleaseAsset([string]$Name) {
    $File = Join-Path $DownloadDir $Name
    Invoke-WebRequest "$ReleaseUrl/$Name" -OutFile $File
    Invoke-WebRequest "$ReleaseUrl/$Name.sha256" -OutFile "$File.sha256"
    $Checksum = (Get-Content "$File.sha256" -Raw).Trim() -split '\s+'
    if ($Checksum.Count -ne 2 -or $Checksum[0] -notmatch '^[0-9a-fA-F]{64}$' -or
        $Checksum[1] -ne $Name -or (Get-FileHash $File -Algorithm SHA256).Hash -ne $Checksum[0]) {
        throw "Checksum mismatch: $Name"
    }
    return $File
}
```

A missing download or checksum mismatch stops that installation attempt. Optionally verify build provenance with `gh attestation verify FILE --repo codotcodes/co`, replacing `FILE` with the downloaded asset. Repeat the setup with a new release tag before upgrading from local metadata.

### Direct ZIP installation

```powershell
$Archive = Get-CoReleaseAsset 'co-x86_64-pc-windows-msvc.zip'
$Stage = Join-Path $DownloadDir $Version
Expand-Archive $Archive -DestinationPath $Stage -Force
$Metadata = Get-Content "$Stage\co-release.json" -Raw | ConvertFrom-Json
if ($Metadata.version -ne $Version.TrimStart('v') -or
    $Metadata.target -ne 'x86_64-pc-windows-msvc' -or
    $Metadata.binarySha256 -ne (Get-FileHash "$Stage\co.exe" -Algorithm SHA256).Hash) {
    throw 'Archive metadata does not match the selected release'
}
$InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\co\bin'
New-Item -ItemType Directory -Force $InstallDir | Out-Null
Copy-Item "$Stage\co.exe" "$InstallDir\co.exe" -Force
$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($UserPath -split ';') -notcontains $InstallDir) {
    [Environment]::SetEnvironmentVariable('Path', "$InstallDir;$UserPath", 'User')
}
$env:Path = "$InstallDir;$env:Path"
Get-Command co -All
co version
```

The ZIP also contains licenses, this installation guide and `co-release.json`, which records the version, target, full source commit and binary SHA-256. For upgrades, stop running `co` processes and repeat the verified download/extraction/copy steps. The stable installation path keeps linked Git helpers usable. Uninstall with `Remove-Item "$InstallDir\co.exe"` and remove the directory's user `PATH` entry in Windows Environment Variables settings.

### Scoop release manifest

Requires an installed [Scoop](https://scoop.sh/). Run as your normal user:

```powershell
$Manifest = Get-CoReleaseAsset 'co-codes-cli.json'
scoop install $Manifest
co version
```

Keep this manifest at the same path. For an upgrade, download the next release's verified `co-codes-cli.json` over that file, then run `scoop update co-codes-cli` and `co version`. Uninstall with `scoop uninstall co-codes-cli`. Scoop provides the `co.exe` shim on `PATH` and validates the ZIP hash in the manifest. A maintained bucket entry can provide routine updates after it is published; the release manifest is the current source-defined route.

### WinGet local manifests

Requires a WinGet version supporting ZIP-wrapped portable packages and manifest schema 1.10.0, such as WinGet 1.11. Enable local manifests once from an elevated terminal with `winget settings --enable LocalManifestFiles`. Then use a normal user terminal:

```powershell
$Manifests = Get-CoReleaseAsset 'co-winget.zip'
$ManifestDir = Join-Path $DownloadDir 'winget'
Expand-Archive $Manifests -DestinationPath $ManifestDir -Force
winget validate --manifest $ManifestDir
winget install --manifest $ManifestDir --scope user --accept-package-agreements
co version
```

For an upgrade, extract the next release's verified `co-winget.zip` into the same directory, validate it and run `winget upgrade --manifest $ManifestDir --scope user --accept-package-agreements`. Keep those extracted manifests and uninstall with `winget uninstall --manifest $ManifestDir --scope user`. This selects the local portable package without requiring a catalog entry. WinGet creates the portable command link; open a new terminal if its `PATH` update is not visible. `CoCodes.Co` is a proposed identifier in the generated local manifests, not a claim that the public WinGet source contains the package.

### Chocolatey local package

Requires an installed [Chocolatey](https://chocolatey.org/). Download and verify the release's real `.nupkg`; run package install, upgrade and removal from an elevated PowerShell terminal:

```powershell
$PackageVersion = $Version.TrimStart('v')
$Package = Get-CoReleaseAsset "co-codes-cli.$PackageVersion.nupkg"
choco install co-codes-cli --version $PackageVersion --source $DownloadDir --yes
co version
```

For an upgrade, download the next release's verified `.nupkg` into that directory and run `choco upgrade co-codes-cli --version $PackageVersion --source $DownloadDir --yes` with the new version. Uninstall with `choco uninstall co-codes-cli --yes`. Chocolatey downloads the same checksum-pinned ZIP and creates its `co.exe` shim. Use `co login` from your normal user terminal. `co-codes-cli` is the proposed package ID; a community-feed install requires separate maintainer publication.

### Configuration and first use on Windows

Open a new terminal after manager installation, then run `Get-Command co -All`, `co version`, `co doctor`, `co login` and `co whoami`. Git operations require Git for Windows; `co link OWNER/REPO` records a path-scoped helper command rather than a credential. After changing installation methods or moving the executable, rerun `co link` in affected repositories.

Configuration defaults to `%APPDATA%\co\config.json`. A nonempty `CO_CONFIG_DIR` takes precedence over `XDG_CONFIG_HOME`, which takes precedence over that Windows default. The config directory, files and lock have protected current-user-only ACLs; the CLI rejects another identity's ownership and reparse points. To isolate a test session:

```powershell
$env:CO_CONFIG_DIR = "$env:LOCALAPPDATA\co-test"
co login
co logout
Remove-Item Env:CO_CONFIG_DIR
```

Package removal preserves configuration. Run `co logout` before uninstalling to revoke the human session. `CO_AGENT_ID` continues to select only an agent's repository-scoped grant, with credential failures stopping lookup instead of falling back to the human session.

## Build from source

From a checkout, use Go 1.26 or `nix develop`:

```sh
go build -mod=readonly -trimpath -o co ./cmd/co
./co version
```

On Windows, use `-o co.exe` and run `.\co.exe version`. Copy the binary into a directory on `PATH`. Rebuild and replace it to upgrade; remove it to uninstall. No C or Rust toolchain is required. Previously published Rust crates remain historical releases, not an upgrade path for the Go CLI.

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

Release `.tar.gz` archives remain available for all six Linux/macOS targets. Download the archive and its `.sha256`, verify using `sha256sum -c` on Linux or `shasum -a 256 -c` on macOS, extract, and install `co` in a directory on `PATH`. Replace that binary to upgrade; remove it to uninstall. Windows uses the ZIP route above when the selected release contains its assets.

```sh
co version
co doctor
co login
co whoami
```

Package removal does not remove your login configuration. Run `co logout` before uninstalling if you want to revoke the session. Configuration is under `${XDG_CONFIG_HOME:-$HOME/.config}/co` on Linux/macOS or `%APPDATA%\co` on Windows, unless `CO_CONFIG_DIR` or `XDG_CONFIG_HOME` overrides it.

## Release maintenance

The release tag must match `internal/cli/version.go`. The release workflow builds the six Linux/macOS archives plus the Windows x64 ZIP, packages both static Linux binaries with nFPM 2.47.0, emits per-file SHA-256 checksums and build attestations, and generates a version-pinned `co.rb` from verified archive checksums. Automated runtime and package-manager jobs gate release publication. Their definitions do not establish that a native run passed; retain the actual run URL, source OID and results for the release.

After publishing a release, download its `co.rb` and update `Formula/co.rb` on the default branch as the tap's version update. Review its four versioned URLs and checksums against that release. Homebrew users receive that version through `brew update` after the change is merged. To regenerate locally from downloaded archives and checksum files:

```sh
python3 scripts/homebrew-formula.py vX.Y.Z /path/to/release-assets > Formula/co.rb
```

Keep this formula at the latest published version, not an unreleased package version. The release also attaches the generated formula so a tap update never requires guessing checksums.

### Windows artifacts and catalogs

The Windows job builds on `windows-2025` with Go and cgo disabled. `scripts/windows-packages.py archive` executes the native binary's `version` command, checks the x64 PE header, and embeds `co-release.json`. `manifests` derives all managers' versioned URL and ZIP SHA-256 from the verified archive. The release attaches these files, each with a `.sha256`:

| Asset                           | Purpose                                                                             |
| ------------------------------- | ----------------------------------------------------------------------------------- |
| `co-x86_64-pc-windows-msvc.zip` | Native executable, licenses, README, install guide and exact source/binary identity |
| `co-codes-cli.json`             | Scoop release manifest                                                              |
| `co-winget.zip`                 | WinGet version, locale and ZIP/portable installer manifests                         |
| `co-codes-cli.X.Y.Z.nupkg`      | Chocolatey package built by `choco pack`                                            |
| `windows-packages.json`         | Full source OID, target, version, binary/ZIP hashes, package URL and fixture flag   |

The native jobs define standard-user credential/runtime tests and genuine manager install/upgrade/uninstall checks, including installed binary hashes, `PATH`, Git helpers after upgrades and no agent-to-human credential fallback. The upgrade baseline is fixture package version `0.0.1` around the same current binary, not a prior public Windows release. CI retains fixture metadata and logs as evidence; release downloads contain only production artifacts, and production `windows-packages.json` must report `fixture: false`.

To regenerate metadata from a published release's verified Windows ZIP and checksum, run this template at its corresponding source revision:

```sh
python3 scripts/windows-packages.py manifests vX.Y.Z /path/to/release-assets windows-metadata
```

Compare `windows-packages.json` with the full tagged source OID, archive checksum and binary checksum. Keep the versioned GitHub download URL and hash identical across the three managers. Use the actual release assets; v0.7.0 cannot supply a Windows ZIP or valid manifests for it.

After an authorized release, publish the verified Scoop manifest to a maintained bucket, submit the WinGet manifests under the approved identifier, and upload the checksum-verified Chocolatey package through the maintainer account. These steps require checking identifier ownership and acceptance; generating release assets does not register packages or create a feed. Until each publication is confirmed, document release downloads/local metadata rather than advertising an official catalog install.

Windows review CI accepts `review/windows-*` pushes, pull requests and workflow dispatch. Expect `Rust (windows-2025)` and `windows-packages`; record their actual results separately from release and catalog availability. Snap, Flatpak and additional hosted package repositories are not maintained at present.
