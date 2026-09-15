package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tidesync/internal/fsx"
)

// hashEntry is one cached SHA-256 keyed by (size, mtime).
type hashEntry struct {
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime_unix_nano"`
	SHA256 string `json:"sha256"`
}

// hashCache remembers file digests between scans so that a Raspberry Pi does
// not re-read an unchanged tree on every manifest request.
type hashCache struct {
	mu      sync.Mutex
	path    string
	entries map[string]hashEntry
	dirty   bool
	hits    int
	misses  int
}

func newHashCache(path string) *hashCache {
	return &hashCache{path: path, entries: make(map[string]hashEntry)}
}

func (c *hashCache) load() error {
	if c.path == "" {
		return nil
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var stored struct {
		Version int                  `json:"version"`
		Entries map[string]hashEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		// A corrupt cache is never fatal: it is simply rebuilt.
		return nil
	}
	if stored.Entries != nil {
		c.entries = stored.Entries
	}
	return nil
}

// get returns the cached digest for a file whose size and mtime are unchanged.
func (c *hashCache) get(rel string, size int64, mtime time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[rel]
	if ok && e.Size == size && e.MTime == mtime.UnixNano() && e.SHA256 != "" {
		c.hits++
		return e.SHA256, true
	}
	c.misses++
	return "", false
}

func (c *hashCache) put(rel string, size int64, mtime time.Time, sum string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[rel] = hashEntry{Size: size, MTime: mtime.UnixNano(), SHA256: sum}
	c.dirty = true
}

// prune drops entries for files that no longer exist.
func (c *hashCache) prune(seen map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if _, ok := seen[k]; !ok {
			delete(c.entries, k)
			c.dirty = true
		}
	}
}

// save writes the cache atomically. Failure is logged by the caller but never
// aborts a scan.
func (c *hashCache) save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty || c.path == "" {
		return nil
	}
	payload := struct {
		Version int                  `json:"version"`
		Saved   time.Time            `json:"saved_at"`
		Entries map[string]hashEntry `json:"entries"`
	}{Version: 1, Saved: time.Now(), Entries: c.entries}
	data, err := json.Marshal(&payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(c.path, data, 0o600); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// stats reports cache effectiveness for the scan log line.
func (c *hashCache) stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}
