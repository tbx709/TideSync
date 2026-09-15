package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"tidesync/internal/config"
	"tidesync/internal/fsx"
	"tidesync/internal/proto"
)

// Version is stamped by main so that the agent sees which client build is
// talking to it.
var Version = "dev"

// Syncer performs one pull job.
type Syncer struct {
	cfg     config.ClientConfig
	log     *slog.Logger
	remote  *Remote
	state   *State
	matcher *proto.Matcher

	verify         string
	createDirs     bool
	createSymlinks bool
	preserveMTime  bool
	preservePerms  bool

	dryRun    bool
	forceBulk bool
	fullScan  bool

	features map[string]bool
}

// New builds a Syncer for one job. The state journal is loaded lazily by
// RunOnce.
func New(cfg config.ClientConfig, log *slog.Logger) (*Syncer, error) {
	if log == nil {
		log = slog.Default()
	}
	remote, err := NewRemote(cfg, Version, log)
	if err != nil {
		return nil, err
	}
	state, err := LoadState(cfg.StateFile)
	if err != nil {
		log.Warn("state journal problem", "error", err.Error(), "path", cfg.StateFile)
	}
	s := &Syncer{
		cfg:            cfg,
		log:            log,
		remote:         remote,
		state:          state,
		matcher:        proto.NewMatcher(cfg.Exclude),
		verify:         cfg.Verify,
		createDirs:     cfg.Local.CreateDirs == nil || *cfg.Local.CreateDirs,
		createSymlinks: cfg.Local.CreateSymlinks,
		preserveMTime:  cfg.Local.PreserveMTime == nil || *cfg.Local.PreserveMTime,
		preservePerms:  cfg.Local.PreservePerms,
		features:       map[string]bool{},
	}
	return s, nil
}

// State exposes the loaded journal (diagnostics and tests).
func (s *Syncer) State() *State { return s.state }

// SetDryRun makes a run report what it would do without touching the disk.
func (s *Syncer) SetDryRun(v bool) { s.dryRun = v }

// SetForceBulk forces the tar.gz fast path for the next run.
func (s *Syncer) SetForceBulk(v bool) { s.forceBulk = v }

// RunResult summarises one synchronisation.
type RunResult struct {
	Job         string
	Started     time.Time
	Finished    time.Time
	RemoteFiles int
	RemoteBytes int64
	Planned     int
	UpToDate    int
	Downloaded  int
	Bytes       int64
	Deleted     int
	Extras      int
	TooLarge    int
	Skipped     int
	Errors      int
	Failed      []string
	Bulk        bool
	DryRun      bool
}

// Changed reports whether the destination was modified.
func (r *RunResult) Changed() bool {
	return r.Downloaded > 0 || r.Deleted > 0
}

// Summary renders a one line human readable summary.
func (r *RunResult) Summary() string {
	return fmt.Sprintf("files=%d downloaded=%d (%.1f MiB) up-to-date=%d deleted=%d extras=%d errors=%d took=%s",
		r.RemoteFiles, r.Downloaded, float64(r.Bytes)/(1<<20), r.UpToDate, r.Deleted, r.Extras, r.Errors,
		r.Finished.Sub(r.Started).Round(time.Millisecond))
}

// RunOnce performs a single synchronisation.
//
// A returned error means the run could not proceed at all (unreachable agent,
// unusable manifest). Individual file failures are reported in the result and
// retried on the next run.
func (s *Syncer) RunOnce(ctx context.Context) (*RunResult, error) {
	res := &RunResult{Job: s.cfg.Name, Started: time.Now(), DryRun: s.dryRun}
	if err := os.MkdirAll(s.cfg.Local.Dir, 0o755); err != nil {
		return res, fmt.Errorf("create local dir %s: %w", s.cfg.Local.Dir, err)
	}

	info, err := s.remote.Info(ctx)
	if err != nil {
		return res, fmt.Errorf("agent %s is unreachable: %w", s.remote.URL(), err)
	}
	s.features = map[string]bool{}
	for _, f := range info.Features {
		s.features[f] = true
	}
	s.log.Info("agent reachable",
		"url", s.remote.URL(),
		"agent_version", info.AgentVersion,
		"os", info.OS, "arch", info.Arch,
		"root", info.Root, "remote_path", s.remote.Sub(),
		"local_dir", s.cfg.Local.Dir,
	)

	// Decide how much integrity checking to ask for.
	var hashParam *bool
	if s.verify == "sha256" {
		t := true
		hashParam = &t
	}
	man, err := s.remote.Manifest(ctx, hashParam)
	if err != nil {
		return res, fmt.Errorf("fetch manifest: %w", err)
	}
	if man.Truncated {
		return res, fmt.Errorf("agent truncated the manifest at %d entries; narrow server.root or raise server.manifest_max_entries", len(man.Entries))
	}
	res.RemoteFiles = man.FileCount()
	res.RemoteBytes = man.TotalSize()

	if s.cfg.Sync.FullScanEvery > 0 {
		every := s.cfg.Sync.FullScanEvery.Duration()
		if s.state.FullScanAt.IsZero() || time.Since(s.state.FullScanAt) >= every {
			s.fullScan = true
		}
	}

	pl, err := s.buildPlan(man)
	if err != nil {
		return res, err
	}
	res.Planned = len(pl.downloads)
	res.UpToDate = pl.upToDate
	res.TooLarge = pl.tooLarge
	res.Skipped = pl.skipped
	s.log.Info("plan ready",
		"remote_files", res.RemoteFiles,
		"to_download", len(pl.downloads),
		"to_download_bytes", pl.bytes,
		"up_to_date", pl.upToDate,
		"to_delete", len(pl.extras),
		"skipped", pl.skipped,
		"dry_run", s.dryRun,
	)

	if s.dryRun {
		for _, e := range pl.downloads {
			s.log.Info("dry-run: would download", "path", e.Path, "size", e.Size)
		}
		for _, p := range pl.extras {
			s.log.Info("dry-run: would delete", "path", p)
		}
		res.Finished = time.Now()
		return res, nil
	}

	// Directories first so that an interrupted run still leaves the shape of
	// the tree behind.
	for _, d := range pl.dirs {
		full, err := proto.Resolve(s.cfg.Local.Dir, d.Path)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(full, 0o755); err != nil {
			s.log.Warn("cannot create directory", "path", d.Path, "error", err.Error())
			continue
		}
		if s.preserveMTime {
			_ = fsx.Chtimes(full, d.ModTime)
		}
	}

	// Realign modification times of files whose content already matches, so
	// that the next run recognises them without re-reading either side.
	for _, e := range pl.adopt {
		if !s.preserveMTime {
			break
		}
		local, err := proto.Resolve(s.cfg.Local.Dir, e.Path)
		if err != nil {
			continue
		}
		if err := fsx.Chtimes(local, e.ModTime); err == nil {
			s.state.Set(e.Path, StateEntry{Size: e.Size, MTime: e.ModTime.UnixNano(), SHA256: e.SHA256})
		}
	}

	for _, l := range pl.symlinks {
		if err := s.makeSymlink(l); err != nil {
			s.log.Warn("cannot create symlink", "path", l.Path, "error", err.Error())
			res.Errors++
			res.Failed = append(res.Failed, l.Path)
		}
	}

	// Bulk transfer for a cold destination, then per-file for the remainder.
	pending := pl.downloads
	if s.shouldBulk(man, pl) {
		done, bytes := s.bulkTransfer(ctx, man, pl, res)
		res.Bulk = true
		res.Bytes += bytes
		if len(done) > 0 {
			rest := make([]proto.Entry, 0, len(pending))
			for _, e := range pending {
				if _, ok := done[e.Path]; ok {
					res.Downloaded++
					continue
				}
				rest = append(rest, e)
			}
			pending = rest
			s.log.Info("bulk transfer complete", "via_archive", len(done), "remaining", len(pending), "bytes", bytes)
		}
	}

	s.downloadAll(ctx, pending, res)

	if s.cfg.Local.DeleteExtra {
		deleted, err := s.removeExtras(pl.extras, pl.remoteDirs)
		if err != nil {
			s.log.Warn("mirror delete failed", "error", err.Error())
		}
		res.Deleted = deleted
	} else {
		res.Extras = len(pl.extras)
		if len(pl.extras) > 0 {
			s.log.Info("local-only files kept (set local.delete_extra to mirror deletions)",
				"count", len(pl.extras), "examples", sample(pl.extras, 5))
		}
	}

	s.state.Prune(pl.remoteSet)
	if s.fullScan {
		s.state.FullScanAt = time.Now()
	}
	s.state.Runs++
	s.state.LastRun = time.Now()
	res.Finished = time.Now()
	s.state.LastResult = "ok"
	if res.Errors > 0 {
		s.state.LastResult = "partial"
		s.state.LastError = fmt.Sprintf("%d file(s) failed, first: %s", res.Errors, firstOrEmpty(res.Failed))
	} else {
		s.state.LastError = ""
	}
	s.state.Job = s.cfg.Name
	s.state.RemoteURL = s.remote.URL()
	s.state.RemotePath = s.remote.Sub()
	s.state.LocalDir = s.cfg.Local.Dir
	if err := s.state.Save(false); err != nil {
		s.log.Warn("cannot write state journal", "path", s.state.Path(), "error", err.Error())
	}

	lvl := slog.LevelInfo
	if res.Errors > 0 {
		lvl = slog.LevelWarn
	}
	s.log.Log(ctx, lvl, "sync finished", "summary", res.Summary(), "failed", sample(res.Failed, 5))
	return res, nil
}

// shouldBulk decides whether the archive fast path is worth it.
func (s *Syncer) shouldBulk(man *proto.Manifest, pl *plan) bool {
	if !s.features["archive"] || len(pl.downloads) == 0 {
		return false
	}
	if s.forceBulk {
		return true
	}
	// Only for a cold destination: an incremental run is already minimal.
	if s.state.Count() > 0 {
		return false
	}
	maxBytes := int64(s.cfg.Sync.BulkThresholdMB) << 20
	if int64(len(pl.downloads)) >= int64(s.cfg.Sync.BulkThresholdFiles) {
		return true
	}
	return pl.bytes >= maxBytes
}

// downloadAll runs the download queue with a bounded worker pool.
func (s *Syncer) downloadAll(ctx context.Context, queue []proto.Entry, res *RunResult) {
	if len(queue) == 0 {
		return
	}
	workers := s.cfg.Sync.Concurrency
	if workers <= 0 {
		workers = 4
	}
	if workers > len(queue) {
		workers = len(queue)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		idx     int
		lastLog = time.Now()
		done    int
		total   = len(queue)
	)
	workersCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if idx >= len(queue) || workersCtx.Err() != nil {
					mu.Unlock()
					return
				}
				e := queue[idx]
				idx++
				mu.Unlock()

				n, err := s.download(workersCtx, e)
				mu.Lock()
				done++
				if err != nil {
					res.Errors++
					res.Failed = append(res.Failed, e.Path)
					s.log.Error("download failed", "path", e.Path, "error", err.Error())
					if s.cfg.Sync.StopOnError {
						cancel()
					}
				} else {
					res.Downloaded++
					res.Bytes += n
				}
				if time.Since(lastLog) > 15*time.Second || done == total {
					s.log.Info("progress", "done", done, "total", total, "bytes", res.Bytes, "failed", res.Errors)
					lastLog = time.Now()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

// download transfers one file into place.
func (s *Syncer) download(ctx context.Context, e proto.Entry) (int64, error) {
	local, err := proto.Resolve(s.cfg.Local.Dir, e.Path)
	if err != nil {
		return 0, fmt.Errorf("unsafe path %q: %w", e.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return 0, err
	}
	if st, err := os.Stat(local); err == nil && st.IsDir() {
		// Only removes an empty directory; a populated one is a conflict that
		// needs a human, not a silent recursive delete.
		if err := os.Remove(local); err != nil {
			return 0, fmt.Errorf("local path is a non-empty directory, refusing to replace it: %w", err)
		}
	}
	tmp := fsx.TempPath(local, "")
	defer os.Remove(tmp)

	dl, err := s.remote.DownloadTo(ctx, e.Path, tmp, e.SHA256, s.cfg.Sync.BandwidthLimitKBps)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	if st.Size() != e.Size {
		return 0, fmt.Errorf("size mismatch: received %d bytes, manifest says %d", st.Size(), e.Size)
	}
	// Attributes are applied before the rename so readers never observe a file
	// with the wrong timestamp.
	if s.preserveMTime {
		_ = fsx.Chtimes(tmp, e.ModTime)
	}
	if s.preservePerms && runtime.GOOS != "windows" && e.Mode != 0 {
		_ = os.Chmod(tmp, os.FileMode(e.Mode).Perm())
	}
	if err := fsx.Replace(tmp, local); err != nil {
		return 0, err
	}
	_ = fsx.SyncDir(filepath.Dir(local))

	sum := dl.SHA256
	if e.SHA256 != "" {
		sum = e.SHA256
	}
	s.state.Set(e.Path, StateEntry{Size: e.Size, MTime: e.ModTime.UnixNano(), SHA256: sum})
	return dl.Bytes, nil
}

// bulkTransfer streams the whole subtree as tar.gz. Files that fail
// verification are left for the per-file path.
func (s *Syncer) bulkTransfer(ctx context.Context, man *proto.Manifest, pl *plan, res *RunResult) (map[string]struct{}, int64) {
	done := map[string]struct{}{}
	byPath := make(map[string]proto.Entry, len(man.Entries))
	for _, e := range man.Entries {
		byPath[e.Path] = e
	}
	want := make(map[string]struct{}, len(pl.downloads))
	for _, e := range pl.downloads {
		want[e.Path] = struct{}{}
	}

	body, err := s.remote.OpenArchive(ctx, "")
	if err != nil {
		s.log.Warn("bulk transfer unavailable, falling back to per-file downloads", "error", err.Error())
		return done, 0
	}
	defer body.Close()
	gz, err := gzip.NewReader(body)
	if err != nil {
		s.log.Warn("bulk transfer unavailable, falling back to per-file downloads", "error", err.Error())
		return done, 0
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var bytes int64
	for {
		if err := ctx.Err(); err != nil {
			return done, bytes
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.log.Warn("bulk transfer interrupted", "error", err.Error())
			return done, bytes
		}
		name, cerr := proto.CleanRel(hdr.Name)
		if cerr != nil || name == "" {
			s.log.Warn("archive contains an unsafe path, skipping", "name", hdr.Name)
			continue
		}
		if s.matcher.Match(name, hdr.Typeflag == tar.TypeDir) {
			continue
		}
		if _, ok := want[name]; !ok {
			// Already up to date: drain the payload without writing it.
			continue
		}
		entry := byPath[name]
		switch hdr.Typeflag {
		case tar.TypeDir:
			full, err := proto.Resolve(s.cfg.Local.Dir, name)
			if err == nil {
				_ = os.MkdirAll(full, 0o755)
			}
			continue
		case tar.TypeSymlink:
			if s.createSymlinks && entry.Type == proto.TypeSymlink {
				_ = s.makeSymlink(entry)
			}
			continue
		case tar.TypeReg:
		default:
			continue
		}
		n, err := s.writeFromArchive(tr, entry, hdr)
		if err != nil {
			s.log.Warn("file from archive rejected, will retry individually", "path", name, "error", err.Error())
			continue
		}
		bytes += n
		done[name] = struct{}{}
	}
	return done, bytes
}

// writeFromArchive writes one tar member atomically after verifying its digest.
func (s *Syncer) writeFromArchive(tr *tar.Reader, entry proto.Entry, hdr *tar.Header) (int64, error) {
	local, err := proto.Resolve(s.cfg.Local.Dir, entry.Path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return 0, err
	}
	tmp := fsx.TempPath(local, "")
	defer os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), tr)
	if err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if n != entry.Size {
		return 0, fmt.Errorf("size mismatch: got %d want %d", n, entry.Size)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if entry.SHA256 != "" && !strings.EqualFold(sum, entry.SHA256) {
		return 0, fmt.Errorf("checksum mismatch: got %s want %s", short(sum), short(entry.SHA256))
	}
	if s.preserveMTime {
		_ = fsx.Chtimes(tmp, entry.ModTime)
	}
	if s.preservePerms && runtime.GOOS != "windows" && entry.Mode != 0 {
		_ = os.Chmod(tmp, os.FileMode(entry.Mode).Perm())
	}
	if err := fsx.Replace(tmp, local); err != nil {
		return 0, err
	}
	digest := entry.SHA256
	if digest == "" {
		digest = sum
	}
	s.state.Set(entry.Path, StateEntry{Size: entry.Size, MTime: entry.ModTime.UnixNano(), SHA256: digest})
	return n, nil
}

// makeSymlink recreates a symbolic link published by the agent.
func (s *Syncer) makeSymlink(e proto.Entry) error {
	local, err := proto.Resolve(s.cfg.Local.Dir, e.Path)
	if err != nil {
		return err
	}
	if existing, err := os.Readlink(local); err == nil && existing == e.Target {
		return nil
	}
	_ = os.Remove(local)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	return os.Symlink(filepath.FromSlash(e.Target), local)
}

// removeExtras deletes local files that no longer exist on the agent and then
// prunes directories that became empty and are not published any more.
func (s *Syncer) removeExtras(extras []string, remoteDirs map[string]struct{}) (int, error) {
	deleted := 0
	for _, rel := range extras {
		if s.isInternalPath(rel) {
			continue
		}
		full, err := proto.Resolve(s.cfg.Local.Dir, rel)
		if err != nil {
			continue
		}
		if err := os.Remove(full); err != nil {
			s.log.Warn("cannot delete local-only file", "path", rel, "error", err.Error())
			continue
		}
		s.state.Drop(rel)
		deleted++
	}
	if deleted > 0 {
		removed := fsx.RemoveEmptyDirs(s.cfg.Local.Dir, func(rel string) bool {
			_, ok := remoteDirs[rel]
			return ok
		})
		for _, d := range removed {
			s.log.Debug("removed empty directory", "path", d)
		}
	}
	return deleted, nil
}

// shortHash is a local helper for digest prefixes in messages.
func firstOrEmpty(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

func sample(list []string, n int) []string {
	if len(list) <= n {
		out := append([]string{}, list...)
		sort.Strings(out)
		return out
	}
	out := append([]string{}, list[:n]...)
	sort.Strings(out)
	return out
}
