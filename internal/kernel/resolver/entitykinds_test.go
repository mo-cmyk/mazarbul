package resolver

// entitykinds_test.go pins the entity-kind auto-registration pipeline:
// lexicon alias rewrites, lexicon new-kind registration, LLM-fallback mint,
// low-confidence skip-registration path, per-ingest cap enforcement, and LLM-error
// degradation. Tests are in-package so unexported helpers (kindLexiconLookup,
// autoRegisterEntityKind, ingestMintCounter) are accessible directly.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// newKindTestResolver builds the minimal Resolver needed by autoRegisterEntityKind:
// eventLog, chat, kindRegistry, promptReg, logger. Uses a live SQLite store so
// the entity_kinds table is available for NewKindRegistry.
func newKindTestResolver(t *testing.T, log *captureLog, chat kernel.ChatProvider, store *sqlite.Store) *Resolver {
	t.Helper()
	ctx := t.Context()
	kr, err := NewKindRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewKindRegistry: %v", err)
	}
	reg := prompts.New(nil)
	if err := RegisterPrompts(ctx, reg); err != nil {
		t.Fatalf("RegisterPrompts: %v", err)
	}
	cfg := Config{}
	cfg.ApplyDefaults()
	return &Resolver{
		cfg:          cfg,
		eventLog:     log,
		chat:         chat,
		kindRegistry: kr,
		promptReg:    reg,
		reader:       store.Reader(),
		logger:       zap.NewNop(),
	}
}

// openKindStore opens a fresh SQLite store with all migrations applied.
func openKindStore(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kinds.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// --- lexicon lookup tests ---

// TestKindLexiconLookup_AliasHit verifies alias entries return the canonical
// core kind and are detected as kindLexiconAlias.
func TestKindLexiconLookup_AliasHit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input    string
		wantKind string
	}{
		{"company", "organization"},
		{"firm", "organization"},
		{"human", "person"},
		{"individual", "person"},
		{"location", "place"},
		{"city", "place"},
		{"country", "place"},
		// Document-ish surface forms collapse onto the topic catch-all, NOT the
		// structural `document` kind — extraction must never mint a body-less
		// ghost document. See reserved_kind_test.go.
		{"file", "topic"},
		{"report", "topic"},
		{"article", "topic"},
		{"meeting", "event"},
		{"initiative", "project"},
		// Tech surface forms collapse onto the curated tool kind.
		{"technology", "tool"},
		{"library", "tool"},
		{"database", "tool"},
		{"protocol", "tool"},
		// Value/primitive pseudo-kinds collapse onto the topic catch-all.
		{"integer", "topic"},
		{"count", "topic"},
		{"tick", "topic"},
		// Workflow-state pseudo-kinds (mirroring the prompt denylist) likewise
		// collapse onto topic as a hard backstop.
		{"status", "topic"},
		{"phase", "topic"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			e, ok := kindLexiconLookup(tc.input)
			if !ok {
				t.Fatalf("kindLexiconLookup(%q): not found", tc.input)
			}
			if e.entryType != kindLexiconAlias {
				t.Errorf("entryType = %d, want kindLexiconAlias", e.entryType)
			}
			if e.target != tc.wantKind {
				t.Errorf("target = %q, want %q", e.target, tc.wantKind)
			}
		})
	}
}

// TestKindLexiconLookup_NewKindHit verifies the ten new-kind entries carry
// non-empty displayName and description.
func TestKindLexiconLookup_NewKindHit(t *testing.T) {
	t.Parallel()
	newKinds := []string{"product", "deal", "task", "vehicle", "vessel", "team", "skill", "tool", "topic", "animal"}
	for _, k := range newKinds {
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			e, ok := kindLexiconLookup(k)
			if !ok {
				t.Fatalf("kindLexiconLookup(%q): not found", k)
			}
			if e.entryType != kindLexiconNew {
				t.Errorf("%q entryType = %d, want kindLexiconNew", k, e.entryType)
			}
			if e.displayName == "" {
				t.Errorf("%q: displayName must not be empty", k)
			}
			if e.description == "" {
				t.Errorf("%q: description must not be empty", k)
			}
		})
	}
}

// TestKindLexiconLookup_CaseInsensitive verifies the lookup trims and lowercases
// the input, mirroring the predicate lexicon's behaviour.
func TestKindLexiconLookup_CaseInsensitive(t *testing.T) {
	t.Parallel()
	e, ok := kindLexiconLookup("  Company  ")
	if !ok {
		t.Fatal("lookup must be case-insensitive and trim whitespace")
	}
	if e.target != "organization" {
		t.Errorf("target = %q, want organization", e.target)
	}
}

// TestKindLexiconLookup_CoreKindMisses verifies the six core seed kinds do NOT
// appear in the lexicon (they are always registered; a duplicate entry would be
// dead weight and could shadow the IsRegistered fast path).
func TestKindLexiconLookup_CoreKindMisses(t *testing.T) {
	t.Parallel()
	for k := range coreKinds {
		if _, ok := kindLexiconLookup(k); ok {
			t.Errorf("core kind %q must not appear in entityKindLexicon", k)
		}
	}
}

// TestKindLexiconLookup_Miss returns false for unknown kinds so the LLM path
// still fires for genuinely novel kinds.
func TestKindLexiconLookup_Miss(t *testing.T) {
	t.Parallel()
	if _, ok := kindLexiconLookup("spaceship_component_xyz"); ok {
		t.Error("unknown kind must miss the lexicon")
	}
}

// --- autoRegisterEntityKind: alias path ---

// TestAutoRegisterEntityKind_AliasRewritesKind verifies that an alias lexicon
// entry rewrites the kind to the canonical core value with zero LLM calls and
// zero event appends.
func TestAutoRegisterEntityKind_AliasRewritesKind(t *testing.T) {
	t.Parallel()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &countingChat{}
	r := newKindTestResolver(t, log, chat, store)

	canonical, err := r.autoRegisterEntityKind(t.Context(), "company", "Erebor is a company", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind: %v", err)
	}
	if canonical != "organization" {
		t.Errorf("canonical = %q, want organization", canonical)
	}
	if got := chat.toolUses.Load() + chat.completes.Load(); got != 0 {
		t.Errorf("alias rewrite must use ZERO LLM calls, got %d", got)
	}
	if len(log.events) != 0 {
		t.Errorf("alias rewrite must append ZERO events, got %d", len(log.events))
	}
}

// TestAutoRegisterEntityKind_AliasRegistersNonCoreTarget verifies that an alias
// onto a non-core curated kind (tech form -> "tool") rewrites the kind AND
// registers the target, so the rewritten kind is a known kind rather than an
// unregistered warn-accept. No LLM calls; exactly one registration event.
func TestAutoRegisterEntityKind_AliasRegistersNonCoreTarget(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &countingChat{}
	r := newKindTestResolver(t, log, chat, store)

	canonical, err := r.autoRegisterEntityKind(ctx, "technology", "FTS5 is a search technology", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind: %v", err)
	}
	if canonical != "tool" {
		t.Fatalf("canonical = %q, want tool", canonical)
	}
	if got := chat.toolUses.Load() + chat.completes.Load(); got != 0 {
		t.Errorf("alias rewrite must use ZERO LLM calls, got %d", got)
	}
	if !r.kindRegistry.IsRegistered("tool") {
		t.Errorf("alias target %q must be registered after rewrite", "tool")
	}
	if len(log.events) != 1 {
		t.Errorf("expected exactly one kind-registration event, got %d", len(log.events))
	}

	// Second call: target already registered, so no further event.
	if _, err := r.autoRegisterEntityKind(ctx, "library", "React is a library", &ingestMintCounter{}); err != nil {
		t.Fatalf("second autoRegisterEntityKind: %v", err)
	}
	if len(log.events) != 1 {
		t.Errorf("alias to already-registered target must append no new event, got %d", len(log.events))
	}
}

// TestAutoRegisterEntityKind_CoreKindNoOp verifies that passing a core kind
// returns it unchanged with no events and no LLM calls.
func TestAutoRegisterEntityKind_CoreKindNoOp(t *testing.T) {
	t.Parallel()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &countingChat{}
	r := newKindTestResolver(t, log, chat, store)

	for _, k := range []string{"person", "organization", "place", "project", "event", "document"} {
		canonical, err := r.autoRegisterEntityKind(t.Context(), k, "", &ingestMintCounter{})
		if err != nil {
			t.Fatalf("autoRegisterEntityKind(%q): %v", k, err)
		}
		if canonical != k {
			t.Errorf("core kind %q: canonical = %q, want %q", k, canonical, k)
		}
	}
	if len(log.events) != 0 {
		t.Errorf("core kinds must produce ZERO events, got %d", len(log.events))
	}
}

// --- autoRegisterEntityKind: lexicon new-kind path ---

// TestAutoRegisterEntityKind_LexiconNewKindRegistersExactlyOnce verifies that a
// lexicon new-kind entry appends exactly one EntityKindRegisteredEvent, updates
// the KindRegistry cache, and on a second call is a no-op (idempotent).
func TestAutoRegisterEntityKind_LexiconNewKindRegistersExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &countingChat{}
	r := newKindTestResolver(t, log, chat, store)

	// First call: must register.
	canonical, err := r.autoRegisterEntityKind(ctx, "vehicle", "Frodo drives a Tesla", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("first autoRegisterEntityKind: %v", err)
	}
	if canonical != "vehicle" {
		t.Errorf("canonical = %q, want vehicle", canonical)
	}
	if len(log.events) != 1 {
		t.Fatalf("want exactly 1 EntityKindRegistered event after first call, got %d", len(log.events))
	}
	if got := chat.toolUses.Load() + chat.completes.Load(); got != 0 {
		t.Errorf("lexicon new-kind must use ZERO LLM calls, got %d", got)
	}

	// Verify event payload.
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Name != "vehicle" {
		t.Errorf("event.Name = %q, want vehicle", ev.Name)
	}
	if ev.Provenance != "user" {
		t.Errorf("event.Provenance = %q, want user", ev.Provenance)
	}
	if ev.AddedBy != ontology.EntityKindAddedByLLM {
		t.Errorf("event.AddedBy = %q, want llm", ev.AddedBy)
	}
	if ev.AddedAt.IsZero() {
		t.Error("event.AddedAt must be set")
	}

	// Second call: cache hit, no additional events.
	_, err = r.autoRegisterEntityKind(ctx, "vehicle", "another vehicle", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("second autoRegisterEntityKind: %v", err)
	}
	if len(log.events) != 1 {
		t.Errorf("idempotent: want still 1 event after second call, got %d", len(log.events))
	}
}

// --- autoRegisterEntityKind: LLM fallback mint path ---

// mintingChat is a ChatProvider whose ToolUse response simulates a high-confidence
// "mint" action for a given kind name.
type mintingChat struct {
	countingChat
	action      string
	kind        string
	displayName string
	description string
	confidence  float64
}

func (c *mintingChat) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	c.toolUses.Add(1)
	out := struct {
		Action      string  `json:"action"`
		Kind        string  `json:"kind"`
		DisplayName string  `json:"displayName"`
		Description string  `json:"description"`
		Confidence  float64 `json:"confidence"`
	}{
		Action:      c.action,
		Kind:        c.kind,
		DisplayName: c.displayName,
		Description: c.description,
		Confidence:  c.confidence,
	}
	raw, _ := json.Marshal(out)
	return kernel.ToolUseResponse{
		ToolUse: &kernel.ToolInvocation{Input: raw},
	}, nil
}

// TestAutoRegisterEntityKind_LLMFallbackMint verifies the LLM-fallback path for
// a genuinely novel kind (not in lexicon): high-confidence mint appends one
// EntityKindRegisteredEvent and returns the minted kind name.
func TestAutoRegisterEntityKind_LLMFallbackMint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &mintingChat{
		action:      "mint",
		kind:        "spacecraft",
		displayName: "Spacecraft",
		description: "An extraterrestrial vehicle designed for space travel.",
		confidence:  0.92,
	}
	r := newKindTestResolver(t, log, chat, store)

	canonical, err := r.autoRegisterEntityKind(ctx, "spacecraft", "The Falcon 9 is a spacecraft", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind: %v", err)
	}
	if canonical != "spacecraft" {
		t.Errorf("canonical = %q, want spacecraft", canonical)
	}
	if chat.toolUses.Load() != 1 {
		t.Errorf("LLM fallback must make exactly 1 ToolUse call, got %d", chat.toolUses.Load())
	}
	if len(log.events) != 1 {
		t.Fatalf("want 1 EntityKindRegistered event, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Name != "spacecraft" {
		t.Errorf("event.Name = %q, want spacecraft", ev.Name)
	}
}

// TestAutoRegisterEntityKind_LLMFallbackReuse verifies that a "reuse" action
// returns the existing kind name with no registration event.
func TestAutoRegisterEntityKind_LLMFallbackReuse(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &mintingChat{
		action:     "reuse",
		kind:       "organization",
		confidence: 0.95,
	}
	r := newKindTestResolver(t, log, chat, store)

	canonical, err := r.autoRegisterEntityKind(ctx, "conglomerate", "Berkshire is a conglomerate", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind: %v", err)
	}
	if canonical != "organization" {
		t.Errorf("canonical = %q, want organization", canonical)
	}
	if len(log.events) != 0 {
		t.Errorf("reuse action must produce ZERO events, got %d", len(log.events))
	}
}

// --- autoRegisterEntityKind: low-confidence review path ---

// TestAutoRegisterEntityKind_LowConfidencePermissive verifies that a mint
// action below kindClassifyConfidenceThreshold in permissive mode routes to
// the proposed review queue: exactly one EntityKindRegisteredEvent with
// Status="proposed" is appended, the kind is returned as-is so the entity
// still writes, and the kind is NOT added to the active KindRegistry cache.
func TestAutoRegisterEntityKind_LowConfidencePermissive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	// Confidence below threshold (0.6).
	chat := &mintingChat{
		action:     "mint",
		kind:       "nebulous_thing",
		confidence: 0.35,
	}
	r := newKindTestResolver(t, log, chat, store)

	canonical, err := r.autoRegisterEntityKind(ctx, "nebulous_thing", "some vague context", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind: %v", err)
	}
	// Kind is returned as-is: entity still writes.
	if canonical != "nebulous_thing" {
		t.Errorf("canonical = %q, want nebulous_thing", canonical)
	}
	// Exactly one proposed event must be appended — NOT zero, NOT active.
	if len(log.events) != 1 {
		t.Fatalf("want 1 proposed event for low-confidence permissive mint, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != ontology.EntityKindStatusProposed {
		t.Errorf("event.Status = %q, want proposed", ev.Status)
	}
	// Kind must NOT be in the active registry cache.
	if r.kindRegistry.IsRegistered("nebulous_thing") {
		t.Error("low-confidence kind must not be added to active KindRegistry cache")
	}
}

// TestAutoRegisterEntityKind_HighConfidencePermissive verifies that a mint
// action at or above kindClassifyConfidenceThreshold in permissive mode
// appends an EntityKindRegisteredEvent with Status="" (active) and adds the
// kind to the KindRegistry cache. Pinned as the paired counterpart to the
// low-confidence test above to document the full permissive routing matrix.
func TestAutoRegisterEntityKind_HighConfidencePermissive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &mintingChat{
		action:      "mint",
		kind:        "gadget",
		displayName: "Gadget",
		description: "A small mechanical or electronic device.",
		confidence:  0.85,
	}
	r := newKindTestResolver(t, log, chat, store)

	canonical, err := r.autoRegisterEntityKind(ctx, "gadget", "some context", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind: %v", err)
	}
	if canonical != "gadget" {
		t.Errorf("canonical = %q, want gadget", canonical)
	}
	// One active event must be appended.
	if len(log.events) != 1 {
		t.Fatalf("want 1 active event for high-confidence permissive mint, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != "" {
		t.Errorf("event.Status = %q, want empty (active default)", ev.Status)
	}
	// Kind MUST be in the active registry cache after a high-confidence mint.
	if !r.kindRegistry.IsRegistered("gadget") {
		t.Error("high-confidence permissive kind must be added to KindRegistry cache")
	}
}

// --- autoRegisterEntityKind: per-ingest cap ---

// TestAutoRegisterEntityKind_CapEnforced verifies that after maxKindMintsPerIngest
// mints the cap prevents further registrations and Warn-logs are emitted
// (indirectly verified by confirming the kind is left as-is with no event).
func TestAutoRegisterEntityKind_CapEnforced(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	// High-confidence mint chat: every novel kind gets minted.
	chat := &mintingChatN{}
	r := newKindTestResolver(t, log, chat, store)

	counter := &ingestMintCounter{}

	// Exhaust the cap with lexicon new-kinds (each appends one event and
	// increments the counter via tryIncrement inside appendEntityKindRegistered
	// — but wait: lexicon hits do NOT call tryIncrement. The cap guards the
	// LLM-fallback path only. Use the LLM path by passing kinds not in the
	// lexicon.
	novelKinds := []string{"spacecraft", "drone", "submarine", "robot", "cyborg"}
	for _, k := range novelKinds {
		chat.setNext("mint", k, "A "+k+".", 0.9)
		_, err := r.autoRegisterEntityKind(ctx, k, "ctx "+k, counter)
		if err != nil {
			t.Fatalf("autoRegisterEntityKind(%q): %v", k, err)
		}
	}
	if int(counter.n.Load()) != maxKindMintsPerIngest {
		t.Errorf("counter = %d, want %d", counter.n.Load(), maxKindMintsPerIngest)
	}

	// One more beyond cap: must be a no-op (no event, kind returned as-is).
	eventsBefore := len(log.events)
	chat.setNext("mint", "alien_device", "An alien device.", 0.9)
	canonical, err := r.autoRegisterEntityKind(ctx, "alien_device", "some ctx", counter)
	if err != nil {
		t.Fatalf("autoRegisterEntityKind beyond cap: %v", err)
	}
	if canonical != "alien_device" {
		t.Errorf("beyond cap: canonical = %q, want alien_device", canonical)
	}
	if len(log.events) != eventsBefore {
		t.Errorf("beyond cap: want no new events, got %d new events", len(log.events)-eventsBefore)
	}
}

// mintingChatN is a ToolUse chat that returns configurable per-call responses.
type mintingChatN struct {
	countingChat
	nextAction      string
	nextKind        string
	nextDescription string
	nextConfidence  float64
}

func (c *mintingChatN) setNext(action, kind, description string, confidence float64) {
	c.nextAction = action
	c.nextKind = kind
	c.nextDescription = description
	c.nextConfidence = confidence
}

func (c *mintingChatN) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	c.toolUses.Add(1)
	out := struct {
		Action      string  `json:"action"`
		Kind        string  `json:"kind"`
		DisplayName string  `json:"displayName"`
		Description string  `json:"description"`
		Confidence  float64 `json:"confidence"`
	}{
		Action:      c.nextAction,
		Kind:        c.nextKind,
		DisplayName: strings.Title(c.nextKind), //nolint:staticcheck
		Description: c.nextDescription,
		Confidence:  c.nextConfidence,
	}
	raw, _ := json.Marshal(out)
	return kernel.ToolUseResponse{
		ToolUse: &kernel.ToolInvocation{Input: raw},
	}, nil
}

// --- autoRegisterEntityKind: LLM error degradation ---

// errorChat returns an error on every ToolUse call to simulate LLM unavailability.
type errorChat struct {
	countingChat
}

func (c *errorChat) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	c.toolUses.Add(1)
	return kernel.ToolUseResponse{}, errors.New("LLM unavailable")
}

// TestAutoRegisterEntityKind_LLMErrorDegrades verifies that an LLM error on the
// fallback path returns the original kind as-is without error and without
// appending any event. Ingest must never fail due to a kind-classification error.
func TestAutoRegisterEntityKind_LLMErrorDegrades(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	chat := &errorChat{}
	r := newKindTestResolver(t, log, chat, store)

	canonical, err := r.autoRegisterEntityKind(ctx, "unknown_novel_kind", "some context", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("LLM error must not propagate: %v", err)
	}
	if canonical != "unknown_novel_kind" {
		t.Errorf("canonical = %q, want unknown_novel_kind", canonical)
	}
	if len(log.events) != 0 {
		t.Errorf("LLM error path must produce ZERO events, got %d", len(log.events))
	}
}

// --- KindRegistry ---

// TestKindRegistry_LoadsCoreSeedKinds verifies that NewKindRegistry loads the
// six core kinds seeded by genesis events (via migration 0019 + genesis boot).
// In a fresh test store the genesis events haven't run, so entity_kinds is
// empty — this test instead verifies the load succeeds and IsRegistered
// correctly returns false for a kind not in the table.
func TestKindRegistry_IsRegistered_Miss(t *testing.T) {
	t.Parallel()
	store := openKindStore(t)
	kr, err := NewKindRegistry(t.Context(), store.Reader())
	if err != nil {
		t.Fatalf("NewKindRegistry: %v", err)
	}
	if kr.IsRegistered("totally_unknown") {
		t.Error("IsRegistered(unknown) = true, want false on fresh store")
	}
}

// TestKindRegistry_MarkRegistered verifies that MarkRegistered makes a kind
// visible to IsRegistered without a DB round-trip.
func TestKindRegistry_MarkRegistered(t *testing.T) {
	t.Parallel()
	store := openKindStore(t)
	kr, err := NewKindRegistry(t.Context(), store.Reader())
	if err != nil {
		t.Fatalf("NewKindRegistry: %v", err)
	}
	kr.MarkRegistered("my_kind")
	if !kr.IsRegistered("my_kind") {
		t.Error("IsRegistered(my_kind) = false after MarkRegistered")
	}
}

// --- ingestMintCounter ---

// TestIngestMintCounter_EnforcesMax verifies that tryIncrement returns true
// exactly maxKindMintsPerIngest times and false thereafter.
func TestIngestMintCounter_EnforcesMax(t *testing.T) {
	t.Parallel()
	c := &ingestMintCounter{}
	for i := range maxKindMintsPerIngest {
		if !c.tryIncrement() {
			t.Errorf("call %d: tryIncrement = false, want true", i+1)
		}
	}
	// One beyond cap.
	if c.tryIncrement() {
		t.Errorf("call %d (beyond cap): tryIncrement = true, want false", maxKindMintsPerIngest+1)
	}
}

// --- KindRegistry: RegisterPrompts registers entity-kind-classify-v1 ---

// TestRegisterPrompts_EntityKindClassifyTemplateRegistered verifies that
// RegisterPrompts includes the entity-kind-classify-v1 template so
// llmClassifyEntityKind can Render it.
func TestRegisterPrompts_EntityKindClassifyTemplateRegistered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	reg := prompts.New(nil)
	if err := RegisterPrompts(ctx, reg); err != nil {
		t.Fatalf("RegisterPrompts: %v", err)
	}
	tpl, err := reg.Get(ctx, entityKindClassifyTemplateName, entityKindClassifyTemplateVersion)
	if err != nil {
		t.Fatalf("Get entity-kind-classify-v1: %v", err)
	}
	if tpl.Version != entityKindClassifyTemplateVersion {
		t.Errorf("version = %d, want %d", tpl.Version, entityKindClassifyTemplateVersion)
	}
	if tpl.Body == "" {
		t.Error("entity-kind-classify-v1 body must not be empty")
	}
}

// helpers used by cap test — keep below declarations so they don't conflict.
// strings.Title is deprecated but acceptable for test display names.
var _ = time.Now // keep time import used (AddedAt in event payloads)
