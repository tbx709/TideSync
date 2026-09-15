package fsx

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWriteFileAtomicAndReplace(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "sub", "file.txt")
	if err := WriteFileAtomic(dst, []byte("first"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "first" {
		t.Fatalf("content = %q", got)
	}
	if err := WriteFileAtomic(dst, []byte("second"), 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "second" {
		t.Fatalf("content after overwrite = %q", got)
	}
	// No staging files may survive.
	entries, _ := os.ReadDir(filepath.Dir(dst))
	for _, e := range entries {
		if IsTempName(e.Name()) {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
}

func TestReplaceOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := TempPath(dst, "t1")
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Replace(tmp, dst); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
}

func TestSHA256File(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	const want = "a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447"
	if sum != want {
		t.Errorf("SHA256File = %s, want %s", sum, want)
	}
	if !SameFileContent(p, p) {
		t.Error("SameFileContent must be true for identical files")
	}
}

func TestRemoveEmptyDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "empty", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "keep", "file.txt"), "x")
	removed := RemoveEmptyDirs(dir, func(rel string) bool { return false })
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want the two empty directories", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "file.txt")); err != nil {
		t.Error("a populated directory was removed")
	}
}

func TestChtimesAndGofmtSafeTime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "x")
	when := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := Chtimes(p, when); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("filesystem does not support sub-second timestamps")
		}
		t.Fatalf("Chtimes: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if d := st.ModTime().Sub(when); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("modification time not applied: %v", st.ModTime())
	}
}

func TestFreeSpace(t *testing.T) {
	free, err := FreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if free <= 0 {
		t.Errorf("FreeSpace = %d, want a positive number", free)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
