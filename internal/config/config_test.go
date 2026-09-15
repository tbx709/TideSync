package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStripJSONExtras(t *testing.T) {
	in := `{
  // a line comment
  "a": 1, /* inline */ "b": "text with // not a comment",
  "c": [1, 2, 3,],
  "d": "trailing comma in string, ok",
}`
	out := stripJSONExtras([]byte(in))
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("cleaned JSON does not parse: %v\n%s", err, out)
	}
	if v["b"] != "text with // not a comment" {
		t.Errorf("comment markers inside a string were damaged: %v", v["b"])
	}
	if v["d"] != "trailing comma in string, ok" {
		t.Errorf("string with a comma was damaged: %v", v["d"])
	}
	if len(v["c"].([]any)) != 3 {
		t.Errorf("array with a trailing comma was damaged: %v", v["c"])
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var v struct {
		A Duration `json:"a"`
		B Duration `json:"b"`
		C Duration `json:"c"`
	}
	raw := []byte(`{"a":"5m","b":90,"c":"1h30m"}`)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.A.Duration() != 5*time.Minute {
		t.Errorf("a = %v, want 5m", v.A)
	}
	if v.B.Duration() != 90*time.Second {
		t.Errorf("b = %v, want 90s", v.B)
	}
	if v.C.Duration() != 90*time.Minute {
		t.Errorf("c = %v, want 1h30m", v.C)
	}
	if v.A.Or(time.Second) != 5*time.Minute {
		t.Error("Or did not return the configured value")
	}
	if (Duration(0)).Or(time.Second) != time.Second {
		t.Error("Or did not apply the default")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("TIDESYNC_TEST_TOKEN", "s3cret")
	if got := expandEnv("${TIDESYNC_TEST_TOKEN}"); got != "s3cret" {
		t.Errorf("expandEnv = %q", got)
	}
	if got := expandEnv("${TIDESYNC_MISSING:-fallback}"); got != "fallback" {
		t.Errorf("default expansion = %q", got)
	}
	if got := expandEnv("plain$value"); got != "plain$value" {
		t.Errorf("a bare $ must be left alone, got %q", got)
	}
}

func TestLoadClientConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "client.json")
	content := `{
	  // a real-world commented config
	  "server": {"url": "192.168.1.10:8787", "remote_path": "share/data/", "token": "abc"},
	  "local": {"dir": "` + filepath.ToSlash(filepath.Join(dir, "dst")) + `", "delete_extra": true},
	  "sync": {"interval": "30s", "schedule": ["02:30", "14:00"], "concurrency": 2},
	  "verify": "size-mtime",
	  "state_file": "` + filepath.ToSlash(filepath.Join(dir, "state.json")) + `",
	  "lock_file": "` + filepath.ToSlash(filepath.Join(dir, "lock.lock")) + `"
	}`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClient(cfgPath)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.Server.URL != "http://192.168.1.10:8787" {
		t.Errorf("url = %q, want the http:// prefix added", cfg.Server.URL)
	}
	if cfg.Server.RemotePath != "share/data" {
		t.Errorf("remote_path = %q, want trailing slash trimmed", cfg.Server.RemotePath)
	}
	if !cfg.Local.DeleteExtra {
		t.Error("delete_extra was not applied")
	}
	if cfg.Sync.Interval.Duration() != 30*time.Second {
		t.Errorf("interval = %v", cfg.Sync.Interval)
	}
	if cfg.Verify != "size-mtime" {
		t.Errorf("verify = %q", cfg.Verify)
	}
	if got := cfg.ScheduleSeconds(); len(got) != 2 || got[0] != 2*3600+30*60 || got[1] != 14*3600 {
		t.Errorf("schedule seconds = %v", got)
	}
	if !filepath.IsAbs(cfg.Local.Dir) {
		t.Errorf("local.dir was not made absolute: %q", cfg.Local.Dir)
	}
}

func TestLoadClientConfigRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(cfgPath, []byte(`{"server":{"url":"http://x"},"local":{"dir":"`+filepath.ToSlash(dir)+`"},"typo_field":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClient(cfgPath); err == nil {
		t.Fatal("an unknown field must be reported, so typos do not silently disable options")
	}
}

func TestClientValidateDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := ClientDefaults()
	cfg.Server.URL = "http://127.0.0.1:9"
	cfg.Local.Dir = filepath.Join(dir, "dst")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Sync.Interval.Duration() != 5*time.Minute {
		t.Errorf("default interval = %v", cfg.Sync.Interval)
	}
	if cfg.Sync.Concurrency != 4 {
		t.Errorf("default concurrency = %d", cfg.Sync.Concurrency)
	}
	if cfg.StateFile == "" || cfg.LockFile == "" {
		t.Error("state/lock paths were not derived")
	}
	if cfg.Name == "" {
		t.Error("job name was not derived")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate must be idempotent: %v", err)
	}
}

func TestClientValidateErrors(t *testing.T) {
	cfg := ClientDefaults()
	if err := cfg.Validate(); err == nil {
		t.Error("missing server.url must be rejected")
	}
	cfg.Server.URL = "http://127.0.0.1:9"
	if err := cfg.Validate(); err == nil {
		t.Error("missing local.dir must be rejected")
	}
	cfg.Local.Dir = t.TempDir()
	cfg.Verify = "magic"
	if err := cfg.Validate(); err == nil {
		t.Error("an unknown verify mode must be rejected")
	}
	cfg.Verify = "sha256"
	cfg.Sync.Schedule = []string{"25:00"}
	if err := cfg.Validate(); err == nil {
		t.Error("an invalid schedule time must be rejected")
	}
}

func TestServerValidate(t *testing.T) {
	dir := t.TempDir()
	cfg := ServerDefaults()
	cfg.Root = dir
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.HashMode != "auto" || cfg.MaxConcurrent != 4 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	cfg.HashMode = "sometimes"
	if err := cfg.Validate(); err == nil {
		t.Error("an invalid hash_mode must be rejected")
	}
	cfg.HashMode = "auto"
	cfg.AllowCIDRs = []string{"not-a-cidr"}
	if err := cfg.Validate(); err == nil {
		t.Error("an invalid allow_cidrs entry must be rejected")
	}
	cfg.AllowCIDRs = []string{"192.168.1.0/24", "10.1.2.3"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid allow_cidrs rejected: %v", err)
	}
	cfg.TLSCert = "cert.pem"
	if err := cfg.Validate(); err == nil {
		t.Error("tls_cert without tls_key must be rejected")
	}
}

func TestStateLocationFallsBackWhenUnwritable(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	// A path below a regular file can never be created, which simulates a
	// read-only or missing state directory.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ClientDefaults()
	cfg.Server.URL = "http://127.0.0.1:9"
	cfg.Local.Dir = dst
	cfg.StateFile = filepath.Join(blocker, "state.json")
	cfg.LockFile = filepath.Join(blocker, "lock.lock")
	if err := cfg.Validate(); err == nil {
		t.Fatal("an explicitly configured but unwritable state_file must be rejected")
	}

	// The derived default, in contrast, is relocated into the destination.
	cfg2 := ClientDefaults()
	cfg2.Server.URL = "http://127.0.0.1:9"
	cfg2.Local.Dir = dst
	cfg2.StateFile = filepath.Join(blocker, "state.json")
	cfg2.LockFile = filepath.Join(blocker, "lock.lock")
	cfg2.ensureStateLocation()
	if len(cfg2.Notes) == 0 {
		t.Error("relocating the journal must be reported in Notes")
	}
	if filepath.Dir(cfg2.StateFile) != filepath.Join(dst, StateDirName) {
		t.Errorf("journal was not relocated: %s", cfg2.StateFile)
	}
	if filepath.Dir(cfg2.LockFile) != filepath.Join(dst, StateDirName) {
		t.Errorf("lock was not relocated: %s", cfg2.LockFile)
	}
}
