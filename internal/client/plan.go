package client

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"tidesync/internal/config"
	"tidesync/internal/fsx"
	"tidesync/internal/proto"
)

// action is the outcome of comparing one remote entry with the local disk.
type action int

const (
	actSkip action = iota
	actDownload
	actTooLarge
	actSymlink
)

// plan is what a run intends to do, computed before any byte is transferred.
type plan struct {
	dirs       []proto.Entry
	downloads  []proto.Entry
	symlinks   []proto.Entry
	extras     []string
	remoteSet  map[string]struct{}
	remoteDirs map[string]struct{}
	upToDate   int
	tooLarge   int
	skipped    int
	bytes      int64
	// adopt records up-to-date files whose modification time should be
	// realigned with the agent so the next run takes the fast path.
	adopt []proto.Entry
}

// buildPlan compares the manifest with the local directory.
func (s *Syncer) buildPlan(man *proto.Manifest) (*plan, error) {
	p := &plan{
		remoteSet:  make(map[string]struct{}, len(man.Entries)),
		remoteDirs: make(map[string]struct{}),
	}
	maxBytes := int64(s.cfg.Sync.MaxFileSizeMB) << 20

	for _, e := range man.Entries {
		p.remoteSet[e.Path] = struct{}{}
		// Remember every directory the agent publishes, including implicit
		// parents, so mirror mode keeps them.
		if e.Type == proto.TypeDir {
			p.remoteDirs[e.Path] = struct{}{}
		}
		for dir := pathDir(e.Path); dir != ""; dir = pathDir(dir) {
			p.remoteDirs[dir] = struct{}{}
		}
		if s.matcher.Match(e.Path, e.Type == proto.TypeDir) {
			p.skipped++
			s.log.Debug("skipping excluded path", "path", e.Path)
			continue
		}
		switch e.Type {
		case proto.TypeDir:
			if s.createDirs {
				if st, err := os.Stat(mustResolve(s.cfg.Local.Dir, e.Path)); err != nil || !st.IsDir() {
					p.dirs = append(p.dirs, e)
				}
			}
		case proto.TypeSymlink:
			if s.createSymlinks {
				p.symlinks = append(p.symlinks, e)
			} else {
				p.skipped++
				s.log.Debug("skipping symlink (create_symlinks is false)", "path", e.Path)
			}
		case proto.TypeFile:
			if s.matcher.Match(e.Path, false) {
				p.skipped++
				s.log.Debug("skipping excluded file", "path", e.Path)
				continue
			}
			if maxBytes > 0 && e.Size > maxBytes {
				p.tooLarge++
				s.log.Warn("file exceeds sync.max_file_size_mb and is skipped", "path", e.Path, "size", e.Size)
				continue
			}
			local, err := proto.Resolve(s.cfg.Local.Dir, e.Path)
			if err != nil {
				s.log.Warn("refusing suspicious remote path", "path", e.Path)
				p.skipped++
				continue
			}
			act, why := s.decide(e, local)
			switch act {
			case actSkip:
				p.upToDate++
				if why == whyContentSame {
					p.adopt = append(p.adopt, e)
				}
				s.noteUpToDate(e, local)
			case actDownload:
				p.downloads = append(p.downloads, e)
				p.bytes += e.Size
				s.log.Debug("will download", "path", e.Path, "size", e.Size, "reason", why)
			}
		}
	}

	// Local files that the agent does not know about. They are always reported
	// (and counted) but only removed when mirror mode is enabled.
	extras, err := s.findExtras(p.remoteSet)
	if err != nil {
		return nil, err
	}
	p.extras = extras
	return p, nil
}

// reasons returned by decide, used for debug logging and the adopt decision.
const (
	whyNone         = ""
	whyMissing      = "missing locally"
	whySize         = "size differs"
	whyMTime        = "modification time differs"
	whyContent      = "content differs"
	whyNoRemoteHash = "no remote digest available"
	whyContentSame  = "content identical"
	whyJournal      = "journal match"
	whyVerifyNone   = "size matches (verify=none)"
)

// decide compares one remote file entry with its local counterpart.
func (s *Syncer) decide(e proto.Entry, local string) (action, string) {
	st, err := os.Stat(local)
	if err != nil {
		if os.IsNotExist(err) {
			return actDownload, whyMissing
		}
		return actDownload, "cannot stat local file: " + err.Error()
	}
	if st.IsDir() {
		// A directory where a file belongs: never delete user data silently.
		return actDownload, "local path is a directory, will be replaced by a file"
	}
	if !st.Mode().IsRegular() {
		return actDownload, "local path is not a regular file"
	}
	if st.Size() != e.Size {
		return actDownload, whySize
	}
	sameTime := sameModTime(st.ModTime(), e.ModTime)

	switch s.verify {
	case "none":
		return actSkip, whyVerifyNone
	case "size-mtime":
		if sameTime {
			return actSkip, whyNone
		}
		return actDownload, whyMTime
	}

	// verify == sha256
	//
	// The journal only proves that this file was synchronised once. The remote
	// digest must still be the one that was written, otherwise the agent is
	// publishing different content with unchanged size and timestamp.
	if se, ok := s.state.Get(e.Path); ok && se.SHA256 != "" &&
		se.Size == e.Size && se.MTime == e.ModTime.UnixNano() && sameTime &&
		(e.SHA256 == "" || strings.EqualFold(se.SHA256, e.SHA256)) {
		return actSkip, whyJournal
	}
	if e.SHA256 == "" {
		if sameTime {
			return actSkip, whyNone
		}
		return actDownload, whyNoRemoteHash
	}
	sum, err := fsx.SHA256File(local)
	if err == nil && strings.EqualFold(sum, e.SHA256) {
		return actSkip, whyContentSame
	}
	return actDownload, whyContent
}

// noteUpToDate refreshes the journal for a file that needs no transfer.
func (s *Syncer) noteUpToDate(e proto.Entry, local string) {
	se := StateEntry{Size: e.Size, MTime: e.ModTime.UnixNano()}
	if st, err := os.Stat(local); err == nil {
		switch s.verify {
		case "none":
			se.MTime = 0
		case "size-mtime":
			se.MTime = st.ModTime().UnixNano()
		default:
			if e.SHA256 != "" {
				se.SHA256 = e.SHA256
				se.MTime = st.ModTime().UnixNano()
			} else {
				se.MTime = st.ModTime().UnixNano()
			}
		}
	}
	s.state.Set(e.Path, se)
}

// staleTempAge is how old a staging file must be before it is considered
// debris from a killed process rather than another job's transfer in progress.
const staleTempAge = 24 * time.Hour

// findExtras lists local files that are absent from the manifest and, in the
// same traversal, removes staging files left behind by a process that was
// killed mid-transfer. Those files are never published and would otherwise
// accumulate for ever on the destination.
func (s *Syncer) findExtras(remoteSet map[string]struct{}) ([]string, error) {
	var extras []string
	root := s.cfg.Local.Dir
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if fsx.IsTempName(info.Name()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			if age := time.Since(info.ModTime()); age > staleTempAge {
				if os.Remove(p) == nil {
					s.log.Warn("removed a partial download left by an interrupted run",
						"path", rel, "age", age.Round(time.Minute).String())
				}
			}
			return nil
		}
		if info.IsDir() {
			if s.matcher.Match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if s.isInternalPath(rel) || s.matcher.Match(rel, false) {
			return nil
		}
		if _, ok := remoteSet[rel]; !ok {
			extras = append(extras, rel)
		}
		return nil
	})
	return extras, err
}

// isInternalPath reports whether a path belongs to TideSync itself and must
// never be deleted by mirror mode.
func (s *Syncer) isInternalPath(rel string) bool {
	if rel == config.StateDirName || strings.HasPrefix(rel, config.StateDirName+"/") {
		return true
	}
	if s.state != nil && s.state.Path() != "" {
		if same, err := filepath.Rel(s.cfg.Local.Dir, s.state.Path()); err == nil {
			if filepath.ToSlash(same) == rel {
				return true
			}
		}
	}
	if s.cfg.LockFile != "" {
		if same, err := filepath.Rel(s.cfg.Local.Dir, s.cfg.LockFile); err == nil {
			if filepath.ToSlash(same) == rel {
				return true
			}
		}
	}
	return fsx.IsTempName(filepath.Base(rel))
}

// sameModTime compares two timestamps at filesystem granularity. FAT32 stores
// times with two second resolution and many filesystems round to the second, so
// anything below two seconds counts as equal.
func sameModTime(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d < 2*time.Second
}

func mustResolve(root, rel string) string {
	p, err := proto.Resolve(root, rel)
	if err != nil {
		return filepath.Join(root, filepath.FromSlash(rel))
	}
	return p
}

// pathDir returns the parent directory of a slash separated relative path, or
// "" for a top level entry.
func pathDir(rel string) string {
	i := strings.LastIndex(rel, "/")
	if i <= 0 {
		return ""
	}
	return rel[:i]
}
