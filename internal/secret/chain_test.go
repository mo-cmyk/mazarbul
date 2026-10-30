package secret

import (
	"context"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// fakeProvider is an in-memory Provider for chain ordering tests. It does
// not interact with the OS keychain, so chain tests stay deterministic
// without touching the zalando mock backend.
type fakeProvider struct {
	store    map[string]string
	readOnly bool
	getErr   error // when non-nil, Get returns this error for every name
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{store: map[string]string{}}
}

func (f *fakeProvider) Get(_ context.Context, name string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.store[name]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *fakeProvider) Set(_ context.Context, name string, value string) error {
	if f.readOnly {
		return ErrReadOnly
	}
	f.store[name] = value
	return nil
}

func (f *fakeProvider) Delete(_ context.Context, name string) error {
	if f.readOnly {
		return ErrReadOnly
	}
	if _, ok := f.store[name]; !ok {
		return ErrNotFound
	}
	delete(f.store, name)
	return nil
}

// TestChain_FirstHitWins covers the read precedence: the first provider's
// value wins even when later providers also have an entry.
func TestChain_FirstHitWins(t *testing.T) {
	first := newFakeProvider()
	first.store["name"] = "first"
	second := newFakeProvider()
	second.store["name"] = "second"

	got, err := NewChain(first, second).Get(context.Background(), "name")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "first" {
		t.Errorf("got %q, want %q", got, "first")
	}
}

// TestChain_FallsThroughOnNotFound verifies a missing entry in the first
// provider does not short-circuit the chain.
func TestChain_FallsThroughOnNotFound(t *testing.T) {
	first := newFakeProvider() // empty
	second := newFakeProvider()
	second.store["name"] = "second"

	got, err := NewChain(first, second).Get(context.Background(), "name")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "second" {
		t.Errorf("got %q, want %q", got, "second")
	}
}

// TestChain_GetAllMissReturnsErrNotFound covers the all-empty case.
func TestChain_GetAllMissReturnsErrNotFound(t *testing.T) {
	_, err := NewChain(newFakeProvider(), newFakeProvider()).Get(context.Background(), "name")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestChain_GetShortCircuitsOnRealError verifies any error other than
// ErrNotFound short-circuits the chain so the user sees the real cause.
func TestChain_GetShortCircuitsOnRealError(t *testing.T) {
	first := newFakeProvider()
	first.getErr = errors.New("dbus broken")
	second := newFakeProvider()
	second.store["name"] = "value"

	_, err := NewChain(first, second).Get(context.Background(), "name")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want short-circuited error", err)
	}
}

// TestChain_WriteThroughSkipsReadOnly verifies Set targets the first
// writable member, skipping read-only members in front of it.
func TestChain_WriteThroughSkipsReadOnly(t *testing.T) {
	envLike := newFakeProvider()
	envLike.readOnly = true
	writable := newFakeProvider()

	if err := NewChain(envLike, writable).Set(context.Background(), "name", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := writable.store["name"]; got != "value" {
		t.Errorf("writable store has %q, want %q", got, "value")
	}
	if len(envLike.store) != 0 {
		t.Errorf("read-only provider should not have been touched, got %v", envLike.store)
	}
}

// TestChain_SetAllReadOnly verifies Set returns ErrReadOnly when no member
// can accept writes.
func TestChain_SetAllReadOnly(t *testing.T) {
	a := newFakeProvider()
	a.readOnly = true
	b := newFakeProvider()
	b.readOnly = true

	err := NewChain(a, b).Set(context.Background(), "name", "value")
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("got %v, want ErrReadOnly", err)
	}
}

// TestChain_DeleteWriteThroughSkipsReadOnly mirrors Set behaviour for
// Delete.
func TestChain_DeleteWriteThroughSkipsReadOnly(t *testing.T) {
	envLike := newFakeProvider()
	envLike.readOnly = true
	writable := newFakeProvider()
	writable.store["name"] = "value"

	if err := NewChain(envLike, writable).Delete(context.Background(), "name"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := writable.store["name"]; ok {
		t.Errorf("entry not deleted from writable store")
	}
}

// TestChain_RealEnvAndKeychain wires up the actual EnvProvider and
// KeychainProvider (with the mock keychain backend) to confirm the chain
// composes the real types without surprises.
func TestChain_RealEnvAndKeychain(t *testing.T) {
	keyring.MockInit()
	t.Setenv("MAZ_SECRET_ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	ctx := context.Background()
	chain := NewChain(NewEnvProvider(), NewKeychainProvider())

	// With env unset, Set should write through to the keychain.
	if err := chain.Set(ctx, NameAnthropicAPIKey, "sk-keychain"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := chain.Get(ctx, NameAnthropicAPIKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-keychain" {
		t.Errorf("got %q, want %q", got, "sk-keychain")
	}

	// Now set the env var; it should win over the stored keychain value.
	t.Setenv("MAZ_SECRET_ANTHROPIC_API_KEY", "sk-env")
	got, err = chain.Get(ctx, NameAnthropicAPIKey)
	if err != nil {
		t.Fatalf("Get with env set: %v", err)
	}
	if got != "sk-env" {
		t.Errorf("got %q, want env to win with %q", got, "sk-env")
	}
}
