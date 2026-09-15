package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"tidesync/internal/fsx"
)

// stateVersion is bumped when the on-disk layout changes.
const stateVersion = 1

// StateEntry records what the client last wrote for one file, so that an
// unchanged file can be skipped without reading either side.
type StateEntry struct {
	Size     int64     `json:"size"`
	MTime    int64     `json:"mtime_unix_nano"`
	SHA256   string    `json:"sha256,omitempty"`
	SyncedAt time.Time `json:"synced_at"`
}

// State is the persistent sync journal of one job.
type State struct {
	Version    int                   `json:"version"`
	Job        string                `json:"job"`
	RemoteURL  string                `json:"remote_url"`
	RemotePath string                `json:"remote_path"`
	LocalDir   string                `json:"local_dir"`
	LastRun    time.Time             `json:"last_run"`
	LastResult string                `json:"last_result"`
	LastError  string                `json:"last_error,omitempty"`
	Runs       int64                 `json:"runs"`
	FullScanAt time.Time             `json:"full_scan_at,omitempty"`
	Entries    map[string]StateEntry `json:"entries"`

	// The journal is touched from every download worker, so all access goes
	// through mu.
	mu    sync.Mutex
	path  string
	dirty bool
}

// LoadState reads the journal, returning an empty one when the file is missing
// or unreadable. A damaged journal only costs a full comparison.
func LoadState(path string) (*State, error) {
	st := &State{Version: stateVersion, Entries: map[string]StateEntry{}, path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		return st, fmt.Errorf("state file %s is unreadable (%v); starting from an empty journal", path, err)
	}
	if loaded.Entries == nil {
		loaded.Entries = map[string]StateEntry{}
	}
	loaded.path = path
	loaded.dirty = false
	if loaded.Version == 0 {
		loaded.Version = stateVersion
	}
	return &loaded, nil
}

// Get returns the journal row for a relative path.
func (s *State) Get(rel string) (StateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.Entries[rel]
	return e, ok
}

// Set records a successful transfer or an up-to-date observation.
func (s *State) Set(rel string, e StateEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Entries == nil {
		s.Entries = map[string]StateEntry{}
	}
	e.SyncedAt = time.Now()
	s.Entries[rel] = e
	s.dirty = true
}

// Drop forgets a path, used when the file is deleted locally or remotely.
func (s *State) Drop(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Entries == nil {
		return
	}
	if _, ok := s.Entries[rel]; ok {
		delete(s.Entries, rel)
		s.dirty = true
	}
}

// Prune removes journal rows that are no longer part of the remote tree.
func (s *State) Prune(keep map[string]struct{}) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.Entries {
		if _, ok := keep[k]; !ok {
			delete(s.Entries, k)
			n++
			s.dirty = true
		}
	}
	return n
}

// Count returns the number of journal rows.
func (s *State) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Entries)
}

// Save writes the journal atomically when it changed.
func (s *State) Save(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	if !s.dirty && !force {
		return nil
	}
	s.Version = stateVersion
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Path returns the journal location.
func (s *State) Path() string { return s.path }

// KnownPaths returns the sorted tracked paths, used by diagnostics.
func (s *State) KnownPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.Entries))
	for k := range s.Entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
