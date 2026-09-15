// Package config loads and validates TideSync agent and client configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tidesync/internal/proto"
)

// LogConfig configures the rotating logger shared by agent and client.
type LogConfig struct {
	Level     string `json:"level"`       // debug | info | warn | error
	File      string `json:"file"`        // empty = stderr only
	MaxSizeMB int    `json:"max_size_mb"` // rotate above this size (default 10)
	MaxFiles  int    `json:"max_files"`   // rotated files kept (default 3)
	JSON      bool   `json:"json"`        // machine readable lines
}

// ServerConfig configures `tidesync serve`, the agent that publishes a
// directory on the source machine.
type ServerConfig struct {
	// Listen is the TCP address to bind, e.g. "0.0.0.0:8787".
	Listen string `json:"listen"`
	// Root is the directory whose contents are published.
	Root string `json:"root"`
	// Token enables bearer authentication when non-empty.
	Token string `json:"token"`
	// AllowCIDRs restricts access to the listed networks, e.g. ["192.168.1.0/24"].
	AllowCIDRs []string `json:"allow_cidrs"`
	// TLSCert/TLSKey enable HTTPS when both are set.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
	// HashMode is auto | always | never. "auto" hashes files up to
	// HashMaxSizeMB and reports size+mtime for larger ones.
	HashMode      string `json:"hash_mode"`
	HashMaxSizeMB int    `json:"hash_max_size_mb"`
	// FollowSymlinks publishes the contents a symlink points at. Off by
	// default: a symlink pointing outside Root must never be published.
	FollowSymlinks bool `json:"follow_symlinks"`
	// Exclude holds .gitignore style patterns, relative to Root.
	Exclude []string `json:"exclude"`
	// StateDir stores the hash cache. Defaults to the per-user state dir.
	StateDir string `json:"state_dir"`
	// MaxConcurrent caps simultaneous transfers (default 4).
	MaxConcurrent int `json:"max_concurrent"`
	// RateLimitKBps throttles each transfer, 0 = unlimited.
	RateLimitKBps int `json:"rate_limit_kbps"`
	// ArchiveEnabled allows the bulk tar.gz endpoint (default true).
	ArchiveEnabled *bool `json:"archive_enabled"`
	// ManifestMaxEntries guards against pathological trees (default 200000).
	ManifestMaxEntries int `json:"manifest_max_entries"`
	// HashCache enables the on-disk digest cache (default true). Set it to false
	// to re-read every file on every scan, which is authoritative even when a
	// file is rewritten with an unchanged size and timestamp.
	HashCache *bool `json:"hash_cache"`
	// HashGrace is how long a file must be untouched before a cached digest is
	// trusted again. It closes the window in which a file rewritten within one
	// filesystem timestamp tick keeps its size and mtime (default 2s).
	HashGrace Duration `json:"hash_grace"`
	// ManifestTTL reuses a scan for this long. Zero (the default) means every
	// client request sees the current state of the disk; raise it only when the
	// tree is huge and many clients sync simultaneously.
	ManifestTTL Duration `json:"manifest_ttl"`
	// Log configures logging.
	Log LogConfig `json:"logging"`
}

// RemoteConfig describes the agent a client pulls from.
type RemoteConfig struct {
	URL        string   `json:"url"`         // http://host:port or https://host:port
	Token      string   `json:"token"`       // shared secret, may be ${ENV_VAR}
	RemotePath string   `json:"remote_path"` // sub path below the agent root
	Timeout    Duration `json:"timeout"`
	// CACert is a PEM bundle used to verify a self signed agent certificate.
	CACert string `json:"ca_cert"`
	// InsecureSkipVerify disables certificate verification (LAN only).
	InsecureSkipVerify bool `json:"insecure_skip_verify"`
}

// LocalConfig describes the destination directory.
type LocalConfig struct {
	Dir string `json:"dir"`
	// DeleteExtra mirrors deletions: local files that no longer exist on the
	// agent are removed. Off by default, because it deletes user data.
	DeleteExtra bool `json:"delete_extra"`
	// PreserveMTime copies the source modification time (default true).
	PreserveMTime *bool `json:"preserve_mtime"`
	// PreservePerms copies unix permission bits (default false).
	PreservePerms bool `json:"preserve_perms"`
	// CreateDirs creates empty directories that exist on the agent (default true).
	CreateDirs *bool `json:"create_dirs"`
	// CreateSymlinks recreates symbolic links published by the agent
	// (default false; on Windows this needs Developer Mode or admin rights).
	CreateSymlinks bool `json:"create_symlinks"`
}

// SyncConfig controls scheduling and transfer behaviour.
type SyncConfig struct {
	// Interval between runs in daemon mode (default 5m).
	Interval Duration `json:"interval"`
	// Jitter adds a random delay in [0,Jitter) to each run.
	Jitter Duration `json:"jitter"`
	// Schedule lists wall clock times ("02:30") for daily runs. When set, runs
	// happen at those times instead of every Interval.
	Schedule []string `json:"schedule"`
	// Concurrency is the number of parallel downloads (default 4).
	Concurrency int `json:"concurrency"`
	// Retries per file and per manifest request (default 5).
	Retries int `json:"retries"`
	// RetryBackoff is the first retry delay, doubled per attempt (default 2s).
	RetryBackoff Duration `json:"retry_backoff"`
	// RunOnStart syncs immediately when the daemon starts (default true).
	RunOnStart *bool `json:"run_on_start"`
	// FullScanEvery forces a hash comparison against every remote file.
	FullScanEvery Duration `json:"full_scan_every"`
	// BulkThresholdMB / BulkThresholdFiles trigger the tar.gz fast path when
	// the initial transfer is large (defaults 512 MB / 2000 files).
	BulkThresholdMB    int `json:"bulk_threshold_mb"`
	BulkThresholdFiles int `json:"bulk_threshold_files"`
	// MaxFileSizeMB skips larger files, 0 = unlimited.
	MaxFileSizeMB int `json:"max_file_size_mb"`
	// BandwidthLimitKBps throttles downloads, 0 = unlimited.
	BandwidthLimitKBps int `json:"bandwidth_limit_kbps"`
	// StopOnError aborts the run at the first failed file (default false).
	StopOnError bool `json:"stop_on_error"`
}

// ClientConfig is the whole client side configuration.
type ClientConfig struct {
	Server    RemoteConfig `json:"server"`
	Local     LocalConfig  `json:"local"`
	Sync      SyncConfig   `json:"sync"`
	Exclude   []string     `json:"exclude"`
	Verify    string       `json:"verify"` // sha256 | size-mtime | none
	StateFile string       `json:"state_file"`
	LockFile  string       `json:"lock_file"`
	Log       LogConfig    `json:"logging"`
	// Name is a friendly label used in log lines and the state file.
	Name string `json:"name"`
	// Notes collects non-fatal remarks made while validating (for example that
	// the journal had to be relocated). It is never read from the config file.
	Notes []string `json:"-"`
}

// DefaultStateDir returns the platform specific directory for TideSync state.
func DefaultStateDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "TideSync")
		}
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return filepath.Join(la, "TideSync")
		}
		return filepath.Join(os.TempDir(), "TideSync")
	}
	if os.Geteuid() == 0 {
		return "/var/lib/tidesync"
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "tidesync")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "tidesync")
	}
	return filepath.Join(home, ".local", "state", "tidesync")
}

// ServerDefaults returns a config populated with the documented defaults.
func ServerDefaults() ServerConfig {
	return ServerConfig{
		Listen:             "0.0.0.0:" + strconv.Itoa(8787),
		HashMode:           "auto",
		HashMaxSizeMB:      64,
		MaxConcurrent:      4,
		ManifestMaxEntries: 200000,
		Log:                LogConfig{Level: "info", MaxSizeMB: 10, MaxFiles: 3},
	}
}

// ClientDefaults returns a config populated with the documented defaults.
func ClientDefaults() ClientConfig {
	t := true
	f := false
	return ClientConfig{
		Server: RemoteConfig{Timeout: D(30 * time.Second)},
		Local:  LocalConfig{PreserveMTime: &t, CreateDirs: &t, DeleteExtra: f},
		Sync: SyncConfig{
			Interval:           D(5 * time.Minute),
			Concurrency:        4,
			Retries:            5,
			RetryBackoff:       D(2 * time.Second),
			RunOnStart:         &t,
			BulkThresholdMB:    512,
			BulkThresholdFiles: 2000,
		},
		Verify: "sha256",
		Log:    LogConfig{Level: "info", MaxSizeMB: 10, MaxFiles: 3},
	}
}

// LoadServer reads and validates an agent configuration file.
func LoadServer(path string) (ServerConfig, error) {
	cfg := ServerDefaults()
	if path != "" {
		if err := decodeFile(path, &cfg); err != nil {
			return cfg, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	cfg.Token = expandEnv(cfg.Token)
	return cfg, nil
}

// LoadClient reads and validates a client configuration file.
func LoadClient(path string) (ClientConfig, error) {
	cfg := ClientDefaults()
	if path != "" {
		if err := decodeFile(path, &cfg); err != nil {
			return cfg, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	cfg.Server.Token = expandEnv(cfg.Server.Token)
	return cfg, nil
}

// Validate checks the agent configuration and fills derived defaults.
func (c *ServerConfig) Validate() error {
	if strings.TrimSpace(c.Root) == "" {
		return fmt.Errorf("server.root is required (the directory to publish)")
	}
	abs, err := filepath.Abs(c.Root)
	if err != nil {
		return fmt.Errorf("server.root: %w", err)
	}
	c.Root = filepath.Clean(abs)
	if st, err := os.Stat(c.Root); err != nil {
		return fmt.Errorf("server.root %s: %w", c.Root, err)
	} else if !st.IsDir() {
		return fmt.Errorf("server.root %s is not a directory", c.Root)
	}
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = "0.0.0.0:" + strconv.Itoa(8787)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("server.listen %q: %w", c.Listen, err)
	}
	switch c.HashMode {
	case "":
		c.HashMode = "auto"
	case "auto", "always", "never":
	default:
		return fmt.Errorf("server.hash_mode must be auto, always or never (got %q)", c.HashMode)
	}
	if c.HashMaxSizeMB <= 0 {
		c.HashMaxSizeMB = 64
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.ManifestMaxEntries <= 0 {
		c.ManifestMaxEntries = 200000
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.MaxSizeMB <= 0 {
		c.Log.MaxSizeMB = 10
	}
	if c.Log.MaxFiles <= 0 {
		c.Log.MaxFiles = 3
	}
	for _, cidr := range c.AllowCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if ip := net.ParseIP(cidr); ip == nil {
				return fmt.Errorf("server.allow_cidrs: %q is neither an IP nor a CIDR", cidr)
			}
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("server.tls_cert and server.tls_key must be set together")
	}
	return nil
}

// Validate checks the client configuration. It does not touch the network and
// does not require the destination to exist yet.
func (c *ClientConfig) Validate() error {
	if strings.TrimSpace(c.Server.URL) == "" {
		return fmt.Errorf("server.url is required, e.g. \"http://192.168.1.10:8787\"")
	}
	u := strings.TrimSpace(c.Server.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	c.Server.URL = strings.TrimRight(u, "/")
	rp, err := proto.CleanRel(c.Server.RemotePath)
	if err != nil {
		return fmt.Errorf("server.remote_path: %w", err)
	}
	c.Server.RemotePath = rp
	if strings.TrimSpace(c.Local.Dir) == "" {
		return fmt.Errorf("local.dir is required (the destination directory)")
	}
	abs, err := filepath.Abs(c.Local.Dir)
	if err != nil {
		return fmt.Errorf("local.dir: %w", err)
	}
	c.Local.Dir = filepath.Clean(abs)
	if c.Server.Timeout <= 0 {
		c.Server.Timeout = D(30 * time.Second)
	}
	if c.Sync.Interval == 0 {
		c.Sync.Interval = D(5 * time.Minute)
	}
	if c.Sync.Interval < 0 {
		return fmt.Errorf("sync.interval must not be negative")
	}
	if c.Sync.Concurrency <= 0 {
		c.Sync.Concurrency = 4
	}
	if c.Sync.Concurrency > 64 {
		c.Sync.Concurrency = 64
	}
	if c.Sync.Retries < 0 {
		c.Sync.Retries = 0
	} else if c.Sync.Retries == 0 {
		c.Sync.Retries = 5
	}
	if c.Sync.RetryBackoff <= 0 {
		c.Sync.RetryBackoff = D(2 * time.Second)
	}
	if c.Sync.RunOnStart == nil {
		t := true
		c.Sync.RunOnStart = &t
	}
	if c.Local.PreserveMTime == nil {
		t := true
		c.Local.PreserveMTime = &t
	}
	if c.Local.CreateDirs == nil {
		t := true
		c.Local.CreateDirs = &t
	}
	if c.Sync.BulkThresholdMB <= 0 {
		c.Sync.BulkThresholdMB = 512
	}
	if c.Sync.BulkThresholdFiles <= 0 {
		c.Sync.BulkThresholdFiles = 2000
	}
	switch strings.ToLower(strings.TrimSpace(c.Verify)) {
	case "":
		c.Verify = "sha256"
	case "sha256", "size-mtime", "none":
		c.Verify = strings.ToLower(strings.TrimSpace(c.Verify))
	default:
		return fmt.Errorf("verify must be sha256, size-mtime or none (got %q)", c.Verify)
	}
	for _, t := range c.Sync.Schedule {
		if _, err := parseClock(t); err != nil {
			return fmt.Errorf("sync.schedule: %w", err)
		}
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.MaxSizeMB <= 0 {
		c.Log.MaxSizeMB = 10
	}
	if c.Log.MaxFiles <= 0 {
		c.Log.MaxFiles = 3
	}
	explicitState := c.StateFile != "" || c.LockFile != ""
	if !explicitState {
		dir := DefaultStateDir()
		slug := c.JobSlug()
		c.StateFile = filepath.Join(dir, "state-"+slug+".json")
		c.LockFile = filepath.Join(dir, "lock-"+slug+".lock")
		c.ensureStateLocation()
	} else {
		if c.StateFile == "" || c.LockFile == "" {
			dir := filepath.Dir(c.StateFile)
			if c.StateFile == "" {
				dir = filepath.Dir(c.LockFile)
			}
			slug := c.JobSlug()
			if c.StateFile == "" {
				c.StateFile = filepath.Join(dir, "state-"+slug+".json")
			}
			if c.LockFile == "" {
				c.LockFile = filepath.Join(dir, "lock-"+slug+".lock")
			}
		}
		if dir := filepath.Dir(c.StateFile); !writableDir(dir) {
			return fmt.Errorf("state_file: %s is not writable; point state_file/lock_file at a writable directory", dir)
		}
	}
	if c.Name == "" {
		c.Name = c.JobSlug()
	}
	return nil
}

// StateDirName is the directory created inside the destination when the
// per-user state directory is not writable.
const StateDirName = ".tidesync"

// ensureStateLocation verifies that the journal can be written and, when it
// cannot (a read-only system directory, a service account without a home, a
// container), falls back to a directory inside the destination. Any fallback is
// recorded in Notes so the caller can report it.
func (c *ClientConfig) ensureStateLocation() {
	dir := filepath.Dir(c.StateFile)
	if writableDir(dir) {
		return
	}
	fallback := filepath.Join(c.Local.Dir, StateDirName)
	if writableDir(fallback) {
		c.StateFile = filepath.Join(fallback, filepath.Base(c.StateFile))
		c.LockFile = filepath.Join(fallback, filepath.Base(c.LockFile))
		c.Notes = append(c.Notes, fmt.Sprintf("%s is not writable; the sync journal moved to %s", dir, fallback))
		return
	}
	c.Notes = append(c.Notes, fmt.Sprintf("warning: neither %s nor %s is writable; the journal will not survive a restart", dir, fallback))
}

// writableDir reports whether a directory exists (or can be created) and
// accepts a new file.
func writableDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".tidesync-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}

// JobSlug is a short stable identifier derived from the sync job identity, so
// that several jobs can share one state directory without collisions.
func (c *ClientConfig) JobSlug() string {
	id := c.Server.URL + "|" + c.Server.RemotePath + "|" + c.Local.Dir
	return shortHash(id)
}

// parseClock parses "HH:MM" or "HH:MM:SS" into seconds since midnight.
func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("%q is not a wall clock time like 02:30", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("%q has an invalid hour", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q has an invalid minute", s)
	}
	sec := 0
	if len(parts) == 3 {
		sec, err = strconv.Atoi(parts[2])
		if err != nil || sec < 0 || sec > 59 {
			return 0, fmt.Errorf("%q has an invalid second", s)
		}
	}
	return h*3600 + m*60 + sec, nil
}

// ScheduleSeconds returns the configured daily run times as seconds since
// midnight, sorted ascending.
func (c *ClientConfig) ScheduleSeconds() []int {
	out := make([]int, 0, len(c.Sync.Schedule))
	for _, t := range c.Sync.Schedule {
		if v, err := parseClock(t); err == nil {
			out = append(out, v)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
