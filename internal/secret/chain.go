package secret

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// DefaultChain returns the standard read/write secret chain: environment
// variables in front of the persistent store (see persistentStore).
func DefaultChain() *Chain {
	return NewChain(NewEnvProvider(), persistentStore())
}

// PersistentChain returns a write-targeted chain over the persistent store
// only (no environment layer). Use it where a value must be written to the
// durable store rather than satisfied by a read-only environment override.
func PersistentChain() *Chain {
	return NewChain(persistentStore())
}

// persistentStore picks the durable secret store:
//
//  1. A directory-rooted FileProvider when EnvSecretsDir is set. This keeps a
//     Mazarbul instance's secrets in an isolated namespace so multiple brains
//     or device profiles can coexist on one host without colliding on the
//     shared keychain. An invalid value falls through to the next option.
//  2. The OS keychain when it is reachable.
//  3. A FileProvider under the user's state directory when the keychain is
//     not reachable (a headless Linux server or container), so `maz` keeps
//     working instead of failing every secret read. The files are 0600 in a
//     0700 directory, the same protection EnvSecretsDir gives.
//
//nolint:ireturn // selects between two concrete stores at runtime.
func persistentStore() Provider {
	if dir := os.Getenv(EnvSecretsDir); dir != "" {
		if fp, err := NewFileProvider(dir); err == nil {
			return fp
		}
	}
	if !keychainReachable() {
		if dir := headlessSecretsDir(); dir != "" {
			if fp, err := NewFileProvider(dir); err == nil {
				return fp
			}
		}
	}
	return NewKeychainProvider()
}

// keychainReachable reports whether the OS credential store can be used. On
// macOS and Windows it always can. On Linux go-keyring talks to the Secret
// Service over the D-Bus session bus and, when DBUS_SESSION_BUS_ADDRESS is
// unset, tries to spawn dbus-launch; on a headless host neither exists and
// every keychain call fails. A variable so tests can force either branch.
//
//nolint:gochecknoglobals // test seam over host detection.
var keychainReachable = func() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	_, err := exec.LookPath("dbus-launch")
	return err == nil
}

// headlessSecretsDir returns $XDG_STATE_HOME/mazarbul/secrets, defaulting
// XDG_STATE_HOME to ~/.local/state. Empty when no home directory is known.
func headlessSecretsDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "mazarbul", "secrets")
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
