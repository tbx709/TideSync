package proto

import (
	"path"
	"strings"
)

// Matcher implements the exclude/include patterns used by both the agent
// (which files to publish) and the client (which files to sync).
//
// Semantics are intentionally close to .gitignore so that the rules are easy to
// reason about:
//
//   - a pattern without "/" matches the base name of any path segment,
//     e.g. "*.tmp" drops "a/b/x.tmp" and "x.tmp";
//   - a pattern containing "/" is anchored at the manifest root,
//     e.g. "logs/**" drops "logs/a/b.log" but not "x/logs/a.log";
//   - "**" matches zero or more whole segments;
//   - a trailing "/" restricts the rule to directories;
//   - a leading "!" re-includes a previously excluded path.
type Matcher struct {
	rules []rule
}

type rule struct {
	pattern  string
	segments []string
	anyDepth bool // pattern has no "/" -> match any single segment
	dirOnly  bool
	negate   bool
}

// NewMatcher compiles the given patterns. Invalid patterns are ignored rather
// than fatal: a bad exclude rule must never break a sync run.
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		r := rule{}
		if strings.HasPrefix(p, "!") {
			r.negate = true
			p = strings.TrimSpace(p[1:])
			if p == "" {
				continue
			}
		}
		p = strings.ReplaceAll(p, "\\", "/")
		if strings.HasSuffix(p, "/") {
			r.dirOnly = true
			p = strings.TrimSuffix(p, "/")
		}
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			continue
		}
		r.pattern = p
		r.anyDepth = !strings.Contains(p, "/")
		r.segments = strings.Split(p, "/")
		m.rules = append(m.rules, r)
	}
	return m
}

// Empty reports whether the matcher has no rules.
func (m *Matcher) Empty() bool { return m == nil || len(m.rules) == 0 }

// Match reports whether the slash separated relative path is excluded.
// isDir must describe the final path segment.
func (m *Matcher) Match(rel string, isDir bool) bool {
	if m.Empty() {
		return false
	}
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || rel == "." {
		return false
	}
	segs := strings.Split(rel, "/")
	excluded := false
	for _, r := range m.rules {
		if r.matches(segs, isDir) {
			excluded = !r.negate
		}
	}
	return excluded
}

func (r rule) matches(segs []string, isDir bool) bool {
	if r.anyDepth {
		// Match the base name of any segment. Every segment except the last is
		// a directory, which matters for dir-only rules.
		for i, s := range segs {
			segIsDir := i < len(segs)-1 || isDir
			if r.dirOnly && !segIsDir {
				continue
			}
			if ok, err := path.Match(r.pattern, s); err == nil && ok {
				return true
			}
		}
		return false
	}
	// An anchored rule matches the path itself or any of its parent
	// directories, so excluding "logs/old" also excludes "logs/old/a.log".
	for k := 1; k <= len(segs); k++ {
		if !matchSegments(r.segments, segs[:k]) {
			continue
		}
		if k == len(segs) {
			if !r.dirOnly || isDir {
				return true
			}
			continue
		}
		return true
	}
	return false
}

// matchSegments matches a "/" split pattern against a "/" split path, where a
// "**" pattern segment consumes zero or more path segments.
func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
}
