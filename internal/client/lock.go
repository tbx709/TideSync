package client

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Lock is an advisory single-run lock. It prevents two overlapping syncs of the
// same job (a slow run plus the next scheduled tick, or a manual run next to a
// service run) from fighting over the same files.
//
// The lock is a file holding "<pid> <host> <started>"; a heartbeat goroutine
// keeps its mtime fresh, so a lock whose mtime is older than staleAfter is
// considered abandoned (for example after a power loss) and is taken over.
type Lock struct {
	path  string
	log   *slog.Logger
	stop  chan struct{}
	done  chan struct{}
	mine  bool
	stale time.Duration
}

// AcquireLock takes the lock at path, waiting up to staleAfter for an
// abandoned lock to be reclaimed. It returns ErrLocked when another run is
// alive and healthy.
func AcquireLock(path string, staleAfter time.Duration, log *slog.Logger) (*Lock, error) {
	if log == nil {
		log = slog.Default()
	}
	if staleAfter <= 0 {
		staleAfter = 30 * time.Minute
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l := &Lock{path: path, log: log, stop: make(chan struct{}), done: make(chan struct{}), stale: staleAfter}

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			hn, _ := os.Hostname()
			fmt.Fprintf(f, "%d %s %s\n", os.Getpid(), hn, time.Now().Format(time.RFC3339))
			f.Close()
			l.mine = true
			go l.heartbeat()
			return l, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock %s: %w", path, err)
		}
		st, serr := os.Stat(path)
		if serr != nil {
			continue // vanished between create and stat: retry
		}
		age := time.Since(st.ModTime())
		if age > staleAfter {
			owner, _ := os.ReadFile(path)
			log.Warn("taking over an abandoned sync lock",
				"lock", path, "age", age.Round(time.Second).String(), "owner", strings.TrimSpace(string(owner)))
			os.Remove(path)
			continue
		}
		return nil, fmt.Errorf("another sync is already running (lock %s, age %s): %w",
			path, age.Round(time.Second), ErrLocked)
	}
	return nil, fmt.Errorf("could not acquire lock %s (held by another process): %w", path, ErrLocked)
}

// ErrLocked reports that another synchroniser holds the job lock.
var ErrLocked = fmt.Errorf("sync already running")

func (l *Lock) heartbeat() {
	defer close(l.done)
	t := time.NewTicker(l.stale / 3)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			now := time.Now()
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

// Release removes the lock file.
func (l *Lock) Release() {
	if l == nil || !l.mine {
		return
	}
	close(l.stop)
	<-l.done
	os.Remove(l.path)
}
