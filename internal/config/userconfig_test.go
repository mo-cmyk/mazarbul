package config_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func sampleConfig() config.UserConfig {
	return config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Providers: map[string]config.ProviderTable{
			"anthropic": {
				Model:  "claude-opus-4-7",
				Secret: config.SecretURIScheme + "anthropic.api_key",
			},
			"ollama": {
				BaseURL:    "http://localhost:11434",
				EmbedModel: "bge-m3",
			},
		},
		Defaults: config.DefaultsTable{
			Chat:  "anthropic",
			Embed: "ollama",
		},
	}
}

func TestWriteUserConfig_WatchFSRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	want := sampleConfig()
	want.WatchFS = config.WatchFSTable{
		Roots:        []string{"/home/me/repo", "/home/me/notes"},
		Scope:        "project",
		SizeCapBytes: 8192,
		DebounceMs:   750,
		Ignore:       []string{"*.tmp", "scratch/"},
		Confidence:   0.55,
	}
	if err := config.WriteUserConfig(dir, want); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	w := got.WatchFS
	if !slices.Equal(w.Roots, want.WatchFS.Roots) {
		t.Errorf("WatchFS.Roots: got %v, want %v", w.Roots, want.WatchFS.Roots)
	}
	if !slices.Equal(w.Ignore, want.WatchFS.Ignore) {
		t.Errorf("WatchFS.Ignore: got %v, want %v", w.Ignore, want.WatchFS.Ignore)
	}
	if w.Scope != "project" || w.SizeCapBytes != 8192 || w.DebounceMs != 750 || w.Confidence != 0.55 {
		t.Errorf("WatchFS scalars: got %+v", w)
	}
}

func TestUserConfigPath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("var", "lib", "mazarbul")
	got := config.UserConfigPath(dir)
	want := filepath.Join(dir, "config.toml")
	if got != want {
		t.Errorf("UserConfigPath = %q, want %q", got, want)
	}
}

func TestWriteUserConfig_Roundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	want := sampleConfig()
	if err := config.WriteUserConfig(dir, want); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}

	if got.SchemaVersion != want.SchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", got.SchemaVersion, want.SchemaVersion)
	}
	if got.Defaults != want.Defaults {
		t.Errorf("Defaults: got %+v, want %+v", got.Defaults, want.Defaults)
	}
	if len(got.Providers) != len(want.Providers) {
		t.Fatalf("Providers len: got %d, want %d", len(got.Providers), len(want.Providers))
	}
	for name, table := range want.Providers {
		if got.Providers[name] != table {
			t.Errorf("Providers[%q]: got %+v, want %+v", name, got.Providers[name], table)
		}
	}
}

func TestWriteUserConfig_Mode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes not meaningful on Windows")
	}
	dir := t.TempDir()
	if err := config.WriteUserConfig(dir, sampleConfig()); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	info, err := os.Stat(config.UserConfigPath(dir))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0o600", perm)
	}
}

func TestWriteUserConfig_Stable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := sampleConfig()

	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, err := os.ReadFile(config.UserConfigPath(dir))
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second, err := os.ReadFile(config.UserConfigPath(dir))
	if err != nil {
		t.Fatalf("read second: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("non-deterministic encoding:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestWriteUserConfig_Atomic_NoTmpLeftovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	for range 5 {
		if err := config.WriteUserConfig(dir, sampleConfig()); err != nil {
			t.Fatalf("WriteUserConfig: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.toml" {
			t.Errorf("unexpected leftover entry %q in data dir", e.Name())
		}
	}
}

func TestWriteUserConfig_ConcurrentReaderSeesCompleteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := config.WriteUserConfig(dir, sampleConfig()); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader: every observed file must be parseable as UserConfig. Atomic
	// rename means we either see the previous or the new contents, never a
	// half-written file.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				cfg, err := config.LoadUserConfig(dir)
				if err != nil {
					t.Errorf("concurrent Load: %v", err)
					return
				}
				if cfg.SchemaVersion == 0 {
					t.Errorf("concurrent Load: zero SchemaVersion (saw partial file)")
					return
				}
			}
		}
	}()

	for i := range 50 {
		if err := config.WriteUserConfig(dir, sampleConfig()); err != nil {
			t.Errorf("Write iter %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestLoadUserConfig_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := config.LoadUserConfig(dir)
	if !errors.Is(err, config.ErrUserConfigNotFound) {
		t.Errorf("err = %v, want ErrUserConfigNotFound", err)
	}
}

func TestLoadUserConfig_NewerSchemaRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := sampleConfig()
	cfg.SchemaVersion = config.CurrentUserConfigSchemaVersion + 1
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	_, err := config.LoadUserConfig(dir)
	if err == nil {
		t.Fatalf("LoadUserConfig: nil error, want schema-version rejection")
	}
}

func TestWriteUserConfig_EmptyDataDir(t *testing.T) {
	t.Parallel()
	err := config.WriteUserConfig("", sampleConfig())
	if err == nil {
		t.Fatalf("WriteUserConfig(\"\"): nil error, want failure")
	}
}

// TestWriteUserConfig_EmbedDisabled covers the "skip embeddings" outcome:
// defaults.embed is empty and the encoded TOML omits the key entirely so a
// later reader cannot mistake the absence for a typo.
func TestWriteUserConfig_EmbedDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := sampleConfig()
	cfg.Defaults.Embed = ""
	delete(cfg.Providers, "ollama")
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	data, err := os.ReadFile(config.UserConfigPath(dir))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if bytes.Contains(data, []byte("embed =")) || bytes.Contains(data, []byte("embed=")) {
		t.Errorf("encoded TOML contains an `embed = ...` line; omitempty should have dropped it.\n%s", data)
	}

	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.Defaults.Embed != "" {
		t.Errorf("Defaults.Embed roundtrip: got %q, want empty", got.Defaults.Embed)
	}
}

// TestLoadEffectiveConfig_PropagatesParseError pins the contract that a
// corrupt config.toml surfaces as an error from LoadEffectiveConfig
// rather than silently returning compiled defaults. Greptile P1 on
// PR #144: hiding parse errors lets `maz config get` show a clean
// effective config while the daemon would fail to boot, masking the
// real fault from the operator.
func TestLoadEffectiveConfig_PropagatesParseError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(config.UserConfigPath(dir), []byte("[providers.ollama\nbroken = "), 0o600); err != nil {
		t.Fatalf("seed corrupt config: %v", err)
	}
	_, err := config.LoadEffectiveConfig(dir, "")
	if err == nil {
		t.Fatal("LoadEffectiveConfig returned nil for corrupt config.toml; expected the parse error to propagate")
	}
}

// TestLoadEffectiveConfig_PropagatesSchemaVersionError pins the same
// contract for the "config is newer than this binary" case: refuse to
// pretend the config is valid by falling back to defaults.
func TestLoadEffectiveConfig_PropagatesSchemaVersionError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := sampleConfig()
	cfg.SchemaVersion = config.CurrentUserConfigSchemaVersion + 1
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("seed newer-schema config: %v", err)
	}
	_, err := config.LoadEffectiveConfig(dir, "")
	if err == nil {
		t.Fatal("LoadEffectiveConfig returned nil for newer-schema config.toml; expected the schema-version error to propagate")
	}
}

// TestLoadEffectiveConfig_ToleratesMissingFiles pins the inverse: a
// fresh data-dir with no files at all is the happy path; the function
// must still return the compiled defaults without an error so `maz
// config list` works before `maz init`.
func TestLoadEffectiveConfig_ToleratesMissingFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := config.LoadEffectiveConfig(dir, "")
	if err != nil {
		t.Fatalf("LoadEffectiveConfig on fresh data-dir: %v", err)
	}
	if cfg.Defaults.Chat == "" {
		t.Error("expected compiled defaults to be present; got empty Defaults.Chat")
	}
}

// TestOntologyTable_ZeroValuePreservesBehavior asserts that an absent
// [ontology] block round-trips to a zero-value OntologyTable and does not
// affect any other field — mirroring the IngestionTable zero-value contract.
func TestOntologyTable_ZeroValuePreservesBehavior(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg := sampleConfig()
	// OntologyTable is intentionally absent (zero value).
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.Ontology.StrictAttributes {
		t.Error("OntologyTable.StrictAttributes should default false; got true")
	}
}

// TestOntologyTable_StrictAttributesRoundTrip asserts that writing
// strict_attributes = true round-trips faithfully through WriteUserConfig /
// LoadUserConfig.
func TestOntologyTable_StrictAttributesRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg := sampleConfig()
	cfg.Ontology = config.OntologyTable{StrictAttributes: true}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if !got.Ontology.StrictAttributes {
		t.Error("OntologyTable.StrictAttributes: want true after round-trip, got false")
	}
}

// TestOntologyTable_Merge_StrictAttributesWins asserts that config.Merge
// propagates a true StrictAttributes from UserState to the merged UserConfig
// (true wins over false).
func TestOntologyTable_Merge_StrictAttributesWins(t *testing.T) {
	t.Parallel()

	base := sampleConfig()
	base.Ontology = config.OntologyTable{StrictAttributes: false}

	state := config.UserState{
		Ontology: config.OntologyTable{StrictAttributes: true},
	}

	merged := config.Merge(base, state)
	if !merged.Ontology.StrictAttributes {
		t.Error("merged StrictAttributes: want true (state wins), got false")
	}
}

// TestOntologyTable_Merge_FalseDoesNotOverrideTrue asserts that a false
// StrictAttributes in UserState does not clear a true value already in
// UserConfig (only true wins, false inherits).
func TestOntologyTable_Merge_FalseDoesNotOverrideTrue(t *testing.T) {
	t.Parallel()

	base := sampleConfig()
	base.Ontology = config.OntologyTable{StrictAttributes: true}

	state := config.UserState{
		Ontology: config.OntologyTable{StrictAttributes: false},
	}

	merged := config.Merge(base, state)
	if !merged.Ontology.StrictAttributes {
		t.Error("merged StrictAttributes: want true (base preserved), got false")
	}
}

// TestRecallTable_ZeroPointerRoundtrip asserts that an explicit zero set on a
// pointer-typed recall knob (rrf_min_score, mmr_lambda, recency_boost) round-
// trips through WriteUserConfig / LoadUserConfig as a non-nil pointer to 0 so
// that config get / config list can report "0" rather than "key not found".
func TestRecallTable_ZeroPointerRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	zero32 := float32(0)
	zero64 := float64(0)
	cfg := sampleConfig()
	cfg.Recall = config.RecallTable{
		RRFMinScore:  &zero32,
		MMRLambda:    &zero64,
		RecencyBoost: &zero64,
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.Recall.RRFMinScore == nil {
		t.Error("RRFMinScore: nil after round-trip, want non-nil pointer to 0")
	} else if *got.Recall.RRFMinScore != 0 {
		t.Errorf("RRFMinScore: %v, want 0", *got.Recall.RRFMinScore)
	}
	if got.Recall.MMRLambda == nil {
		t.Error("MMRLambda: nil after round-trip, want non-nil pointer to 0")
	} else if *got.Recall.MMRLambda != 0 {
		t.Errorf("MMRLambda: %v, want 0", *got.Recall.MMRLambda)
	}
	if got.Recall.RecencyBoost == nil {
		t.Error("RecencyBoost: nil after round-trip, want non-nil pointer to 0")
	} else if *got.Recall.RecencyBoost != 0 {
		t.Errorf("RecencyBoost: %v, want 0", *got.Recall.RecencyBoost)
	}
}

// TestSyncTable_CoalesceSecondsZeroRoundtrip asserts that an explicit
// coalesce_seconds = 0 written via WriteUserConfig survives the
// LoadEffectiveConfig layering (compiled default = 30) as 0, not 30. Before
// the pointer fix, mergeUserConfig treated stored 0 as "unset" and fell back
// to the compiled default.
func TestSyncTable_CoalesceSecondsZeroRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	zero := 0
	cfg := sampleConfig()
	cfg.Sync = config.SyncTable{CoalesceSeconds: &zero}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
	resolved, err := config.LoadEffectiveConfig(dir, "")
	if err != nil {
		t.Fatalf("LoadEffectiveConfig: %v", err)
	}
	if resolved.Sync.CoalesceSeconds == nil {
		t.Fatal("CoalesceSeconds: nil in resolved config, want non-nil pointer to 0")
	}
	if *resolved.Sync.CoalesceSeconds != 0 {
		t.Errorf("CoalesceSeconds after set-to-zero: %d, want 0 (not the compiled default 30)", *resolved.Sync.CoalesceSeconds)
	}
}

// TestVaultPath pins the vault-directory resolution: default <dataDir>/vault,
// relative [vault] path against the data dir, absolute path verbatim.
func TestVaultPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if got, want := config.VaultPath(dir, nil), filepath.Join(dir, "vault"); got != want {
		t.Errorf("nil config: %q, want %q", got, want)
	}
	empty := config.UserConfig{}
	if got, want := config.VaultPath(dir, &empty), filepath.Join(dir, "vault"); got != want {
		t.Errorf("empty config: %q, want %q", got, want)
	}
	rel := config.UserConfig{Vault: config.VaultTable{Path: "notes"}}
	if got, want := config.VaultPath(dir, &rel), filepath.Join(dir, "notes"); got != want {
		t.Errorf("relative path: %q, want %q", got, want)
	}
	abs := config.UserConfig{Vault: config.VaultTable{Path: "/srv/obsidian"}}
	if got := config.VaultPath(dir, &abs); got != "/srv/obsidian" {
		t.Errorf("absolute path: %q, want /srv/obsidian", got)
	}
}
