// Package proto defines the wire types and path rules shared by the TideSync
// agent (server) and the TideSync client.
//
// The protocol is deliberately dependency free: plain JSON over HTTP with
// optional bearer-token authentication, so that the same code runs unchanged
// on Windows, Linux, x86-64, ARM64 and 32-bit ARM devices such as a Raspberry
// Pi or an old ARM NAS.
package proto

import (
	"errors"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Version is the protocol revision implemented by this build. The client
// refuses to talk to an agent whose major version differs.
const Version = 1

// HTTP surface of the agent.
const (
	Prefix       = "/api/v1"
	PathInfo     = Prefix + "/info"
	PathManifest = Prefix + "/manifest"
	PathFile     = Prefix + "/file"
	PathArchive  = Prefix + "/archive"
	PathHealth   = "/healthz"

	// DefaultPort is used when neither the config nor the CLI sets one.
	DefaultPort = 8787
)

// Header names carrying out-of-band metadata.
const (
	// HeaderAuth carries "Bearer <token>".
	HeaderAuth = "Authorization"
	// BearerPrefix precedes the shared token.
	BearerPrefix = "Bearer "
	// HeaderSHA256 carries the digest of a file body when the agent has it.
	HeaderSHA256 = "X-Tidesync-Sha256"
	// HeaderAgent identifies the agent build that answered.
	HeaderAgent = "X-Tidesync-Agent"
	// HeaderComputedAt timestamps a generated body (used by the archive).
	HeaderComputedAt = "X-Tidesync-Generated"
)

// EntryType classifies a manifest row.
type EntryType string

// Entry types.
const (
	TypeFile    EntryType = "file"
	TypeDir     EntryType = "dir"
	TypeSymlink EntryType = "symlink"
)

// Entry is one row of a manifest, always relative to the manifest root and
// always slash separated so that manifests are portable between Windows and
// Linux agents.
type Entry struct {
	Path    string    `json:"path"`
	Type    EntryType `json:"type"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mtime"`
	Mode    uint32    `json:"mode,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	Target  string    `json:"target,omitempty"`
}

// IsFile reports whether the entry is a regular file.
func (e Entry) IsFile() bool { return e.Type == TypeFile }

// IsDir reports whether the entry is a directory.
func (e Entry) IsDir() bool { return e.Type == TypeDir }

// Manifest is the complete description of a served subtree.
type Manifest struct {
	Version     int       `json:"version"`
	Root        string    `json:"root"`
	GeneratedAt time.Time `json:"generated_at"`
	Hashed      bool      `json:"hashed"`
	Truncated   bool      `json:"truncated,omitempty"`
	Entries     []Entry   `json:"entries"`
}

// FileCount returns the number of regular files in the manifest.
func (m *Manifest) FileCount() int {
	n := 0
	for _, e := range m.Entries {
		if e.IsFile() {
			n++
		}
	}
	return n
}

// TotalSize returns the sum of the sizes of all regular files.
func (m *Manifest) TotalSize() int64 {
	var n int64
	for _, e := range m.Entries {
		if e.IsFile() {
			n += e.Size
		}
	}
	return n
}

// Info is returned by the agent to describe itself.
type Info struct {
	Version      int       `json:"version"`
	App          string    `json:"app"`
	AgentVersion string    `json:"agent_version"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Hostname     string    `json:"hostname"`
	Root         string    `json:"root"`
	ServerTime   time.Time `json:"server_time"`
	Features     []string  `json:"features"`
	ReadOnly     bool      `json:"read_only"`
}

// Error is the JSON body returned for every non-2xx API response.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// ErrUnsafePath is returned for any client supplied path that is absolute,
// escapes the served root, or contains a NUL byte.
var ErrUnsafePath = errors.New("unsafe path")

// CleanRel normalises a client supplied relative path and rejects anything
// that could escape the served root. The empty string and "." both denote the
// root itself.
func CleanRel(p string) (string, error) {
	p = strings.TrimSpace(p)
	// Windows clients may send backslashes; normalise before validating.
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.IndexByte(p, 0) >= 0 {
		return "", ErrUnsafePath
	}
	if p == "" || p == "." || p == "/" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", ErrUnsafePath
	}
	// Reject drive letters (C:/x) and UNC remnants.
	if len(p) >= 2 && p[1] == ':' {
		return "", ErrUnsafePath
	}
	clean := path.Clean(p)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrUnsafePath
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", ErrUnsafePath
		}
	}
	return clean, nil
}

// Resolve joins a cleaned relative path onto root and verifies that the result
// stays below root. It is the only supported way to turn a request path into a
// filesystem path.
func Resolve(root, rel string) (string, error) {
	clean, err := CleanRel(rel)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	r, err := filepath.Rel(root, full)
	if err != nil {
		return "", ErrUnsafePath
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", ErrUnsafePath
	}
	return full, nil
}

// ToSlash converts a filesystem relative path to its wire representation.
func ToSlash(p string) string { return filepath.ToSlash(p) }

// Rel returns the slash separated path of full relative to root.
func Rel(root, full string) (string, error) {
	r, err := filepath.Rel(root, full)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(r), nil
}
