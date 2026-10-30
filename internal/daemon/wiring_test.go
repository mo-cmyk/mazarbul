package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
)

// TestBuildEmbedder_UsesDefaultsKey pins the Greptile #110 fix on the
// provider-resolution path: buildEmbedder must look up the provider
// table via user.Defaults.Embed, not via a hardcoded literal. The bug
// it guards is "valid config with defaults.embed pointing at a renamed
// table yields wrong endpoint/model"; the resolver now honors the
// contract that Defaults.Embed is the lookup key.
func TestBuildEmbedder_UsesDefaultsKey(t *testing.T) {
	t.Parallel()

	// Probe against a guaranteed-unreachable endpoint so buildEmbedder
	// fails at the Ollama /api/tags probe; we then inspect the wrapped
	// error for the URL to confirm the right table was read.
	const baseURL = "http://127.0.0.1:1" // privileged-only, refuses connections in test
	user := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Providers: map[string]config.ProviderTable{
			"ollama": {BaseURL: baseURL, EmbedModel: "probe-canary"},
		},
		Defaults: config.DefaultsTable{Embed: "ollama"},
	}

	_, err := buildEmbedder(t.Context(), user, zap.NewNop())
	if err == nil {
		t.Fatalf("buildEmbedder against unreachable endpoint: err = nil, want failure")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error %q does not mention the configured BaseURL; resolver may have read a different table", err.Error())
	}
}

// TestBuildEmbedder_EmptyDefaultsReturnsNil pins the #102 contract that
// an explicitly empty Defaults.Embed means "embeddings disabled":
// buildEmbedder returns (nil, nil) without probing any Ollama endpoint,
// so OpenWiring can branch on the nil and register a daemon without
// embed surfaces. Earlier behaviour silently rewrote empty to "ollama"
// and then ran an HTTP probe, defeating the wizard's skip-embeddings
// flow.
func TestBuildEmbedder_EmptyDefaultsReturnsNil(t *testing.T) {
	t.Parallel()

	user := config.UserConfig{
		Providers: map[string]config.ProviderTable{
			// Include the table so we can prove buildEmbedder does NOT
			// reach into it. If the old fallback re-enabled itself, the
			// probe would try http://127.0.0.1:1 and surface the URL in
			// the error path.
			"ollama": {BaseURL: "http://127.0.0.1:1", EmbedModel: "would-probe-if-fallback-re-enabled"},
		},
		Defaults: config.DefaultsTable{Embed: ""},
	}
	got, err := buildEmbedder(t.Context(), user, zap.NewNop())
	if err != nil {
		t.Fatalf("buildEmbedder with empty Embed: err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("buildEmbedder with empty Embed: client = %v, want nil (skip-embed sentinel)", got)
	}
}

// TestBuildEmbedder_UnknownProviderRejected pins the M1 contract that
// only "ollama" is a recognised embed provider type. A future PR adds
// a `type` discriminator to ProviderTable so an "ollama-local" rename
// can declare itself ollama-shaped; until then the strict gate keeps
// silent misconfigurations from poisoning the vector index with the
// wrong dimension.
func TestBuildEmbedder_UnknownProviderRejected(t *testing.T) {
	t.Parallel()

	user := config.UserConfig{
		Providers: map[string]config.ProviderTable{
			"voyage": {BaseURL: "https://api.voyageai.com", EmbedModel: "voyage-3"},
		},
		Defaults: config.DefaultsTable{Embed: "voyage"},
	}
	_, err := buildEmbedder(t.Context(), user, zap.NewNop())
	if err == nil {
		t.Fatalf("buildEmbedder with unsupported provider: err = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "voyage") {
		t.Errorf("error %q should name the offending provider", err.Error())
	}
	// Sanity: the rejection should NOT mention the Voyage URL,
	// because the resolver must reject before any HTTP probe runs.
	if strings.Contains(err.Error(), "api.voyageai.com") {
		t.Errorf("rejection should fail before HTTP probe; error %q leaked the URL", err.Error())
	}

	// The error must be the configured-provider rejection, not a
	// downstream wrap of a network failure (which would imply we
	// tried to probe).
	if errors.Is(err, errSentinelUnused) {
		t.Errorf("sanity check; never reached")
	}

	// Pins the sentinel that OpenWiring uses to keep this class of
	// error fatal (vs the degraded-write fallback that swallows probe
	// failures). A regression that loses the wrap here would silently
	// let unknown providers downgrade the daemon to write-only mode.
	if !errors.Is(err, errUnsupportedEmbedProvider) {
		t.Errorf("err does not wrap errUnsupportedEmbedProvider: %v", err)
	}
}

// TestOpenWiring_SingleProjectorInstance pins the I2/I3 fix: after
// OpenWiring completes, w.Projector must be non-nil (registerNavigationTools
// built it and stored it on w) and startVaultWatcher reuses that same
// instance rather than minting a second one. The two-projector bug caused
// concurrent SyncPath calls from the MCP tools and the Writeback goroutine
// to race on the sidecar via os.Rename, silently losing path entries with
// no panic and no -race report. A single projector with one mutex is the
// structural fix; this test pins that the wiring sets it.
func TestOpenWiring_SingleProjectorInstance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "none"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	w, err := OpenWiring(t.Context(), WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}
	t.Cleanup(func() { _ = w.Close(t.Context()) })

	// registerNavigationTools must have stored exactly one projector on w.
	// If it is nil the wiring either skipped registration (storage mismatch)
	// or regressed to the old code that kept projector as a local variable.
	if w.Projector == nil {
		t.Error("w.Projector is nil after OpenWiring; registerNavigationTools must store it on w so startVaultWatcher can share the same instance (I3)")
	}

	// registerNavigationTools must likewise have stored exactly one importer on
	// w so startVaultWatcher reuses it. A second importer in the watcher would
	// own a separate per-document prose-extraction lock map, reopening the
	// duplicate-edge race between an MCP edit and an fsnotify import of the same
	// document (both reading the pre-append edge set, both persisting the same
	// "mentions" edge). One importer -> one proseLocks map -> real serialization.
	if w.Importer == nil {
		t.Error("w.Importer is nil after OpenWiring; registerNavigationTools must store it on w so startVaultWatcher shares the same instance (one prose-extraction lock map)")
	}
}

var errSentinelUnused = errors.New("unused")

// TestClassifyEmbedderErr pins the degraded-write contract Greptile P1
// flagged on cdb8a2d: only genuinely transient / unreachable probe
// failures may downgrade the daemon. Configuration errors
// (errUnsupportedEmbedProvider), model-missing errors
// (ollama.ErrModelMissing, fixable via `ollama pull ...`), and
// caller-controlled cancellation (context.Canceled,
// context.DeadlineExceeded) MUST surface as-is so the operator gets an
// actionable failure rather than a silent recall-less daemon.
func TestClassifyEmbedderErr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      error
		wantNil bool // nil result == degrade to write-only mode
	}{
		{name: "nil", in: nil, wantNil: true},
		{name: "unsupported provider", in: errUnsupportedEmbedProvider, wantNil: false},
		{
			name:    "wrapped unsupported provider",
			in:      &wrappedError{inner: errUnsupportedEmbedProvider},
			wantNil: false,
		},
		{name: "invalid config", in: ollama.ErrInvalidConfig, wantNil: false},
		{
			// Pins the Greptile #119 P1: a malformed BaseURL surfaces from
			// ollama.New BEFORE any HTTP probe, wrapped through
			// buildEmbedder's fmt.Errorf("daemon: build ollama client: %w",
			// ...). Without keeping this class fatal, a config typo would
			// silently start the daemon in remember-only mode and hide
			// recall from tools/list.
			name:    "wrapped invalid config",
			in:      &wrappedError{inner: ollama.ErrInvalidConfig},
			wantNil: false,
		},
		{
			// #236: a not-yet-pulled embed model is an availability gap, not a
			// config bug. It degrades to FTS-only (loudly — doctor WARN + a
			// startup log naming `ollama pull`) rather than walling a fresh
			// install's first recall. `maz rebuild` backfills once pulled.
			name:    "model missing",
			in:      ollama.ErrModelMissing,
			wantNil: true,
		},
		{
			name:    "wrapped model missing",
			in:      &wrappedError{inner: ollama.ErrModelMissing},
			wantNil: true,
		},
		{name: "context canceled", in: context.Canceled, wantNil: false},
		{name: "context deadline", in: context.DeadlineExceeded, wantNil: false},
		{name: "generic transient", in: errors.New("dial tcp 127.0.0.1:11434: connection refused"), wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyEmbedderErr(tc.in, zap.NewNop())
			if tc.wantNil && got != nil {
				t.Errorf("classifyEmbedderErr(%v) = %v, want nil (degrade)", tc.in, got)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("classifyEmbedderErr(%v) = nil (degrade), want fatal", tc.in)
			}
			if !tc.wantNil && got != nil && !errors.Is(got, tc.in) {
				t.Errorf("classifyEmbedderErr(%v) = %v, must wrap or equal original", tc.in, got)
			}
		})
	}
}

// wrappedError is a tiny test helper that wraps an inner error so
// errors.Is can walk past one layer of fmt.Errorf("%w", ...) wrapping
// in the table above.
type wrappedError struct{ inner error }

func (w *wrappedError) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrappedError) Unwrap() error { return w.inner }

// TestBuildEmbedder_NoneReturnsNilNil pins the issue-#105 contract that
// `defaults.embed = "none"` is a first-class sentinel: the resolver
// returns (nil, nil) so OpenWiring can register the disabled recall
// tool and skip the Ollama probe entirely. Anything other than
// `(nil, nil)` here is a regression that breaks the Anthropic-only
// install path.
func TestBuildEmbedder_NoneReturnsNilNil(t *testing.T) {
	t.Parallel()

	user := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "none"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
		},
	}
	emb, err := buildEmbedder(t.Context(), user, zap.NewNop())
	if err != nil {
		t.Fatalf("buildEmbedder(embed=none) err = %v, want nil", err)
	}
	if emb != nil {
		t.Errorf("buildEmbedder(embed=none) embedder = %v, want nil", emb)
	}
}

// TestOpenWiring_EmbedNoneRegistersDisabledRecall pins that the wiring
// layer respects the nil-embedder contract end-to-end: a config with
// embed=none completes OpenWiring (no Ollama daemon required) and
// registers the disabled-variant recall tool alongside the write-only
// remember tool. Invoking recall surfaces a deterministic E_VALIDATION
// "embeddings disabled" message; remember stays callable so the daemon
// is genuinely usable in the Anthropic-only install path (#105 / #43).
func TestOpenWiring_EmbedNoneRegistersDisabledRecall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "none"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	w, err := OpenWiring(t.Context(), WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring with embed=none: err = %v, want nil", err)
	}
	defer func() {
		if cerr := w.Close(t.Context()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if w.Embedder != nil {
		t.Errorf("Embedder = %v, want nil", w.Embedder)
	}
	if w.Query != nil {
		t.Errorf("Query = %v, want nil (no HybridQuery when embeddings are disabled)", w.Query)
	}

	// Tools surface: remember + forget (write-only, always registered) +
	// recall-disabled variant + the read-only usage tools (getUsageSnapshot,
	// getCapacity) + the LLM-free navigation verbs, which are wired before the
	// embedder check and so remain available in degraded (no-embedder) mode.
	// listTopEntities folded into `entities` (sortBy=recallCount). The order
	// of remember at Tools[0] matters for the daemon supervisor's tool
	// registration loop; the rest are asserted by name lookup so the set
	// stays additive as new read-only tools land.
	names := make([]string, len(w.Tools))
	for i, tl := range w.Tools {
		names[i] = tl.Name()
	}
	if got, want := names[0], "remember"; got != want {
		t.Errorf("Tools[0].Name() = %q, want %q", got, want)
	}
	for _, want := range []string{
		"forget", "recall", "getUsageSnapshot", "getCapacity",
		"get", "neighbors", "relations", "entities", "count", "query", "introspect", "relate", "fact",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("Tools missing %q (got %v)", want, names)
		}
	}
	if slices.Contains(names, "listTopEntities") {
		t.Errorf("listTopEntities should be folded into entities, but is still registered (got %v)", names)
	}
	// Invoke the registered recall tool with a well-formed request; the
	// disabled variant must reject it with E_VALIDATION.
	recallIdx := slices.Index(names, "recall")
	if recallIdx < 0 {
		t.Fatalf("recall tool not registered (got %v)", names)
	}
	_, invokeErr := w.Tools[recallIdx].Invoke(t.Context(), []byte(`{"query":"anything"}`))
	if invokeErr == nil {
		t.Fatalf("recall on disabled tool: err = nil, want E_VALIDATION")
	}
	if !strings.Contains(invokeErr.Error(), "embeddings disabled") {
		t.Errorf("error %q should mention 'embeddings disabled'", invokeErr.Error())
	}
}

// TestOpenWiring_DeferEmbedderDegradesToFTS verifies that MAZ_DEFER_EMBEDDER
// keeps the daemon bootable when Ollama is unreachable. The env var
// short-circuits resolveEmbedder, so the daemon never makes the HTTP probe that
// was the prime suspect in the macOS-freeze report. Under #236 a deferred
// (transiently-unavailable) embedder no longer disables recall: it degrades to
// an FTS-only HybridQuery so the daemon serves BM25 hits on first contact while
// remaining live to debug from. No live embedder is wired, but Query is.
func TestOpenWiring_DeferEmbedderDegradesToFTS(t *testing.T) {
	// t.Setenv so the env mutation does not leak across parallel tests
	// that share the process env.
	t.Setenv(DeferEmbedderEnv, "1")

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		// Defaults.Embed names ollama on purpose — the env override is
		// what skips the probe, NOT the config setting. If the env
		// short-circuit ever regresses, the resolveEmbedder call will
		// blow up here trying to reach a non-existent Ollama daemon.
		Defaults: config.DefaultsTable{Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {BaseURL: "http://127.0.0.1:1"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	w, err := OpenWiring(t.Context(), WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring with MAZ_DEFER_EMBEDDER: err = %v, want nil", err)
	}
	t.Cleanup(func() { _ = w.Close(t.Context()) })

	if w.Embedder != nil {
		t.Errorf("Embedder = %v, want nil (probe was deferred)", w.Embedder)
	}
	if w.Query == nil {
		t.Fatal("Query = nil, want a degraded FTS-only HybridQuery (#236)")
	}

	// The registered recall must be the working (degraded) variant, not the
	// disabled stub: a well-formed request returns hits + degraded markers,
	// never E_VALIDATION "embeddings disabled".
	names := make([]string, len(w.Tools))
	for i, tl := range w.Tools {
		names[i] = tl.Name()
	}
	recallIdx := slices.Index(names, "recall")
	if recallIdx < 0 {
		t.Fatalf("recall tool not registered (got %v)", names)
	}
	resp, invokeErr := w.Tools[recallIdx].Invoke(t.Context(), []byte(`{"query":"anything"}`))
	if invokeErr != nil {
		t.Fatalf("degraded recall: err = %v, want nil (FTS-only should not hard-fail)", invokeErr)
	}
	var got struct {
		Degraded bool     `json:"degraded"`
		Lanes    []string `json:"lanes"`
	}
	if uerr := json.Unmarshal(resp, &got); uerr != nil {
		t.Fatalf("unmarshal recall response: %v", uerr)
	}
	if !got.Degraded {
		t.Error("recall response degraded = false, want true (no live embedder)")
	}
	if !slices.Contains(got.Lanes, "fts") {
		t.Errorf("recall lanes = %v, want fts present", got.Lanes)
	}
}

// TestOpenWiring_UnreachableOllamaDegradesToFTS is the #236 launch-gate E2E:
// a fresh install configured for Ollama but with NO Ollama daemon running
// (the BaseURL refuses connections) must still boot and serve working recall
// on the first call. This exercises the real probe path — buildEmbedder ->
// ollama.New /api/tags probe fails -> classifyEmbedderErr classifies it
// transient -> embedderDegraded -> FTS-only recall — NOT the env shortcut.
func TestOpenWiring_UnreachableOllamaDegradesToFTS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			// Privileged-only port: refuses connections in test, so the
			// /api/tags probe fails as "unreachable" (transient), not as a
			// config error (which would be fatal).
			"ollama": {BaseURL: "http://127.0.0.1:1"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	w, err := OpenWiring(t.Context(), WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring with unreachable Ollama: err = %v, want nil (should degrade, not fail)", err)
	}
	t.Cleanup(func() { _ = w.Close(t.Context()) })

	if w.Embedder != nil {
		t.Errorf("Embedder = %v, want nil (Ollama unreachable)", w.Embedder)
	}
	if w.Query == nil {
		t.Fatal("Query = nil, want a degraded FTS-only HybridQuery (#236)")
	}

	names := make([]string, len(w.Tools))
	for i, tl := range w.Tools {
		names[i] = tl.Name()
	}
	recallIdx := slices.Index(names, "recall")
	if recallIdx < 0 {
		t.Fatalf("recall tool not registered (got %v)", names)
	}
	resp, invokeErr := w.Tools[recallIdx].Invoke(t.Context(), []byte(`{"query":"anything"}`))
	if invokeErr != nil {
		t.Fatalf("first recall with no Ollama: err = %v, want nil (FTS-only must serve)", invokeErr)
	}
	var got struct {
		Degraded bool     `json:"degraded"`
		Lanes    []string `json:"lanes"`
	}
	if uerr := json.Unmarshal(resp, &got); uerr != nil {
		t.Fatalf("unmarshal recall response: %v", uerr)
	}
	if !got.Degraded {
		t.Error("recall degraded = false, want true (Ollama down)")
	}
	if !slices.Contains(got.Lanes, "fts") {
		t.Errorf("recall lanes = %v, want fts present", got.Lanes)
	}
}

// TestOpenWiring_MinimalBootRegistersOnlyRemember pins the most
// stripped boot mode: only the write-only remember tool registers; the
// embedder, hybrid query, and ladybug-dependent tools are all skipped.
// This is the operator escape hatch for "daemon refuses to boot at all"
// — if minimal boot succeeds, the failing component is one of the
// skipped optional layers.
func TestOpenWiring_MinimalBootRegistersOnlyRemember(t *testing.T) {
	t.Setenv(MinimalBootEnv, "1")

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {BaseURL: "http://127.0.0.1:1"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	w, err := OpenWiring(t.Context(), WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring with MAZ_MINIMAL_BOOT: err = %v, want nil", err)
	}
	t.Cleanup(func() { _ = w.Close(t.Context()) })

	if w.Embedder != nil {
		t.Errorf("Embedder = %v, want nil", w.Embedder)
	}
	if w.Query != nil {
		t.Errorf("Query = %v, want nil", w.Query)
	}
	names := make([]string, len(w.Tools))
	for i, tl := range w.Tools {
		names[i] = tl.Name()
	}
	if names[0] != "remember" {
		t.Errorf("Tools[0] = %q, want %q", names[0], "remember")
	}
	// Minimal boot returns before forget + the navigation verbs are wired, so
	// only the always-on initial block + the recall-disabled variant register.
	for _, want := range []string{"recall", "getUsageSnapshot", "getCapacity"} {
		if !slices.Contains(names, want) {
			t.Errorf("Tools missing %q (got %v)", want, names)
		}
	}
	for _, absent := range []string{"listTopEntities", "get", "neighbors", "relations", "entities"} {
		if slices.Contains(names, absent) {
			t.Errorf("minimal boot must not register %q (got %v)", absent, names)
		}
	}
}

// TestLoadUserConfigOrDefault_MergesStateOverlay pins gap #3: the daemon
// must merge state.toml on top of config.toml at boot. The hub TUI
// writes operator-level overrides (e.g. flip auto_push, change a remote
// URL) into state.toml so the user-curated config.toml stays
// untouched; if the daemon only reads config.toml those overrides
// vanish at restart. Failing before the fix at internal/daemon/wiring.go:585.
func TestLoadUserConfigOrDefault_MergesStateOverlay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	base := config.UserConfig{
		SchemaVersion: 1,
		Defaults: config.DefaultsTable{
			Chat:  "ollama",
			Embed: "ollama",
		},
		Sync: config.SyncTable{
			AutoPush: false,
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "git@example.com:base.git"},
			},
		},
	}
	if err := config.WriteUserConfig(dir, base); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	enabled := true
	overlay := config.UserState{
		Sync: config.SyncStateTable{
			AutoPush: &enabled,
		},
	}
	if err := config.WriteUserState(dir, overlay); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	got, err := loadUserConfigOrDefault(dir, zap.NewNop())
	if err != nil {
		t.Fatalf("loadUserConfigOrDefault: %v", err)
	}
	if !got.Sync.AutoPush {
		t.Errorf("Sync.AutoPush = false after state.toml overlay; want true (state.toml ignored at boot)")
	}
	// Non-overridden fields from config.toml must survive untouched.
	if got.Sync.Remotes["personal"].URL != "git@example.com:base.git" {
		t.Errorf("base remote URL lost after merge: %+v", got.Sync.Remotes["personal"])
	}
}
