package proto

import (
	"testing"
	"time"
)

func TestCleanRel(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{".", "", false},
		{"/", "", false},
		{"a.txt", "a.txt", false},
		{"dir/sub/file.txt", "dir/sub/file.txt", false},
		{"./dir/file.txt", "dir/file.txt", false},
		{"dir//file.txt", "dir/file.txt", false},
		{`dir\sub\file.txt`, "dir/sub/file.txt", false},
		{"../etc/passwd", "", true},
		{"a/../../b", "", true},
		{"/etc/passwd", "", true},
		{`C:\Windows\system32`, "", true},
		{"C:/Windows", "", true},
		{"a/b/../../../c", "", true},
		{"\x00evil", "", true},
		{"  spaced.txt  ", "spaced.txt", false},
		{"中文/文件.txt", "中文/文件.txt", false},
	}
	for _, tc := range cases {
		got, err := CleanRel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("CleanRel(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("CleanRel(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CleanRel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveStaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	p, err := Resolve(root, "a/b.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p == root {
		t.Fatalf("Resolve returned the root for a relative path")
	}
	if _, err := Resolve(root, "../../etc/passwd"); err == nil {
		t.Fatalf("Resolve accepted an escaping path")
	}
	if _, err := Resolve(root, ""); err != nil {
		t.Fatalf("Resolve(root, \"\") should return the root: %v", err)
	}
}

func TestMatcher(t *testing.T) {
	m := NewMatcher([]string{
		"*.tmp",
		"logs/**",
		".git/",
		"keep/important.tmp",
		"!keep/important.tmp",
		"exact/file.txt",
	})
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"a.tmp", false, true},
		{"dir/a.tmp", false, true},
		{"dir/keep.txt", false, false},
		{"logs/a.log", false, true},
		{"logs/deep/nested/a.log", false, true},
		{"other/logs/a.log", false, false},
		{".git", true, true},
		{".git", false, false}, // dir-only rule
		{".git/config", false, true},
		{"keep/important.tmp", false, false}, // negated again
		{"exact/file.txt", false, true},
		{"nested/exact/file.txt", false, false}, // anchored pattern
	}
	for _, tc := range cases {
		if got := m.Match(tc.path, tc.isDir); got != tc.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestMatcherEmpty(t *testing.T) {
	m := NewMatcher(nil)
	if !m.Empty() {
		t.Fatal("expected an empty matcher")
	}
	if m.Match("anything", false) {
		t.Fatal("empty matcher must not exclude anything")
	}
}

func TestEntryHelpers(t *testing.T) {
	man := &Manifest{Entries: []Entry{
		{Path: "a", Type: TypeFile, Size: 10, ModTime: time.Now()},
		{Path: "b", Type: TypeDir},
		{Path: "c", Type: TypeFile, Size: 5, ModTime: time.Now()},
		{Path: "d", Type: TypeSymlink, Target: "a"},
	}}
	if got := man.FileCount(); got != 2 {
		t.Errorf("FileCount = %d, want 2", got)
	}
	if got := man.TotalSize(); got != 15 {
		t.Errorf("TotalSize = %d, want 15", got)
	}
	if !man.Entries[0].IsFile() || man.Entries[1].IsFile() {
		t.Error("IsFile misclassified an entry")
	}
}
