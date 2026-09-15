package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tidesync/internal/client"
	"tidesync/internal/config"
	"tidesync/internal/fsx"
)

// runCheck implements `tidesync check`: validate a configuration file and, for
// a client, verify that the agent is reachable and what the next run would do.
func runCheck(args []string) error {
	fs := newFlagSet("check", "Validate a configuration file and test the connection to the agent.")
	cfgPath := fs.String("config", "", "configuration file to inspect (server or client)")
	deep := fs.Bool("deep", true, "also fetch the manifest and report the transfer plan")
	asJSON := fs.Bool("json", false, "machine readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectPositional(fs, "check"); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}
	kind, err := detectConfigKind(*cfgPath)
	if err != nil {
		return err
	}

	results := []checkResult{}
	ok := func(name, detail string) {
		results = append(results, checkResult{Name: name, OK: true, Detail: detail})
	}
	fail := func(name, detail string) {
		results = append(results, checkResult{Name: name, OK: false, Detail: detail})
	}

	switch kind {
	case "server":
		cfg, err := config.LoadServer(*cfgPath)
		if err != nil {
			fail("configuration", err.Error())
			break
		}
		ok("configuration", fmt.Sprintf("agent config, root=%s listen=%s hash_mode=%s", cfg.Root, cfg.Listen, cfg.HashMode))
		if st, err := os.Stat(cfg.StateDir); err == nil && st.IsDir() {
			ok("state directory", cfg.StateDir)
		} else if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
			fail("state directory", fmt.Sprintf("%s is not writable: %v", cfg.StateDir, err))
		} else {
			ok("state directory", cfg.StateDir+" (created)")
		}
		if cfg.Token == "" {
			fail("authentication", "no token set: anyone on the network could read the published directory")
		} else {
			ok("authentication", fmt.Sprintf("token set (%d characters)", len(cfg.Token)))
		}
		if cfg.TLSCert == "" {
			ok("transport", "plain HTTP (fine on a trusted LAN; use tls_cert/tls_key for HTTPS)")
		} else {
			ok("transport", "HTTPS with "+cfg.TLSCert)
		}
	case "client":
		cfg, err := config.LoadClient(*cfgPath)
		if err != nil {
			fail("configuration", err.Error())
			break
		}
		ok("configuration", fmt.Sprintf("client config, %s -> %s (remote path %q)",
			cfg.Server.URL, cfg.Local.Dir, cfg.Server.RemotePath))
		if err := os.MkdirAll(cfg.Local.Dir, 0o755); err != nil {
			fail("destination", fmt.Sprintf("%s is not writable: %v", cfg.Local.Dir, err))
		} else {
			probe := filepath.Join(cfg.Local.Dir, ".tidesync-write-test")
			if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
				fail("destination", fmt.Sprintf("%s is not writable: %v", cfg.Local.Dir, err))
			} else {
				os.Remove(probe)
				ok("destination", cfg.Local.Dir+" is writable")
			}
		}
		if err := os.MkdirAll(filepath.Dir(cfg.StateFile), 0o755); err != nil {
			fail("state directory", err.Error())
		} else {
			ok("state directory", cfg.StateFile)
		}
		if !*deep {
			break
		}
		log, closeLog, err := newQuietLogger()
		if err != nil {
			return err
		}
		defer closeLog()
		syncer, err := client.New(cfg, log)
		if err != nil {
			fail("agent", err.Error())
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		syncer.SetDryRun(true)
		res, err := syncer.RunOnce(ctx)
		if err != nil {
			fail("agent", err.Error())
			break
		}
		ok("agent", fmt.Sprintf("reachable, %d file(s), %.1f MiB on the agent",
			res.RemoteFiles, float64(res.RemoteBytes)/(1<<20)))
		ok("plan", fmt.Sprintf("%d to download (%.1f MiB), %d already up to date, %d local-only, %d too large, %d skipped",
			res.Planned, float64(res.Bytes)/(1<<20), res.UpToDate, res.Extras, res.TooLarge, res.Skipped))
		if free, err := fsx.FreeSpace(cfg.Local.Dir); err == nil {
			need := res.Bytes
			if free < need {
				fail("free space", fmt.Sprintf("%.1f MiB free, %.1f MiB needed", float64(free)/(1<<20), float64(need)/(1<<20)))
			} else {
				ok("free space", fmt.Sprintf("%.1f MiB free, %.1f MiB needed",
					float64(free)/(1<<20), float64(need)/(1<<20)))
			}
		}
	default:
		return fmt.Errorf("cannot tell whether %s is a server or client configuration", *cfgPath)
	}

	if *asJSON {
		data, _ := json.MarshalIndent(struct {
			Config  string        `json:"config"`
			Kind    string        `json:"kind"`
			Checks  []checkResult `json:"checks"`
			Success bool          `json:"success"`
		}{*cfgPath, kind, results, allOK(results)}, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("TideSync check: %s (%s configuration)\n\n", *cfgPath, kind)
		for _, r := range results {
			mark := "OK  "
			if !r.OK {
				mark = "FAIL"
			}
			fmt.Printf("  [%s] %-16s %s\n", mark, r.Name, r.Detail)
		}
		fmt.Println()
		if allOK(results) {
			fmt.Println("all checks passed")
		} else {
			fmt.Println("some checks failed")
		}
	}
	if !allOK(results) {
		return fmt.Errorf("check failed")
	}
	return nil
}

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func allOK(results []checkResult) bool {
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

// detectConfigKind inspects the top level keys of a configuration file.
func detectConfigKind(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Comments are allowed in TideSync configs; fall back to a scan.
		text := string(raw)
		switch {
		case strings.Contains(text, "\"local\"") || strings.Contains(text, "\"server\""):
			return "client", nil
		case strings.Contains(text, "\"root\""):
			return "server", nil
		}
		return "", fmt.Errorf("parse config %s: %w", path, err)
	}
	if _, ok := probe["server"]; ok {
		return "client", nil
	}
	if _, ok := probe["local"]; ok {
		return "client", nil
	}
	if _, ok := probe["root"]; ok {
		return "server", nil
	}
	return "", fmt.Errorf("unrecognised configuration file")
}
