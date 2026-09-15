package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"tidesync/internal/fsx"
	"tidesync/internal/proto"
)

// Scanner walks the published root and produces a manifest.
type Scanner struct {
	Root     string
	Matcher  *proto.Matcher
	HashMode string // auto | always | never
	HashMax  int64  // bytes, used by auto
	// HashGrace is the quiet period after which a cached digest is trusted.
	// Filesystems stamp modification times with a coarse clock (typically
	// 1-10 ms), so a file rewritten twice within one tick keeps the same size
	// and timestamp while its content changed. Re-hashing files that were
	// touched within HashGrace closes that window.
	HashGrace time.Duration
	// UseCache enables the on-disk digest cache. Disabling it makes every scan
	// fully authoritative at the cost of reading every file.
	UseCache   bool
	FollowLink bool
	MaxEntries int
	Cache      *hashCache
	Log        *slog.Logger
}

// ScanStats summarises one scan for logging and diagnostics.
type ScanStats struct {
	Files       int
	Dirs        int
	Symlinks    int
	Bytes       int64
	Hashed      int
	HashHits    int
	HashMisses  int
	Skipped     int
	Duration    time.Duration
	Truncated   bool
	MaxExceeded bool
}

// defaultHashGrace is the quiet period used when ServerConfig.HashGrace is
// unset.
const defaultHashGrace = 2 * time.Second

// rawEntry pairs a wire entry with the local path needed to hash it.
type rawEntry struct {
	entry  proto.Entry
	fsPath string
	hash   bool
}

// Scan builds the manifest of everything below the requested sub path.
func (s *Scanner) Scan(ctx context.Context, sub string) (*proto.Manifest, *ScanStats, error) {
	start := time.Now()
	root, err := proto.Resolve(s.Root, sub)
	if err != nil {
		return nil, nil, err
	}
	// A manifest describes a subtree, so the requested path must be a
	// directory. Reporting this early turns a silently empty sync into a clear
	// error on the client.
	if info, err := os.Stat(root); err != nil {
		return nil, nil, err
	} else if !info.IsDir() {
		return nil, nil, fmt.Errorf("%w: %s is a file, not a directory", os.ErrInvalid, sub)
	}
	st := &ScanStats{}
	if s.HashMax <= 0 {
		s.HashMax = 64 << 20
	}
	if s.MaxEntries <= 0 {
		s.MaxEntries = 200000
	}
	if s.HashGrace <= 0 {
		s.HashGrace = defaultHashGrace
	}

	var raw []rawEntry
	seen := make(map[string]struct{})

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			if s.Log != nil {
				s.Log.Warn("skipping unreadable entry", "path", p, "error", err.Error())
			}
			st.Skipped++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := proto.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		base := filepath.Base(p)
		if fsx.IsTempName(base) {
			st.Skipped++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		isDir := d.IsDir()
		if s.Matcher.Match(rel, isDir) {
			st.Skipped++
			if isDir {
				return fs.SkipDir
			}
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			st.Skipped++
			return nil
		}

		if info.Mode()&os.ModeSymlink != 0 {
			target, lerr := os.Readlink(p)
			if lerr != nil {
				st.Skipped++
				return nil
			}
			if !s.FollowLink {
				st.Symlinks++
				raw = append(raw, rawEntry{entry: proto.Entry{
					Path:    rel,
					Type:    proto.TypeSymlink,
					Target:  filepath.ToSlash(target),
					ModTime: info.ModTime(),
					Mode:    uint32(info.Mode().Perm()),
				}})
				return nil
			}
			// Follow: classify by the resolved target, refusing anything that
			// resolves outside the published root.
			real, serr := filepath.EvalSymlinks(p)
			if serr != nil || !within(root, real) {
				st.Skipped++
				if s.Log != nil {
					s.Log.Warn("refusing symlink outside root", "path", rel, "target", target)
				}
				return nil
			}
			ti, terr := os.Stat(real)
			if terr != nil {
				st.Skipped++
				return nil
			}
			if ti.IsDir() {
				return nil // followed directories are rare; publish their files via the walk
			}
			if !ti.Mode().IsRegular() {
				return nil
			}
			info = ti
			isDir = false
		}

		if isDir {
			st.Dirs++
			raw = append(raw, rawEntry{entry: proto.Entry{
				Path:    rel,
				Type:    proto.TypeDir,
				ModTime: info.ModTime(),
				Mode:    uint32(info.Mode().Perm()),
			}})
			return nil
		}
		if !info.Mode().IsRegular() {
			st.Skipped++
			return nil
		}
		st.Files++
		st.Bytes += info.Size()
		needHash := s.HashMode == "always" || (s.HashMode == "auto" && info.Size() <= s.HashMax)
		raw = append(raw, rawEntry{
			fsPath: p,
			hash:   needHash,
			entry: proto.Entry{
				Path:    rel,
				Type:    proto.TypeFile,
				Size:    info.Size(),
				ModTime: info.ModTime(),
				Mode:    uint32(info.Mode().Perm()),
			},
		})
		seen[rel] = struct{}{}
		if len(raw) > s.MaxEntries {
			st.Truncated = true
			st.MaxExceeded = true
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && !st.MaxExceeded {
		return nil, st, walkErr
	}

	// Hash in parallel: on a Raspberry Pi this is the slow part of a cold scan.
	s.hashEntries(ctx, raw, st)

	entries := make([]proto.Entry, 0, len(raw))
	for _, r := range raw {
		entries = append(entries, r.entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	if s.Cache != nil {
		s.Cache.prune(seen)
		if err := s.Cache.save(); err != nil && s.Log != nil {
			s.Log.Warn("could not persist hash cache", "error", err.Error())
		}
		st.HashHits, st.HashMisses = s.Cache.stats()
	}
	st.Duration = time.Since(start)
	hashed := s.HashMode == "always" || s.HashMode == "auto"
	if st.Truncated {
		hashed = false
	}
	return &proto.Manifest{
		Version:     proto.Version,
		Root:        strings.TrimPrefix(filepath.ToSlash(sub), "/"),
		GeneratedAt: time.Now(),
		Hashed:      hashed,
		Truncated:   st.Truncated,
		Entries:     entries,
	}, st, nil
}

// hashEntries fills SHA256 fields using the cache and a bounded worker pool.
func (s *Scanner) hashEntries(ctx context.Context, raw []rawEntry, st *ScanStats) {
	type job struct{ idx int }
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > 8 {
		workers = 8
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		next int
	)
	work := make([]int, 0, len(raw))
	for i := range raw {
		if raw[i].hash {
			work = append(work, i)
		}
	}
	if len(work) == 0 {
		return
	}
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(work) {
					mu.Unlock()
					return
				}
				idx := work[next]
				next++
				mu.Unlock()
				if ctx.Err() != nil {
					return
				}
				r := raw[idx]
				trustedQuiet := time.Since(r.entry.ModTime) >= s.HashGrace
				if s.Cache != nil && s.UseCache && trustedQuiet {
					if sum, ok := s.Cache.get(r.entry.Path, r.entry.Size, r.entry.ModTime); ok {
						raw[idx].entry.SHA256 = sum
						continue
					}
				}
				sum, err := fsx.SHA256File(r.fsPath)
				if err != nil {
					if s.Log != nil {
						s.Log.Warn("hashing failed", "path", r.entry.Path, "error", err.Error())
					}
					continue
				}
				raw[idx].entry.SHA256 = sum
				if s.Cache != nil && s.UseCache {
					s.Cache.put(r.entry.Path, r.entry.Size, r.entry.ModTime, sum)
				}
			}
		}()
	}
	wg.Wait()
	if s.Cache != nil {
		hits, misses := s.Cache.stats()
		st.HashHits, st.HashMisses = hits, misses
	}
}

// within reports whether p is inside root.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
