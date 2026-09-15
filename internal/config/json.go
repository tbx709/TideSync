package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that unmarshals from either a Go duration string
// ("5m", "1h30m") or a plain number of seconds (300). It marshals back as a
// human readable string.
type Duration time.Duration

// D wraps a time.Duration.
func D(d time.Duration) Duration { return Duration(d) }

// Duration returns the underlying value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Or returns the underlying value, or def when unset.
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

// String implements fmt.Stringer.
func (d Duration) String() string {
	if d == 0 {
		return "0s"
	}
	return time.Duration(d).String()
}

// UnmarshalJSON accepts "5m", "300s", 300 or "300".
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		*d = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		v, err := ParseDuration(str)
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	// Bare number: seconds, fractional allowed.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(time.Duration(f * float64(time.Second)))
	return nil
}

// MarshalJSON renders the duration as a string such as "5m0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// ParseDuration parses "5m", "1h30m", "500ms", "90" (seconds) or "0".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(v * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: want forms like 30s, 5m, 1h30m", s)
	}
	return d, nil
}

// stripJSONExtras removes // and /* */ comments plus trailing commas from a
// JSON document so that configuration files can be commented by hand. The
// scanner is string aware, so comment markers inside string values survive.
func stripJSONExtras(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString := false
	escaped := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				i++
			}
			if i < len(in) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(in) && in[i+1] == '*':
			i += 2
			for i+1 < len(in) && !(in[i] == '*' && in[i+1] == '/') {
				i++
			}
			i++ // skip the closing '/'
			out = append(out, ' ')
		default:
			out = append(out, c)
		}
	}
	// Second pass: drop commas that precede a closing brace or bracket.
	cleaned := make([]byte, 0, len(out))
	inString = false
	escaped = false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			cleaned = append(cleaned, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			cleaned = append(cleaned, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\r' || out[j] == '\n') {
				j++
			}
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				continue // drop the trailing comma
			}
		}
		cleaned = append(cleaned, c)
	}
	return cleaned
}

// expandEnv replaces ${VAR} and ${VAR:-default} occurrences. A bare $VAR is
// left untouched so that tokens containing '$' are not mangled.
func expandEnv(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteString(s[i:])
				break
			}
			expr := s[i+2 : i+2+end]
			name, def := expr, ""
			if idx := strings.Index(expr, ":-"); idx >= 0 {
				name, def = expr[:idx], expr[idx+2:]
			}
			if v, ok := os.LookupEnv(name); ok && v != "" {
				b.WriteString(v)
			} else {
				b.WriteString(def)
			}
			i += 2 + end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// decodeFile reads, cleans and unmarshals a JSON configuration file into v.
func decodeFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	cleaned := stripJSONExtras(raw)
	dec := json.NewDecoder(strings.NewReader(string(cleaned)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// WriteFile renders v as indented JSON, creating parent directories.
func WriteFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
