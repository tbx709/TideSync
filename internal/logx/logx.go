// Package logx provides the small leveled, rotating logger used by every
// TideSync component. It writes to stderr, to a file, or both, and rotates the
// file when it grows past a configured size. Log files stay readable on a
// Raspberry Pi SD card, so rotation is size based rather than time based.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Options mirrors config.LogConfig without importing it.
type Options struct {
	Level     string
	File      string
	MaxSizeMB int
	MaxFiles  int
	JSON      bool
	// Console, when non-nil, replaces stderr as the console sink.
	Console io.Writer
}

// ParseLevel maps a configuration string to a slog level.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New builds a logger plus a Close function that releases the log file.
func New(opts Options) (*slog.Logger, func() error, error) {
	if opts.Console == nil {
		opts.Console = os.Stderr
	}
	level := ParseLevel(opts.Level)
	var writers []io.Writer
	writers = append(writers, opts.Console)
	var closer func() error = func() error { return nil }
	if strings.TrimSpace(opts.File) != "" {
		w, err := NewRotatingWriter(opts.File, opts.MaxSizeMB, opts.MaxFiles)
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, w)
		closer = w.Close
	}
	out := io.MultiWriter(writers...)
	handlerOpts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if opts.JSON {
		handler = slog.NewJSONHandler(out, handlerOpts)
	} else {
		handler = slog.NewTextHandler(out, handlerOpts)
	}
	return slog.New(handler), closer, nil
}

// RotatingWriter is an io.WriteCloser that rotates path when it exceeds
// maxSizeMB, keeping up to maxFiles previous generations (path.1 is newest).
type RotatingWriter struct {
	mu        sync.Mutex
	path      string
	maxBytes  int64
	maxFiles  int
	f         *os.File
	size      int64
	openError error
}

// NewRotatingWriter creates the writer and opens the current log file.
func NewRotatingWriter(path string, maxSizeMB, maxFiles int) (*RotatingWriter, error) {
	if maxSizeMB <= 0 {
		maxSizeMB = 10
	}
	if maxFiles <= 0 {
		maxFiles = 3
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	w := &RotatingWriter{
		path:     path,
		maxBytes: int64(maxSizeMB) * 1024 * 1024,
		maxFiles: maxFiles,
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = st.Size()
	return nil
}

// Write implements io.Writer.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			// Rotation failures must never lose log lines.
			_ = err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) rotate() error {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	// path.(maxFiles-1) is dropped, path.N -> path.(N+1).
	oldest := fmt.Sprintf("%s.%d", w.path, w.maxFiles)
	os.Remove(oldest)
	for i := w.maxFiles - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path, i)
		to := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(from); err == nil {
			os.Rename(from, to)
		}
	}
	if _, err := os.Stat(w.path); err == nil {
		os.Rename(w.path, w.path+".1")
	}
	return w.open()
}

// Close flushes and closes the current log file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// RotatedFiles lists existing log generations, newest first. It is used by
// `tidesync logs` style diagnostics.
func (w *RotatingWriter) RotatedFiles() []string {
	var out []string
	if _, err := os.Stat(w.path); err == nil {
		out = append(out, w.path)
	}
	for i := 1; i <= w.maxFiles; i++ {
		p := w.path + "." + strconv.Itoa(i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
