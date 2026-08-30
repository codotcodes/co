#!/bin/sh
set -eu

repository="${CO_REPOSITORY:-codotcodes/co}"
version="${CO_VERSION:-latest}"
install_dir="${CO_INSTALL_DIR:-${HOME}/.local/bin}"

fail() {
  printf 'co installer: %s\n' "$1" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

case "$(uname -s)" in
  Linux)
    os="unknown-linux"
    if command -v ldd >/dev/null 2>&1 && ldd --version 2>&1 | grep -qi musl; then
      libc="musl"
    elif ls /lib/ld-musl-*.so.1 >/dev/null 2>&1; then
      libc="musl"
    else
      libc="gnu"
    fi
    ;;
  Darwin)
    os="apple-darwin"
    libc=""
    ;;
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    fail "Windows is not supported yet; follow https://github.com/${repository}/issues for availability"
    ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch="x86_64" ;;
  arm64|aarch64) arch="aarch64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

if [ "$os" = "unknown-linux" ]; then
  target="${arch}-${os}-${libc}"
else
  target="${arch}-${os}"
fi
asset="co-${target}.tar.gz"

if [ -n "${CO_DOWNLOAD_BASE:-}" ]; then
  base="${CO_DOWNLOAD_BASE%/}"
  case "$base" in
    https://*) protocols="=https" ;;
    file://*) protocols="=file" ;;
    *) fail "CO_DOWNLOAD_BASE must use https:// or file://" ;;
  esac
elif [ "$version" = "latest" ]; then
  base="https://github.com/${repository}/releases/latest/download"
  protocols="=https"
else
  case "$version" in v*) ;; *) version="v${version}" ;; esac
  base="https://github.com/${repository}/releases/download/${version}"
  protocols="=https"
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t co-install)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

printf 'Downloading co for %s...\n' "$target"
curl -fLsS --retry 3 --proto "$protocols" --tlsv1.2 "${base}/${asset}" -o "${tmp}/${asset}"
curl -fLsS --retry 3 --proto "$protocols" --tlsv1.2 "${base}/${asset}.sha256" -o "${tmp}/${asset}.sha256"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$tmp" && sha256sum -c "${asset}.sha256") >/dev/null
elif command -v shasum >/dev/null 2>&1; then
  (cd "$tmp" && shasum -a 256 -c "${asset}.sha256") >/dev/null
else
  fail "sha256sum or shasum is required to verify the download"
fi

tar -xzf "${tmp}/${asset}" -C "$tmp"
test -f "${tmp}/co" || fail "release archive does not contain the co binary"
mkdir -p "$install_dir"
install -m 0755 "${tmp}/co" "${install_dir}/co"

printf 'Installed co to %s/co\n' "$install_dir"
case ":${PATH}:" in
  *:"${install_dir}":*) ;;
  *)
    printf 'Add %s to PATH, then run: co login\n' "$install_dir"
    exit 0
    ;;
esac
printf 'Run: co login\n'
