#!/bin/sh
# Builds the kvz pod-VM kernel inside a Linux container (Apple `container`),
# on the container's own ext4 rootfs (the kernel tree has case-conflicting
# paths that break on case-insensitive APFS). Output: $OUT/Image
set -eu
VER=${VER:-6.18.60}
OUT=${OUT:-$HOME/.kvz/kernel}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$OUT"
cp "$HERE/kvz.config" "$OUT/kvz.config"
container rm -f kvz-kbuild >/dev/null 2>&1 || true
container run --rm --name kvz-kbuild -c 16 -m 16G -v "$OUT:/out" docker.io/library/alpine:3.22 sh -c "
set -e
apk add -q build-base bc flex bison perl linux-headers elfutils-dev openssl-dev xz curl diffutils findutils
mkdir -p /build && cd /build
curl -sL https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$VER.tar.xz | tar xJ
cd linux-$VER
make -s ARCH=arm64 allnoconfig
./scripts/kconfig/merge_config.sh -m .config /out/kvz.config >/dev/null
make -s ARCH=arm64 olddefconfig
make -s ARCH=arm64 -j16 Image 2>&1 | tail -20
cp arch/arm64/boot/Image /out/Image
cp .config /out/config
ls -la /out/Image
"
