package secret

import (
	"context"
	"os"
	"strings"
)

// EnvProvider reads secrets from environment variables. It is the fallback
// layer behind KeychainProvider in CI, where the keychain may not be
// available. Writes are not supported: Set and Delete return ErrReadOnly so
// callers see a deterministic error rather than a silent drop.
//
// EnvProvider looks for two environment variable forms, in order:
//
//  1. The canonical MAZ_SECRET_* form, derived from the secret name by
//     uppercasing it and replacing every "." with "_". For example,
//     "anthropic.api_key" becomes "MAZ_SECRET_ANTHROPIC_API_KEY".
//
//  2. A legacy form (e.g., "ANTHROPIC_API_KEY"), looked up via
//     legacyEnvName. This keeps existing user setups working without a
//     rename.
//
// When both are set, the canonical form wins.
type EnvProvider struct{}

// NewEnvProvider returns a ready-to-use EnvProvider.
func NewEnvProvider() EnvProvider {
	return EnvProvider{}
}

// CanonicalEnvName returns the MAZ_SECRET_* variable name for the given
// canonical secret name. Exported for use by tests and the init wizard.
func CanonicalEnvName(name string) string {
	upper := strings.ToUpper(name)
	upper = strings.ReplaceAll(upper, ".", "_")
	return "MAZ_SECRET_" + upper
}

// Get returns the first non-empty value among the canonical and legacy
// environment variables for name. It returns ErrNotFound when both are
// unset or empty.
func (EnvProvider) Get(_ context.Context, name string) (string, error) {
	if v := os.Getenv(CanonicalEnvName(name)); v != "" {
		return v, nil
	}
	if legacy := legacyEnvName(name); legacy != "" {
		if v := os.Getenv(legacy); v != "" {
			return v, nil
		}
	}
	return "", ErrNotFound
}

// Set always returns ErrReadOnly. EnvProvider does not mutate the process
// environment.
func (EnvProvider) Set(_ context.Context, _ string, _ string) error {
	return ErrReadOnly
}

// Delete always returns ErrReadOnly.
func (EnvProvider) Delete(_ context.Context, _ string) error {
	return ErrReadOnly
}
