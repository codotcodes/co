#!/bin/sh
set -eu

tag="${1:-${GITHUB_REF_NAME:-}}"
test -n "$tag" || { printf 'usage: %s vX.Y.Z\n' "$0" >&2; exit 2; }
case "$tag" in v*) version="${tag#v}" ;; *) printf 'release tag must start with v\n' >&2; exit 1 ;; esac

manifest_version="$(sh "$(dirname "$0")/version.sh")"
test "$version" = "$manifest_version" || {
  printf 'tag %s does not match internal/cli/version.go version %s\n' "$tag" "$manifest_version" >&2
  exit 1
}
