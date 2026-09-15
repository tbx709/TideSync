#!/usr/bin/env bash
# Cross compiles TideSync for every supported platform, then verifies that each
# binary really is a static executable for its target architecture and that the
# same source passes the test suite.
#
#   ./scripts/release.sh
#
# Environment: GPROXY/GOCACHE are honoured; no network access is required.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "=== gofmt ==="
unformatted=$(gofmt -l main.go main_test.go cmd_*.go internal/ || true)
if [[ -n "$unformatted" ]]; then
  echo "these files need gofmt:"; echo "$unformatted"; exit 1
fi
echo "clean"

echo
echo "=== go vet (all targets) ==="
for target in linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64 windows/386; do
  IFS='/' read -r goos goarch <<< "$target"
  printf '  %-16s' "$goos/$goarch"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go vet ./... && echo ok
done

echo
echo "=== unit and integration tests ==="
go test ./...

echo
echo "=== cross compilation ==="
./scripts/build-all.sh

echo
echo "=== binary inspection ==="
failed=0
check_binary() {
  local file="$1" want="$2"
  if [[ ! -f "$file" ]]; then echo "  MISSING $file"; failed=1; return; fi
  local desc
  desc="$(file -b "$file")"
  if [[ "$desc" != *"$want"* ]]; then
    echo "  FAIL    $(basename "$file"): expected '$want', got '$desc'"; failed=1
  else
    echo "  ok      $(basename "$file"): $desc"
  fi
}
check_binary dist/tidesync-*-linux-amd64            "ELF 64-bit LSB executable, x86-64"
check_binary dist/tidesync-*-linux-arm64            "ELF 64-bit LSB executable, ARM aarch64"
check_binary dist/tidesync-*-linux-armv7            "ELF 32-bit LSB executable, ARM"
check_binary dist/tidesync-*-linux-armv6            "ELF 32-bit LSB executable, ARM"
check_binary dist/tidesync-*-windows-amd64.exe      "PE32+ executable (console) x86-64"
check_binary dist/tidesync-*-windows-arm64.exe      "PE32+ executable (console) Aarch64"
[[ $failed -eq 0 ]] || { echo "binary inspection failed"; exit 1; }

echo
echo "=== static linking (no runtime dependencies) ==="
if command -v ldd >/dev/null 2>&1; then
  ok=1
  for bin in dist/tidesync-*-linux-amd64 dist/tidesync-*-linux-arm64 dist/tidesync-*-linux-armv7; do
    out=$(ldd "$bin" 2>&1 || true)
    # "not a dynamic executable" (English) / "不是动态可执行文件" (localised)
    if echo "$out" | grep -qiE "not a dynamic executable|不是动态可执行文件|statically linked"; then
      echo "  ok      $(basename "$bin") is statically linked"
    else
      echo "  FAIL    $(basename "$bin") is dynamically linked: $out"; ok=0
    fi
  done
  [[ $ok -eq 1 ]] || exit 1
fi

echo
echo "release checks passed"
