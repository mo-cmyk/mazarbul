// Package correlation carries a per-request correlation ID through context.
package correlation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type correlationKey struct{}

// Header is the HTTP header name to look for an inbound correlation ID.
const Header = "X-Correlation-ID"

// WithID returns ctx carrying id. If id is empty, a new random ID is generated.
func WithID(ctx context.Context, id string) context.Context {
	if id == "" {
		id = New()
	}
	return context.WithValue(ctx, correlationKey{}, id)
}

// FromContext returns the correlation ID in ctx, or empty string if none.
func FromContext(ctx context.Context) string {
	if id, ok := ctx.Value(correlationKey{}).(string); ok {
		return id
	}
	return ""
}

// New generates a fresh 16-byte hex correlation ID.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}
