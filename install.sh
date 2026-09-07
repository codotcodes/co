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
command -v install >/dev/null 2>&1 || fail "install is required"

case "$(uname -s)" in
  Linux)
    os="unknown-linux"
    # Static musl releases also run on older glibc distributions.
    libc="${CO_LIBC:-musl}"
    case "$libc" in musl|gnu) ;; *) fail "CO_LIBC must be musl or gnu" ;; esac
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
  protocols="=https"
  release_url="$(curl -fLsS --retry 3 --proto '=https' --proto-redir '=https' --tlsv1.2 -o /dev/null -w '%{url_effective}' "https://github.com/${repository}/releases/latest")"
  version="${release_url##*/}"
  printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || fail "unable to resolve the latest release"
  base="https://github.com/${repository}/releases/download/${version}"
else
  case "$version" in v*) ;; *) version="v${version}" ;; esac
  base="https://github.com/${repository}/releases/download/${version}"
  protocols="=https"
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t co-install)"
staged=""
trap 'rm -rf "$tmp"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT HUP INT TERM

printf 'Downloading co for %s...\n' "$target"
curl -fLsS --retry 3 --proto "$protocols" --proto-redir "$protocols" --tlsv1.2 "${base}/${asset}" -o "${tmp}/${asset}"
curl -fLsS --retry 3 --proto "$protocols" --proto-redir "$protocols" --tlsv1.2 "${base}/${asset}.sha256" -o "${tmp}/${asset}.sha256"

expected="$(awk -v file="$asset" '$2 == file || $2 == "*" file { print $1 }' "${tmp}/${asset}.sha256")"
printf '%s\n' "$expected" | grep -Eq '^[a-fA-F0-9]{64}$' || fail "checksum file must identify the release archive"
printf '%s  %s\n' "$expected" "$asset" > "${tmp}/verified.sha256"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$tmp" && sha256sum -c verified.sha256) >/dev/null
elif command -v shasum >/dev/null 2>&1; then
  (cd "$tmp" && shasum -a 256 -c verified.sha256) >/dev/null
else
  fail "sha256sum or shasum is required to verify the download"
fi

member="$(tar -tzf "${tmp}/${asset}" | grep -E '^(\./)?co$')"
test -n "$member" || fail "release archive does not contain the co binary"
tar -xzf "${tmp}/${asset}" -C "$tmp" "$member"
test -f "${tmp}/co" || fail "release archive does not contain the co binary"
test ! -L "${tmp}/co" || fail "release binary must not be a symlink"
mkdir -p "$install_dir"
staged="$(mktemp "${install_dir}/.co.XXXXXX")"
install -m 0755 "${tmp}/co" "$staged"
mv -f "$staged" "${install_dir}/co"
staged=""

printf 'Installed co to %s/co\n' "$install_dir"
case ":${PATH}:" in
  *:"${install_dir}":*) ;;
  *)
    printf 'Add %s to PATH, then run: co login\n' "$install_dir"
    exit 0
    ;;
esac
printf 'Run: co login\n'
