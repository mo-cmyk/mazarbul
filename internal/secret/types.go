// Package secret defines the Provider interface used by Mazarbul to read
// and write third-party API keys (Anthropic, OpenAI, etc.) and other
// sensitive material. Implementations include the OS keychain (default)
// and environment variables (CI fallback).
//
// Callers should never log a value returned by Get; the package itself
// never logs values either. Use Chain to compose a read-only provider
// (env) in front of a writable one (keychain) so CI gets to win without
// branching at the call site.
package secret

import (
	"context"
	"errors"
)

// Provider reads, writes, and deletes named secrets. Implementations are
// safe for concurrent use unless their docs say otherwise.
type Provider interface {
	// Get returns the secret value for name. It returns ErrNotFound when
	// the secret is not stored.
	Get(ctx context.Context, name string) (string, error)

	// Set stores value under name, overwriting any existing entry. It
	// returns ErrReadOnly when the implementation does not support writes.
	Set(ctx context.Context, name string, value string) error

	// Delete removes the entry under name. It returns ErrReadOnly when the
	// implementation does not support writes, and ErrNotFound when no entry
	// exists to delete (implementations may also return nil in that case;
	// callers should not rely on either choice).
	Delete(ctx context.Context, name string) error
}

// Canonical secret names. Dotted lowercase. Add new entries here and add
// a matching case to legacyEnvName so EnvProvider can find a legacy form.
const (
	NameAnthropicAPIKey = "anthropic.api_key"
	NameOpenAIAPIKey    = "openai.api_key"
	NameGranolaAPIKey   = "granola.api_key"
	// NameCaptureHMACKey is the per-install secret captureExternal uses to bind a
	// caller-supplied contentHash to its summary (the SourceRef freshnessToken).
	// It is generated once on first capture and never leaves the device; it is not
	// a third-party credential and has no legacy env form.
	NameCaptureHMACKey = "capture.hmac_key"
)

// legacyEnvName returns the historical environment-variable name some users
// already export for the given canonical secret name. It returns an empty
// string when no legacy mapping exists. EnvProvider.Get consults this after
// the canonical MAZ_SECRET_* form.
func legacyEnvName(name string) string {
	switch name {
	case NameAnthropicAPIKey:
		return "ANTHROPIC_API_KEY"
	case NameOpenAIAPIKey:
		return "OPENAI_API_KEY"
	}
	return ""
}

// Sentinel errors. Callers should use errors.Is to check.
var (
	// ErrNotFound is returned by Get when no value is stored under the
	// requested name.
	ErrNotFound = errors.New("secret: not found")

	// ErrReadOnly is returned by Set or Delete on a read-only provider
	// (e.g., EnvProvider).
	ErrReadOnly = errors.New("secret: provider is read-only")
)
