package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tidesync/internal/config"
)

func TestNextRunIntervalAndJitter(t *testing.T) {
	cfg := config.ClientDefaults()
	cfg.Sync.Interval = config.D(10 * time.Minute)
	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	got := nextRun(cfg, now)
	if want := now.Add(10 * time.Minute); !got.Equal(want) {
		t.Errorf("nextRun = %v, want %v", got, want)
	}

	cfg.Sync.Jitter = config.D(30 * time.Second)
	for i := 0; i < 20; i++ {
		got := nextRun(cfg, now)
		if got.Before(now.Add(10*time.Minute)) || got.After(now.Add(10*time.Minute+30*time.Second)) {
			t.Fatalf("jittered nextRun out of range: %v", got)
		}
	}
}

func TestNextRunDailySchedule(t *testing.T) {
	cfg := config.ClientDefaults()
	cfg.Sync.Schedule = []string{"02:30", "14:00"}
	loc := time.UTC

	// Before the first slot: today at 02:30.
	now := time.Date(2024, 5, 1, 1, 0, 0, 0, loc)
	if got := nextRun(cfg, now); !got.Equal(time.Date(2024, 5, 1, 2, 30, 0, 0, loc)) {
		t.Errorf("nextRun = %v, want 2024-05-01 02:30", got)
	}
	// Between slots: today at 14:00.
	now = time.Date(2024, 5, 1, 3, 0, 0, 0, loc)
	if got := nextRun(cfg, now); !got.Equal(time.Date(2024, 5, 1, 14, 0, 0, 0, loc)) {
		t.Errorf("nextRun = %v, want 2024-05-01 14:00", got)
	}
	// After the last slot: tomorrow at 02:30.
	now = time.Date(2024, 5, 1, 23, 0, 0, 0, loc)
	if got := nextRun(cfg, now); !got.Equal(time.Date(2024, 5, 2, 2, 30, 0, 0, loc)) {
		t.Errorf("nextRun = %v, want 2024-05-02 02:30", got)
	}
}

func TestStaleLockAfterScalesWithInterval(t *testing.T) {
	cfg := config.ClientDefaults()
	cfg.Sync.Interval = config.D(time.Minute)
	if got := staleLockAfter(cfg); got != 30*time.Minute {
		t.Errorf("staleLockAfter = %v, want the 30m floor", got)
	}
	cfg.Sync.Interval = config.D(time.Hour)
	if got := staleLockAfter(cfg); got != 3*time.Hour {
		t.Errorf("staleLockAfter = %v, want 3h", got)
	}
}

func TestSplitComma(t *testing.T) {
	got := splitComma(" 02:30 , 14:00 ,, 23:59 ")
	want := []string{"02:30", "14:00", "23:59"}
	if len(got) != len(want) {
		t.Fatalf("splitComma = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitComma = %v, want %v", got, want)
		}
	}
}

func TestDetectConfigKind(t *testing.T) {
	dir := t.TempDir()
	client := dir + "/client.json"
	server := dir + "/server.json"
	writeFileForTest(t, client, `{"server":{"url":"http://x"},"local":{"dir":"/tmp/x"}}`)
	writeFileForTest(t, server, "{\n // agent\n \"root\": \"/srv/data\",\n \"listen\": \"0.0.0.0:8787\"\n}")
	if kind, err := detectConfigKind(client); err != nil || kind != "client" {
		t.Errorf("detectConfigKind(client) = %q, %v", kind, err)
	}
	if kind, err := detectConfigKind(server); err != nil || kind != "server" {
		t.Errorf("detectConfigKind(server) = %q, %v", kind, err)
	}
}

func TestClientTemplateIsValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/client.json"
	writeFileForTest(t, path, clientTemplate("192.168.1.10:8787", "share/data", filepathSlash(dir)+"/dst", "tok"))
	cfg, err := config.LoadClient(path)
	if err != nil {
		t.Fatalf("the generated client template does not parse: %v", err)
	}
	if cfg.Server.RemotePath != "share/data" {
		t.Errorf("remote_path = %q", cfg.Server.RemotePath)
	}
	if len(cfg.Sync.Schedule) != 0 {
		t.Errorf("the commented-out schedule must not be active: %v", cfg.Sync.Schedule)
	}
}

func TestServerTemplateIsValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/server.json"
	writeFileForTest(t, path, serverTemplate(dir, "tok"))
	cfg, err := config.LoadServer(path)
	if err != nil {
		t.Fatalf("the generated server template does not parse: %v", err)
	}
	if cfg.Root != dir {
		t.Errorf("root = %q, want %q", cfg.Root, dir)
	}
	if cfg.Token != "tok" {
		t.Errorf("token = %q", cfg.Token)
	}
}

func writeFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func filepathSlash(p string) string { return filepath.ToSlash(p) }

func TestParseAllAcceptsInterleavedFlags(t *testing.T) {
	fs := newFlagSet("init", "test")
	out := fs.String("out", "", "")
	server := fs.String("server", "", "")
	pos, err := parseAll(fs, []string{"client", "--server", "10.0.0.1:8787", "-out", "/tmp/x.json"})
	if err != nil {
		t.Fatalf("parseAll: %v", err)
	}
	if len(pos) != 1 || pos[0] != "client" {
		t.Errorf("positional = %v, want [client]", pos)
	}
	if *out != "/tmp/x.json" {
		t.Errorf("-out after a positional argument was ignored: %q", *out)
	}
	if *server != "10.0.0.1:8787" {
		t.Errorf("-server = %q", *server)
	}
}
