#!/bin/sh
set -eu

tag="${1:-${GITHUB_REF_NAME:-}}"
test -n "$tag" || { printf 'usage: %s vX.Y.Z\n' "$0" >&2; exit 2; }
case "$tag" in v*) version="${tag#v}" ;; *) printf 'release tag must start with v\n' >&2; exit 1 ;; esac

manifest_version="$(sed -n 's/^version = "\([^"]*\)"/\1/p' Cargo.toml | sed -n '1p')"
test "$version" = "$manifest_version" || {
  printf 'tag %s does not match Cargo.toml version %s\n' "$tag" "$manifest_version" >&2
  exit 1
}
