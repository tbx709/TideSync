#!/usr/bin/env bash
# Runs the ARM builds of TideSync under QEMU user mode emulation: an ARM agent
# serves a tree, an ARM client synchronises it. This is the closest thing to a
# Raspberry Pi that can be done without the hardware.
#
#   QEMU_DIR=/path/to/qemu/bin ./scripts/arm-emulation-test.sh
#
# Get the emulator on Debian/Ubuntu without root:
#   curl -O http://mirrors.aliyun.com/ubuntu/pool/universe/q/qemu/qemu-user-static_8.2.2+ds-0ubuntu1_amd64.deb
#   dpkg-deb -x qemu-user-static_*.deb ./qemu && QEMU_DIR=./qemu/usr/bin ./scripts/arm-emulation-test.sh
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

TARGETS="${TARGETS:-linux/arm64 linux/arm/7}"
QEMU_DIR="${QEMU_DIR:-}"
WORK="$ROOT_DIR/.work/arm"
FAILED=0
PASSED=0

pass() { PASSED=$((PASSED+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { FAILED=$((FAILED+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; }
check() { if [[ "$1" == "0" ]]; then pass "$2"; else fail "$2"; fi; }

if [[ -z "$QEMU_DIR" ]]; then
  for candidate in "$ROOT_DIR/.work/qemu/root/usr/bin" /usr/bin; do
    [[ -x "$candidate/qemu-aarch64-static" || -x "$candidate/qemu-aarch64" ]] && QEMU_DIR="$candidate" && break
  done
fi
if [[ -z "$QEMU_DIR" ]]; then
  echo "qemu user emulation not found; set QEMU_DIR to the directory containing qemu-aarch64-static"
  exit 2
fi

qemu_for() { # $1 = GOARCH
  local arch="$1" name
  case "$arch" in
    arm64) name="aarch64" ;;
    arm)   name="arm" ;;
    386)   name="i386" ;;
    *)     name="$arch" ;;
  esac
  for suffix in "-static" ""; do
    if [[ -x "$QEMU_DIR/qemu-$name$suffix" ]]; then echo "$QEMU_DIR/qemu-$name$suffix"; return 0; fi
  done
  return 1
}

rm -rf "$WORK"; mkdir -p "$WORK"
run_case() { # $1 = target label, $2 = goarch, $3 = binary
  local label="$1" arch="$2" bin="$3"
  local qemu; qemu="$(qemu_for "$arch")" || { fail "$label: no emulator for $arch"; return; }
  local src="$WORK/src-$arch" dst="$WORK/dst-$arch" port
  case "$arch" in arm64) port=18801 ;; arm) port=18802 ;; *) port=18803 ;; esac
  mkdir -p "$src/sub" "$src/emptydir" "$dst"
  printf 'hello from %s\n' "$label" > "$src/a.txt"
  printf 'nested\n' > "$src/sub/b.txt"
  printf 'unicode\n' > "$src/中文.txt"
  head -c 1048576 /dev/urandom > "$src/random.bin"
  printf 'excluded\n' > "$src/skip.tmp"

  printf '\n== %s (%s) ==\n' "$label" "$qemu"
  "$qemu" "$bin" version | sed 's/^/  /'
  check $? "$label: version runs under emulation"

  "$qemu" "$bin" serve --root "$src" --listen "127.0.0.1:$port" --token armtoken --exclude '*.tmp' \
    > "$WORK/agent-$arch.log" 2>&1 &
  local agent=$!
  for _ in $(seq 1 60); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
  curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1
  check $? "$label: agent answers over the network"

  "$qemu" "$bin" sync --server "http://127.0.0.1:$port" --local "$dst" --token armtoken --once \
    > "$WORK/client-$arch.log" 2>&1
  check $? "$label: client completed a synchronisation"

  diff -r -x .tidesync -x '*.tmp' "$src" "$dst" >/dev/null 2>&1
  check $? "$label: destination identical to source"
  cmp -s "$src/random.bin" "$dst/random.bin"
  check $? "$label: 1 MiB binary transferred byte for byte"
  [[ "$(cat "$dst/中文.txt")" == "unicode" ]]
  check $? "$label: unicode file name survived emulation"
  [[ ! -e "$dst/skip.tmp" ]]
  check $? "$label: exclude rule honoured"

  # Incremental run: nothing left to do.
  "$qemu" "$bin" sync --server "http://127.0.0.1:$port" --local "$dst" --token armtoken --once 2>&1 \
    | grep -q "downloaded=0"
  check $? "$label: second run is a no-op"

  # A change must be picked up.
  printf 'changed\n' > "$src/a.txt"
  "$qemu" "$bin" sync --server "http://127.0.0.1:$port" --local "$dst" --token armtoken --once >/dev/null 2>&1
  [[ "$(cat "$dst/a.txt")" == "changed" ]]
  check $? "$label: change propagated on the next run"

  kill "$agent" 2>/dev/null; wait "$agent" 2>/dev/null
}

printf 'ARM emulation test\n'
for target in $TARGETS; do
  IFS='/' read -r goos goarch goarm <<< "$target"
  if [[ "$goos" != "linux" ]]; then
    fail "$target: only linux targets can be emulated"
    continue
  fi
  suffix=""
  [[ -n "${goarm:-}" ]] && suffix="v$goarm"
  bin="$(ls "$ROOT_DIR"/dist/tidesync-*-"$goos"-"$goarch$suffix" 2>/dev/null | head -1)"
  if [[ -z "$bin" ]]; then
    echo "missing binary for $target; run ./scripts/build-all.sh first"
    FAILED=$((FAILED+1))
    continue
  fi
  run_case "$goos/$goarch$suffix" "$goarch" "$bin"
done

printf '\n  %d checks passed, %d failed\n' "$PASSED" "$FAILED"
[[ $FAILED -eq 0 ]] && { printf '\n\033[32mARM EMULATION CHECKS PASSED\033[0m\n'; exit 0; }
printf '\n\033[31mARM EMULATION CHECKS FAILED\033[0m\n'
exit 1
