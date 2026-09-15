# Verification records

Raw output from the commands that were run against the released build
(`VERSION=1.0.0`). Everything here can be reproduced from a checkout with:

```bash
go test ./...                       # unit and integration tests
go test -race ./...                 # same, with the race detector (needs cgo)
./scripts/release.sh                # gofmt, vet on 6 targets, tests, cross build, binary inspection
./scripts/e2e-test.sh               # 44 checks: real agent + real client on one machine
./scripts/arm-emulation-test.sh     # 18 checks: ARM64 and ARMv7 agent + client under QEMU
```

## Files

| File | Command | Result |
| --- | --- | --- |
| [`release-verification.log`](release-verification.log) | `VERSION=1.0.0 ./scripts/release.sh` | gofmt clean, `go vet` ok on 6 targets, all tests pass, 8 binaries cross compiled and inspected |
| [`e2e-test.log`](e2e-test.log) | `./scripts/e2e-test.sh dist/tidesync-1.0.0-linux-amd64` | 44 checks passed, 0 failed |
| [`arm-emulation-test.log`](arm-emulation-test.log) | `QEMU_DIR=... ./scripts/arm-emulation-test.sh` | 18 checks passed, 0 failed (linux/arm64, linux/armv7) |
| [`race-test.log`](race-test.log) | `CGO_ENABLED=1 go test -race ./...` | all packages pass with the race detector enabled |

## What the end to end suite covers

- agent starts, answers `/healthz`, and publishes a manifest with digests
- token authentication, IP allow list, directory traversal (plain and URL encoded)
- first synchronisation produces a byte identical tree (unicode names, spaces, 2 MiB binary, empty directory)
- modification times are preserved; a second run downloads nothing
- server side changes (modify, add, delete) are picked up on the next run
- a corrupted or truncated destination file is repaired, then the run converges
- mirror mode deletes local-only and stale files, keeps empty directories and its own journal
- `--dry-run` writes nothing but reports the plan
- cold start uses the tar.gz fast path and produces the same tree
- a stopped agent fails with a non-zero exit code and a clear message, then recovers
- an abandoned lock file is reclaimed automatically
- daemon mode syncs on start, schedules the next run, picks up later changes and stops on SIGTERM
- the CLI surface: `version`, `init`, `check`, `--print-config`, and the missing-flag error message

## What the emulation suite covers

For `linux/arm64` (Raspberry Pi 3/4/5 on a 64 bit OS, ARM servers) and
`linux/armv7` (Pi 2/3/4 on a 32 bit OS):

- the binary starts and reports `platform linux/arm64` / `linux/arm`
- an ARM agent serves a tree over the network
- an ARM client synchronises it, including a 1 MiB binary and unicode file names
- exclude rules are honoured, a second run is a no-op, and a change propagates

QEMU user mode emulation is not the same as real hardware, but it executes the
actual ARM machine code, which is the part that differs between x86 and ARM.
The Go runtime is built from the same source with `CGO_ENABLED=0`, so there is
no C library or architecture specific code path involved.

## Not covered by automation

- Windows execution: the Windows binaries are cross compiled, `go vet` clean,
  and inspected with `file` (PE32+ x86-64 / Aarch64, PE32 i386); running them
  needs a Windows machine or Wine, which was not available here. The Windows
  specific code paths (Task Scheduler via `schtasks.exe`, service control via
  `advapi32.dll`) are present in the binary and every one of them has a console
  fallback, so a failure to attach to the service manager degrades to running in
  the foreground rather than failing silently.
- systemd on real hardware: unit file generation is verified
  (`~/.config/systemd/user/tidesync.service` is written correctly), but this
  container has no running systemd bus, so `systemctl enable --now` could not be
  exercised. The command reports what to run by hand in that case.
