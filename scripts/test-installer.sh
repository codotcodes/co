#!/bin/sh
set -eu

root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t co-installer-test)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

mkdir -p "$tmp/release" "$tmp/stage" "$tmp/bin"
case "$(uname -s)" in
  Linux) os=unknown-linux-musl ;;
  Darwin) os=apple-darwin ;;
  *) printf 'unsupported installer test platform\n' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=x86_64 ;;
  aarch64|arm64) arch=aarch64 ;;
  *) printf 'unsupported installer test architecture\n' >&2; exit 1 ;;
esac
target="$arch-$os"
sh "$root/scripts/build.sh" "$target" "$tmp/stage/co"
asset="co-${target}.tar.gz"
tar -C "$tmp/stage" -czf "$tmp/release/$asset" co
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$tmp/release" && sha256sum "$asset" > "$asset.sha256")
else
  (cd "$tmp/release" && shasum -a 256 "$asset" > "$asset.sha256")
fi

CO_DOWNLOAD_BASE="file://$tmp/release" \
CO_INSTALL_DIR="$tmp/bin" \
CO_VERSION="v0.0.0" \
CO_LIBC="${target##*-}" \
PATH="$PATH:$tmp/bin" \
sh "$root/install.sh"

test "$("$tmp/bin/co" version)" = "co $(sh "$root/scripts/version.sh")"
python3 "$root/scripts/test-distribution.py"
