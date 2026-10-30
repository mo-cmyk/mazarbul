package chunk

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentHash returns the canonical content hash of a chunk file's raw
// bytes: lowercase hex SHA-256. It is the single source of truth for the
// chunk-integrity contract — the producer records it in MANIFEST.json and
// the consumer recomputes it over the bytes it pulled. A mismatch means
// the chunk was edited outside Mazarbul. Hashing the framed bytes (not the
// sealed payload) lets the same function cover plaintext and encrypted
// chunks identically.
func ContentHash(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}
