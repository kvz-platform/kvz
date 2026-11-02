#!/bin/sh
# Builds dist/kvz-<version>-darwin-arm64.tar.gz and its .sha256.
# Layout (matches what Homebrew installs):
#   bin/kvz
#   share/kvz/kernel/Image     the pod kernel (from $KVZ_KERNEL_IMAGE or ~/.kvz/kernel/Image)
#   share/kvz/kernel/config    its .config (kvz reads it to enable CPU hotplug features)
#   README.md, LICENSE
set -eu
cd "$(dirname "$0")/../.."
ver=${1:?usage: package.sh <version>}
kernel=${KVZ_KERNEL_IMAGE:-$HOME/.kvz/kernel/Image}
[ -f "$kernel" ] || { echo "pod kernel not found at $kernel (run kernel/build.sh)" >&2; exit 1; }

name=kvz-${ver}-darwin-arm64
stage=$(mktemp -d)/$name
mkdir -p "$stage/bin" "$stage/share/kvz/kernel" dist
cp bin/kvz "$stage/bin/kvz"
cp "$kernel" "$stage/share/kvz/kernel/Image"
kconfig="$(dirname "$kernel")/config"
grep -q '^CONFIG_HOTPLUG_CPU=y' "$kconfig" 2>/dev/null || { echo "kernel config missing or without CONFIG_HOTPLUG_CPU at $kconfig (rebuild with kernel/build.sh)" >&2; exit 1; }
cp "$kconfig" "$stage/share/kvz/kernel/config"
cp README.md "$stage/"
[ -f LICENSE ] && cp LICENSE "$stage/"
tar -C "$(dirname "$stage")" -czf "dist/$name.tar.gz" "$name"
(cd dist && shasum -a 256 "$name.tar.gz" > "$name.tar.gz.sha256")
cat "dist/$name.tar.gz.sha256"
