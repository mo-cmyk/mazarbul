package secret

import (
	"context"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestKeychain_RoundTrip drives KeychainProvider through Set, Get, Delete.
// It uses keyring.MockInit, the library's in-memory backend, so no real OS
// keychain is touched.
func TestKeychain_RoundTrip(t *testing.T) {
	keyring.MockInit()

	p := NewKeychainProvider()
	ctx := context.Background()
	const name = "anthropic.api_key"
	const value = "sk-ant-test-1234"

	if err := p.Set(ctx, name, value); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := p.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if got != value {
		t.Errorf("Get returned %q, want %q", got, value)
	}

	if err := p.Delete(ctx, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := p.Get(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete returned %v, want ErrNotFound", err)
	}
}

// TestKeychain_GetMissingReturnsErrNotFound verifies the not-found
// translation: zalando's keyring.ErrNotFound becomes secret.ErrNotFound.
func TestKeychain_GetMissingReturnsErrNotFound(t *testing.T) {
	keyring.MockInit()

	p := NewKeychainProvider()
	_, err := p.Get(context.Background(), "never.set")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on missing key returned %v, want ErrNotFound", err)
	}
}

// TestKeychain_DeleteMissingReturnsErrNotFound mirrors the Get test for
// Delete on an absent entry.
func TestKeychain_DeleteMissingReturnsErrNotFound(t *testing.T) {
	keyring.MockInit()

	p := NewKeychainProvider()
	err := p.Delete(context.Background(), "never.set")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete on missing key returned %v, want ErrNotFound", err)
	}
}

// TestKeychain_CustomService verifies the Service field is honored.
func TestKeychain_CustomService(t *testing.T) {
	keyring.MockInit()

	p := &KeychainProvider{Service: "mazarbul-test"}
	ctx := context.Background()

	if err := p.Set(ctx, "alpha", "one"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// A different service must not see the entry.
	other := &KeychainProvider{Service: "mazarbul-other"}
	if _, err := other.Get(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-service Get returned %v, want ErrNotFound", err)
	}
	if got, err := p.Get(ctx, "alpha"); err != nil || got != "one" {
		t.Errorf("same-service Get: got (%q, %v), want (\"one\", nil)", got, err)
	}
}
