#!/usr/bin/env bash
# Cross compiles TideSync for every supported platform.
#
#   ./scripts/build-all.sh            # everything
#   TARGETS="linux/arm64" ./scripts/build-all.sh
#
# Output lands in dist/ together with SHA256SUMS and a manifest describing each
# binary. The build needs no network access: TideSync only uses the Go standard
# library, so GOFLAGS=-mod=mod and an empty module cache are enough.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

# linux/arm with GOARM=6 covers Raspberry Pi 1, Zero and other ARMv6 boards;
# GOARM=7 covers Pi 2 and newer.
DEFAULT_TARGETS="
linux/amd64
linux/arm64
linux/arm/7
linux/arm/6
linux/386
windows/amd64
windows/arm64
windows/386
"
TARGETS="${TARGETS:-$DEFAULT_TARGETS}"

LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$BUILD_DATE"
OUT_DIR="$ROOT_DIR/dist"
mkdir -p "$OUT_DIR"
# Drop artifacts from an earlier version so dist/ always matches one build.
rm -f "$OUT_DIR"/tidesync-*
: > "$OUT_DIR/SHA256SUMS"
: > "$OUT_DIR/MANIFEST.txt"

echo "TideSync $VERSION ($COMMIT) built $BUILD_DATE"
echo "toolchain: $(go version)"
echo

for target in $TARGETS; do
  IFS='/' read -r goos goarch goarm <<< "$target"
  name="tidesync-${VERSION}-${goos}-${goarch}"
  [[ -n "${goarm:-}" ]] && name="${name}v${goarm}"
  ext=""
  [[ "$goos" == "windows" ]] && ext=".exe"
  printf '  %-28s' "${goos}/${goarch}${goarm:+v$goarm}"
  env GOOS="$goos" GOARCH="$goarch" ${goarm:+GOARM="$goarm"} CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$OUT_DIR/${name}${ext}" . || {
      echo "FAILED"
      exit 1
    }
  size=$(stat -c %s "$OUT_DIR/${name}${ext}" 2>/dev/null || stat -f %z "$OUT_DIR/${name}${ext}")
  printf 'ok  %6s KiB  %s\n' "$((size / 1024))" "$(basename "${name}${ext}")"
  echo "  ${name}${ext}  $(file -b "$OUT_DIR/${name}${ext}" | cut -c1-90)" >> "$OUT_DIR/MANIFEST.txt"
done

# A host binary under the plain name, for convenience.
cp "$OUT_DIR"/tidesync-"$VERSION"-"$(go env GOOS)"-"$(go env GOARCH)" "$OUT_DIR/tidesync"

( cd "$OUT_DIR" && sha256sum tidesync-* > SHA256SUMS )
echo
echo "wrote $(grep -c . "$OUT_DIR/SHA256SUMS") binaries to $OUT_DIR"
cat "$OUT_DIR/SHA256SUMS"
