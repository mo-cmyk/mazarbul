package config

import (
	"context"
	"sync"
)

// SelfBinder resolves the device-local self->principalUID binding for the
// write-boundary self-resolution sites (the fact/relate MCP tools, document
// frontmatter, and the extractor's first-person path). It reads the binding
// from state.toml lazily and caches the first successful resolution: the
// binding is written once at genesis and is immutable thereafter, so a single
// successful read is authoritative for the process lifetime.
//
// Until the device is bound (no Watchword yet, or genesis deferred) every
// call returns ("", false), so callers leave the "self" sentinel verbatim and
// preserve pre-identity behavior.
type SelfBinder struct {
	dataDir string

	mu     sync.Mutex
	cached string
}

// NewSelfBinder returns a binder reading the self binding from dataDir's
// state.toml.
func NewSelfBinder(dataDir string) *SelfBinder {
	return &SelfBinder{dataDir: dataDir}
}

// SelfPrincipal returns the bound principal UID and true once the device is
// bound, else ("", false). The ctx parameter keeps the signature uniform with
// other resolver interfaces; the underlying read is local and synchronous.
func (b *SelfBinder) SelfPrincipal(_ context.Context) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cached != "" {
		return b.cached, true
	}
	uid, err := SelfPrincipalUID(b.dataDir)
	if err != nil || uid == "" {
		return "", false
	}
	b.cached = uid
	return uid, true
}
