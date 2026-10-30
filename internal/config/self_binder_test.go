package config_test

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func TestSelfPrincipalUID_UnboundReturnsEmpty(t *testing.T) {
	t.Parallel()
	// A fresh data dir has no state.toml: unbound is a valid pre-genesis state,
	// reported as ("", nil) rather than an error.
	uid, err := config.SelfPrincipalUID(t.TempDir())
	if err != nil {
		t.Fatalf("SelfPrincipalUID on fresh dir: %v", err)
	}
	if uid != "" {
		t.Errorf("uid = %q, want empty (unbound)", uid)
	}
}

func TestBindSelfPrincipal_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const (
		pid = "01PRINCIPAL000000000000001"
		did = "01DEVICE0000000000000000001"
		kid = "kid:test"
	)
	if err := config.BindSelfPrincipal(dir, pid, did, kid); err != nil {
		t.Fatalf("BindSelfPrincipal: %v", err)
	}
	got, err := config.SelfPrincipalUID(dir)
	if err != nil {
		t.Fatalf("SelfPrincipalUID: %v", err)
	}
	if got != pid {
		t.Errorf("bound principal = %q, want %q", got, pid)
	}

	// The full identity block round-trips through state.toml.
	st, err := config.LoadUserState(dir)
	if err != nil {
		t.Fatalf("LoadUserState: %v", err)
	}
	if st.Identity.DeviceID != did || st.Identity.SigKeyID != kid {
		t.Errorf("identity block = %+v, want device %q / sigKeyID %q", st.Identity, did, kid)
	}
}

func TestBindSelfPrincipal_PreservesOtherState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Seed an unrelated state field, then bind: the RMW must preserve it.
	if err := config.WriteUserState(dir, config.UserState{
		Defaults: config.DefaultsStateTable{Chat: "anthropic"},
	}); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}
	if err := config.BindSelfPrincipal(dir, "P", "D", "kid:test"); err != nil {
		t.Fatalf("BindSelfPrincipal: %v", err)
	}
	st, err := config.LoadUserState(dir)
	if err != nil {
		t.Fatalf("LoadUserState: %v", err)
	}
	if st.Defaults.Chat != "anthropic" {
		t.Errorf("Defaults.Chat = %q, want preserved \"anthropic\"", st.Defaults.Chat)
	}
	if st.Identity.PrincipalUID != "P" {
		t.Errorf("Identity.PrincipalUID = %q, want P", st.Identity.PrincipalUID)
	}
}

func TestSelfBinder_CachesAfterBind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := config.NewSelfBinder(dir)

	// Unbound: ("", false), preserving pre-identity behavior.
	if uid, ok := b.SelfPrincipal(context.Background()); ok || uid != "" {
		t.Fatalf("unbound SelfPrincipal = (%q, %v), want (\"\", false)", uid, ok)
	}

	if err := config.BindSelfPrincipal(dir, "01PRINCIPAL000000000000001", "D", "kid:test"); err != nil {
		t.Fatalf("BindSelfPrincipal: %v", err)
	}

	uid, ok := b.SelfPrincipal(context.Background())
	if !ok || uid != "01PRINCIPAL000000000000001" {
		t.Fatalf("bound SelfPrincipal = (%q, %v), want the principal + true", uid, ok)
	}
	// Second call returns the cached value (no error path even if state.toml
	// were removed); just assert stability.
	if uid2, ok2 := b.SelfPrincipal(context.Background()); !ok2 || uid2 != uid {
		t.Errorf("cached SelfPrincipal = (%q, %v), want (%q, true)", uid2, ok2, uid)
	}
}
