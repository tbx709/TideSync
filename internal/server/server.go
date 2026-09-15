// Package server implements the TideSync agent: a read-only HTTP file service
// that publishes one directory to the local network.
package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tidesync/internal/config"
	"tidesync/internal/proto"
)

// Server is a running agent instance.
type Server struct {
	cfg     config.ServerConfig
	log     *slog.Logger
	matcher *proto.Matcher
	cache   *hashCache
	scanner *Scanner
	http    *http.Server

	scanMu    sync.Mutex
	manifest  *manifestCacheEntry
	sem       chan struct{}
	startedAt time.Time

	byteCount syncAtoms
}

type manifestCacheEntry struct {
	key      string
	manifest *proto.Manifest
	stored   time.Time
}

// New builds an agent from a validated configuration.
func New(cfg config.ServerConfig, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	excludes := append([]string{}, cfg.Exclude...)

	// The hash cache must never be published, even when state_dir lives inside
	// the served root.
	cachePath := filepath.Join(cfg.StateDir, "hashcache-"+shortKey(cfg.Root)+".json")
	if rel, err := filepath.Rel(cfg.Root, cachePath); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		excludes = append(excludes, proto.ToSlash(rel))
	}

	cache := newHashCache(cachePath)
	if err := cache.load(); err != nil {
		log.Warn("could not read hash cache", "path", cachePath, "error", err.Error())
	}
	matcher := proto.NewMatcher(excludes)
	s := &Server{
		cfg:       cfg,
		log:       log,
		matcher:   matcher,
		cache:     cache,
		startedAt: time.Now(),
		sem:       make(chan struct{}, cfg.MaxConcurrent),
	}
	s.scanner = &Scanner{
		Root:       cfg.Root,
		Matcher:    matcher,
		HashMode:   cfg.HashMode,
		HashMax:    int64(cfg.HashMaxSizeMB) << 20,
		HashGrace:  cfg.HashGrace.Or(2 * time.Second),
		UseCache:   cfg.HashCache == nil || *cfg.HashCache,
		FollowLink: cfg.FollowSymlinks,
		MaxEntries: cfg.ManifestMaxEntries,
		Cache:      cache,
		Log:        log,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(proto.PathHealth, s.handleHealth)
	mux.HandleFunc(proto.PathInfo, s.guard(s.handleInfo))
	mux.HandleFunc(proto.PathManifest, s.guard(s.handleManifest))
	mux.HandleFunc(proto.PathFile, s.guard(s.handleFile))
	mux.HandleFunc(proto.PathArchive, s.guard(s.handleArchive))
	mux.HandleFunc("/", s.handleRoot)

	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.withLogging(mux),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.cfg.Listen }

// Handler exposes the HTTP surface, which lets tests drive the real agent
// without opening a socket.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// ListenAndServe blocks until the agent stops.
func (s *Server) ListenAndServe() error {
	scheme := "http"
	if s.cfg.TLSCert != "" {
		scheme = "https"
	}
	hn, _ := os.Hostname()
	s.log.Info("tidesync agent listening",
		"addr", s.cfg.Listen,
		"scheme", scheme,
		"root", s.cfg.Root,
		"hostname", hn,
		"auth", s.cfg.Token != "",
		"hash_mode", s.cfg.HashMode,
		"exclude_rules", len(s.cfg.Exclude),
		"state_dir", s.cfg.StateDir,
	)
	if s.cfg.Token == "" {
		s.log.Warn("no token configured: anyone on the local network can read the published directory")
	}
	if s.cfg.TLSCert != "" {
		return s.http.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
	}
	return s.http.ListenAndServe()
}

// Shutdown stops the agent gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// ---- middleware -----------------------------------------------------------

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		defer func() {
			if rv := recover(); rv != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(rv))
				if !rec.wrote {
					writeError(rec, http.StatusInternalServerError, "internal error")
				}
			}
			lvl := slog.LevelDebug
			if rec.status >= 500 {
				lvl = slog.LevelError
			} else if rec.status >= 400 {
				lvl = slog.LevelWarn
			}
			s.log.Log(r.Context(), lvl, "request",
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", rec.status,
				"bytes", rec.bytes,
				"remote", clientIP(r),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// guard enforces the IP allow list and the shared token.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ipAllowed(r) {
			s.log.Warn("rejected client outside allow_cidrs", "remote", clientIP(r))
			writeError(w, http.StatusForbidden, "client address not allowed")
			return
		}
		if s.cfg.Token != "" {
			got := r.Header.Get(proto.HeaderAuth)
			got = strings.TrimSpace(strings.TrimPrefix(got, proto.BearerPrefix))
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, "invalid or missing token")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) ipAllowed(r *http.Request) bool {
	if len(s.cfg.AllowCIDRs) == 0 {
		return true
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return false
	}
	for _, entry := range s.cfg.AllowCIDRs {
		if _, netw, err := net.ParseCIDR(entry); err == nil {
			if netw.Contains(ip) {
				return true
			}
			continue
		}
		if parsed := net.ParseIP(entry); parsed != nil && parsed.Equal(ip) {
			return true
		}
	}
	return false
}

// ---- handlers -------------------------------------------------------------

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "unknown endpoint")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "TideSync agent\nendpoints:\n  %s\n  %s?path=REL\n  %s?path=REL\n  %s?path=REL\n  %s\n",
		proto.PathInfo, proto.PathManifest, proto.PathFile, proto.PathArchive, proto.PathHealth)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "ok uptime=%s\n", time.Since(s.startedAt).Round(time.Second))
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	hn, _ := os.Hostname()
	info := proto.Info{
		Version:      proto.Version,
		App:          "tidesync-agent",
		AgentVersion: Version,
		OS:           goos(),
		Arch:         goarch(),
		Hostname:     hn,
		Root:         s.cfg.Root,
		ServerTime:   time.Now(),
		ReadOnly:     true,
		Features:     []string{"sha256", "range", "archive", "manifest"},
	}
	if s.cfg.ArchiveEnabled != nil && !*s.cfg.ArchiveEnabled {
		info.Features = []string{"sha256", "range", "manifest"}
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	sub := r.URL.Query().Get("path")
	clean, err := proto.CleanRel(sub)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	mode := s.cfg.HashMode
	switch r.URL.Query().Get("hash") {
	case "1", "true", "yes":
		mode = "always"
	case "0", "false", "no":
		mode = "never"
	}
	key := clean + "|" + mode

	man, err := s.manifestFor(r.Context(), key, clean, mode)
	if err != nil {
		if errors.Is(err, proto.ErrUnsafePath) {
			writeError(w, http.StatusBadRequest, "invalid path")
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "path not found on agent")
			return
		}
		s.log.Error("manifest failed", "path", clean, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "scan failed: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, man)
}

// manifestFor scans under a per-server lock.
//
// A manifest produced after this request arrived is reused, which coalesces the
// burst of requests created when several clients sync at the same moment. A
// manifest produced *before* the request arrived is never reused, so a client
// that syncs right after a file changed always observes the change. Setting
// server.manifest_ttl enables explicit reuse for very large trees with many
// clients.
func (s *Server) manifestFor(ctx context.Context, key, sub, mode string) (*proto.Manifest, error) {
	arrived := time.Now()
	ttl := s.cfg.ManifestTTL.Duration()

	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.manifest != nil && s.manifest.key == key {
		reusable := s.manifest.stored.After(arrived)
		if !reusable && ttl > 0 && time.Since(s.manifest.stored) < ttl {
			reusable = true
		}
		if reusable {
			return s.manifest.manifest, nil
		}
	}
	sc := *s.scanner
	sc.HashMode = mode
	man, stats, err := sc.Scan(ctx, sub)
	if err != nil {
		return nil, err
	}
	s.log.Info("scanned",
		"path", sub,
		"files", stats.Files,
		"dirs", stats.Dirs,
		"bytes", stats.Bytes,
		"hashed", stats.HashHits+stats.HashMisses,
		"hash_cache_hits", stats.HashHits,
		"skipped", stats.Skipped,
		"truncated", stats.Truncated,
		"duration_ms", stats.Duration.Milliseconds(),
	)
	s.manifest = &manifestCacheEntry{key: key, manifest: man, stored: time.Now()}
	return man, nil
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	clean, err := proto.CleanRel(r.URL.Query().Get("path"))
	if err != nil || clean == "" {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	full, err := proto.Resolve(s.cfg.Root, clean)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	release, ok := s.acquire(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "agent is busy, retry later")
		return
	}
	defer release()

	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found on agent")
			return
		}
		if os.IsPermission(err) {
			writeError(w, http.StatusForbidden, "file not readable by agent")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !st.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "not a regular file")
		return
	}

	etag := `"sm:` + strconv.FormatInt(st.Size(), 10) + "-" + strconv.FormatInt(st.ModTime().UnixNano(), 10) + `"`
	sum := ""
	if s.cache != nil {
		if cached, ok := s.cache.get(clean, st.Size(), st.ModTime()); ok {
			sum = cached
			etag = `"sha256:` + cached + `"`
		}
	}
	w.Header().Set("ETag", etag)
	w.Header().Set(proto.HeaderSHA256, sum)
	w.Header().Set(proto.HeaderAgent, Version)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	var body io.ReadSeeker = f
	if s.cfg.RateLimitKBps > 0 {
		body = &rateLimitedReadSeeker{rs: f, t: newThrottle(s.cfg.RateLimitKBps)}
	}
	http.ServeContent(w, r, filepath.Base(full), st.ModTime(), body)
	s.addBytes(st.Size())
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ArchiveEnabled != nil && !*s.cfg.ArchiveEnabled {
		writeError(w, http.StatusNotFound, "archive endpoint disabled")
		return
	}
	clean, err := proto.CleanRel(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	man, err := s.manifestFor(r.Context(), "archive|"+clean, clean, "never")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	release, ok := s.acquire(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "agent is busy, retry later")
		return
	}
	defer release()

	root, err := proto.Resolve(s.cfg.Root, clean)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar+gzip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(proto.HeaderAgent, Version)
	w.Header().Set(proto.HeaderComputedAt, time.Now().UTC().Format(time.RFC3339))
	w.WriteHeader(http.StatusOK)

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, e := range man.Entries {
		if err := r.Context().Err(); err != nil {
			return
		}
		full, err := proto.Resolve(root, e.Path)
		if err != nil {
			continue
		}
		switch e.Type {
		case proto.TypeDir:
			hdr := &tar.Header{
				Name:     e.Path + "/",
				Typeflag: tar.TypeDir,
				Mode:     int64(e.Mode),
				ModTime:  e.ModTime,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				s.log.Warn("archive write failed", "error", err.Error())
				return
			}
		case proto.TypeSymlink:
			hdr := &tar.Header{
				Name:     e.Path,
				Typeflag: tar.TypeSymlink,
				Linkname: e.Target,
				Mode:     int64(e.Mode),
				ModTime:  e.ModTime,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return
			}
		case proto.TypeFile:
			f, err := os.Open(full)
			if err != nil {
				s.log.Warn("archive: cannot open file", "path", e.Path, "error", err.Error())
				continue
			}
			hdr := &tar.Header{
				Name:     e.Path,
				Typeflag: tar.TypeReg,
				Mode:     int64(e.Mode),
				Size:     e.Size,
				ModTime:  e.ModTime,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				f.Close()
				return
			}
			n, err := io.Copy(tw, f)
			f.Close()
			s.addBytes(n)
			if err != nil {
				s.log.Warn("archive: copy failed", "path", e.Path, "error", err.Error())
				return
			}
		}
	}
}

// ---- helpers --------------------------------------------------------------

func (s *Server) acquire(ctx context.Context) (func(), bool) {
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, true
	default:
	}
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, true
	case <-time.After(30 * time.Second):
		return func() {}, false
	case <-ctx.Done():
		return func() {}, false
	}
}

func (s *Server) addBytes(n int64) {
	s.byteCount.add(n)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, proto.Error{Error: msg})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusRecorder captures the status code and byte count for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Flush keeps streaming responses working through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rateLimitedReadSeeker throttles reads to an average rate.
type rateLimitedReadSeeker struct {
	rs io.ReadSeeker
	t  *throttle
}

func (r *rateLimitedReadSeeker) Read(p []byte) (int, error) {
	n, err := r.rs.Read(p)
	if n > 0 {
		r.t.throttle(n)
	}
	return n, err
}

func (r *rateLimitedReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.rs.Seek(offset, whence)
}

type throttle struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
}

func newThrottle(kbps int) *throttle {
	rate := float64(kbps) * 1024
	return &throttle{rate: rate, burst: rate, tokens: rate, last: time.Now()}
}

func (t *throttle) throttle(n int) {
	if t == nil || t.rate <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.tokens += now.Sub(t.last).Seconds() * t.rate
	t.last = now
	if t.tokens > t.burst {
		t.tokens = t.burst
	}
	t.tokens -= float64(n)
	if t.tokens < 0 {
		sleep := time.Duration(-t.tokens / t.rate * float64(time.Second))
		t.tokens = 0
		if sleep > 0 {
			time.Sleep(sleep)
			t.last = time.Now()
		}
	}
}

// syncAtoms is a tiny atomic counter that avoids importing sync/atomic at every
// call site while remaining race free.
type syncAtoms struct {
	mu sync.Mutex
	n  int64
}

func (a *syncAtoms) add(delta int64) {
	a.mu.Lock()
	a.n += delta
	a.mu.Unlock()
}

// Value returns the current counter.
func (a *syncAtoms) Value() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

func shortKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:10]
}
