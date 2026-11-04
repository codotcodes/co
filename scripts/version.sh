#!/bin/sh
set -eu

root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
version="$(sed -n 's/^var Version = "\([0-9][0-9.]*\)"$/\1/p' "$root/internal/cli/version.go")"
test -n "$version" || { printf 'missing CLI release version\n' >&2; exit 1; }
printf '%s\n' "$version"
