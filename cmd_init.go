package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"tidesync/internal/logx"
)

// runInit implements `tidesync init`: write commented example configurations.
func runInit(args []string) error {
	fs := newFlagSet("init", "Write an example configuration file.")
	out := fs.String("out", "", "write to this path instead of stdout")
	force := fs.Bool("force", false, "overwrite an existing file")
	server := fs.String("server", "192.168.1.10:8787", "agent address used in the client template")
	local := fs.String("local", "", "destination directory used in the client template")
	remote := fs.String("remote", "", "path below the agent root used in the client template")
	root := fs.String("root", "", "directory to publish used in the server template")
	token := fs.String("token", "", "shared secret written into the template")
	positional, err := parseAll(fs, args)
	if err != nil {
		return err
	}
	kind := "client"
	if len(positional) > 0 {
		kind = positional[0]
	}
	if len(positional) > 1 {
		return fmt.Errorf("unexpected extra argument %q", positional[1])
	}
	if *local == "" {
		*local = defaultLocalDirExample()
	}
	if *root == "" {
		*root = defaultRootExample()
	}

	var text string
	switch kind {
	case "client":
		text = clientTemplate(*server, *remote, *local, *token)
	case "server", "agent":
		kind = "server"
		text = serverTemplate(*root, *token)
	default:
		return fmt.Errorf("unknown template %q (want client or server)", kind)
	}

	if *out == "" || *out == "-" {
		fmt.Print(text)
		return nil
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", *out)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(text), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s configuration to %s\n", kind, *out)
	if kind == "client" {
		fmt.Printf("next: tidesync check -config %s && tidesync sync -config %s -once\n", *out, *out)
	} else {
		fmt.Printf("next: tidesync serve -config %s\n", *out)
	}
	return nil
}

// newQuietLogger returns a logger that only reports warnings, used by check.
func newQuietLogger() (*slog.Logger, func() error, error) {
	return logx.New(logx.Options{Level: "warn", Console: os.Stderr})
}

func defaultLocalDirExample() string {
	if isWindows() {
		return `D:\TideSyncData`
	}
	return "/srv/tidesync/data"
}

func defaultRootExample() string {
	if isWindows() {
		return `D:\Shared`
	}
	return "/srv/shared"
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}

// clientTemplate documents every commonly used option.
func clientTemplate(server, remote, local, token string) string {
	if token == "" {
		token = "${TIDESYNC_TOKEN}"
	}
	return fmt.Sprintf(`{
  // TideSync client: pull a published directory on a schedule.
  // Comments and trailing commas are allowed. ${VAR} is read from the
  // environment, so secrets do not have to be stored in this file.
  "name": "daily-data",

  "server": {
    "url": "http://%s",          // the agent, e.g. http://192.168.1.10:8787
    "token": "%s",
    "remote_path": "%s",                  // "" mirrors everything below server.root
    "timeout": "30s"
  },

  "local": {
    "dir": "%s",
    "delete_extra": false,                // true = mirror deletions from the agent
    "preserve_mtime": true,
    "preserve_perms": false,              // unix permission bits, ignored on Windows
    "create_dirs": true,
    "create_symlinks": false
  },

  "sync": {
    "interval": "5m",                     // daemon mode: wait between runs
    // "schedule": ["02:30", "14:30"],    // instead of interval: fixed daily times
    "jitter": "30s",                      // spread load across many clients
    "concurrency": 4,                     // parallel downloads
    "retries": 5,
    "retry_backoff": "2s",
    "run_on_start": true,
    "bulk_threshold_mb": 512,             // use the tar.gz fast path for cold starts
    "bulk_threshold_files": 2000,
    "max_file_size_mb": 0,                // 0 = no limit
    "bandwidth_limit_kbps": 0             // 0 = unlimited
  },

  "verify": "sha256",                     // sha256 | size-mtime | none
  "exclude": ["*.tmp", "*.part", ".git/**", "**/~$*"],

  "logging": {
    "level": "info",                      // debug | info | warn | error
    "file": "",                           // empty = console only
    "max_size_mb": 10,
    "max_files": 3
  }
}
`, server, token, remote, escapeForJSON(local))
}

// serverTemplate documents the agent side.
func serverTemplate(root, token string) string {
	if token == "" {
		token = "${TIDESYNC_TOKEN}"
	}
	return fmt.Sprintf(`{
  // TideSync agent: publish one directory, read only, to the local network.
  "listen": "0.0.0.0:8787",
  "root": "%s",
  "token": "%s",

  "allow_cidrs": ["192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"],
  // "tls_cert": "/etc/tidesync/agent.crt",
  // "tls_key":  "/etc/tidesync/agent.key",

  "hash_mode": "auto",                    // auto | always | never
  "hash_max_size_mb": 64,                 // auto: hash files up to this size
  "follow_symlinks": false,               // never publish outside root
  "exclude": ["*.tmp", ".git/**", "**/~$*", "**/.DS_Store"],
  "max_concurrent": 4,
  "rate_limit_kbps": 0,                   // per transfer, 0 = unlimited

  "logging": {
    "level": "info",
    "file": "",
    "max_size_mb": 10,
    "max_files": 3
  }
}
`, escapeForJSON(root), token)
}

func escapeForJSON(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\\' {
			out = append(out, '\\', '\\')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
