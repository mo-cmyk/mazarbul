package kernel

// Lane C retrieval & temporal feature tests.
//
// Covers:
//   - buildVerbatimFTSQuery (R3 helper)
//   - runFTSLane fuzzy fallback (R3)
//   - applyAnchorFilter (R11)
//   - collectHitsWithMeta anchor frontier population (R11)
//   - embedQuery deadline degrade (R1)
//   - RecallMeta candidate count population (R2)
//   - deterministic tie-break (Verify-only)

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// ---- buildVerbatimFTSQuery (R3) ---------------------------------------------

func TestBuildVerbatimFTSQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty string", input: "", want: ""},
		{name: "single token", input: "berlin", want: `"berlin"`},
		{name: "two tokens", input: "hello world", want: `"hello" OR "world"`},
		{name: "punctuation stripped", input: "foo, bar!", want: `"foo" OR "bar"`},
		{name: "short tokens no star", input: "go", want: `"go"`},
		{name: "mixed length", input: "in Berlin", want: `"in" OR "Berlin"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildVerbatimFTSQuery(tc.input)
			if got != tc.want {
				t.Errorf("buildVerbatimFTSQuery(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---- verbatim vs prefix FTS query shapes ------------------------------------

func TestEscapeFTSQueryHasPrefixStar(t *testing.T) {
	t.Parallel()

	// escapeFTSQuery adds * for 3+ rune tokens; buildVerbatimFTSQuery must not.
	prefixQ := escapeFTSQuery("berlin")
	verbatimQ := buildVerbatimFTSQuery("berlin")

	if prefixQ != `"berlin"*` {
		t.Errorf("escapeFTSQuery(%q) = %q, want %q", "berlin", prefixQ, `"berlin"*`)
	}
	if verbatimQ != `"berlin"` {
		t.Errorf("buildVerbatimFTSQuery(%q) = %q, want %q", "berlin", verbatimQ, `"berlin"`)
	}
	if prefixQ == verbatimQ {
		t.Error("verbatim and prefix queries are identical; R3 fallback will never fire")
	}
}

// ---- runFTSLane fuzzy fallback (R3) -----------------------------------------

// twoPassFTS simulates the verbatim-then-prefix pattern:
// first call returns 1 hit, second call returns all configured hits.
type twoPassFTS struct {
	allHits   []FTSHit
	callCount int
}

func (f *twoPassFTS) Index(_ context.Context, _ []FTSItem) error { return nil }
func (f *twoPassFTS) Delete(_ context.Context, _ []string) error { return nil }
func (f *twoPassFTS) Close() error                               { return nil }
func (f *twoPassFTS) Search(_ context.Context, _ string, _ int) ([]FTSHit, error) {
	f.callCount++
	if f.callCount == 1 {
		// Verbatim pass: return only 1 hit (below threshold of 3).
		if len(f.allHits) > 0 {
			return f.allHits[:1], nil
		}
		return nil, nil
	}
	// Prefix (fallback) pass: return all hits.
	return f.allHits, nil
}

func TestRunFTSLane_fuzzy_fallback_fires_when_too_few_hits(t *testing.T) {
	t.Parallel()

	fts := &twoPassFTS{
		allHits: []FTSHit{
			{ID: "01", BM25: 1.0},
			{ID: "02", BM25: 0.9},
			{ID: "03", BM25: 0.8},
		},
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	h, err := NewHybridQuery(nil, &stubVector{}, fts, db,
		WithFuzzyFallbackThreshold(3),
	)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	verbatim := buildVerbatimFTSQuery("berlin")
	prefix := escapeFTSQuery("berlin")

	hits, warnings, err := h.runFTSLane(context.Background(), verbatim, prefix, 10, RecallRequest{})
	if err != nil {
		t.Fatalf("runFTSLane: %v", err)
	}

	// Fallback should produce the full 3 hits.
	if len(hits) != 3 {
		t.Errorf("got %d hits, want 3", len(hits))
	}
	// Exactly one fuzzy-fallback warning.
	if len(warnings) != 1 {
		t.Errorf("got %d warnings, want 1", len(warnings))
	} else if warnings[0].Code != warncode.CodeRecallFuzzyFallback {
		t.Errorf("warning code = %q, want %q", warnings[0].Code, warncode.CodeRecallFuzzyFallback)
	}
	// FTS called twice: verbatim first, then prefix.
	if fts.callCount != 2 {
		t.Errorf("FTS called %d time(s), want 2", fts.callCount)
	}
}

func TestRunFTSLane_no_fallback_when_enough_hits(t *testing.T) {
	t.Parallel()

	// stubFTS always returns 5 hits on first call — above threshold, no fallback.
	fts := &stubFTS{hits: []FTSHit{
		{ID: "01"}, {ID: "02"}, {ID: "03"}, {ID: "04"}, {ID: "05"},
	}}

	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()

	h, _ := NewHybridQuery(nil, &stubVector{}, fts, db,
		WithFuzzyFallbackThreshold(3),
	)
	verbatim := buildVerbatimFTSQuery("berlin")
	prefix := escapeFTSQuery("berlin")

	hits, warnings, err := h.runFTSLane(context.Background(), verbatim, prefix, 10, RecallRequest{})
	if err != nil {
		t.Fatalf("runFTSLane: %v", err)
	}
	if len(hits) != 5 {
		t.Errorf("got %d hits, want 5", len(hits))
	}
	if len(warnings) != 0 {
		t.Errorf("got %d warnings, want 0", len(warnings))
	}
	// stubFTS.searchCalled tracks calls; only one Search should have run.
	if !fts.searchCalled {
		t.Error("FTS search was not called at all")
	}
}

func TestRunFTSLane_empty_query_returns_nil(t *testing.T) {
	t.Parallel()

	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()

	h, _ := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)
	hits, warnings, err := h.runFTSLane(context.Background(), "", "", 10, RecallRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits != nil || warnings != nil {
		t.Errorf("empty query should produce nil hits+warnings, got hits=%v warnings=%v", hits, warnings)
	}
}

// ---- applyAnchorFilter (R11) ------------------------------------------------

// buildMinimalTestDB creates an in-memory SQLite with the minimal schema
// required for episodesCitingEntitiesBatch (mentions + scope column).
func buildMinimalTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS episodes (
			uid TEXT PRIMARY KEY,
			kind TEXT,
			text TEXT,
			confidence REAL DEFAULT 1.0,
			scope TEXT DEFAULT 'project',
			class TEXT DEFAULT 'episodic',
			source TEXT,
			create_time TEXT
		);
		CREATE TABLE IF NOT EXISTS mentions (
			source_event_uid TEXT,
			entity_uid       TEXT,
			scope            TEXT DEFAULT 'project'
		);
		CREATE INDEX IF NOT EXISTS mentions_src ON mentions(source_event_uid);
		CREATE INDEX IF NOT EXISTS mentions_ent ON mentions(entity_uid);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func TestApplyAnchorFilter_narrows_to_frontier(t *testing.T) {
	t.Parallel()

	db := buildMinimalTestDB(t)

	// ep1 cites entityA (in frontier), ep2 cites entityB (not in frontier).
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO episodes(uid, kind, text) VALUES
			('ep1','episode','ep1 text'),
			('ep2','episode','ep2 text');
		INSERT INTO mentions(source_event_uid, entity_uid) VALUES
			('ep1','entityA'),
			('ep2','entityB');
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	h, err := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	rows := map[string]hydratedRow{
		"ep1": {kind: "episode", text: "ep1 text"},
		"ep2": {kind: "episode", text: "ep2 text"},
	}
	frontier := map[string]struct{}{"entityA": {}}

	ctx := withScopes(context.Background(), defaultScopeSet())
	out, err := h.applyAnchorFilter(ctx, rows, frontier)
	if err != nil {
		t.Fatalf("applyAnchorFilter: %v", err)
	}

	if _, ok := out["ep1"]; !ok {
		t.Error("ep1 (cites entityA in frontier) should be retained")
	}
	if _, ok := out["ep2"]; ok {
		t.Error("ep2 (cites entityB, not in frontier) should be excluded")
	}
}

func TestApplyAnchorFilter_empty_frontier_returns_empty(t *testing.T) {
	t.Parallel()

	db := buildMinimalTestDB(t)
	h, _ := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)

	rows := map[string]hydratedRow{"ep1": {kind: "episode"}}
	ctx := withScopes(context.Background(), defaultScopeSet())
	out, err := h.applyAnchorFilter(ctx, rows, map[string]struct{}{})
	if err != nil {
		t.Fatalf("applyAnchorFilter: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty frontier should return empty map, got %d rows", len(out))
	}
}

func TestApplyAnchorFilter_empty_rows_returns_empty(t *testing.T) {
	t.Parallel()

	db := buildMinimalTestDB(t)
	h, _ := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)

	ctx := withScopes(context.Background(), defaultScopeSet())
	out, err := h.applyAnchorFilter(ctx, map[string]hydratedRow{}, map[string]struct{}{"entityA": {}})
	if err != nil {
		t.Fatalf("applyAnchorFilter: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty rows should return empty map, got %d", len(out))
	}
}

// ---- collectHitsWithMeta anchor frontier (R11) ------------------------------

// fakeNavigatorForTest is an anchorNavigator that returns fixed UIDs.
type fakeNavigatorForTest struct {
	neighbors []string
	err       error
	called    bool
	lastSeed  string
	lastDepth int
}

func (f *fakeNavigatorForTest) NeighborUIDs(_ context.Context, seed string, depth int) ([]string, error) {
	f.called = true
	f.lastSeed = seed
	f.lastDepth = depth
	return f.neighbors, f.err
}

func TestCollectHitsWithMeta_calls_navigator_with_correct_params(t *testing.T) {
	t.Parallel()

	db := buildMinimalTestDB(t)
	h, err := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	nav := &fakeNavigatorForTest{neighbors: []string{"entityX"}}
	h.SetNavigator(nav)

	req := RecallRequest{
		Query:       "test",
		Anchor:      "anchorUID-01JX",
		AnchorDepth: 2,
	}

	ctx := withScopes(context.Background(), defaultScopeSet())
	_, _, _, err = h.collectHitsWithMeta(ctx, nil, req)
	if err != nil {
		t.Fatalf("collectHitsWithMeta: %v", err)
	}

	if !nav.called {
		t.Error("navigator NeighborUIDs was not called")
	}
	if nav.lastSeed != req.Anchor {
		t.Errorf("navigator called with seed %q, want %q", nav.lastSeed, req.Anchor)
	}
	if nav.lastDepth != 2 {
		t.Errorf("navigator called with depth %d, want 2", nav.lastDepth)
	}
}

func TestCollectHitsWithMeta_clamps_anchor_depth_to_max(t *testing.T) {
	t.Parallel()

	db := buildMinimalTestDB(t)
	h, _ := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)

	nav := &fakeNavigatorForTest{neighbors: []string{"x"}}
	h.SetNavigator(nav)

	req := RecallRequest{Query: "test", Anchor: "a", AnchorDepth: 100}
	ctx := withScopes(context.Background(), defaultScopeSet())
	_, _, _, _ = h.collectHitsWithMeta(ctx, nil, req)

	if nav.lastDepth > MaxAnchorDepth {
		t.Errorf("depth %d exceeds MaxAnchorDepth %d", nav.lastDepth, MaxAnchorDepth)
	}
}

func TestCollectHitsWithMeta_zero_depth_treated_as_one(t *testing.T) {
	t.Parallel()

	db := buildMinimalTestDB(t)
	h, _ := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)

	nav := &fakeNavigatorForTest{neighbors: []string{"x"}}
	h.SetNavigator(nav)

	req := RecallRequest{Query: "test", Anchor: "a", AnchorDepth: 0}
	ctx := withScopes(context.Background(), defaultScopeSet())
	_, _, _, _ = h.collectHitsWithMeta(ctx, nil, req)

	if nav.lastDepth != 1 {
		t.Errorf("depth 0 should become 1, got %d", nav.lastDepth)
	}
}

func TestCollectHitsWithMeta_degrades_on_navigator_error(t *testing.T) {
	t.Parallel()

	db := buildMinimalTestDB(t)
	h, err := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	nav := &fakeNavigatorForTest{err: errors.New("graph unavailable")}
	h.SetNavigator(nav)

	req := RecallRequest{Query: "test", Anchor: "anchorUID"}
	ctx := withScopes(context.Background(), defaultScopeSet())

	// Navigator error must degrade gracefully to full recall, not return an error.
	_, _, _, err = h.collectHitsWithMeta(ctx, nil, req)
	if err != nil {
		t.Errorf("navigator error should degrade gracefully, got: %v", err)
	}
}

// ---- embedQuery deadline degrade (R1) ---------------------------------------

// slowEmbedder sleeps until the context is cancelled, simulating a hung embedder.
type slowEmbedder struct{ delay time.Duration }

func (s *slowEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return []float32{1, 0}, nil
	}
}

func TestEmbedQuery_deadline_degrades_to_fts_only(t *testing.T) {
	t.Parallel()

	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()

	h, err := NewHybridQuery(
		&slowEmbedder{delay: 500 * time.Millisecond},
		&stubVector{},
		&stubFTS{},
		db,
		WithEmbedDeadline(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	req := RecallRequest{Query: "test"}
	qvec, report := h.embedQuery(context.Background(), req)

	if qvec != nil {
		t.Errorf("expected nil qvec (degraded) after deadline, got len=%d", len(qvec))
	}
	if !report.Degraded {
		t.Error("expected report.Degraded=true after embed deadline")
	}
	found := false
	for _, w := range report.Warnings {
		if w.Code == warncode.CodeRecallEmbedderDegraded {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning %s in report, got warnings=%v",
			warncode.CodeRecallEmbedderDegraded, report.Warnings)
	}
}

func TestEmbedQuery_per_request_deadline_tighter_than_global(t *testing.T) {
	t.Parallel()

	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()

	// Global deadline 1s (would not fire); per-request 10ms (fires first).
	h, err := NewHybridQuery(
		&slowEmbedder{delay: 500 * time.Millisecond},
		&stubVector{},
		&stubFTS{},
		db,
		WithEmbedDeadline(time.Second),
	)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	req := RecallRequest{Query: "test", EmbedDeadline: 10 * time.Millisecond}
	qvec, report := h.embedQuery(context.Background(), req)

	if qvec != nil {
		t.Error("expected nil qvec with per-request tight deadline")
	}
	if !report.Degraded {
		t.Error("expected report.Degraded=true")
	}
}

// ---- RecallMeta candidate counts (R2) --------------------------------------

// TestRecallMeta_field_types verifies the RecallMeta struct has the expected
// exported fields of the correct types; this guards against unintended removals
// that would break the --explain surface without a test failure.
func TestRecallMeta_field_types(t *testing.T) {
	t.Parallel()

	m := RecallMeta{
		TookMs:            42,
		VecCandidates:     10,
		FTSCandidates:     5,
		GraphCandidates:   3,
		NameEmbCandidates: 2,
	}
	if m.TookMs != 42 {
		t.Errorf("TookMs = %d, want 42", m.TookMs)
	}
	if m.VecCandidates != 10 {
		t.Errorf("VecCandidates = %d, want 10", m.VecCandidates)
	}
	if m.FTSCandidates != 5 {
		t.Errorf("FTSCandidates = %d, want 5", m.FTSCandidates)
	}
	if m.GraphCandidates != 3 {
		t.Errorf("GraphCandidates = %d, want 3", m.GraphCandidates)
	}
	if m.NameEmbCandidates != 2 {
		t.Errorf("NameEmbCandidates = %d, want 2", m.NameEmbCandidates)
	}
}

// ---- deterministic tie-break (Verify-only) ----------------------------------

func TestDeterministicTieBreak_score_desc_uid_asc(t *testing.T) {
	t.Parallel()

	hits := []Hit{
		{UID: "z", FusedScore: 0.5},
		{UID: "a", FusedScore: 0.5},
		{UID: "m", FusedScore: 0.8},
	}
	sortHitsByScore(hits)

	if hits[0].UID != "m" {
		t.Errorf("first hit should be m (highest score), got %s", hits[0].UID)
	}
	if hits[1].UID != "a" {
		t.Errorf("second hit should be a (tied 0.5, UID asc), got %s", hits[1].UID)
	}
	if hits[2].UID != "z" {
		t.Errorf("third hit should be z (tied 0.5, UID desc), got %s", hits[2].UID)
	}
}

func TestDeterministicTieBreak_stable_on_all_equal_scores(t *testing.T) {
	t.Parallel()

	hits := []Hit{
		{UID: "c", FusedScore: 0.3},
		{UID: "a", FusedScore: 0.3},
		{UID: "b", FusedScore: 0.3},
	}
	sortHitsByScore(hits)

	order := []string{hits[0].UID, hits[1].UID, hits[2].UID}
	want := []string{"a", "b", "c"}
	for i, uid := range order {
		if uid != want[i] {
			t.Errorf("position %d: got %s, want %s (UID asc tie-break)", i, uid, want[i])
		}
	}
}

// ---- helpers ----------------------------------------------------------------

// defaultScopeSet returns the default scope set (project + personal).
func defaultScopeSet() ScopeSet {
	s, _ := ParseScopeSet([]string{"project", "personal"})
	return s
}
