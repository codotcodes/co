#!/bin/sh
set -eu

# Disposable container transactions: install an older fixture, upgrade to the
# real release binary, verify version, then uninstall. No host packages change.
directory="${1:?usage: test-packages.sh RELEASE_DIRECTORY VERSION}"
version="${2:?version required}"
directory="$(CDPATH='' cd -- "$directory" && pwd)"
engine="${CONTAINER_ENGINE:-docker}"
case "$(uname -m)" in
  x86_64) target=x86_64-unknown-linux-musl ;;
  aarch64|arm64) target=aarch64-unknown-linux-musl ;;
  *) exit 1 ;;
esac
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
printf '#!/bin/sh\nprintf "co 0.0.0\\n"\n' > "$tmp/co"
chmod 755 "$tmp/co"
sh scripts/package-linux.sh 0.0.0 "$target" "$tmp/co" "$tmp/old"

for image in docker.io/library/debian:12 docker.io/library/ubuntu:24.04 docker.io/library/fedora:43 docker.io/library/rockylinux:9 docker.io/archlinux/archlinux:latest; do
  case "$image:$target" in *archlinux*:aarch64*) continue ;; esac
  printf 'Testing install/upgrade/uninstall in %s\n' "$image"
  # The inner shell expands its own positional version argument.
  # shellcheck disable=SC2016
  "$engine" run --rm -v "$directory:/packages:ro" -v "$tmp/old:/old:ro" "$image" sh -ec '
    if command -v apt-get >/dev/null; then
      apt-get update -qq
      apt-get install -y /old/*.deb
      test "$(co version)" = "co 0.0.0"
      apt-get install -y /packages/*.deb
      test "$(co version)" = "co $1"
      apt-get remove -y co-codes-cli
    elif command -v dnf >/dev/null; then
      dnf install -y /old/*.rpm
      test "$(co version)" = "co 0.0.0"
      dnf upgrade -y /packages/*.rpm
      test "$(co version)" = "co $1"
      dnf remove -y co-codes-cli
    else
      pacman -Syu --noconfirm git ca-certificates
      pacman -U --noconfirm /old/*.pkg.tar.zst
      test "$(co version)" = "co 0.0.0"
      pacman -U --noconfirm /packages/*.pkg.tar.zst
      test "$(co version)" = "co $1"
      pacman -R --noconfirm co-codes-cli
    fi
    ! command -v co
  ' sh "${version#v}"
done
