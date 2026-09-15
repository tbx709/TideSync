// Command tidesync synchronises a directory from one machine on the local
// network to many others, on a schedule.
//
// It ships as a single static binary for Windows and Linux, x86-64, ARM64 and
// 32-bit ARM, so the same program runs on a PC, a server, a NAS or a Raspberry
// Pi.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"tidesync/internal/client"
	"tidesync/internal/server"
)

// Build metadata, overridden with -ldflags "-X main.version=...".
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// Exit codes: 0 success, 1 fatal error, 2 usage error, 3 finished with per-file
// failures (useful for monitoring).
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitPartial = 3
)

func main() {
	client.Version = version
	server.Version = version

	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "serve", "server", "agent":
		err = runServe(args)
	case "sync", "pull", "client", "run":
		err = runSync(args)
	case "check", "doctor":
		err = runCheck(args)
	case "service":
		err = runService(args)
	case "init":
		err = runInit(args)
	case "version", "-v", "--version":
		printVersion()
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "tidesync: unknown command %q\n\n", cmd)
		usage()
		os.Exit(exitUsage)
	}

	if err != nil {
		if code, ok := err.(exitCoder); ok {
			fmt.Fprintf(os.Stderr, "tidesync: %v\n", err)
			os.Exit(code.Code())
		}
		fmt.Fprintf(os.Stderr, "tidesync: %v\n", err)
		os.Exit(exitError)
	}
}

// exitCoder lets a command choose a specific exit status.
type exitCoder interface{ Code() int }

type partialError struct{ msg string }

func (e partialError) Error() string { return e.msg }
func (e partialError) Code() int     { return exitPartial }

func printVersion() {
	fmt.Printf("tidesync %s (commit %s, built %s)\n", version, commit, buildDate)
	fmt.Printf("platform %s/%s, %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func usage() {
	out := os.Stderr
	fmt.Fprintf(out, `TideSync %s - scheduled file synchronisation over the local network

Usage:
  tidesync <command> [flags]

Commands:
  serve     run the agent on the source machine and publish a directory
  sync      run the synchroniser on a target machine (daemon, or --once)
  check     validate a configuration file and test connectivity
  service   install/remove/query the background service (systemd, Task Scheduler)
  init      write example configuration files
  version   print build information
  help      show this text

Quick start (source machine):
  tidesync serve --root /srv/data --listen 0.0.0.0:8787 --token SECRET

Quick start (target machine):
  tidesync sync --server http://192.168.1.10:8787 --remote "" --local /srv/data \
                --token SECRET --interval 5m

Documentation: README.md
`, version)
}

// newFlagSet builds a flag set that prints the common usage banner.
func newFlagSet(name, hint string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: tidesync %s [flags]\n\n%s\n\nFlags:\n", name, hint)
		fs.PrintDefaults()
	}
	return fs
}

// multiFlag collects repeated string flags.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseAll parses flags even when they are interleaved with positional
// arguments. The standard flag package stops at the first non-flag argument,
// which would silently ignore "tidesync init client -out file.json".
func parseAll(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return positional, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}

// rejectPositional reports unexpected extra arguments instead of ignoring them.
func rejectPositional(fs *flag.FlagSet, command string) error {
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (run 'tidesync %s -h' for usage)", fs.Arg(0), command)
	}
	return nil
}

// printConfig renders a configuration as pretty JSON on stdout.
func printConfig(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
