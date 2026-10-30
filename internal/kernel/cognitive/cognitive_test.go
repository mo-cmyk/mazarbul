package cognitive_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/cognitive"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/provider/resilience"
)

// seedCognitiveNavigator opens a fresh migrated SQLite projection, seeds a small
// graph, and returns a Navigator over its reader. The graph:
//
//	s1 --related_to--> n1   (relation r1, origin episode ep1)
//	ep1 mentions s1 and c1
//	ep2 mentions c1
//
// So Neighbors(s1) and Neighbors(n1) are non-empty (reflection has clusters to
// work on), and EpisodesCitingEntity(c1) returns two episodes (consolidation has
// a group >= the minimum size to compress).
func seedCognitiveNavigator(t *testing.T) *navigate.Navigator {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "s1", "person", "Seed One", 1.0, "confirmed", now)
	exec(ent, "n1", "organization", "Neighbor One", 1.0, "confirmed", now)
	exec(ent, "c1", "person", "Cited One", 1.0, "confirmed", now)

	exec(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes)
	      VALUES (?,?,?,?,?,?,?,?)`,
		"r1", "s1", "related_to", "n1", 1.0, "ep1", now, `{}`)

	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)`,
		"ep1", now, "episode", `{"text":"Seed One relates to Neighbor One; Cited One is involved.","source":"cli:test","confidence":1.0}`)
	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)`,
		"ep2", now, "episode", `{"text":"Cited One appears again in a later session.","source":"cli:test","confidence":1.0}`)

	mention := `INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid) VALUES (?,?,?,?,?,?)`
	exec(mention, "m1", "person", "Seed One", "ep1", now, "s1")
	exec(mention, "m2", "person", "Cited One", "ep1", now, "c1")
	exec(mention, "m3", "person", "Cited One", "ep2", now, "c1")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

// fakeChat is a ChatProvider double whose ToolUse returns a configured tool
// invocation (or error). The Input it returns carries BOTH the reflection and
// consolidation field names so a single fake serves either job; each job's
// decoder picks the fields it needs.
type fakeChat struct {
	citeUIDs   []string
	err        error
	nilToolUse bool
}

func (f *fakeChat) Complete(context.Context, kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	return kernel.CompleteResponse{}, nil
}

func (f *fakeChat) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	if f.err != nil {
		return kernel.ToolUseResponse{}, f.err
	}
	if f.nilToolUse {
		return kernel.ToolUseResponse{ToolUse: nil, StopReason: "end_turn"}, nil
	}
	input, _ := json.Marshal(map[string]any{
		"statement":   "A synthesized insight about the cluster.",
		"summary":     "A consolidated summary of the episode group.",
		"insightType": "pattern",
		"memoryType":  "semantic",
		"sourceUids":  f.citeUIDs,
		"confidence":  0.8,
	})
	return kernel.ToolUseResponse{
		ToolUse:    &kernel.ToolInvocation{Name: "tool", Input: input},
		StopReason: "tool_use",
	}, nil
}

// fakeDeduper is a Deduper double that records every CheckDuplicate call and
// returns a configured duplicate UID (empty = not a duplicate).
type fakeDeduper struct {
	mu       sync.Mutex
	calls    int
	dupUID   string // non-empty means "this is a duplicate of dupUID"
	checkErr error  // non-nil means return this error from CheckDuplicate
}

func (f *fakeDeduper) CheckDuplicate(_ context.Context, _, _, _ string, _ float64) (string, []float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.checkErr != nil {
		return "", nil, f.checkErr
	}
	return f.dupUID, nil, nil
}

func (f *fakeDeduper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// captureAppender records every appended event and returns a receipt per event.
type captureAppender struct {
	mu     sync.Mutex
	events []kernel.Event
}

func (c *captureAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, events...)
	receipts := make([]kernel.Receipt, len(events))
	for i := range events {
		receipts[i] = kernel.Receipt{UID: ulid.Make().String()}
	}
	return receipts, nil
}

func (c *captureAppender) all() []kernel.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]kernel.Event, len(c.events))
	copy(out, c.events)
	return out
}

// TestReflector_Run_AppendsCitedInsight verifies a valid LLM response yields at
// least one insightSynthesized event whose sourceUids are non-empty and drawn
// only from the offered candidate set (no hallucinated provenance).
func TestReflector_Run_AppendsCitedInsight(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigator(t)
	// "s1" and "ep1" are real candidates in s1/n1's neighborhood cluster.
	chat := &fakeChat{citeUIDs: []string{"s1", "ep1"}}
	app := &captureAppender{}
	r := cognitive.NewReflector(chat, nav, app, nil, "test-model", nil)

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.AppendedEventUIDs) == 0 {
		t.Fatalf("expected >= 1 appended insight, got skipped=%q", res.Skipped)
	}
	for _, ev := range app.all() {
		var in struct {
			SourceUids []string `json:"sourceUids"`
		}
		if uerr := json.Unmarshal(ev.Payload, &in); uerr != nil {
			t.Fatalf("decode insight payload: %v", uerr)
		}
		if len(in.SourceUids) == 0 {
			t.Error("appended insight has empty sourceUids (auditable-provenance violation)")
		}
		for _, u := range in.SourceUids {
			if u != "s1" && u != "ep1" && u != "n1" {
				t.Errorf("insight cites %q, not in the offered candidate set", u)
			}
		}
	}
}

// TestReflector_Run_HallucinatedUIDRejected verifies that when the LLM cites
// only a UID it was never shown, the insight is rejected (filtered to an empty
// source set) and no event is appended.
func TestReflector_Run_HallucinatedUIDRejected(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigator(t)
	chat := &fakeChat{citeUIDs: []string{"01HALLUCINATED0000000000ZZ"}}
	app := &captureAppender{}
	r := cognitive.NewReflector(chat, nav, app, nil, "test-model", nil)

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(app.all()) != 0 {
		t.Errorf("appended %d events citing a hallucinated UID, want 0", len(app.all()))
	}
	if res.Skipped == "" {
		t.Error("expected a skipped reason when nothing was appended")
	}
}

// TestReflector_Run_BreakerOpen is the #236 degradation contract: a tripped
// circuit breaker stops the pass cleanly (Skipped="breaker-open"), appends
// nothing, and never panics.
func TestReflector_Run_BreakerOpen(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigator(t)
	chat := &fakeChat{err: resilience.ErrOpen}
	app := &captureAppender{}
	r := cognitive.NewReflector(chat, nav, app, nil, "test-model", nil)

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run must not error on breaker-open, got %v", err)
	}
	if res.Skipped != "breaker-open" {
		t.Errorf("Skipped = %q, want breaker-open", res.Skipped)
	}
	if len(app.all()) != 0 {
		t.Errorf("appended %d events on breaker-open, want 0", len(app.all()))
	}
}

// TestConsolidator_Run_AppendsConsolidated verifies a valid response over the
// two-episode group citing c1 yields exactly one memoryConsolidated event with
// non-empty, non-hallucinated sourceUids.
func TestConsolidator_Run_AppendsConsolidated(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigator(t)
	chat := &fakeChat{citeUIDs: []string{"ep1", "ep2"}}
	app := &captureAppender{}
	c := cognitive.NewConsolidator(chat, nav, app, nil, "test-model", nil)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.AppendedEventUIDs) != 1 {
		t.Fatalf("appended %d consolidated memories, want 1 (only c1 has a group >= 2); skipped=%q",
			len(res.AppendedEventUIDs), res.Skipped)
	}
	var in struct {
		SourceUids []string `json:"sourceUids"`
	}
	if uerr := json.Unmarshal(app.all()[0].Payload, &in); uerr != nil {
		t.Fatalf("decode consolidated payload: %v", uerr)
	}
	if len(in.SourceUids) == 0 {
		t.Error("consolidated memory has empty sourceUids")
	}
	for _, u := range in.SourceUids {
		if u != "ep1" && u != "ep2" {
			t.Errorf("consolidated memory cites %q, not in the offered episode group", u)
		}
	}
}

// TestConsolidator_Run_BreakerOpen pins the degradation contract for the
// consolidation job.
func TestConsolidator_Run_BreakerOpen(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigator(t)
	chat := &fakeChat{err: resilience.ErrOpen}
	app := &captureAppender{}
	c := cognitive.NewConsolidator(chat, nav, app, nil, "test-model", nil)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run must not error on breaker-open, got %v", err)
	}
	if res.Skipped != "breaker-open" {
		t.Errorf("Skipped = %q, want breaker-open", res.Skipped)
	}
	if len(app.all()) != 0 {
		t.Errorf("appended %d events on breaker-open, want 0", len(app.all()))
	}
}

// TestReflector_Run_DeduplicatesRepeatPass verifies that two consecutive
// reflection passes over the same seed entity produce exactly ONE insight: the
// deduper reports a match on the second pass, so the second synthesizeForSeed
// call skips appending and the pass returns Skipped="no-candidates".
func TestReflector_Run_DeduplicatesRepeatPass(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigator(t)
	chat := &fakeChat{citeUIDs: []string{"s1", "ep1"}}
	app := &captureAppender{}
	// First pass: deduper finds nothing (empty dupUID = not a dup).
	dedup := &fakeDeduper{}
	r := cognitive.NewReflector(chat, nav, app, dedup, "test-model", nil)

	res1, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(res1.AppendedEventUIDs) == 0 {
		t.Fatalf("first pass: expected >= 1 appended insight, got skipped=%q", res1.Skipped)
	}
	firstCount := len(app.all())
	if firstCount == 0 {
		t.Fatal("first pass: captureAppender has no events")
	}

	// Second pass: simulate a matching insight already existing (dupUID non-empty).
	dedup2 := &fakeDeduper{dupUID: "existing-insight-uid"}
	r2 := cognitive.NewReflector(chat, nav, app, dedup2, "test-model", nil)
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.Skipped != "no-candidates" {
		t.Errorf("second pass Skipped = %q, want no-candidates (all seeds dedup-suppressed)", res2.Skipped)
	}
	if len(app.all()) != firstCount {
		t.Errorf("second pass appended %d extra events, want 0 (dedup must suppress all)", len(app.all())-firstCount)
	}
	if dedup2.callCount() == 0 {
		t.Error("deduper was never called on second pass; dedup guard not wired")
	}
}

// TestConsolidator_Run_DeduplicatesSemanticMatch verifies that when the deduper
// signals a match for the candidate summary, the consolidation pass skips
// appending it and returns Skipped="no-candidates".
func TestConsolidator_Run_DeduplicatesSemanticMatch(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigator(t)
	chat := &fakeChat{citeUIDs: []string{"ep1", "ep2"}}
	app := &captureAppender{}
	// Deduper always reports a match — the candidate is a duplicate.
	dedup := &fakeDeduper{dupUID: "existing-memory-uid"}
	c := cognitive.NewConsolidator(chat, nav, app, dedup, "test-model", nil)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != "no-candidates" {
		t.Errorf("Skipped = %q, want no-candidates (duplicate suppressed)", res.Skipped)
	}
	if len(app.all()) != 0 {
		t.Errorf("appended %d events for a duplicate candidate, want 0", len(app.all()))
	}
	if dedup.callCount() == 0 {
		t.Error("deduper was never called; consolidation dedup guard not wired")
	}
}

// seedCognitiveNavigatorWithHotInsight opens a fresh migrated SQLite projection
// with the same base graph as seedCognitiveNavigator PLUS hot insights anchored
// to every entity (s1, n1, c1). All three seeds are saturated so the reflection
// pass has no work to do. This lets the saturation-guard tests exercise the real
// HasHotInsightForSeed SQL path rather than a mock.
func seedCognitiveNavigatorWithHotInsight(t *testing.T) *navigate.Navigator {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "s1", "person", "Seed One", 1.0, "confirmed", now)
	exec(ent, "n1", "organization", "Neighbor One", 1.0, "confirmed", now)
	exec(ent, "c1", "person", "Cited One", 1.0, "confirmed", now)

	exec(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes)
	      VALUES (?,?,?,?,?,?,?,?)`,
		"r1", "s1", "related_to", "n1", 1.0, "ep1", now, `{}`)

	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)`,
		"ep1", now, "episode", `{"text":"Seed One relates to Neighbor One; Cited One is involved.","source":"cli:test","confidence":1.0}`)
	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)`,
		"ep2", now, "episode", `{"text":"Cited One appears again in a later session.","source":"cli:test","confidence":1.0}`)

	mention := `INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid) VALUES (?,?,?,?,?,?)`
	exec(mention, "m1", "person", "Seed One", "ep1", now, "s1")
	exec(mention, "m2", "person", "Cited One", "ep1", now, "c1")
	exec(mention, "m3", "person", "Cited One", "ep2", now, "c1")

	insightRow := `INSERT INTO insights
		(uid, statement, insight_type, source_uids, seed_entity_uid,
		 synthesized_confidence, current_confidence, state, source_event_uid,
		 synthesized_at, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	// Hot insights for all three entities so every seed in the ListEntities result
	// is already covered; the reflection pass must append nothing.
	exec(insightRow,
		"ins_s1_hot", "Seed One is the central node of the cluster.", "pattern",
		`["s1","ep1"]`, "s1", 0.9, 0.9, "hot", "ep1", now, now, "personal",
	)
	exec(insightRow,
		"ins_n1_hot", "Neighbor One connects to Seed One.", "pattern",
		`["n1","ep1"]`, "n1", 0.9, 0.9, "hot", "ep1", now, now, "personal",
	)
	exec(insightRow,
		"ins_c1_hot", "Cited One appears across multiple episodes.", "pattern",
		`["c1","ep1"]`, "c1", 0.9, 0.9, "hot", "ep1", now, now, "personal",
	)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

// TestReflector_Run_SaturatedSeedSkipped verifies that when every seed entity
// already has a hot insight the pass appends nothing and returns
// Skipped="no-candidates".
func TestReflector_Run_SaturatedSeedSkipped(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigatorWithHotInsight(t)
	chat := &fakeChat{citeUIDs: []string{"s1", "ep1"}}
	app := &captureAppender{}
	r := cognitive.NewReflector(chat, nav, app, nil, "test-model", nil)

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(app.all()) != 0 {
		t.Errorf("appended %d events for saturated seeds, want 0", len(app.all()))
	}
	if res.Skipped != "no-candidates" {
		t.Errorf("Skipped = %q, want no-candidates when all seeds are saturated", res.Skipped)
	}
}

// TestReflector_Run_SaturatedSeedNoChatCall asserts the chat provider is NOT
// called for a seed that already has a hot insight (no wasted LLM round-trip).
func TestReflector_Run_SaturatedSeedNoChatCall(t *testing.T) {
	t.Parallel()
	nav := seedCognitiveNavigatorWithHotInsight(t)

	callCount := 0
	counting := &countingChat{inner: &fakeChat{citeUIDs: []string{"s1", "ep1"}}, count: &callCount}
	app := &captureAppender{}
	r := cognitive.NewReflector(counting, nav, app, nil, "test-model", nil)

	_, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if callCount != 0 {
		t.Errorf("chat provider called %d times for saturated seed, want 0", callCount)
	}
}

// countingChat wraps a ChatProvider and increments count on every ToolUse call.
type countingChat struct {
	inner kernel.ChatProvider
	count *int
}

func (c *countingChat) Complete(ctx context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	return c.inner.Complete(ctx, req)
}

func (c *countingChat) ToolUse(ctx context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	*c.count++
	return c.inner.ToolUse(ctx, req)
}

// TestReflector_Run_NoCandidatesEmptyGraph verifies that a navigator with no
// entities returns Skipped="no-candidates" without appending anything.
func TestReflector_Run_NoCandidatesEmptyGraph(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	chat := &fakeChat{citeUIDs: []string{"s1"}}
	app := &captureAppender{}
	r := cognitive.NewReflector(chat, nav, app, nil, "test-model", nil)

	res, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != "no-candidates" {
		t.Errorf("Skipped = %q, want no-candidates for empty graph", res.Skipped)
	}
	if len(app.all()) != 0 {
		t.Errorf("appended %d events on empty graph, want 0", len(app.all()))
	}
}
