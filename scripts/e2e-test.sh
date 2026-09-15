#!/usr/bin/env bash
# End to end verification of TideSync on one machine: a real agent, a real
# client, real files on disk. Every check prints PASS or FAIL and the script
# exits non-zero if anything failed.
#
#   ./scripts/e2e-test.sh [path-to-tidesync-binary]
#
# With no argument the script builds the binary from the repository root.
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${1:-}"
WORK="${TIDESYNC_E2E_DIR:-$ROOT_DIR/.work/e2e}"
PORT="${TIDESYNC_E2E_PORT:-18791}"
TOKEN="e2e-$$-token"
FAILED=0
PASSED=0

if [[ -z "$BIN" ]]; then
  BIN="$ROOT_DIR/dist/tidesync"
  if [[ ! -x "$BIN" ]]; then
    echo "building $BIN"
    ( cd "$ROOT_DIR" && go build -o "$BIN" . ) || { echo "build failed"; exit 1; }
  fi
fi
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

pass() { PASSED=$((PASSED+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { FAILED=$((FAILED+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; }
check() { if [[ "$1" == "0" ]]; then pass "$2"; else fail "$2"; fi; }
section() { printf '\n== %s ==\n' "$1"; }

cleanup() {
  [[ -n "${AGENT_PID:-}" ]] && kill "$AGENT_PID" 2>/dev/null
  [[ -n "${CLIENT_PID:-}" ]] && kill "$CLIENT_PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT

sync_once() { "$BIN" sync --server "http://127.0.0.1:$PORT" --remote "${REMOTE:-}" \
  --local "$DST" --token "$TOKEN" --once "$@" 2>&1; }

tree_diff() { # compare source and destination, ignoring agent-side extras
  diff -r -x .tidesync -x link.txt -x '*.tmp' "$SRC" "$DST" >/dev/null 2>&1
}

rm -rf "$WORK"; mkdir -p "$WORK/src/sub/deep" "$WORK/src/emptydir" "$WORK/dst"
SRC="$WORK/src"; DST="$WORK/dst"

section "fixtures"
printf 'hello world\n' > "$SRC/a.txt"
printf 'nested content\n' > "$SRC/sub/b.txt"
printf 'deep content\n' > "$SRC/sub/deep/c.txt"
printf 'unicode\n' > "$SRC/中文文件名.txt"
printf 'spaces\n' > "$SRC/file with spaces.txt"
: > "$SRC/empty.bin"
head -c 2097152 /dev/urandom > "$SRC/random.bin"
printf 'temporary\n' > "$SRC/scratch.tmp"
ln -s a.txt "$SRC/link.txt" 2>/dev/null
echo "  source tree:"
find "$SRC" -mindepth 1 | sed "s|$SRC|    src|" | sort

section "agent startup"
"$BIN" serve --root "$SRC" --listen "127.0.0.1:$PORT" --token "$TOKEN" \
  --exclude '*.tmp' --log-level debug --log-file "$WORK/agent.log" > "$WORK/agent.out" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1
check $? "agent answers /healthz"

section "security"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/api/v1/manifest?path=")
[[ "$code" == "401" ]]; check $? "manifest without a token is refused (HTTP $code)"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong" "http://127.0.0.1:$PORT/api/v1/manifest?path=")
[[ "$code" == "401" ]]; check $? "manifest with a wrong token is refused (HTTP $code)"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:$PORT/api/v1/file?path=../../../etc/passwd")
[[ "$code" == "400" || "$code" == "404" ]]; check $? "path traversal is refused (HTTP $code)"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:$PORT/api/v1/file?path=%2e%2e%2f%2e%2e%2fetc%2fpasswd")
[[ "$code" == "400" || "$code" == "404" ]]; check $? "url encoded traversal is refused (HTTP $code)"

section "first synchronisation"
out=$(sync_once)
echo "$out" | grep -q "sync finished"; check $? "run completed"
tree_diff; check $? "destination matches source"
grep -q "scratch.tmp" <(find "$DST" -name '*.tmp') ; [[ $? -ne 0 ]]; check $? "excluded file was not transferred"
[[ "$(cat "$DST/a.txt")" == "hello world" ]]; check $? "file content is correct"
[[ "$(cat "$DST/中文文件名.txt")" == "unicode" ]]; check $? "unicode file name transferred"
cmp -s "$SRC/random.bin" "$DST/random.bin"; check $? "binary file is byte identical"
src_mtime=$(stat -c %Y "$SRC/a.txt"); dst_mtime=$(stat -c %Y "$DST/a.txt")
[[ "$src_mtime" == "$dst_mtime" ]]; check $? "modification time preserved"
[[ -d "$DST/emptydir" ]]; check $? "empty directory recreated"

section "idempotence"
out=$(sync_once)
echo "$out" | grep -q "downloaded=0"; check $? "second run downloads nothing"
echo "$out" | grep -qE "up-to-date=[1-9]"; check $? "second run recognises up-to-date files"

section "incremental changes"
printf 'CHANGED CONTENT\n' > "$SRC/a.txt"
printf 'new file\n' > "$SRC/sub/new.txt"
rm -f "$SRC/sub/deep/c.txt"
out=$(sync_once)
echo "$out" | grep -q "downloaded=2"; check $? "changed and new files downloaded (downloaded=2)"
[[ "$(cat "$DST/a.txt")" == "CHANGED CONTENT" ]]; check $? "changed file content applied"
[[ "$(cat "$DST/sub/new.txt")" == "new file" ]]; check $? "new file transferred"
[[ -e "$DST/sub/deep/c.txt" ]]; check $? "deleted-on-server file kept while mirror mode is off"

section "repair and integrity"
printf 'corrupted!' > "$DST/sub/b.txt"
out=$(sync_once)
[[ "$(cat "$DST/sub/b.txt")" == "nested content" ]]; check $? "corrupted destination file repaired"
truncate -s 100 "$DST/random.bin"
out=$(sync_once)
cmp -s "$SRC/random.bin" "$DST/random.bin"; check $? "truncated destination file repaired"
out=$(sync_once)
echo "$out" | grep -q "downloaded=0"; check $? "repair converged (nothing left to do)"

section "mirror mode"
printf 'user data\n' > "$DST/local-only.txt"
out=$(sync_once --delete-extra)
echo "$out" | grep -q "deleted=2"; check $? "local-only and stale files deleted (deleted=2)"
[[ ! -e "$DST/local-only.txt" ]]; check $? "user file removed in mirror mode"
[[ ! -e "$DST/sub/deep/c.txt" ]]; check $? "server-side deletion mirrored"
tree_diff; check $? "trees identical after mirror run"
ls "$DST/.tidesync/"*.json >/dev/null 2>&1; check $? "sync journal survives mirror mode"

section "dry run"
printf 'dry\n' > "$SRC/dryrun.txt"
out=$(sync_once --dry-run)
[[ ! -e "$DST/dryrun.txt" ]]; check $? "dry run writes nothing"
echo "$out" | grep -q "dry-run: would download"; check $? "dry run reports planned work"
rm -f "$SRC/dryrun.txt"; sync_once >/dev/null

section "bulk archive transfer (cold destination)"
rm -rf "$DST"; mkdir -p "$DST"
out=$(sync_once --bulk)
echo "$out" | grep -q "bulk transfer complete"; check $? "tar.gz fast path used"
tree_diff; check $? "bulk transfer produced an identical tree"

section "resilience"
kill "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
out=$("$BIN" sync --server "http://127.0.0.1:$PORT" --local "$DST" --token "$TOKEN" --once 2>&1)
rc=$?
[[ $rc -ne 0 ]]; check $? "a stopped agent fails the run with a non-zero exit code"
echo "$out" | grep -qi "unreachable"; check $? "the failure names the unreachable agent"
"$BIN" serve --root "$SRC" --listen "127.0.0.1:$PORT" --token "$TOKEN" --exclude '*.tmp' >> "$WORK/agent.out" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 50); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

section "stale lock recovery"
mkdir -p "$WORK/locktest"
lock="$WORK/locktest/job.lock"
printf '999999 ghost\n' > "$lock"
touch -d '2 hours ago' "$lock" 2>/dev/null || touch -t "$(date -d '2 hours ago' +%Y%m%d%H%M)" "$lock"
out=$("$BIN" sync --server "http://127.0.0.1:$PORT" --local "$DST" --token "$TOKEN" \
  --once --state-file "$WORK/locktest/state.json" --log-level info 2>&1)
echo "$out" | grep -q "sync finished"; check $? "an abandoned lock is reclaimed automatically"

section "scheduled daemon mode"
printf 'daemon\n' > "$SRC/daemon.txt"
"$BIN" sync --server "http://127.0.0.1:$PORT" --local "$WORK/dst-daemon" --token "$TOKEN" \
  --interval 2s --log-level info > "$WORK/daemon.log" 2>&1 &
CLIENT_PID=$!
for _ in $(seq 1 60); do [[ -f "$WORK/dst-daemon/daemon.txt" ]] && break; sleep 0.2; done
[[ -f "$WORK/dst-daemon/daemon.txt" ]]; check $? "daemon performed its first run on start"
grep -q "next run scheduled" "$WORK/daemon.log"; check $? "daemon scheduled the next run"
printf 'daemon updated\n' > "$SRC/daemon.txt"
for _ in $(seq 1 60); do
  [[ "$(cat "$WORK/dst-daemon/daemon.txt" 2>/dev/null)" == "daemon updated" ]] && break
  sleep 0.25
done
[[ "$(cat "$WORK/dst-daemon/daemon.txt" 2>/dev/null)" == "daemon updated" ]]; check $? "daemon picked up a later change automatically"
kill -TERM "$CLIENT_PID" 2>/dev/null; sleep 0.5
if kill -0 "$CLIENT_PID" 2>/dev/null; then fail "daemon stopped on SIGTERM"; else pass "daemon stopped on SIGTERM"; fi
CLIENT_PID=""
rm -f "$SRC/daemon.txt"

section "cli surface"
"$BIN" version | grep -q "tidesync"; check $? "version command"
"$BIN" init client --server 192.168.1.10:8787 --local "$WORK/init-dst" > "$WORK/client.tpl" 2>&1
"$BIN" init client --server 192.168.1.10:8787 --local "$WORK/init-dst" --out "$WORK/client.json" >/dev/null 2>&1
out=$("$BIN" check -config "$WORK/client.json" 2>&1)
echo "$out" | grep -q "destination"; check $? "check validates a generated client config"
"$BIN" serve --root "$SRC" --token tok --print-config > "$WORK/server-print.json" 2>&1
grep -q '"root"' "$WORK/server-print.json"; check $? "serve --print-config emits JSON"
"$BIN" sync --server "http://127.0.0.1:$PORT" --local "$DST" --token "$TOKEN" --print-config > "$WORK/client-print.json" 2>&1
grep -q '"local"' "$WORK/client-print.json"; check $? "sync --print-config emits JSON"
out=$("$BIN" sync --once 2>&1); rc=$?
[[ $rc -ne 0 ]]; check $? "missing required flags exit non-zero"
echo "$out" | grep -qi "hint"; check $? "the error explains what is missing"

section "summary"
printf '  %d checks passed, %d failed\n' "$PASSED" "$FAILED"
[[ -s "$WORK/agent.log" ]]; check $? "agent wrote a log file"
if [[ $FAILED -eq 0 ]]; then
  printf '\n\033[32mALL E2E CHECKS PASSED\033[0m\n'
  exit 0
fi
printf '\n\033[31m%d E2E CHECK(S) FAILED\033[0m\n' "$FAILED"
exit 1
