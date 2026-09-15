package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tidesync/internal/config"
	"tidesync/internal/proto"
	"tidesync/internal/server"
)

// newAgent starts a real agent over httptest so the client is exercised against
// the production HTTP surface, not a mock.
func newAgent(t *testing.T, root string, mutate func(*config.ServerConfig)) *httptest.Server {
	t.Helper()
	cfg := config.ServerDefaults()
	cfg.Root = root
	cfg.Listen = "127.0.0.1:0"
	cfg.StateDir = t.TempDir()
	cfg.Token = "test-token"
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("agent config: %v", err)
	}
	srv, err := server.New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newJob(t *testing.T, agentURL, dst string, mutate func(*config.ClientConfig)) *Syncer {
	t.Helper()
	cfg := config.ClientDefaults()
	cfg.Server.URL = agentURL
	cfg.Server.Token = "test-token"
	cfg.Local.Dir = dst
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.LockFile = filepath.Join(t.TempDir(), "lock.lock")
	cfg.Sync.RetryBackoff = config.D(time.Millisecond)
	cfg.Sync.Retries = 3
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("client config: %v", err)
	}
	s, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func run(t *testing.T, s *Syncer) *RunResult {
	t.Helper()
	res, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Errors > 0 {
		t.Fatalf("run reported %d failed file(s): %v", res.Errors, res.Failed)
	}
	return res
}

func TestInitialSyncThenNoOp(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "bravo")
	writeFile(t, filepath.Join(root, "unicode", "文件名.txt"), "charlie")
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, nil)

	res := run(t, s)
	if res.Downloaded != 3 {
		t.Fatalf("downloaded = %d, want 3", res.Downloaded)
	}
	if got := readFile(t, filepath.Join(dst, "sub", "b.txt")); got != "bravo" {
		t.Errorf("content = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "unicode", "文件名.txt")); got != "charlie" {
		t.Errorf("unicode content = %q", got)
	}
	if st, err := os.Stat(filepath.Join(dst, "emptydir")); err != nil || !st.IsDir() {
		t.Error("empty directory was not recreated")
	}

	// Modification times must match so the next run can skip cheaply.
	srcInfo, _ := os.Stat(filepath.Join(root, "a.txt"))
	dstInfo, _ := os.Stat(filepath.Join(dst, "a.txt"))
	if srcInfo.ModTime().Sub(dstInfo.ModTime()).Abs() > 2*time.Second {
		t.Errorf("mtime not preserved: src %v dst %v", srcInfo.ModTime(), dstInfo.ModTime())
	}

	second := run(t, s)
	if second.Downloaded != 0 {
		t.Errorf("second run downloaded %d file(s), want 0", second.Downloaded)
	}
	if second.UpToDate != 3 {
		t.Errorf("second run up-to-date = %d, want 3", second.UpToDate)
	}
}

func TestIncrementalChanges(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeFile(t, filepath.Join(root, "change.txt"), "before")
	writeFile(t, filepath.Join(root, "gone.txt"), "bye")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, nil)
	run(t, s)

	writeFile(t, filepath.Join(root, "change.txt"), "after")
	writeFile(t, filepath.Join(root, "new.txt"), "newcomer")
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	res := run(t, s)
	if res.Downloaded != 2 {
		t.Errorf("downloaded = %d, want 2 (change.txt and new.txt)", res.Downloaded)
	}
	if got := readFile(t, filepath.Join(dst, "change.txt")); got != "after" {
		t.Errorf("change.txt = %q, want after", got)
	}
	if got := readFile(t, filepath.Join(dst, "new.txt")); got != "newcomer" {
		t.Errorf("new.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "gone.txt")); err != nil {
		t.Error("gone.txt must remain while delete_extra is off")
	}
	if res.Extras != 1 {
		t.Errorf("extras = %d, want 1 (local-only files are always reported)", res.Extras)
	}
}

func TestMirrorModeDeletesAndPrunes(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeFile(t, filepath.Join(root, "sub", "old.txt"), "old")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) { c.Local.DeleteExtra = true })
	run(t, s)

	writeFile(t, filepath.Join(root, "sub", "fresh.txt"), "fresh")
	if err := os.Remove(filepath.Join(root, "sub", "old.txt")); err != nil {
		t.Fatal(err)
	}
	res := run(t, s)
	if res.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", res.Deleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "sub", "old.txt")); !os.IsNotExist(err) {
		t.Error("mirror mode did not delete the stale file")
	}
	if got := readFile(t, filepath.Join(dst, "sub", "fresh.txt")); got != "fresh" {
		t.Errorf("fresh.txt = %q", got)
	}
	// A file that only exists locally is user data and must go too in mirror
	// mode, but the journal directory must never be touched.
	writeFile(t, filepath.Join(dst, "local-only.txt"), "user data")
	run(t, s)
	if _, err := os.Stat(filepath.Join(dst, "local-only.txt")); !os.IsNotExist(err) {
		t.Error("mirror mode did not remove a local-only file")
	}
	if _, err := os.Stat(s.State().Path()); err != nil {
		t.Errorf("mirror mode removed the journal: %v", err)
	}
}

func TestCorruptedAndTruncatedLocalFilesAreRepaired(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "data.bin"), strings.Repeat("abcdefgh", 64))
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, nil)
	run(t, s)

	writeFile(t, filepath.Join(dst, "data.bin"), "corrupted")
	res := run(t, s)
	if res.Downloaded != 1 {
		t.Errorf("downloaded = %d, want 1", res.Downloaded)
	}
	if got := readFile(t, filepath.Join(dst, "data.bin")); got != strings.Repeat("abcdefgh", 64) {
		t.Error("the corrupted file was not restored")
	}
}

// TestSameSizeRecentChangeIsDetected covers the hardest realistic case: the
// content changed but the size did not, inside one filesystem timestamp tick.
// Without the agent's hash grace window the cached digest would be served and
// the change would be missed.
func TestSameSizeRecentChangeIsDetected(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	src := filepath.Join(root, "same.txt")
	writeFile(t, src, "aaaa")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, nil)
	run(t, s)

	// Rewrite with different content of the same length, immediately.
	writeFile(t, src, "bbbb")
	res := run(t, s)
	if res.Downloaded != 1 {
		t.Fatalf("downloaded = %d, want 1: a same-size rewrite was missed", res.Downloaded)
	}
	if got := readFile(t, filepath.Join(dst, "same.txt")); got != "bbbb" {
		t.Errorf("content = %q, want bbbb", got)
	}
}

// TestHashCacheDisabledIsAuthoritative covers the case no metadata based
// comparison can see: content rewritten with an unchanged size *and* an
// unchanged (backdated) timestamp. Turning the digest cache off makes every
// scan authoritative.
func TestHashCacheDisabledIsAuthoritative(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	src := filepath.Join(root, "same.txt")
	writeFile(t, src, "aaaa")
	fixed := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(src, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	no := false
	ts := newAgent(t, root, func(c *config.ServerConfig) { c.HashCache = &no })
	s := newJob(t, ts.URL, dst, nil)
	run(t, s)

	writeFile(t, src, "bbbb")
	if err := os.Chtimes(src, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	res := run(t, s)
	if res.Downloaded != 1 {
		t.Fatalf("downloaded = %d, want 1 with hash_cache disabled", res.Downloaded)
	}
	if got := readFile(t, filepath.Join(dst, "same.txt")); got != "bbbb" {
		t.Errorf("content = %q, want bbbb", got)
	}
}

func TestExcludesAreHonouredOnBothSides(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeFile(t, filepath.Join(root, "junk.tmp"), "junk")
	writeFile(t, filepath.Join(root, "cache", "x.dat"), "cached")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) {
		c.Exclude = []string{"*.tmp", "cache/"}
	})
	res := run(t, s)
	if res.Downloaded != 1 {
		t.Errorf("downloaded = %d, want 1", res.Downloaded)
	}
	if _, err := os.Stat(filepath.Join(dst, "junk.tmp")); !os.IsNotExist(err) {
		t.Error("an excluded file was downloaded")
	}
	if _, err := os.Stat(filepath.Join(dst, "cache")); !os.IsNotExist(err) {
		t.Error("an excluded directory was downloaded")
	}
}

func TestSubPathSync(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "top.txt"), "top")
	writeFile(t, filepath.Join(root, "share", "data", "a.txt"), "a")
	writeFile(t, filepath.Join(root, "share", "data", "nested", "b.txt"), "b")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) {
		c.Server.RemotePath = "share/data"
	})
	run(t, s)
	if got := readFile(t, filepath.Join(dst, "a.txt")); got != "a" {
		t.Errorf("a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "nested", "b.txt")); got != "b" {
		t.Errorf("nested/b.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "top.txt")); !os.IsNotExist(err) {
		t.Error("a file above the configured remote path was synchronised")
	}
}

func TestWrongTokenFails(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) { c.Server.Token = "nope" })
	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("an invalid token must fail the run")
	}
}

func TestUnreachableAgentFails(t *testing.T) {
	dst := t.TempDir()
	s := newJob(t, "http://127.0.0.1:1", dst, func(c *config.ClientConfig) {
		c.Sync.Retries = 1
		c.Server.Timeout = config.D(time.Second)
	})
	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("an unreachable agent must fail the run")
	}
}

// flakyHandler fails the first n requests for every file to exercise retries.
type flakyHandler struct {
	h     http.Handler
	mu    sync.Mutex
	count map[string]int
	fail  int
}

func (f *flakyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/file") {
		key := r.URL.Query().Get("path")
		f.mu.Lock()
		if f.count == nil {
			f.count = map[string]int{}
		}
		f.count[key]++
		n := f.count[key]
		f.mu.Unlock()
		if n <= f.fail {
			http.Error(w, "transient failure", http.StatusInternalServerError)
			return
		}
	}
	f.h.ServeHTTP(w, r)
}

func TestRetriesTransientFailures(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "b.txt"), "bravo")
	ts := newAgent(t, root, nil)
	flaky := &flakyHandler{h: ts.Config.Handler, fail: 2}
	ts.Config.Handler = flaky

	s := newJob(t, ts.URL, dst, nil)
	res := run(t, s)
	if res.Downloaded != 2 {
		t.Fatalf("downloaded = %d, want 2 after retries", res.Downloaded)
	}
	if got := readFile(t, filepath.Join(dst, "b.txt")); got != "bravo" {
		t.Errorf("content = %q", got)
	}
}

func TestPartialDownloadIsResumed(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	content := strings.Repeat("0123456789", 1000)
	writeFile(t, filepath.Join(root, "big.txt"), content)
	ts := newAgent(t, root, nil)

	cfg := config.ClientDefaults()
	cfg.Server.URL = ts.URL
	cfg.Server.Token = "test-token"
	cfg.Local.Dir = dst
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.LockFile = filepath.Join(t.TempDir(), "lock.lock")
	cfg.Sync.RetryBackoff = config.D(time.Millisecond)
	cfg.Sync.Retries = 2
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	remote, err := NewRemote(cfg, "test", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	tmp := filepath.Join(dst, ".tidesync-tmp-big.txt-test")
	if err := os.WriteFile(tmp, []byte(content[:4000]), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := remote.DownloadTo(context.Background(), "big.txt", tmp, hex.EncodeToString(sum[:]), 0)
	if err != nil {
		t.Fatalf("DownloadTo: %v", err)
	}
	if !res.Resumed {
		t.Error("a partial file was not resumed with a range request")
	}
	if got := readFile(t, tmp); got != content {
		t.Errorf("resumed content length = %d, want %d", len(got), len(content))
	}
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Error("the digest of a resumed transfer is wrong")
	}

	// A corrupt transfer must be rejected and the staging file removed.
	bad := filepath.Join(dst, ".tidesync-tmp-bad")
	os.Remove(bad)
	if _, err := remote.DownloadTo(context.Background(), "big.txt", bad, strings.Repeat("0", 64), 0); err == nil {
		t.Error("a checksum mismatch must fail the transfer")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("the staging file must be removed after a failed verification")
	}
}

func TestBulkArchiveTransfer(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	for i := 0; i < 20; i++ {
		writeFile(t, filepath.Join(root, "dir", string(rune('a'+i%26)), "file.txt"), strings.Repeat("x", 100+i))
	}
	writeFile(t, filepath.Join(root, "root.txt"), "root file")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) {
		c.Sync.BulkThresholdFiles = 1
		c.Sync.BulkThresholdMB = 1
	})
	res := run(t, s)
	if !res.Bulk {
		t.Fatal("the bulk archive path was not used for a cold destination")
	}
	if res.Downloaded != 21 {
		t.Errorf("downloaded = %d, want 21", res.Downloaded)
	}
	if got := readFile(t, filepath.Join(dst, "root.txt")); got != "root file" {
		t.Errorf("root.txt = %q", got)
	}
	// A second run must find everything up to date.
	second := run(t, s)
	if second.Downloaded != 0 || second.UpToDate != 21 {
		t.Errorf("second run: downloaded=%d up-to-date=%d, want 0/21", second.Downloaded, second.UpToDate)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) {
		c.Local.DeleteExtra = true
	})
	run(t, s)
	writeFile(t, filepath.Join(root, "b.txt"), "bravo")

	s.SetDryRun(true)
	res, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Downloaded != 0 {
		t.Error("dry run downloaded files")
	}
	if _, err := os.Stat(filepath.Join(dst, "b.txt")); !os.IsNotExist(err) {
		t.Error("dry run wrote a file")
	}
}

func TestStateJournalPersistsAndPrunes(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "b.txt"), "bravo")
	statePath := filepath.Join(t.TempDir(), "state.json")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) { c.StateFile = statePath })
	run(t, s)
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 2 {
		t.Fatalf("journal has %d rows, want 2", len(st.Entries))
	}
	if st.Runs != 1 || st.LastResult != "ok" {
		t.Errorf("journal run bookkeeping: runs=%d result=%s", st.Runs, st.LastResult)
	}
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	run(t, s)
	st2, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Entries) != 1 {
		t.Errorf("journal was not pruned: %v", st2.KnownPaths())
	}
}

func TestExcludedLocalFilesAreNotDeletedInMirrorMode(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) {
		c.Local.DeleteExtra = true
		c.Exclude = []string{"*.local"}
	})
	run(t, s)
	writeFile(t, filepath.Join(dst, "notes.local"), "user data")
	run(t, s)
	if _, err := os.Stat(filepath.Join(dst, "notes.local")); err != nil {
		t.Error("an excluded local file was deleted by mirror mode")
	}
}

func TestLockPreventsConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "job.lock")
	first, err := AcquireLock(lockPath, time.Minute, discardLogger())
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if _, err := AcquireLock(lockPath, time.Minute, discardLogger()); err == nil {
		t.Fatal("a second concurrent run must be refused")
	}
	first.Release()
	second, err := AcquireLock(lockPath, time.Minute, discardLogger())
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	second.Release()

	// A lock whose heartbeat stopped long ago is taken over.
	if err := os.WriteFile(lockPath, []byte("999999 ghost old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireLock(lockPath, 10*time.Minute, discardLogger())
	if err != nil {
		t.Fatalf("a stale lock must be reclaimed: %v", err)
	}
	third.Release()
}

func TestManifestTruncationIsReported(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(root, "f"+string(rune('0'+i))+".txt"), "x")
	}
	ts := newAgent(t, root, func(c *config.ServerConfig) { c.ManifestMaxEntries = 2 })
	s := newJob(t, ts.URL, dst, nil)
	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("a truncated manifest must abort the run rather than sync a partial tree")
	}
}

func TestAgentProtocolVersionMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"version":99,"app":"tidesync-agent","root":"/x","features":[]}`)
	}))
	defer ts.Close()
	s := newJob(t, ts.URL, t.TempDir(), nil)
	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("a protocol version mismatch must be reported")
	}
}

func TestHelpersSampleAndFirst(t *testing.T) {
	if got := firstOrEmpty(nil); got != "" {
		t.Errorf("firstOrEmpty(nil) = %q", got)
	}
	if got := firstOrEmpty([]string{"a"}); got != "a" {
		t.Errorf("firstOrEmpty = %q", got)
	}
	if got := sample([]string{"c", "a", "b"}, 2); len(got) != 2 || got[0] != "a" {
		t.Errorf("sample = %v, want the first two sorted", got)
	}
	if got := pathDir("a"); got != "" {
		t.Errorf("pathDir(a) = %q", got)
	}
	if got := pathDir("a/b/c"); got != "a/b" {
		t.Errorf("pathDir(a/b/c) = %q", got)
	}
}

var _ = proto.Version

func TestStaleStagingFilesAreCleanedUp(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	ts := newAgent(t, root, nil)
	s := newJob(t, ts.URL, dst, func(c *config.ClientConfig) {
		c.Local.DeleteExtra = true
	})
	run(t, s)

	// A transfer killed mid-flight leaves a staging file behind.
	oldTemp := filepath.Join(dst, ".tidesync-tmp-a.txt-123")
	writeFile(t, oldTemp, "half written")
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldTemp, past, past); err != nil {
		t.Fatal(err)
	}
	// A recent one may belong to another job and must survive.
	recentTemp := filepath.Join(dst, ".tidesync-tmp-a.txt-456")
	writeFile(t, recentTemp, "in flight")

	run(t, s)
	if _, err := os.Stat(oldTemp); !os.IsNotExist(err) {
		t.Error("a stale staging file was not cleaned up")
	}
	if _, err := os.Stat(recentTemp); err != nil {
		t.Error("a recent staging file must be left alone")
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Error("cleanup removed a real file")
	}
}
