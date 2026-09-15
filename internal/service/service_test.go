package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuoteCommandLine(t *testing.T) {
	got := QuoteCommandLine([]string{`C:\Program Files\tidesync.exe`, "sync", "-config", `C:\ProgramData\TideSync\client config.json`})
	want := `"C:\Program Files\tidesync.exe" sync -config "C:\ProgramData\TideSync\client config.json"`
	if got != want {
		t.Errorf("QuoteCommandLine = %q, want %q", got, want)
	}
	got = QuoteCommandLine([]string{"/usr/local/bin/tidesync", "sync", "-config", "/etc/tidesync/client.json"})
	want = "/usr/local/bin/tidesync sync -config /etc/tidesync/client.json"
	if got != want {
		t.Errorf("QuoteCommandLine = %q, want %q", got, want)
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{Role: RoleClient}
	if o.NameOrDefault() != "tidesync" {
		t.Errorf("name = %q", o.NameOrDefault())
	}
	if o.IntervalOrDefault() != 5*time.Minute {
		t.Errorf("interval = %v", o.IntervalOrDefault())
	}
	if got := o.ExecArgs(); len(got) != 3 || got[0] != "sync" {
		t.Errorf("ExecArgs = %v", got)
	}
	if got := o.OneShotArgs(); len(got) != 4 || got[3] != "-once" {
		t.Errorf("OneShotArgs = %v", got)
	}
	o.Role = RoleServer
	if got := o.NameOrDefault(); got != "tidesync-agent" {
		t.Errorf("server name = %q", got)
	}
	if got := o.ExecArgs(); got[0] != "serve" {
		t.Errorf("server ExecArgs = %v", got)
	}
}

func TestValidateRequiresExistingFiles(t *testing.T) {
	dir := t.TempDir()
	o := Options{Role: RoleClient, Binary: filepath.Join(dir, "missing"), Config: filepath.Join(dir, "missing.json")}
	if err := o.Validate(); err == nil {
		t.Fatal("a missing configuration file must be rejected")
	}
	bin := filepath.Join(dir, "tidesync")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "client.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	o = Options{Role: RoleClient, Binary: bin, Config: cfg}
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !filepath.IsAbs(o.Binary) || !filepath.IsAbs(o.Config) {
		t.Error("paths must be absolute after validation")
	}
}

func TestHostDescriptionNonEmpty(t *testing.T) {
	if HostDescription() == "" {
		t.Error("HostDescription returned an empty string")
	}
}
