// Package client implements the TideSync synchroniser: it pulls a published
// directory from an agent to a local directory, incrementally, with retries,
// integrity verification and atomic replacement.
package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"tidesync/internal/config"
	"tidesync/internal/proto"
)

// Remote is an HTTP client for one TideSync agent.
type Remote struct {
	base      string
	token     string
	remoteSub string
	hc        *http.Client
	retries   int
	backoff   time.Duration
	log       *slog.Logger
	agent     string
}

// NewRemote builds a Remote from a client configuration.
func NewRemote(cfg config.ClientConfig, agentVersion string, log *slog.Logger) (*Remote, error) {
	if log == nil {
		log = slog.Default()
	}
	base := strings.TrimRight(cfg.Server.URL, "/")
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("server.url %q: %w", base, err)
	}
	sub, err := proto.CleanRel(cfg.Server.RemotePath)
	if err != nil {
		return nil, fmt.Errorf("server.remote_path: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Server.InsecureSkipVerify {
		tlsCfg.InsecureSkipVerify = true
	}
	if cfg.Server.CACert != "" {
		pem, err := os.ReadFile(cfg.Server.CACert)
		if err != nil {
			return nil, fmt.Errorf("server.ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server.ca_cert %s contains no usable certificate", cfg.Server.CACert)
		}
		tlsCfg.RootCAs = pool
	}
	timeout := cfg.Server.Timeout.Or(30 * time.Second)
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       tlsCfg,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &Remote{
		base:      base,
		token:     cfg.Server.Token,
		remoteSub: sub,
		hc: &http.Client{
			Transport: transport,
			// No global client timeout: large transfers are streamed and
			// bounded by the per-request context instead.
			Timeout: 0,
		},
		retries: cfg.Sync.Retries,
		backoff: cfg.Sync.RetryBackoff.Or(2 * time.Second),
		log:     log,
		agent:   agentVersion,
	}, nil
}

// Sub returns the configured remote sub path.
func (r *Remote) Sub() string { return r.remoteSub }

// URL returns the agent base URL.
func (r *Remote) URL() string { return r.base }

// join builds a request URL for an API path.
func (r *Remote) join(api, rel string) string {
	u := r.base + api
	q := url.Values{}
	full := rel
	if r.remoteSub != "" {
		if full == "" {
			full = r.remoteSub
		} else {
			full = r.remoteSub + "/" + rel
		}
	}
	if full != "" {
		q.Set("path", full)
	}
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return u
}

func (r *Remote) newRequest(ctx context.Context, method, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if r.token != "" {
		req.Header.Set(proto.HeaderAuth, proto.BearerPrefix+r.token)
	}
	req.Header.Set("User-Agent", "tidesync/"+r.agent)
	req.Header.Set("Accept", "application/json, application/octet-stream")
	return req, nil
}

// apiError is a non-2xx response from the agent.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("agent returned HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// checksumError marks a transfer whose bytes did not match the expected digest.
// It is retried, but without the exponential backoff used for transport
// failures, because the connection itself was fine.
type checksumError struct{ msg string }

func (e *checksumError) Error() string { return e.msg }

// retryable reports whether an error is worth another attempt.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.Status >= 500, ae.Status == 408, ae.Status == 429:
			return true
		default:
			return false
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Transport level failures (refused, reset, EOF) are always retryable.
	return true
}

// withRetry runs op until it succeeds, a non-retryable error occurs, or the
// retry budget is exhausted. The delay doubles from RetryBackoff, with jitter.
func (r *Remote) withRetry(ctx context.Context, what string, op func(attempt int) error) error {
	attempts := r.retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	delay := r.backoff
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		lastErr = op(i)
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) {
			return lastErr
		}
		if i == attempts-1 {
			break
		}
		var bad *checksumError
		wait := delay + time.Duration(rand.Int63n(int64(delay/2+1)))
		if errors.As(lastErr, &bad) {
			// Re-download immediately: nothing is wrong with the network.
			wait = r.backoff
		}
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		r.log.Warn("retrying", "op", what, "attempt", i+1, "of", attempts, "in", wait.Round(time.Millisecond).String(), "error", lastErr.Error())
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return lastErr
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
	return lastErr
}

// Info fetches the agent description; it doubles as a connectivity probe.
func (r *Remote) Info(ctx context.Context) (*proto.Info, error) {
	var info proto.Info
	err := r.withRetry(ctx, "info", func(int) error {
		req, err := r.newRequest(ctx, http.MethodGet, r.base+proto.PathInfo)
		if err != nil {
			return err
		}
		resp, err := r.hc.Do(req)
		if err != nil {
			return err
		}
		defer drainClose(resp)
		if resp.StatusCode != http.StatusOK {
			return apiErrorFrom(resp)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info)
	})
	if err != nil {
		return nil, err
	}
	if info.Version != proto.Version {
		return &info, fmt.Errorf("agent speaks protocol v%d but this client speaks v%d: upgrade both sides",
			info.Version, proto.Version)
	}
	return &info, nil
}

// Manifest fetches the file list. hash forces (true) or suppresses (false)
// digests; the agent decides by configuration when the pointer is nil.
func (r *Remote) Manifest(ctx context.Context, hash *bool) (*proto.Manifest, error) {
	u := r.join(proto.PathManifest, "")
	if hash != nil {
		sep := "&"
		if !strings.Contains(u, "?") {
			sep = "?"
		}
		if *hash {
			u += sep + "hash=1"
		} else {
			u += sep + "hash=0"
		}
	}
	var man proto.Manifest
	err := r.withRetry(ctx, "manifest", func(int) error {
		req, err := r.newRequest(ctx, http.MethodGet, u)
		if err != nil {
			return err
		}
		resp, err := r.hc.Do(req)
		if err != nil {
			return err
		}
		defer drainClose(resp)
		if resp.StatusCode != http.StatusOK {
			return apiErrorFrom(resp)
		}
		dec := json.NewDecoder(resp.Body)
		return dec.Decode(&man)
	})
	if err != nil {
		return nil, err
	}
	if man.Version != proto.Version {
		return nil, fmt.Errorf("agent manifest version %d is not supported (want %d)", man.Version, proto.Version)
	}
	return &man, nil
}

// DownloadResult reports what a single file transfer produced.
type DownloadResult struct {
	Bytes    int64
	SHA256   string
	Resumed  bool
	Attempts int
}

// DownloadTo streams one file into tmp (created if missing, resumed when it
// already holds a partial transfer) and returns the digest of the result.
func (r *Remote) DownloadTo(ctx context.Context, rel, tmp string, expectSHA string, limitKBps int) (*DownloadResult, error) {
	res := &DownloadResult{}
	err := r.withRetry(ctx, "download "+rel, func(attempt int) error {
		res.Attempts = attempt + 1
		n, sum, resumed, err := r.transferOnce(ctx, rel, tmp, limitKBps)
		if err != nil {
			return err
		}
		res.Bytes, res.SHA256, res.Resumed = n, sum, resumed
		if expectSHA != "" && !strings.EqualFold(sum, expectSHA) {
			// A corrupted transfer restarts from scratch on the next attempt.
			os.Remove(tmp)
			return &checksumError{msg: fmt.Sprintf("checksum mismatch for %s: got %s want %s",
				rel, short(sum), short(expectSHA))}
		}
		return nil
	})
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return res, nil
}

// transferOnce performs a single HTTP GET, appending to an existing partial
// file when the agent supports ranges.
func (r *Remote) transferOnce(ctx context.Context, rel, tmp string, limitKBps int) (int64, string, bool, error) {
	offset := int64(0)
	if st, err := os.Stat(tmp); err == nil && st.Size() > 0 {
		offset = st.Size()
	}
	u := r.join(proto.PathFile, rel)
	req, err := r.newRequest(ctx, http.MethodGet, u)
	if err != nil {
		return 0, "", false, err
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, "", false, err
	}
	defer resp.Body.Close()

	resumed := false
	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 {
			// The agent ignored the range: restart from zero.
			offset = 0
		}
	case http.StatusPartialContent:
		resumed = offset > 0
		if !resumed {
			// A 206 for a fresh download means the agent streamed a range we
			// did not ask for; safest is to restart.
			offset = 0
		}
	default:
		drainClose(resp)
		return 0, "", false, apiErrorFrom(resp)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resumed {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return 0, "", false, err
	}
	defer f.Close()

	// Because a resumed transfer cannot hash the bytes already on disk, the
	// prefix is hashed first so that the digest still covers the whole file.
	h := sha256.New()
	if resumed {
		prefix, err := os.Open(tmp)
		if err != nil {
			return 0, "", false, err
		}
		if _, err := io.Copy(h, prefix); err != nil {
			prefix.Close()
			return 0, "", false, err
		}
		prefix.Close()
	}

	var src io.Reader = resp.Body
	if limitKBps > 0 {
		src = newThrottledReader(resp.Body, limitKBps)
	}
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		return offset + n, "", resumed, err
	}
	if err := f.Sync(); err != nil {
		return offset + n, "", resumed, err
	}
	if cl := resp.ContentLength; cl >= 0 && resp.StatusCode == http.StatusOK {
		if n != cl {
			return n, "", resumed, fmt.Errorf("short read: got %d of %d bytes", n, cl)
		}
	}
	return offset + n, hex.EncodeToString(h.Sum(nil)), resumed, nil
}

// OpenArchive starts a bulk tar.gz download of the configured remote path.
func (r *Remote) OpenArchive(ctx context.Context, sub string) (io.ReadCloser, error) {
	u := r.join(proto.PathArchive, sub)
	req, err := r.newRequest(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer drainClose(resp)
		return nil, apiErrorFrom(resp)
	}
	return resp.Body, nil
}

func apiErrorFrom(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	msg := strings.TrimSpace(string(body))
	var pe proto.Error
	if json.Unmarshal(body, &pe) == nil && pe.Error != "" {
		msg = pe.Error
	}
	return &apiError{Status: resp.StatusCode, Body: msg}
}

func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// throttledReader limits the average read rate to kbps kilobytes per second.
type throttledReader struct {
	r io.Reader
	t *clientThrottle
}

func newThrottledReader(r io.Reader, kbps int) *throttledReader {
	return &throttledReader{r: r, t: newClientThrottle(kbps)}
}

func (t *throttledReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.t.wait(n)
	}
	return n, err
}

type clientThrottle struct {
	rate   float64
	tokens float64
	last   time.Time
}

func newClientThrottle(kbps int) *clientThrottle {
	rate := float64(kbps) * 1024
	return &clientThrottle{rate: rate, tokens: rate, last: time.Now()}
}

func (t *clientThrottle) wait(n int) {
	if t == nil || t.rate <= 0 {
		return
	}
	now := time.Now()
	t.tokens += now.Sub(t.last).Seconds() * t.rate
	t.last = now
	if t.tokens > t.rate {
		t.tokens = t.rate
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
