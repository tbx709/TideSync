package config

import (
	"crypto/sha256"
	"encoding/hex"
)

// shortHash returns the first 10 hex digits of the SHA-256 of s. It is used to
// derive stable per-job file names from the job identity.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:10]
}
