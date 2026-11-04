#!/bin/sh
# Keep published archive target names stable while building without a C toolchain.
set -eu

root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
target="${1:-}"
output="${2:-}"
test -n "$output" || { printf 'usage: %s TARGET OUTPUT\n' "$0" >&2; exit 2; }
case "$target" in
  x86_64-unknown-linux-gnu|x86_64-unknown-linux-musl) GOOS=linux; GOARCH=amd64 ;;
  aarch64-unknown-linux-gnu|aarch64-unknown-linux-musl) GOOS=linux; GOARCH=arm64 ;;
  x86_64-apple-darwin) GOOS=darwin; GOARCH=amd64 ;;
  aarch64-apple-darwin) GOOS=darwin; GOARCH=arm64 ;;
  x86_64-pc-windows-msvc) GOOS=windows; GOARCH=amd64 ;;
  *) printf 'unsupported release target: %s\n' "$target" >&2; exit 2 ;;
esac
export GOOS GOARCH
export CGO_ENABLED=0
version="$(sh "$root/scripts/version.sh")"
cd "$root"
go build -mod=readonly -trimpath -ldflags "-s -w -X git.co.codes/co/co/internal/cli.Version=$version" -o "$output" ./cmd/co
