// Package fsx holds small filesystem helpers that must behave identically on
// Windows, Linux, x86-64 and ARM.
package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// TempPrefix marks in-progress downloads. It is excluded from every scan so a
// half written file is never published or mistaken for content.
const TempPrefix = ".tidesync-tmp-"

// IsTempName reports whether the base name belongs to an in-progress transfer.
func IsTempName(name string) bool { return strings.HasPrefix(name, TempPrefix) }

// TempPath returns the staging path used for an atomic write next to dst.
func TempPath(dst string, tag string) string {
	dir := filepath.Dir(dst)
	base := filepath.Base(dst)
	if tag == "" {
		tag = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return filepath.Join(dir, TempPrefix+base+"-"+tag)
}

// Replace atomically replaces dst with tmp, creating missing parents.
//
// os.Rename maps to MoveFileEx(MOVEFILE_REPLACE_EXISTING) on Windows and
// rename(2) on Unix, so both replace atomically. Windows additionally fails
// with a sharing violation when an antivirus or indexer holds the target open,
// which is why transient failures are retried a few times.
func Replace(tmp, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = os.Rename(tmp, dst)
		if err == nil {
			return nil
		}
		if !isTransientRenameErr(err) {
			return err
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	return err
}

func isTransientRenameErr(err error) bool {
	if err == nil {
		return false
	}
	// Windows sharing violations and Unix ETXTBSY/EBUSY style errors.
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	if errors.Is(err, os.ErrExist) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "being used by another process") ||
		strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "resource busy") ||
		strings.Contains(msg, "text file busy")
}

// WriteFileAtomic writes data to dst via a staging file in the same directory,
// fsyncs it, then renames it into place.
func WriteFileAtomic(dst string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := TempPath(dst, "")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil && runtime.GOOS != "windows" {
		os.Remove(tmp)
		return err
	}
	if err := Replace(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return SyncDir(filepath.Dir(dst))
}

// SyncDir flushes a directory entry to disk so a rename survives a power cut.
// It is a no-op on Windows, which does not allow opening directories for sync.
func SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil // best effort: a missing directory is reported elsewhere
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// SHA256File returns the hex encoded SHA-256 of a file.
func SHA256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SHA256Reader returns the hex encoded SHA-256 of everything read from r.
func SHA256Reader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SameFileContent reports whether two regular files have identical size and
// SHA-256.
func SameFileContent(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	if sa.Size() != sb.Size() {
		return false
	}
	ha, err := SHA256File(a)
	if err != nil {
		return false
	}
	hb, err := SHA256File(b)
	if err != nil {
		return false
	}
	return ha == hb
}

// RemoveEmptyDirs deletes empty directories below root (depth first) for which
// keep returns false. root itself is never removed.
func RemoveEmptyDirs(root string, keep func(rel string) bool) []string {
	var removed []string
	var walk func(dir string) bool
	walk = func(dir string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		empty := true
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if walk(full) {
					continue
				}
			}
			if IsTempName(e.Name()) {
				// Stale staging files do not keep a directory alive.
				if err := os.RemoveAll(full); err == nil {
					continue
				}
			}
			empty = false
		}
		if !empty {
			return false
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." {
			return false
		}
		if keep != nil && keep(filepath.ToSlash(rel)) {
			return false
		}
		if err := os.Remove(dir); err == nil {
			removed = append(removed, filepath.ToSlash(rel))
			return true
		}
		return false
	}
	walk(root)
	sort.Strings(removed)
	return removed
}

// Chtimes sets the modification time, preserving access time where supported.
func Chtimes(p string, mtime time.Time) error {
	if mtime.IsZero() {
		return nil
	}
	return os.Chtimes(p, time.Now(), mtime)
}
