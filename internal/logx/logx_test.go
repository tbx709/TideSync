package logx

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"INFO":     slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"":         slog.LevelInfo,
		"error":    slog.LevelError,
		"nonsense": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRotatingWriterRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// 1 MB is the smallest configurable size; write just over it twice.
	w, err := NewRotatingWriter(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	chunk := strings.Repeat("x", 1024*1024)
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("first generation missing: %v", err)
	}
	// maxFiles=2 keeps at most .1 and .2; the third generation is dropped.
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Error("rotation kept more generations than configured")
	}
	files := w.RotatedFiles()
	if len(files) == 0 || len(files) > 3 {
		t.Errorf("RotatedFiles = %v", files)
	}
}

func TestNewWritesToFileAndConsole(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "tidesync.log")
	log, closeFn, err := New(Options{Level: "debug", File: logPath, MaxSizeMB: 1, MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "key", "value")
	log.Debug("debug line")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello") || !strings.Contains(string(data), "debug line") {
		t.Errorf("log file content = %q", data)
	}
}
