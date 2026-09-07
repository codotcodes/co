#!/bin/sh
set -eu

# Package an already built static-musl release binary; never rebuild per distro.
version="${1:?usage: package-linux.sh VERSION TARGET BINARY OUTPUT_DIR}"
target="${2:?target required}"
binary="${3:?binary required}"
output="${4:?output directory required}"
case "$target" in
  x86_64-unknown-linux-musl) arch=amd64 ;;
  aarch64-unknown-linux-musl) arch=arm64 ;;
  *) printf 'Linux packages require a supported static-musl target\n' >&2; exit 1 ;;
esac
printf '%s\n' "$version" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+$'
export CO_PACKAGE_VERSION="${version#v}" CO_PACKAGE_ARCH="$arch" CO_PACKAGE_BINARY="$binary"
mkdir -p "$output"
for format in deb rpm archlinux; do
  nfpm package --config packaging/nfpm.yaml --packager "$format" --target "$output/"
done
