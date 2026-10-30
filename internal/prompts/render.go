package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// inputHash returns the hex-encoded SHA-256 of the canonical JSON
// serialisation of vars. Keys are sorted deterministically so that two
// RenderVars maps with the same entries but different insertion order
// produce the same hash. An empty or nil vars produces the hash of "{}".
func inputHash(vars RenderVars) string {
	b := canonicalJSON(vars)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalJSON serialises vars as a stable, lexicographically key-sorted JSON
// object. It is kept separate from inputHash so tests can verify the wire
// shape in isolation.
func canonicalJSON(vars RenderVars) []byte {
	if len(vars) == 0 {
		return []byte("{}")
	}
	// encoding/json sorts map[string]string keys lexicographically, producing
	// a stable canonical form regardless of map insertion order.
	b, err := json.Marshal(map[string]string(vars))
	if err != nil {
		// json.Marshal on map[string]string cannot fail; return safe fallback
		// rather than panic (library code must not panic).
		return []byte("{}")
	}
	return b
}
