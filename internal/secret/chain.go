package secret

import (
	"context"
	"errors"
	"os"
)

// DefaultChain returns the standard read/write secret chain: environment
// variables in front of the persistent store. The persistent store is a
// directory-rooted FileProvider when EnvSecretsDir is set, otherwise the OS
// keychain. The FileProvider path keeps a Mazarbul instance's secrets in an
// isolated namespace so multiple brains or device profiles can coexist on one
// host without colliding on the shared keychain. An invalid EnvSecretsDir
// value falls back to the keychain rather than failing the process.
func DefaultChain() *Chain {
	if dir := os.Getenv(EnvSecretsDir); dir != "" {
		if fp, err := NewFileProvider(dir); err == nil {
			return NewChain(NewEnvProvider(), fp)
		}
	}
	return NewChain(NewEnvProvider(), NewKeychainProvider())
}

// PersistentChain returns a write-targeted chain over the persistent store
// only (no environment layer), honoring EnvSecretsDir the same way
// DefaultChain does. Use it where a value must be written to the durable
// store rather than satisfied by a read-only environment override.
func PersistentChain() *Chain {
	if dir := os.Getenv(EnvSecretsDir); dir != "" {
		if fp, err := NewFileProvider(dir); err == nil {
			return NewChain(fp)
		}
	}
	return NewChain(NewKeychainProvider())
}

// Chain composes multiple Providers into one. Reads consult each member in
// order and return the first non-error result. Writes (Set, Delete) target
// the first member that does not return ErrReadOnly.
//
// Typical use: Chain(env, keychain). The env layer wins in CI without a
// branch at the call site; the keychain layer absorbs writes so `maz init`
// stores the user's key persistently. A Chain with only read-only members
// returns ErrReadOnly from Set and Delete.
type Chain struct {
	providers []Provider
}

// NewChain returns a Chain over the given providers. Order matters: the
// first provider is consulted first on Get, and is the first candidate for
// Set and Delete.
func NewChain(providers ...Provider) *Chain {
	return &Chain{providers: providers}
}

// Get returns the first hit. It returns ErrNotFound only when every
// provider returns ErrNotFound. Any non-ErrNotFound error from a member
// short-circuits the chain.
func (c *Chain) Get(ctx context.Context, name string) (string, error) {
	for _, p := range c.providers {
		v, err := p.Get(ctx, name)
		switch {
		case err == nil:
			return v, nil
		case errors.Is(err, ErrNotFound):
			continue
		default:
			return "", err
		}
	}
	return "", ErrNotFound
}

// Set writes to the first provider that accepts writes. It returns
// ErrReadOnly when every member is read-only.
func (c *Chain) Set(ctx context.Context, name string, value string) error {
	for _, p := range c.providers {
		err := p.Set(ctx, name, value)
		if errors.Is(err, ErrReadOnly) {
			continue
		}
		return err
	}
	return ErrReadOnly
}

// Delete removes from the first provider that accepts writes. It returns
// ErrReadOnly when every member is read-only.
func (c *Chain) Delete(ctx context.Context, name string) error {
	for _, p := range c.providers {
		err := p.Delete(ctx, name)
		if errors.Is(err, ErrReadOnly) {
			continue
		}
		return err
	}
	return ErrReadOnly
}
