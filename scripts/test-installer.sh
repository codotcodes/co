#!/bin/sh
set -eu

root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t co-installer-test)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

cargo build --locked --manifest-path "$root/Cargo.toml"
mkdir -p "$tmp/release" "$tmp/stage" "$tmp/bin"
cp "$root/target/debug/co" "$tmp/stage/co"
target="$(rustc -vV | sed -n 's/^host: //p')"
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

test "$("$tmp/bin/co" version)" = "$("$root/target/debug/co" version)"
python3 "$root/scripts/test-distribution.py"
