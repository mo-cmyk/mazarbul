package daemon

// wiring_sync_envelope_test.go covers the envelope-mode seal-key resolution
// (#213): the keychain DEK epoch probe and the buildScopeKeyMap envelope
// branch. The full seal/decode round-trip rides the chunk + transport tests.

import (
	"bytes"
	"context"
	"encoding/base64"
	"strconv"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// mapSecrets is a minimal in-memory secret.Provider for tests.
type mapSecrets struct{ m map[string]string }

func newMapSecrets() *mapSecrets { return &mapSecrets{m: map[string]string{}} }

func (s *mapSecrets) Get(_ context.Context, name string) (string, error) {
	v, ok := s.m[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}
func (s *mapSecrets) Set(_ context.Context, name, value string) error { s.m[name] = value; return nil }
func (s *mapSecrets) Delete(_ context.Context, name string) error     { delete(s.m, name); return nil }

// putDEK caches a fresh DEK at the given epoch.
func putDEK(t *testing.T, s *mapSecrets, epoch int) []byte {
	t.Helper()
	dek, err := watchword.GenerateDEK()
	if err != nil {
		t.Fatalf("gen DEK: %v", err)
	}
	key := watchword.KeychainTeamDEKPrefix + strconv.Itoa(epoch)
	s.m[key] = base64.StdEncoding.EncodeToString(dek)
	return dek
}

func TestResolveCurrentDEK_NoDEK(t *testing.T) {
	t.Parallel()
	_, _, ok := resolveCurrentDEK(context.Background(), newMapSecrets())
	if ok {
		t.Fatal("resolveCurrentDEK with empty keychain should report ok=false")
	}
}

func TestResolveCurrentDEK_PicksHighestContiguous(t *testing.T) {
	t.Parallel()
	s := newMapSecrets()
	putDEK(t, s, 0)
	putDEK(t, s, 1)
	want := putDEK(t, s, 2)
	dek, epoch, ok := resolveCurrentDEK(context.Background(), s)
	if !ok || epoch != 2 {
		t.Fatalf("epoch = %d ok = %v, want 2 true", epoch, ok)
	}
	if !bytes.Equal(dek, want) {
		t.Error("resolveCurrentDEK returned a DEK other than the highest epoch's")
	}
}

func TestResolveCurrentDEK_StopsAtGap(t *testing.T) {
	t.Parallel()
	s := newMapSecrets()
	want := putDEK(t, s, 0)
	// epoch 1 missing; epoch 2 present but unreachable across the gap.
	putDEK(t, s, 2)
	dek, epoch, ok := resolveCurrentDEK(context.Background(), s)
	if !ok || epoch != 0 {
		t.Fatalf("epoch = %d ok = %v, want 0 true (contiguous run ends at the gap)", epoch, ok)
	}
	if !bytes.Equal(dek, want) {
		t.Error("expected the epoch-0 DEK")
	}
}

func TestBuildScopeKeyMap_EnvelopeUsesKeychainDEK(t *testing.T) {
	t.Parallel()
	s := newMapSecrets()
	want := putDEK(t, s, 0)
	encrypted := true
	sync := config.SyncTable{Remotes: map[string]config.ScopeRemoteTable{
		string(kernel.ScopeTeam): {URL: "git@x:t.git", Encrypted: &encrypted, KeyMode: config.KeyModeEnvelope},
	}}
	keys, epochs, err := buildScopeKeyMap(context.Background(), "", sync, s, zap.NewNop())
	if err != nil {
		t.Fatalf("buildScopeKeyMap: %v", err)
	}
	got, ok := keys[kernel.ScopeTeam]
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("team key = %x ok=%v, want the keychain DEK", got, ok)
	}
	if epochs[kernel.ScopeTeam] != 0 {
		t.Errorf("team epoch = %d, want 0", epochs[kernel.ScopeTeam])
	}
}

func TestBuildScopeKeyMap_EnvelopeNoDEKSkips(t *testing.T) {
	t.Parallel()
	encrypted := true
	sync := config.SyncTable{Remotes: map[string]config.ScopeRemoteTable{
		string(kernel.ScopeTeam): {URL: "git@x:t.git", Encrypted: &encrypted, KeyMode: config.KeyModeEnvelope},
	}}
	keys, _, err := buildScopeKeyMap(context.Background(), "", sync, newMapSecrets(), zap.NewNop())
	if err != nil {
		t.Fatalf("buildScopeKeyMap: %v", err)
	}
	if _, ok := keys[kernel.ScopeTeam]; ok {
		t.Error("envelope team scope with no keychain DEK must be skipped (not sealed)")
	}
}
