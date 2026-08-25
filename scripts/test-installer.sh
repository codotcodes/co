#!/bin/sh
set -eu

root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t co-installer-test)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

cargo build --locked --manifest-path "$root/Cargo.toml"
mkdir -p "$tmp/release" "$tmp/stage" "$tmp/bin"
cp "$root/target/debug/co" "$tmp/stage/co"
asset="co-x86_64-unknown-linux-gnu.tar.gz"
tar -C "$tmp/stage" -czf "$tmp/release/$asset" co
(cd "$tmp/release" && sha256sum "$asset" > "$asset.sha256")

CO_DOWNLOAD_BASE="file://$tmp/release" \
CO_INSTALL_DIR="$tmp/bin" \
CO_VERSION="v0.0.0" \
PATH="$PATH:$tmp/bin" \
sh "$root/install.sh"

test "$("$tmp/bin/co" version)" = "co 0.1.0"
