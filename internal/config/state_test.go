package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// ptr returns a pointer to v, for the pointer-typed RecallTable knobs
// (TwoHopFloor/GraphWeight/NameEmbWeight) whose nil-vs-set distinction the
// scope-merge logic relies on.
func ptr[T any](v T) *T { return &v }

// valInt dereferences an optional int knob, yielding 0 when unset.
func valInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// valFloat dereferences an optional float64 knob, yielding 0 when unset.
func valFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func sampleState() config.UserState {
	managed := true
	return config.UserState{
		SchemaVersion: config.CurrentUserStateSchemaVersion,
		Defaults: config.DefaultsStateTable{
			Chat: "ollama",
		},
		Providers: map[string]config.ProviderStateTable{
			"anthropic": {
				Model: "claude-sonnet-4-6",
			},
			"ollama": {
				Managed: &managed,
			},
		},
		Recall: config.RecallTable{
			TwoHopFloor: ptr(5),
			FusionMode:  "weighted",
		},
		Composer: config.ComposerTable{
			AbstainThreshold: 0.18,
		},
		Log: config.LogStateTable{
			Level: "debug",
		},
	}
}

func TestUserStatePath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("var", "lib", "mazarbul")
	got := config.UserStatePath(dir)
	want := filepath.Join(dir, "state.toml")
	if got != want {
		t.Errorf("UserStatePath = %q, want %q", got, want)
	}
}

func TestWriteUserState_Roundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	want := sampleState()
	if err := config.WriteUserState(dir, want); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	got, err := config.LoadUserState(dir)
	if err != nil {
		t.Fatalf("LoadUserState: %v", err)
	}

	if got.SchemaVersion != want.SchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", got.SchemaVersion, want.SchemaVersion)
	}
	if got.Defaults.Chat != want.Defaults.Chat {
		t.Errorf("Defaults.Chat: got %q, want %q", got.Defaults.Chat, want.Defaults.Chat)
	}
	if valInt(got.Recall.TwoHopFloor) != valInt(want.Recall.TwoHopFloor) {
		t.Errorf("Recall.TwoHopFloor: got %d, want %d", valInt(got.Recall.TwoHopFloor), valInt(want.Recall.TwoHopFloor))
	}
	if got.Recall.FusionMode != want.Recall.FusionMode {
		t.Errorf("Recall.FusionMode: got %q, want %q", got.Recall.FusionMode, want.Recall.FusionMode)
	}
	if got.Composer.AbstainThreshold != want.Composer.AbstainThreshold {
		t.Errorf("Composer.AbstainThreshold: got %v, want %v",
			got.Composer.AbstainThreshold, want.Composer.AbstainThreshold)
	}
	if got.Log.Level != want.Log.Level {
		t.Errorf("Log.Level: got %q, want %q", got.Log.Level, want.Log.Level)
	}
	if got.Providers["anthropic"].Model != want.Providers["anthropic"].Model {
		t.Errorf("Providers[anthropic].Model: got %q, want %q",
			got.Providers["anthropic"].Model, want.Providers["anthropic"].Model)
	}
	gotManaged := got.Providers["ollama"].Managed
	if gotManaged == nil || *gotManaged != true {
		t.Errorf("Providers[ollama].Managed: got %v, want pointer to true", gotManaged)
	}
}

func TestWriteUserState_DefaultsSchemaVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// SchemaVersion left at zero; WriteUserState should default it to the
	// current version so loads don't trip the upgrade gate.
	st := config.UserState{
		Defaults: config.DefaultsStateTable{Chat: "anthropic"},
	}
	if err := config.WriteUserState(dir, st); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	got, err := config.LoadUserState(dir)
	if err != nil {
		t.Fatalf("LoadUserState: %v", err)
	}
	if got.SchemaVersion != config.CurrentUserStateSchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d",
			got.SchemaVersion, config.CurrentUserStateSchemaVersion)
	}
}

func TestLoadUserState_Missing(t *testing.T) {
	t.Parallel()
	_, err := config.LoadUserState(t.TempDir())
	if !errors.Is(err, config.ErrUserStateNotFound) {
		t.Errorf("err = %v, want ErrUserStateNotFound", err)
	}
}

func TestLoadUserState_FutureSchemaVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a state file with a future schema version directly so the load
	// path's version check is exercised.
	doc := []byte("schema_version = 999\n")
	if err := os.WriteFile(filepath.Join(dir, "state.toml"), doc, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := config.LoadUserState(dir)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestWriteUserState_FileMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes not meaningful on Windows")
	}
	dir := t.TempDir()
	if err := config.WriteUserState(dir, sampleState()); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}
	info, err := os.Stat(config.UserStatePath(dir))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0o600", perm)
	}
}

func TestWriteUserState_AtomicOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first := config.UserState{Defaults: config.DefaultsStateTable{Chat: "ollama"}}
	if err := config.WriteUserState(dir, first); err != nil {
		t.Fatalf("first WriteUserState: %v", err)
	}
	second := config.UserState{Defaults: config.DefaultsStateTable{Chat: "anthropic"}}
	if err := config.WriteUserState(dir, second); err != nil {
		t.Fatalf("second WriteUserState: %v", err)
	}
	got, err := config.LoadUserState(dir)
	if err != nil {
		t.Fatalf("LoadUserState: %v", err)
	}
	if got.Defaults.Chat != "anthropic" {
		t.Errorf("Defaults.Chat after overwrite = %q, want %q", got.Defaults.Chat, "anthropic")
	}

	// Sibling temp files from os.CreateTemp must be cleaned up after the
	// successful rename — atomic-write must not litter the data dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover temp file after atomic write: %s", e.Name())
		}
	}
}

func TestMerge_StateOverridesConfig(t *testing.T) {
	t.Parallel()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
			"ollama":    {BaseURL: "http://localhost:11434"},
		},
		Defaults: config.DefaultsTable{
			Chat:  "anthropic",
			Embed: "ollama",
		},
		Recall: config.RecallTable{
			TwoHopFloor: ptr(3),
			GraphWeight: ptr(1.0),
		},
		Composer: config.ComposerTable{
			AbstainThreshold: 0.0,
		},
	}
	st := config.UserState{
		Defaults: config.DefaultsStateTable{
			Chat: "ollama",
		},
		Providers: map[string]config.ProviderStateTable{
			"anthropic": {Model: "claude-sonnet-4-6"},
		},
		Recall: config.RecallTable{
			TwoHopFloor: ptr(5),
		},
		Composer: config.ComposerTable{
			AbstainThreshold: 0.18,
		},
	}

	got := config.Merge(cfg, st)

	if got.Defaults.Chat != "ollama" {
		t.Errorf("Defaults.Chat: state should override, got %q", got.Defaults.Chat)
	}
	if got.Defaults.Embed != "ollama" {
		t.Errorf("Defaults.Embed: state empty should inherit, got %q", got.Defaults.Embed)
	}
	if got.Providers["anthropic"].Model != "claude-sonnet-4-6" {
		t.Errorf("Providers[anthropic].Model: state should override, got %q",
			got.Providers["anthropic"].Model)
	}
	if got.Providers["ollama"].BaseURL != "http://localhost:11434" {
		t.Errorf("Providers[ollama].BaseURL: untouched key should inherit, got %q",
			got.Providers["ollama"].BaseURL)
	}
	if valInt(got.Recall.TwoHopFloor) != 5 {
		t.Errorf("Recall.TwoHopFloor: state should override, got %d", valInt(got.Recall.TwoHopFloor))
	}
	if valFloat(got.Recall.GraphWeight) != 1.0 {
		t.Errorf("Recall.GraphWeight: state zero should inherit, got %v", valFloat(got.Recall.GraphWeight))
	}
	if got.Composer.AbstainThreshold != 0.18 {
		t.Errorf("Composer.AbstainThreshold: state should override, got %v",
			got.Composer.AbstainThreshold)
	}
}

func TestMerge_PartialProviderOverride(t *testing.T) {
	t.Parallel()
	cfg := config.UserConfig{
		Providers: map[string]config.ProviderTable{
			"anthropic": {
				Model:  "claude-opus-4-7",
				Secret: "secrets://anthropic.api_key",
			},
		},
	}
	st := config.UserState{
		Providers: map[string]config.ProviderStateTable{
			"anthropic": {Model: "claude-sonnet-4-6"},
		},
	}
	got := config.Merge(cfg, st)
	p := got.Providers["anthropic"]
	if p.Model != "claude-sonnet-4-6" {
		t.Errorf("Model: got %q, want %q", p.Model, "claude-sonnet-4-6")
	}
	if p.Secret != "secrets://anthropic.api_key" {
		t.Errorf("Secret: empty override should inherit, got %q", p.Secret)
	}
}

func TestLoadResolvedConfig_StateOverrides(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7", Secret: "secrets://anthropic.api_key"},
		},
		Defaults: config.DefaultsTable{Chat: "anthropic"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	st := config.UserState{
		Providers: map[string]config.ProviderStateTable{
			"anthropic": {Model: "claude-sonnet-4-6"},
		},
	}
	if err := config.WriteUserState(dir, st); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	got, err := config.LoadResolvedConfig(dir)
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}
	if got.Providers["anthropic"].Model != "claude-sonnet-4-6" {
		t.Errorf("Model: state should override, got %q", got.Providers["anthropic"].Model)
	}
	if got.Providers["anthropic"].Secret != "secrets://anthropic.api_key" {
		t.Errorf("Secret: empty override should inherit, got %q", got.Providers["anthropic"].Secret)
	}
}

func TestLoadResolvedConfig_NoState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	got, err := config.LoadResolvedConfig(dir)
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}
	if got.Defaults.Chat != "anthropic" {
		t.Errorf("Defaults.Chat: got %q, want %q", got.Defaults.Chat, "anthropic")
	}
}

func TestLoadResolvedConfig_MissingConfig(t *testing.T) {
	t.Parallel()
	_, err := config.LoadResolvedConfig(t.TempDir())
	if !errors.Is(err, config.ErrUserConfigNotFound) {
		t.Errorf("err = %v, want ErrUserConfigNotFound", err)
	}
}

func TestMerge_EmptyStateLeavesConfig(t *testing.T) {
	t.Parallel()
	cfg := config.UserConfig{
		SchemaVersion: 1,
		Defaults:      config.DefaultsTable{Chat: "anthropic"},
		Recall:        config.RecallTable{TwoHopFloor: ptr(3)},
	}
	got := config.Merge(cfg, config.UserState{})
	if got.Defaults.Chat != "anthropic" {
		t.Errorf("Defaults.Chat: empty state should not touch, got %q", got.Defaults.Chat)
	}
	if valInt(got.Recall.TwoHopFloor) != 3 {
		t.Errorf("Recall.TwoHopFloor: empty state should not touch, got %d", valInt(got.Recall.TwoHopFloor))
	}
}

// ---------------------------------------------------------------------------
// Genesis-pin helpers
// ---------------------------------------------------------------------------

func TestSetTeamGenesisPin_Roundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const uid = "01HXYZ0000000000000000001"
	const kid = "kid:abc123"

	if err := config.SetTeamGenesisPin(dir, uid, kid); err != nil {
		t.Fatalf("SetTeamGenesisPin: %v", err)
	}

	gotUID, gotKID, ok, err := config.LoadTeamGenesisPin(dir)
	if err != nil {
		t.Fatalf("LoadTeamGenesisPin: %v", err)
	}
	if !ok {
		t.Fatal("LoadTeamGenesisPin: ok=false, want true")
	}
	if gotUID != uid {
		t.Errorf("PrincipalUID: got %q, want %q", gotUID, uid)
	}
	if gotKID != kid {
		t.Errorf("SigKeyID: got %q, want %q", gotKID, kid)
	}
}

func TestSetTeamGenesisPin_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const uid = "01HXYZ0000000000000000002"
	const kid = "kid:def456"

	// Write twice; second write should not change values.
	for i := range 2 {
		if err := config.SetTeamGenesisPin(dir, uid, kid); err != nil {
			t.Fatalf("SetTeamGenesisPin call %d: %v", i+1, err)
		}
	}

	gotUID, gotKID, ok, err := config.LoadTeamGenesisPin(dir)
	if err != nil {
		t.Fatalf("LoadTeamGenesisPin: %v", err)
	}
	if !ok || gotUID != uid || gotKID != kid {
		t.Errorf("after 2 writes: ok=%v uid=%q kid=%q", ok, gotUID, gotKID)
	}
}

func TestSetTeamGenesisPin_PreservesOtherFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a full state first.
	st := sampleState()
	if err := config.WriteUserState(dir, st); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	// Now pin genesis.
	const uid = "01HXYZ0000000000000000003"
	const kid = "kid:ghi789"
	if err := config.SetTeamGenesisPin(dir, uid, kid); err != nil {
		t.Fatalf("SetTeamGenesisPin: %v", err)
	}

	// Ensure the pre-existing fields were preserved.
	got, err := config.LoadUserState(dir)
	if err != nil {
		t.Fatalf("LoadUserState: %v", err)
	}
	if got.Defaults.Chat != st.Defaults.Chat {
		t.Errorf("Defaults.Chat clobbered: got %q, want %q", got.Defaults.Chat, st.Defaults.Chat)
	}
	if got.Log.Level != st.Log.Level {
		t.Errorf("Log.Level clobbered: got %q, want %q", got.Log.Level, st.Log.Level)
	}
	if got.Team.GenesisPin.PrincipalUID != uid {
		t.Errorf("PrincipalUID: got %q, want %q", got.Team.GenesisPin.PrincipalUID, uid)
	}
}

func TestSetTeamGenesisPin_RejectsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := config.SetTeamGenesisPin(dir, "", "kid:abc"); err == nil {
		t.Error("expected error for empty principalUID, got nil")
	}
	if err := config.SetTeamGenesisPin(dir, "01HXYZ", ""); err == nil {
		t.Error("expected error for empty sigKeyID, got nil")
	}
}

func TestLoadTeamGenesisPin_Missing(t *testing.T) {
	t.Parallel()

	// No state file at all.
	uid, kid, ok, err := config.LoadTeamGenesisPin(t.TempDir())
	if err != nil {
		t.Fatalf("LoadTeamGenesisPin: unexpected error: %v", err)
	}
	if ok {
		t.Errorf("ok=true on fresh dir, got uid=%q kid=%q", uid, kid)
	}
}

func TestLoadTeamGenesisPin_EmptyPinFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// State with no pin fields set.
	if err := config.WriteUserState(dir, sampleState()); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	_, _, ok, err := config.LoadTeamGenesisPin(dir)
	if err != nil {
		t.Fatalf("LoadTeamGenesisPin: %v", err)
	}
	if ok {
		t.Error("ok=true when pin fields empty")
	}
}

func TestClearTeamGenesisPin(t *testing.T) {
	t.Parallel()

	t.Run("removes stored pin and preserves identity", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := config.BindSelfPrincipal(dir, "01PRINCIPAL0000000000001", "dev-1", "kid:one"); err != nil {
			t.Fatalf("BindSelfPrincipal: %v", err)
		}
		if err := config.SetTeamGenesisPin(dir, "01GENESIS000000000000001", "kid:genesis"); err != nil {
			t.Fatalf("SetTeamGenesisPin: %v", err)
		}
		if err := config.ClearTeamGenesisPin(dir); err != nil {
			t.Fatalf("ClearTeamGenesisPin: %v", err)
		}
		if _, _, ok, err := config.LoadTeamGenesisPin(dir); err != nil || ok {
			t.Errorf("pin still present after clear (ok=%v, err=%v)", ok, err)
		}
		st, err := config.LoadUserState(dir)
		if err != nil {
			t.Fatalf("LoadUserState: %v", err)
		}
		if st.Identity.PrincipalUID != "01PRINCIPAL0000000000001" {
			t.Errorf("identity binding lost on clear: %q", st.Identity.PrincipalUID)
		}
	})

	t.Run("no-op on missing state file", func(t *testing.T) {
		t.Parallel()
		if err := config.ClearTeamGenesisPin(t.TempDir()); err != nil {
			t.Errorf("ClearTeamGenesisPin on fresh dir: %v", err)
		}
	})
}
