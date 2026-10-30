package secret

import (
	"context"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// DefaultService is the keychain service identifier used for every Mazarbul
// secret. The account portion of the keychain entry is the canonical secret
// name (e.g., "anthropic.api_key").
const DefaultService = "mazarbul"

// KeychainProvider stores secrets in the host operating system's credential
// store: Keychain on macOS, libsecret over D-Bus on Linux, Credential
// Manager on Windows. It is the default Provider for production use.
//
// The implementation is a thin adapter over github.com/zalando/go-keyring,
// which does not require CGO and ships an in-memory backend that tests
// activate via keyring.MockInit.
type KeychainProvider struct {
	// Service overrides DefaultService. Zero value means DefaultService.
	Service string
}

// NewKeychainProvider returns a KeychainProvider that uses DefaultService.
func NewKeychainProvider() *KeychainProvider {
	return &KeychainProvider{Service: DefaultService}
}

// service returns the configured service or DefaultService.
func (k *KeychainProvider) service() string {
	if k.Service == "" {
		return DefaultService
	}
	return k.Service
}

// Get reads name from the OS keychain. It returns ErrNotFound when no entry
// exists.
func (k *KeychainProvider) Get(_ context.Context, name string) (string, error) {
	v, err := keyring.Get(k.service(), name)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("keychain get %q: %w", name, err)
	}
	return v, nil
}

// Set writes value under name in the OS keychain. Existing entries are
// overwritten.
func (k *KeychainProvider) Set(_ context.Context, name string, value string) error {
	if err := keyring.Set(k.service(), name, value); err != nil {
		return fmt.Errorf("keychain set %q: %w", name, err)
	}
	return nil
}

// Delete removes name from the OS keychain. A missing entry returns
// ErrNotFound.
func (k *KeychainProvider) Delete(_ context.Context, name string) error {
	err := keyring.Delete(k.service(), name)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("keychain delete %q: %w", name, err)
	}
	return nil
}
