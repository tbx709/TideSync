package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tidesync/internal/config"
	"tidesync/internal/proto"
)

func testServer(t *testing.T, root string, mutate func(*config.ServerConfig)) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.ServerDefaults()
	cfg.Root = root
	cfg.Listen = "127.0.0.1:0"
	cfg.StateDir = t.TempDir()
	cfg.Token = "secret"
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set(proto.HeaderAuth, proto.BearerPrefix+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestManifestListsEverything(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "hello")
	write(t, filepath.Join(root, "sub", "b.txt"), "world")
	write(t, filepath.Join(root, "sub", "skip.tmp"), "temp")
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, ts := testServer(t, root, func(c *config.ServerConfig) {
		c.Exclude = []string{"*.tmp"}
	})

	resp := get(t, ts.URL+proto.PathManifest+"?path=", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var man proto.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&man); err != nil {
		t.Fatal(err)
	}
	paths := map[string]proto.Entry{}
	for _, e := range man.Entries {
		paths[e.Path] = e
	}
	for _, want := range []string{"a.txt", "sub", "sub/b.txt", "emptydir"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("manifest is missing %q (has %v)", want, keys(paths))
		}
	}
	if _, ok := paths["sub/skip.tmp"]; ok {
		t.Error("excluded file was published")
	}
	if paths["a.txt"].SHA256 == "" {
		t.Error("hash_mode auto should hash a small file")
	}
	if paths["emptydir"].Type != proto.TypeDir {
		t.Error("empty directories must be published so clients can recreate them")
	}
}

func TestAuthAndTraversal(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "hello")
	_, ts := testServer(t, root, nil)

	if resp := get(t, ts.URL+proto.PathManifest+"?path=", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := get(t, ts.URL+proto.PathManifest+"?path=", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", `..\..\windows\win.ini`, "a/../../b"} {
		resp := get(t, ts.URL+proto.PathFile+"?path="+bad, "secret")
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Errorf("path %q: status = %d, want 400/404", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// Health is intentionally public so a monitor can probe the agent.
	if resp := get(t, ts.URL+proto.PathHealth, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestFileDownloadAndRange(t *testing.T) {
	root := t.TempDir()
	content := "0123456789abcdefghij"
	write(t, filepath.Join(root, "data.bin"), content)
	_, ts := testServer(t, root, nil)

	resp := get(t, ts.URL+proto.PathFile+"?path=data.bin", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != content {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("Accept-Ranges") == "" {
		t.Error("the agent must advertise range support so interrupted transfers resume")
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("a strong validator is needed for resumption")
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+proto.PathFile+"?path=data.bin", nil)
	req.Header.Set(proto.HeaderAuth, proto.BearerPrefix+"secret")
	req.Header.Set("Range", "bytes=10-")
	part, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer part.Body.Close()
	if part.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", part.StatusCode)
	}
	got, _ := io.ReadAll(part.Body)
	if string(got) != content[10:] {
		t.Errorf("range body = %q, want %q", got, content[10:])
	}
}

func TestSymlinkPolicy(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "top secret")
	write(t, filepath.Join(root, "real.txt"), "real")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "inner.txt")); err != nil {
		t.Fatal(err)
	}

	// Default: symlinks are published as links, never followed.
	_, ts := testServer(t, root, nil)
	resp := get(t, ts.URL+proto.PathManifest+"?path=", "secret")
	var man proto.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&man); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	kinds := map[string]proto.EntryType{}
	for _, e := range man.Entries {
		kinds[e.Path] = e.Type
	}
	if kinds["escape.txt"] != proto.TypeSymlink {
		t.Errorf("escape.txt type = %v, want symlink", kinds["escape.txt"])
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatal(err)
	}

	// Follow mode must still refuse to publish content outside the root.
	_, ts2 := testServer(t, root, func(c *config.ServerConfig) { c.FollowSymlinks = true })
	resp2 := get(t, ts2.URL+proto.PathManifest+"?path=", "secret")
	var man2 proto.Manifest
	if err := json.NewDecoder(resp2.Body).Decode(&man2); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	for _, e := range man2.Entries {
		if e.Path == "escape.txt" {
			t.Errorf("following symlinks published a file outside the root: %+v", e)
		}
	}
}

func TestArchiveEndpoint(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "aaa")
	write(t, filepath.Join(root, "sub", "b.txt"), "bbbbb")
	_, ts := testServer(t, root, nil)

	resp := get(t, ts.URL+proto.PathArchive+"?path=", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	found := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(tr)
			found[hdr.Name] = string(data)
		}
	}
	if found["a.txt"] != "aaa" || found["sub/b.txt"] != "bbbbb" {
		t.Errorf("archive content = %v", found)
	}
}

func TestManifestReflectsImmediateChanges(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "one")
	srv, _ := testServer(t, root, nil)
	ctx := context.Background()

	man1, _, err := srv.scanner.Scan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "a.txt"), "two")
	man2, _, err := srv.scanner.Scan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if man1.Entries[0].SHA256 == man2.Entries[0].SHA256 {
		t.Error("the scanner returned a stale digest after the file changed")
	}
	// And the HTTP layer must not serve a cached manifest either.
	write(t, filepath.Join(root, "a.txt"), "three")
	_, ts := testServer(t, root, nil)
	first := fetchManifest(t, ts.URL)
	write(t, filepath.Join(root, "a.txt"), "four")
	second := fetchManifest(t, ts.URL)
	if first.Entries[0].SHA256 == second.Entries[0].SHA256 {
		t.Error("the agent served a stale manifest; a client would miss a change")
	}
}

func fetchManifest(t *testing.T, base string) proto.Manifest {
	t.Helper()
	resp := get(t, base+proto.PathManifest+"?path=", "secret")
	defer resp.Body.Close()
	var man proto.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&man); err != nil {
		t.Fatal(err)
	}
	return man
}

func TestIPAllowList(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "x")
	_, ts := testServer(t, root, func(c *config.ServerConfig) {
		c.AllowCIDRs = []string{"10.99.0.0/16"} // never matches the test client
	})
	resp := get(t, ts.URL+proto.PathManifest+"?path=", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a client outside allow_cidrs", resp.StatusCode)
	}
}

func keys(m map[string]proto.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestManifestRejectsNonDirectoryPath(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "hello")
	_, ts := testServer(t, root, nil)
	resp := get(t, ts.URL+proto.PathManifest+"?path=a.txt", "secret")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("asking for a manifest of a regular file must fail, not return an empty tree")
	}
	var apiErr proto.Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if apiErr.Error == "" {
		t.Error("the error body should explain the problem")
	}
}

func TestManifestOfMissingPathReturns404(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "hello")
	_, ts := testServer(t, root, nil)
	resp := get(t, ts.URL+proto.PathManifest+"?path=nope", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
