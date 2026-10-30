package kernel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	// Pulled in for the in-memory test database. The driver is registered as
	// "sqlite3" via its init(). The kernel package's production code does
	// not depend on it; only tests do.
	_ "github.com/mattn/go-sqlite3"

	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
)

// ---- fuseRRF ----------------------------------------------------------------

func TestFuseRRF(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		vecRank  int
		ftsRank  int
		rrfK     int
		wantFuse float32
		wantVec  float32
		wantFTS  float32
	}{
		{
			name:    "both sources rank 1, k=60",
			vecRank: 1, ftsRank: 1, rrfK: 60,
			wantVec: 1.0 / 61, wantFTS: 1.0 / 61, wantFuse: 2.0 / 61,
		},
		{
			name:    "vec only, rank 1, k=60",
			vecRank: 1, ftsRank: 0, rrfK: 60,
			wantVec: 1.0 / 61, wantFTS: 0, wantFuse: 1.0 / 61,
		},
		{
			name:    "fts only, rank 5, k=60",
			vecRank: 0, ftsRank: 5, rrfK: 60,
			wantVec: 0, wantFTS: 1.0 / 65, wantFuse: 1.0 / 65,
		},
		{
			name:    "both sources, asymmetric ranks",
			vecRank: 2, ftsRank: 10, rrfK: 60,
			wantVec: 1.0 / 62, wantFTS: 1.0 / 70, wantFuse: 1.0/62 + 1.0/70,
		},
		{
			name:    "absent from both sources",
			vecRank: 0, ftsRank: 0, rrfK: 60,
			wantVec: 0, wantFTS: 0, wantFuse: 0,
		},
		{
			name:    "small k amplifies top ranks",
			vecRank: 1, ftsRank: 1, rrfK: 10,
			wantVec: 1.0 / 11, wantFTS: 1.0 / 11, wantFuse: 2.0 / 11,
		},
		{
			name:    "large rank, k=60",
			vecRank: 100, ftsRank: 100, rrfK: 60,
			wantVec: 1.0 / 160, wantFTS: 1.0 / 160, wantFuse: 2.0 / 160,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotFuse, gotVec, gotFTS := fuseRRF(tc.vecRank, tc.ftsRank, tc.rrfK)
			if !approxEqual(gotFuse, tc.wantFuse) {
				t.Errorf("fused: got %v, want %v", gotFuse, tc.wantFuse)
			}
			if !approxEqual(gotVec, tc.wantVec) {
				t.Errorf("vec component: got %v, want %v", gotVec, tc.wantVec)
			}
			if !approxEqual(gotFTS, tc.wantFTS) {
				t.Errorf("fts component: got %v, want %v", gotFTS, tc.wantFTS)
			}
		})
	}
}

// TestFuseRRF_RankOrderingPreserved confirms that an item ranked higher in at
// least one source always scores at least as high as a worse-ranked sibling
// when the other source is symmetric. Property-style check guards against an
// arithmetic regression in the formula.
func TestFuseRRF_RankOrderingPreserved(t *testing.T) {
	t.Parallel()

	const k = 60
	for r := 1; r < 50; r++ {
		better, _, _ := fuseRRF(r, r, k)
		worse, _, _ := fuseRRF(r+1, r+1, k)
		if better <= worse {
			t.Errorf("rank %d should beat rank %d: %v vs %v", r, r+1, better, worse)
		}
	}
}

// ---- filterHydrated ---------------------------------------------------------

func TestFilterHydrated(t *testing.T) {
	t.Parallel()

	rows := map[string]hydratedRow{
		"a": {kind: "remember", confidence: 1.0},
		"b": {kind: "remember", confidence: 0.5},
		"c": {kind: "supersede", confidence: 0.9},
		"d": {kind: "forget", confidence: 0.1},
	}

	t.Run("no filters keeps all", func(t *testing.T) {
		t.Parallel()
		out := filterHydrated(rows, RecallRequest{})
		if len(out) != len(rows) {
			t.Errorf("got %d, want %d", len(out), len(rows))
		}
	})

	t.Run("kinds filter applies as a set", func(t *testing.T) {
		t.Parallel()
		out := filterHydrated(rows, RecallRequest{Kinds: []EventKind{"remember", "forget"}})
		if _, ok := out["c"]; ok {
			t.Error("supersede survived kinds filter")
		}
		if len(out) != 3 {
			t.Errorf("got %d, want 3", len(out))
		}
	})

	t.Run("min confidence drops low rows", func(t *testing.T) {
		t.Parallel()
		out := filterHydrated(rows, RecallRequest{MinConfidence: 0.75})
		if _, ok := out["b"]; ok {
			t.Error("0.5 confidence survived MinConfidence=0.75")
		}
		if _, ok := out["d"]; ok {
			t.Error("0.1 confidence survived MinConfidence=0.75")
		}
		if len(out) != 2 {
			t.Errorf("got %d survivors, want 2 (a + c)", len(out))
		}
	})

	t.Run("filters compose AND, not OR", func(t *testing.T) {
		t.Parallel()
		out := filterHydrated(rows, RecallRequest{
			Kinds:         []EventKind{"remember"},
			MinConfidence: 0.75,
		})
		// Only "a" (remember, 1.0) survives both.
		if len(out) != 1 {
			t.Errorf("got %d, want 1", len(out))
		}
		if _, ok := out["a"]; !ok {
			t.Error("a missing from compound filter survivors")
		}
	})

	t.Run("since drops rows before the lower bound", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		timed := map[string]hydratedRow{
			"old":    {kind: "remember", confidence: 1.0, createTime: base.Add(-time.Hour)},
			"onEdge": {kind: "remember", confidence: 1.0, createTime: base},
			"future": {kind: "remember", confidence: 1.0, createTime: base.Add(time.Hour)},
		}
		out := filterHydrated(timed, RecallRequest{Since: base})
		if _, ok := out["old"]; ok {
			t.Error("row before Since survived")
		}
		if _, ok := out["onEdge"]; !ok {
			t.Error("row exactly at Since dropped (bound is inclusive)")
		}
		if _, ok := out["future"]; !ok {
			t.Error("row after Since dropped")
		}
	})

	t.Run("until drops rows at or after the upper bound", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		timed := map[string]hydratedRow{
			"old":    {kind: "remember", confidence: 1.0, createTime: base.Add(-time.Hour)},
			"onEdge": {kind: "remember", confidence: 1.0, createTime: base},
			"future": {kind: "remember", confidence: 1.0, createTime: base.Add(time.Hour)},
		}
		out := filterHydrated(timed, RecallRequest{Until: base})
		if _, ok := out["old"]; !ok {
			t.Error("row before Until dropped")
		}
		if _, ok := out["onEdge"]; ok {
			t.Error("row exactly at Until survived (bound is exclusive)")
		}
		if _, ok := out["future"]; ok {
			t.Error("row after Until survived")
		}
	})

	t.Run("zero since and until are no-ops", func(t *testing.T) {
		t.Parallel()
		out := filterHydrated(rows, RecallRequest{}) // zero time bounds
		if len(out) != len(rows) {
			t.Errorf("zero bounds filtered something out: got %d, want %d", len(out), len(rows))
		}
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		t.Parallel()
		out := filterHydrated(map[string]hydratedRow{}, RecallRequest{MinConfidence: 0.5})
		if len(out) != 0 {
			t.Errorf("got %d, want 0", len(out))
		}
	})
}

// ---- rerankSurvivorsVec / FTS ----------------------------------------------

func TestRerankSurvivors_FillsGapsAfterFilter(t *testing.T) {
	t.Parallel()

	// Original vec ranks: a=1, b=2, c=3, d=4. Filter drops b and d.
	// Survivors should be re-ranked a=1, c=2 (gap-free).
	vecHits := []VectorHit{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	keep := map[string]hydratedRow{
		"a": {}, "c": {},
	}
	got := rerankSurvivorsVec(vecHits, keep)
	want := map[string]int{"a": 1, "c": 2}
	if !mapsEqual(got, want) {
		t.Errorf("vec rerank: got %v, want %v", got, want)
	}

	ftsHits := []FTSHit{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	got = rerankSurvivorsFTS(ftsHits, keep)
	if !mapsEqual(got, want) {
		t.Errorf("fts rerank: got %v, want %v", got, want)
	}
}

func TestRerankSurvivors_DeduplicatesRepeatedUIDs(t *testing.T) {
	t.Parallel()

	// A source may legitimately return the same UID twice (rare but
	// possible if an upstream join misbehaves). Re-rank must not assign
	// two ranks to the same UID.
	vecHits := []VectorHit{
		{ID: "a"}, {ID: "a"}, {ID: "b"},
	}
	keep := map[string]hydratedRow{"a": {}, "b": {}}
	got := rerankSurvivorsVec(vecHits, keep)
	want := map[string]int{"a": 1, "b": 2}
	if !mapsEqual(got, want) {
		t.Errorf("dedup: got %v, want %v", got, want)
	}
}

// ---- unionUIDs / pickSnippet / truncateRunes -------------------------------

func TestUnionUIDs_DedupAcrossSources(t *testing.T) {
	t.Parallel()

	vec := []VectorHit{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	fts := []FTSHit{{ID: "b"}, {ID: "c"}, {ID: "d"}}
	got := unionUIDs(vec, fts)
	want := []string{"a", "b", "c", "d"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPickSnippet(t *testing.T) {
	t.Parallel()

	t.Run("prefers FTS snippet when present", func(t *testing.T) {
		t.Parallel()
		fts := []FTSHit{{ID: "a", Snippet: "<b>match</b> in text"}}
		got := pickSnippet("a", hydratedRow{text: "the full episode text"}, fts, 200)
		if !strings.Contains(got, "<b>match</b>") {
			t.Errorf("expected FTS snippet, got %q", got)
		}
	})

	t.Run("falls back to text prefix for vec-only hits", func(t *testing.T) {
		t.Parallel()
		got := pickSnippet("a", hydratedRow{text: "semantic content"}, nil, 200)
		if got != "semantic content" {
			t.Errorf("got %q, want %q", got, "semantic content")
		}
	})

	t.Run("truncates long text to rune cap", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("x", 500)
		got := pickSnippet("a", hydratedRow{text: long}, nil, 200)
		if len([]rune(got)) != 200 {
			t.Errorf("got %d runes, want 200", len([]rune(got)))
		}
	})

	t.Run("empty snippet from FTS triggers fallback", func(t *testing.T) {
		t.Parallel()
		fts := []FTSHit{{ID: "a", Snippet: ""}}
		got := pickSnippet("a", hydratedRow{text: "fallback"}, fts, 200)
		if got != "fallback" {
			t.Errorf("got %q, want fallback", got)
		}
	})
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"héllo wörld 你好", 8, "héllo wö"}, // rune-aware
		{"", 5, ""},
	}
	for _, tc := range cases {
		got := truncateRunes(tc.in, tc.n)
		if got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// ---- escapeFTSQuery ---------------------------------------------------------

func TestEscapeFTSQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		// Plain words pass through, each wrapped as a phrase literal and
		// joined with FTS5's explicit OR operator. Space-joined tokens
		// in FTS5 are implicit AND, which never matches natural-language
		// queries against episodes that lack question words; OR lets
		// BM25 rank by overlap and the fusion pass fuse against the vec
		// lane. Tokens of 3+ runes get a trailing `*` (prefix token) so
		// morphological variants recall ("peanut" finds "peanuts"); 1-2
		// rune tokens stay exact.
		{"cats are great", `"cats"* OR "are"* OR "great"*`},
		// FTS5 keywords like AND/OR/NOT/NEAR become phrase literals and
		// stop acting as operators (the prefix star does not change that).
		{"cat AND dog", `"cat"* OR "AND"* OR "dog"*`},
		// Unmatched quotes and parens used to make FTSStore.Search
		// return a syntax error; sanitisation strips them. Short tokens
		// (he, hi) stay exact; "said" (4 runes) gets the prefix star.
		{`he said "hi`, `"he" OR "said"* OR "hi"`},
		{"(parens) [brackets]", `"parens"* OR "brackets"*`},
		// Intra-token punctuation must SPLIT the token, not collapse
		// it. FTS5's unicode61 tokenizer treats `'`, `-`, `:`, `*`
		// etc. as token boundaries, so an indexed `you're` lives as
		// two tokens (`you`, `re`). If sanitisation collapsed to
		// `youre`, the query would never match. "you" (3) prefixes; "re" (2) exact.
		{"you're", `"you"* OR "re"`},
		{"foo-bar", `"foo"* OR "bar"*`},
		// Wildcards and column qualifiers are also boundary chars
		// under unicode61; split, then quote each token.
		{"foo:bar*", `"foo"* OR "bar"*`},
		// Single tokens: no join glue.
		{"^anchored", `"anchored"*`},
		// Multilingual / underscore identifiers are preserved. A 2-rune
		// CJK token (你好) stays exact; ASCII content words get the star.
		{"hello 你好 foo_bar", `"hello"* OR "你好" OR "foo_bar"*`},
		// Empty / all-noise inputs return the empty string so the
		// caller can skip the FTS sub-query cleanly.
		{"", ""},
		{"   ", ""},
		{"!!!", ""},
		{`+ - * "`, ""},
	}

	for _, tc := range cases {
		got := escapeFTSQuery(tc.in)
		if got != tc.want {
			t.Errorf("escapeFTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHybridQuery_Recall_AllNoiseQueryStillRunsVec confirms that when a query
func TestApplyAsOf_TightensUntilWhenUnset(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	got := applyAsOf(RecallRequest{AsOf: &asOf})
	if !got.Until.Equal(asOf) {
		t.Errorf("Until = %v, want %v (AsOf should populate empty Until)", got.Until, asOf)
	}
}

func TestApplyAsOf_PreservesTighterUntil(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	tighter := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	got := applyAsOf(RecallRequest{AsOf: &asOf, Until: tighter})
	if !got.Until.Equal(tighter) {
		t.Errorf("Until = %v, want %v (caller's tighter Until must win)", got.Until, tighter)
	}
}

func TestApplyAsOf_NilIsNoop(t *testing.T) {
	t.Parallel()
	got := applyAsOf(RecallRequest{Query: "anything"})
	if !got.Until.IsZero() {
		t.Errorf("Until = %v, want zero when AsOf is nil", got.Until)
	}
}

// is pure FTS syntax noise (sanitisation strips it to nothing), the FTS lane
// is skipped without failing, and vector results still flow through to the
// caller. Before the escape fix this produced an FTS syntax error that
// aborted the whole errgroup and silently dropped valid vector hits.
func TestHybridQuery_Recall_AllNoiseQueryStillRunsVec(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "a", kind: "remember", confidence: 1.0, text: "an episode"},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "a"}}}
	// stubFTS returns an error if called , the escape fix should skip the
	// FTS sub-query entirely when the sanitised query is empty.
	fts := &stubFTS{err: errors.New("fts should not be called")}
	h := newTestHybrid(t, episodes, vec, fts)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: `+ - " *`}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 1 || hits[0].UID != "a" {
		t.Errorf("got %+v, want single vec-only hit a", hits)
	}
}

// ---- time-bound push-down -------------------------------------------------

// TestHybridQuery_Recall_PushesDownTimeBounds guards the starvation fix:
// when the user passes Since/Until, the predicate must be pushed INTO the
// retrievers at the storage layer (VectorFilter.MinTime/MaxTime on the vec
// lane, SearchInTimeRange via ftsTimeRanger on the FTS lane) so the
// returned top-k is already in-window. Oversampling alone is not enough,
// the next in-window hit could be beyond any fixed ceiling.
func TestHybridQuery_Recall_PushesDownTimeBounds(t *testing.T) {
	t.Parallel()

	t.Run("no bounds: vec filter zeroed, FTS uses plain Search", func(t *testing.T) {
		t.Parallel()
		vec := &stubVector{hits: []VectorHit{{ID: "a"}}}
		fts := &stubFTS{}
		episodes := []seedEpisode{{uid: "a", kind: "remember", confidence: 1.0, text: "a"}}
		h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
			WithCandidateK(7))
		if _, err := drain(h.Recall(t.Context(), RecallRequest{Query: "any"})); err != nil {
			t.Fatalf("recall: %v", err)
		}
		if vec.lastK != 7 {
			t.Errorf("vec.lastK = %d, want 7 (configured candidateK)", vec.lastK)
		}
		if !vec.lastFilter.MinTime.IsZero() || !vec.lastFilter.MaxTime.IsZero() {
			t.Errorf("vec.lastFilter = %+v, want zero filter without time bounds", vec.lastFilter)
		}
		if !fts.searchCalled {
			t.Error("fts.Search should be called without time bounds")
		}
		if fts.rangeCalled {
			t.Error("fts.SearchInTimeRange should NOT be called without time bounds")
		}
	})

	t.Run("Since set: vec filter populated, FTS routes to SearchInTimeRange", func(t *testing.T) {
		t.Parallel()
		vec := &stubVector{hits: []VectorHit{{ID: "a"}}}
		fts := &stubFTS{}
		episodes := []seedEpisode{{uid: "a", kind: "remember", confidence: 1.0, text: "a"}}
		h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
			WithCandidateK(7))
		since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		req := RecallRequest{Query: "any", Since: since}
		if _, err := drain(h.Recall(t.Context(), req)); err != nil {
			t.Fatalf("recall: %v", err)
		}
		if vec.lastK != 7 {
			t.Errorf("vec.lastK = %d, want 7 (configured candidateK, no oversample)", vec.lastK)
		}
		if !vec.lastFilter.MinTime.Equal(since) {
			t.Errorf("vec.lastFilter.MinTime = %v, want %v", vec.lastFilter.MinTime, since)
		}
		if !vec.lastFilter.MaxTime.IsZero() {
			t.Errorf("vec.lastFilter.MaxTime = %v, want zero (Until unset)", vec.lastFilter.MaxTime)
		}
		if !fts.rangeCalled {
			t.Error("fts.SearchInTimeRange should be called when Since is set")
		}
		if fts.searchCalled {
			t.Error("fts.Search should NOT be called when push-down is available")
		}
		if !fts.lastMinTime.Equal(since) {
			t.Errorf("fts.lastMinTime = %v, want %v", fts.lastMinTime, since)
		}
	})

	t.Run("Until set: vec filter populated, FTS routes to SearchInTimeRange", func(t *testing.T) {
		t.Parallel()
		vec := &stubVector{hits: []VectorHit{{ID: "a"}}}
		fts := &stubFTS{}
		episodes := []seedEpisode{{uid: "a", kind: "remember", confidence: 1.0, text: "a"}}
		h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
			WithCandidateK(7))
		until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		req := RecallRequest{Query: "any", Until: until}
		if _, err := drain(h.Recall(t.Context(), req)); err != nil {
			t.Fatalf("recall: %v", err)
		}
		if !vec.lastFilter.MaxTime.Equal(until) {
			t.Errorf("vec.lastFilter.MaxTime = %v, want %v", vec.lastFilter.MaxTime, until)
		}
		if !vec.lastFilter.MinTime.IsZero() {
			t.Errorf("vec.lastFilter.MinTime = %v, want zero (Since unset)", vec.lastFilter.MinTime)
		}
		if !fts.rangeCalled {
			t.Error("fts.SearchInTimeRange should be called when Until is set")
		}
		if !fts.lastMaxTime.Equal(until) {
			t.Errorf("fts.lastMaxTime = %v, want %v", fts.lastMaxTime, until)
		}
	})

	t.Run("both Since and Until set: both bounds reach the storage layer", func(t *testing.T) {
		t.Parallel()
		vec := &stubVector{hits: []VectorHit{{ID: "a"}}}
		fts := &stubFTS{}
		episodes := []seedEpisode{{uid: "a", kind: "remember", confidence: 1.0, text: "a"}}
		h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes)
		since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		req := RecallRequest{Query: "any", Since: since, Until: until}
		if _, err := drain(h.Recall(t.Context(), req)); err != nil {
			t.Fatalf("recall: %v", err)
		}
		if !vec.lastFilter.MinTime.Equal(since) || !vec.lastFilter.MaxTime.Equal(until) {
			t.Errorf("vec.lastFilter = %+v, want MinTime=%v MaxTime=%v",
				vec.lastFilter, since, until)
		}
		if !fts.lastMinTime.Equal(since) || !fts.lastMaxTime.Equal(until) {
			t.Errorf("fts range bounds = [%v, %v), want [%v, %v)",
				fts.lastMinTime, fts.lastMaxTime, since, until)
		}
	})
}

// ---- parseCreateTime --------------------------------------------------------

func TestParseCreateTime(t *testing.T) {
	t.Parallel()

	t.Run("rfc3339 nano", func(t *testing.T) {
		t.Parallel()
		s := "2026-05-23T12:34:56.789Z"
		got, err := parseCreateTime(s)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Year() != 2026 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("rfc3339 seconds", func(t *testing.T) {
		t.Parallel()
		s := "2026-05-23T12:34:56Z"
		got, err := parseCreateTime(s)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Second() != 56 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("empty string is an error", func(t *testing.T) {
		t.Parallel()
		if _, err := parseCreateTime(""); err == nil {
			t.Error("empty string returned nil error")
		}
	})

	t.Run("garbage is an error", func(t *testing.T) {
		t.Parallel()
		if _, err := parseCreateTime("not a date"); err == nil {
			t.Error("garbage returned nil error")
		}
	})
}

// ---- NewHybridQuery / option validation ------------------------------------

func TestNewHybridQuery_RejectsNilDeps(t *testing.T) {
	t.Parallel()

	emb := stubEmbedder{}
	vec := &stubVector{}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)

	cases := []struct {
		name string
		make func() (Embedder, VectorConn, FTSConn, *sql.DB)
	}{
		// A nil embedder is intentionally allowed (degraded FTS-only mode,
		// #236); it is exercised separately by the degraded-recall tests.
		{"nil vec", func() (Embedder, VectorConn, FTSConn, *sql.DB) { return emb, nil, fts, db }},
		{"nil fts", func() (Embedder, VectorConn, FTSConn, *sql.DB) { return emb, vec, nil, db }},
		{"nil reader", func() (Embedder, VectorConn, FTSConn, *sql.DB) { return emb, vec, fts, nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, v, f, d := tc.make()
			if _, err := NewHybridQuery(e, v, f, d); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestNewHybridQuery_OptionsApply(t *testing.T) {
	t.Parallel()

	db := openMemoryEpisodes(t)
	h, err := NewHybridQuery(
		stubEmbedder{}, &stubVector{}, &stubFTS{}, db,
		WithCandidateK(42), WithRRFK(7), WithDefaultLimit(3),
		// Zero / negative values should be ignored.
		WithCandidateK(0), WithRRFK(-1), WithDefaultLimit(0),
	)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if h.candidateK != 42 {
		t.Errorf("candidateK = %d, want 42", h.candidateK)
	}
	if h.rrfK != 7 {
		t.Errorf("rrfK = %d, want 7", h.rrfK)
	}
	if h.defaultLimit != 3 {
		t.Errorf("defaultLimit = %d, want 3", h.defaultLimit)
	}
}

// ---- Recall behavior (real in-memory SQLite + stubbed retrievers) ----------

func TestHybridQuery_Recall_EmptyQueryYieldsNothing(t *testing.T) {
	t.Parallel()

	h := newTestHybrid(t, nil, nil, nil)
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "   "}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
}

// TestHybridQuery_Recall_EmbedErrorDoesNotPropagate pins the #236 contract
// inversion: a transient embedder failure no longer surfaces as a hard error.
// Recall swallows it and degrades to FTS-only (here there are no stubbed FTS
// hits, so it simply returns an empty, error-free result).
func TestHybridQuery_Recall_EmbedErrorDoesNotPropagate(t *testing.T) {
	t.Parallel()

	h := newTestHybridWithEmbedder(t, stubEmbedder{err: errors.New("provider down")}, nil, nil, nil)
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "anything"}))
	if err != nil {
		t.Fatalf("embed error should degrade, not propagate; got err = %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
}

func TestHybridQuery_Recall_FusesRanks(t *testing.T) {
	t.Parallel()

	// RRF's value is that items appearing in *both* sources beat items
	// that appear in only one. Vec ranks x=1, y=2 (only); FTS ranks y=1,
	// z=2 (only). Y is the only item with both reciprocal terms, so it
	// should win regardless of its position in either source.
	//   x = 1/(60+1)              ≈ 0.01639
	//   y = 1/(60+2) + 1/(60+1)   ≈ 0.03226
	//   z = 1/(60+2)              ≈ 0.01613
	episodes := []seedEpisode{
		{uid: "x", kind: "remember", confidence: 1.0, text: "x text"},
		{uid: "y", kind: "remember", confidence: 1.0, text: "y text"},
		{uid: "z", kind: "remember", confidence: 1.0, text: "z text"},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "x"}, {ID: "y"}}}
	fts := &stubFTS{hits: []FTSHit{
		{ID: "y", Snippet: "y <b>match</b>"},
		{ID: "z", Snippet: "z <b>match</b>"},
	}}
	// Weighted fusion is the kernel default; this test pins RRF rank-fusion
	// math (both-sources-wins), so opt back into RRF explicitly.
	h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes, WithFusionMode(FusionModeRRF))

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "match"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	if hits[0].UID != "y" {
		t.Errorf("top hit = %q, want %q (item present in both sources wins under RRF)", hits[0].UID, "y")
	}

	// FusedScore must be monotonically non-increasing.
	for i := 1; i < len(hits); i++ {
		if hits[i].FusedScore > hits[i-1].FusedScore {
			t.Errorf("hit %d score %v > hit %d score %v (not sorted)",
				i, hits[i].FusedScore, i-1, hits[i-1].FusedScore)
		}
	}

	// Top hit (y) carries both component scores; single-source hits carry
	// only one. Every episode hit has exactly one citation pointing at its
	// get-able episodes/{uid} name (an episode's uid is its source-event uid).
	byUID := map[string]Hit{}
	for _, hit := range hits {
		byUID[hit.UID] = hit
		if len(hit.Citations) != 1 || hit.Citations[0] != "episodes/"+hit.UID {
			t.Errorf("hit %q citations = %v", hit.UID, hit.Citations)
		}
	}
	if byUID["y"].Components.Vector == 0 || byUID["y"].Components.FTS == 0 {
		t.Errorf("y components = %+v, want both non-zero", byUID["y"].Components)
	}
	if byUID["x"].Components.Vector == 0 || byUID["x"].Components.FTS != 0 {
		t.Errorf("x components = %+v, want vec only", byUID["x"].Components)
	}
	if byUID["z"].Components.FTS == 0 || byUID["z"].Components.Vector != 0 {
		t.Errorf("z components = %+v, want fts only", byUID["z"].Components)
	}
}

// TestHybridQuery_Recall_DegradesToFTSWhenEmbedderUnavailable pins the #236
// launch-gate: with the embedder nil (FTS-only construction) or transiently
// erroring (Embed fails), recall must NOT hard-fail — it returns FTS-ranked
// hits, skips the dense-vector lane, and reports Degraded=true with the active
// lanes so an agent never mistakes BM25-only for full hybrid.
func TestHybridQuery_Recall_DegradesToFTSWhenEmbedderUnavailable(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "a", kind: "remember", confidence: 1.0, text: "alpha match"},
		{uid: "b", kind: "remember", confidence: 1.0, text: "beta noise"},
	}

	cases := []struct {
		name string
		emb  Embedder
	}{
		{"nil embedder", nil},
		{"embed error", stubEmbedder{err: errors.New("ollama unreachable")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vec := &stubVector{hits: []VectorHit{{ID: "a"}, {ID: "b"}}}
			fts := &stubFTS{hits: []FTSHit{{ID: "a", Snippet: "a <b>match</b>"}}}
			h := newTestHybridWithEmbedder(t, tc.emb, vec, fts, episodes)

			hits, report, err := h.RecallWithReport(t.Context(), RecallRequest{Query: "match"})
			if err != nil {
				t.Fatalf("RecallWithReport: %v", err)
			}
			// FTS still produces hits — recall does not hard-fail.
			if len(hits) != 1 || hits[0].UID != "a" {
				t.Fatalf("hits = %+v, want single FTS hit a", hits)
			}
			// The dense-vector lane must be skipped: there is no embedding to
			// search with, so stubVector.Search is never called (lastK stays 0).
			if vec.lastK != 0 {
				t.Errorf("vector lane ran (lastK=%d), want skipped under degradation", vec.lastK)
			}
			if !report.Degraded {
				t.Error("report.Degraded = false, want true")
			}
			// No GraphConn is wired, so FTS is the sole active lane.
			if !slices.Equal(report.Lanes, []string{LaneFTS}) {
				t.Errorf("report.Lanes = %v, want [%q]", report.Lanes, LaneFTS)
			}
			if hits[0].Components.Vector != 0 {
				t.Errorf("hit vector component = %v, want 0 under degradation", hits[0].Components.Vector)
			}
		})
	}
}

// TestHybridQuery_Recall_FullHybridReportsNotDegraded is the non-degraded
// counterpart: with a working embedder the vector lane runs and the report
// advertises Degraded=false.
func TestHybridQuery_Recall_FullHybridReportsNotDegraded(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{{uid: "a", kind: "remember", confidence: 1.0, text: "alpha match"}}
	vec := &stubVector{hits: []VectorHit{{ID: "a"}}}
	fts := &stubFTS{hits: []FTSHit{{ID: "a"}}}
	h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes)

	_, report, err := h.RecallWithReport(t.Context(), RecallRequest{Query: "match"})
	if err != nil {
		t.Fatalf("RecallWithReport: %v", err)
	}
	if report.Degraded {
		t.Error("report.Degraded = true, want false for full hybrid")
	}
	if vec.lastK == 0 {
		t.Error("vector lane did not run under full hybrid")
	}
	if !slices.Contains(report.Lanes, LaneVector) || !slices.Contains(report.Lanes, LaneFTS) {
		t.Errorf("report.Lanes = %v, want vector + fts present", report.Lanes)
	}
}

func TestHybridQuery_Recall_KindsFilterAppliesBeforeFuse(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "a", kind: "remember", confidence: 1.0, text: "alpha"},
		{uid: "b", kind: "supersede", confidence: 1.0, text: "beta"},
		{uid: "c", kind: "remember", confidence: 1.0, text: "gamma"},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	fts := &stubFTS{hits: []FTSHit{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	h := newTestHybrid(t, episodes, vec, fts)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{
		Query: "anything",
		Kinds: []EventKind{"remember"},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, hit := range hits {
		if hit.Kind != "remember" {
			t.Errorf("got kind %q, want remember", hit.Kind)
		}
		if hit.UID == "b" {
			t.Errorf("supersede uid %q survived Kinds=[remember]", hit.UID)
		}
	}
	if len(hits) != 2 {
		t.Errorf("got %d hits, want 2 (a and c)", len(hits))
	}
}

func TestHybridQuery_Recall_MinConfidenceFilters(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "a", kind: "remember", confidence: 1.0, text: "high"},
		{uid: "b", kind: "remember", confidence: 0.3, text: "low"},
		{uid: "c", kind: "remember", confidence: 0.9, text: "high"},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	fts := &stubFTS{hits: []FTSHit{}}
	h := newTestHybrid(t, episodes, vec, fts)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{
		Query:         "any",
		MinConfidence: 0.5,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, hit := range hits {
		if hit.UID == "b" {
			t.Errorf("uid b (conf=0.3) survived MinConfidence=0.5")
		}
	}
	if len(hits) != 2 {
		t.Errorf("got %d hits, want 2", len(hits))
	}
}

func TestHybridQuery_Recall_LimitCaps(t *testing.T) {
	t.Parallel()

	// Seed five episodes, vec ranks them all.
	episodes := make([]seedEpisode, 0, 5)
	vecHits := make([]VectorHit, 0, 5)
	for i := range 5 {
		uid := fmt.Sprintf("e%d", i)
		episodes = append(episodes, seedEpisode{uid: uid, kind: "remember", confidence: 1.0, text: uid})
		vecHits = append(vecHits, VectorHit{ID: uid})
	}
	h := newTestHybrid(t, episodes, &stubVector{hits: vecHits}, &stubFTS{})

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "x", Limit: 2}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("got %d hits, want 2", len(hits))
	}
}

func TestHybridQuery_Recall_DefaultLimitWhenZero(t *testing.T) {
	t.Parallel()

	episodes := make([]seedEpisode, 0, 15)
	vecHits := make([]VectorHit, 0, 15)
	for i := range 15 {
		uid := fmt.Sprintf("e%02d", i)
		episodes = append(episodes, seedEpisode{uid: uid, kind: "remember", confidence: 1.0, text: uid})
		vecHits = append(vecHits, VectorHit{ID: uid})
	}
	h := newTestHybrid(t, episodes, &stubVector{hits: vecHits}, &stubFTS{})

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "x" /* Limit: 0 */}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != recallLimitDefault {
		t.Errorf("got %d hits, want default %d", len(hits), recallLimitDefault)
	}
}

func TestHybridQuery_Recall_DeterministicUIDTieBreak(t *testing.T) {
	t.Parallel()

	// Two episodes ranked identically by both sources -> identical fused
	// scores. UID asc should break the tie deterministically.
	episodes := []seedEpisode{
		{uid: "zzz", kind: "remember", confidence: 1.0, text: "z"},
		{uid: "aaa", kind: "remember", confidence: 1.0, text: "a"},
	}
	// Vec ranks zzz=1, aaa=2. FTS ranks aaa=1, zzz=2. The fused scores
	// are 1/(60+1) + 1/(60+2) on each side, which are exactly equal — so
	// the UID-ascending tie-break is the only thing that can decide the
	// final order. Without the tie-break the result would be
	// non-deterministic.
	vec := &stubVector{hits: []VectorHit{{ID: "zzz"}, {ID: "aaa"}}}
	fts := &stubFTS{hits: []FTSHit{{ID: "aaa"}, {ID: "zzz"}}}
	h := newTestHybrid(t, episodes, vec, fts)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "x"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !approxEqual(hits[0].FusedScore, hits[1].FusedScore) {
		t.Fatalf("expected equal fused scores, got %v vs %v",
			hits[0].FusedScore, hits[1].FusedScore)
	}
	if hits[0].UID != "aaa" || hits[1].UID != "zzz" {
		t.Errorf("tie-break order = %q,%q, want aaa,zzz (UID asc)",
			hits[0].UID, hits[1].UID)
	}
}

func TestHybridQuery_Recall_RetrieverErrorPropagates(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("vec down")
	h := newTestHybridWithEmbedder(t,
		stubEmbedder{},
		&stubVector{err: wantErr},
		&stubFTS{},
		nil,
	)
	_, err := drain(h.Recall(t.Context(), RecallRequest{Query: "x"}))
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wraps %v", err, wantErr)
	}
}

func TestHybridQuery_Recall_VecOnlyHitGetsTextSnippet(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "a", kind: "remember", confidence: 1.0, text: "semantic match without lexical overlap"},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "a"}}}
	fts := &stubFTS{} // no FTS hit for a
	h := newTestHybrid(t, episodes, vec, fts)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "x"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].Snippet != "semantic match without lexical overlap" {
		t.Errorf("snippet = %q, want full text fallback", hits[0].Snippet)
	}
	if strings.Contains(hits[0].Snippet, "<b>") {
		t.Errorf("vec-only snippet contains FTS markup: %q", hits[0].Snippet)
	}
}

func TestHybridQuery_Recall_IteratorRespectsEarlyExit(t *testing.T) {
	t.Parallel()

	episodes := make([]seedEpisode, 0, 10)
	vecHits := make([]VectorHit, 0, 10)
	for i := range 10 {
		uid := fmt.Sprintf("e%d", i)
		episodes = append(episodes, seedEpisode{uid: uid, kind: "remember", confidence: 1.0, text: uid})
		vecHits = append(vecHits, VectorHit{ID: uid})
	}
	h := newTestHybrid(t, episodes, &stubVector{hits: vecHits}, &stubFTS{})

	count := 0
	for _, err := range h.Recall(t.Context(), RecallRequest{Query: "x"}) {
		if err != nil {
			t.Fatalf("yielded err: %v", err)
		}
		count++
		if count == 2 {
			break // exit early; iterator must respect this
		}
	}
	if count != 2 {
		t.Errorf("iterator yielded %d hits before break, want 2", count)
	}
}

// TestHybridQuery_Recall_MarksSupersededHits is the unit-level pin for
// Fix 2 of the four-lane-recall plan. The composer's CURRENCY rule keys
// off Hit.Superseded, which HybridQuery flips after the final top-K
// cut by looking up relations.source_event_uid where valid_until is
// set. Two episodes share the recall path: ep-old has a superseded
// works_at relation; ep-new does not. The recall must mark only ep-old
// as Superseded.
func TestHybridQuery_Recall_MarksSupersededHits(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "ep-old", kind: "remember", confidence: 1.0, text: "Eowyn works at Erebor Inc."},
		{uid: "ep-new", kind: "remember", confidence: 1.0, text: "Eowyn joined Rivendell in 2025."},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "ep-old"}, {ID: "ep-new"}}}
	fts := &stubFTS{}
	h := newTestHybrid(t, episodes, vec, fts)

	// Seed the relations table so ep-old's works_at fact is superseded
	// (valid_until set) and ep-new is current.
	_, err := h.reader.ExecContext(t.Context(),
		`INSERT INTO relations (uid, source_event_uid, valid_until) VALUES
		    ('rel-old', 'ep-old', '2025-03-15T00:00:00Z'),
		    ('rel-new', 'ep-new', NULL)`)
	if err != nil {
		t.Fatalf("seed relations: %v", err)
	}

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Eowyn"}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	byUID := map[string]Hit{}
	for _, hit := range hits {
		byUID[hit.UID] = hit
	}
	if !byUID["ep-old"].Superseded {
		t.Errorf("ep-old: Superseded=false, want true (relation has valid_until)")
	}
	if byUID["ep-new"].Superseded {
		t.Errorf("ep-new: Superseded=true, want false (relation valid_until is NULL)")
	}
}

func TestHybridQuery_Recall_AttachesEntityAnchors(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "ep1", kind: "remember", confidence: 1.0, text: "Merry lives in Zurich."},
		{uid: "ep2", kind: "remember", confidence: 1.0, text: "A note with no resolved entity."},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "ep1"}, {ID: "ep2"}}}
	fts := &stubFTS{}
	h := newTestHybrid(t, episodes, vec, fts)

	// ep1 mentions Merry + Zurich (resolved). The duplicate Merry mention
	// must collapse to a single anchor. ep2 has only an unresolved mention
	// (entity_uid NULL), which must not surface as an anchor.
	if _, err := h.reader.ExecContext(t.Context(), `INSERT INTO entities (uid, kind, display_name) VALUES
	    ('alice', 'person', 'Merry'),
	    ('zurich', 'place', 'Zurich')`); err != nil {
		t.Fatalf("seed entities: %v", err)
	}
	if _, err := h.reader.ExecContext(t.Context(), `INSERT INTO mentions (uid, source_event_uid, entity_uid) VALUES
	    ('m1', 'ep1', 'alice'),
	    ('m2', 'ep1', 'zurich'),
	    ('m3', 'ep1', 'alice'),
	    ('m4', 'ep2', NULL)`); err != nil {
		t.Fatalf("seed mentions: %v", err)
	}

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Merry"}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	byUID := map[string]Hit{}
	for _, hit := range hits {
		byUID[hit.UID] = hit
	}

	got := map[string]EntityRef{}
	for _, ref := range byUID["ep1"].Entities {
		got[ref.UID] = ref
	}
	if len(got) != 2 {
		t.Fatalf("ep1 anchors = %+v, want exactly Merry + Zurich (deduped)", byUID["ep1"].Entities)
	}
	if got["alice"].DisplayName != "Merry" || got["alice"].Kind != "person" {
		t.Errorf("alice anchor = %+v, want hydrated person/Merry", got["alice"])
	}
	if got["zurich"].DisplayName != "Zurich" || got["zurich"].Kind != "place" {
		t.Errorf("zurich anchor = %+v, want hydrated place/Zurich", got["zurich"])
	}
	if len(byUID["ep2"].Entities) != 0 {
		t.Errorf("ep2 anchors = %+v, want none (only an unresolved mention)", byUID["ep2"].Entities)
	}
}

// observeRecorder is a stats.Store fake that records ObserveTopEntity calls so
// a test can assert recall feeds the usage stats that back the entities tool's
// recallCount sort and the context brief's "Key entities" ranking.
type observeRecorder struct {
	stats.Store
	observed []string // UIDs passed to ObserveTopEntity, in call order
}

func (o *observeRecorder) ObserveTopEntity(uid, _ string)    { o.observed = append(o.observed, uid) }
func (o *observeRecorder) IncRecall([]string, time.Duration) {}

// TestHybridQuery_Recall_ObservesPrimaryEntity guards that Recall records each
// hit's primary entity anchor via ObserveTopEntity. Without this wire the
// recall-usage stats stay empty, so the entities recallCount sort and the
// context brief's recall-ranked "Key entities" silently fall back to recency
// forever. ep1 resolves to Merry (+ Zurich); only the primary (first) anchor
// is observed. ep2 has no resolved entity and must not be observed.
func TestHybridQuery_Recall_ObservesPrimaryEntity(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "ep1", kind: "remember", confidence: 1.0, text: "Merry lives in Zurich."},
		{uid: "ep2", kind: "remember", confidence: 1.0, text: "A note with no resolved entity."},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "ep1"}, {ID: "ep2"}}}
	h := newTestHybrid(t, episodes, vec, &stubFTS{})

	rec := &observeRecorder{Store: stats.Noop()}
	h.SetStats(rec)

	if _, err := h.reader.ExecContext(t.Context(), `INSERT INTO entities (uid, kind, display_name) VALUES
	    ('alice', 'person', 'Merry'),
	    ('zurich', 'place', 'Zurich')`); err != nil {
		t.Fatalf("seed entities: %v", err)
	}
	if _, err := h.reader.ExecContext(t.Context(), `INSERT INTO mentions (uid, source_event_uid, entity_uid) VALUES
	    ('m1', 'ep1', 'alice'),
	    ('m2', 'ep1', 'zurich'),
	    ('m4', 'ep2', NULL)`); err != nil {
		t.Fatalf("seed mentions: %v", err)
	}

	if _, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Merry"})); err != nil {
		t.Fatalf("recall: %v", err)
	}

	// ep1 contributes one observation (its primary anchor); ep2 contributes
	// none (no resolved entity). The exact primary UID depends on mention
	// ordering, so assert membership rather than a specific UID.
	if len(rec.observed) != 1 {
		t.Fatalf("ObserveTopEntity calls = %v, want exactly 1 (ep1's primary anchor)", rec.observed)
	}
	if rec.observed[0] != "alice" && rec.observed[0] != "zurich" {
		t.Errorf("observed UID = %q, want one of ep1's resolved anchors", rec.observed[0])
	}
}

func TestHybridQuery_Traverse_PreservesNoopBehaviour(t *testing.T) {
	t.Parallel()

	h := newTestHybrid(t, nil, nil, nil)
	var seen error
	for _, err := range h.Traverse(t.Context(), TraverseRequest{}) {
		seen = err
		break
	}
	if !errors.Is(seen, ErrNotImplemented) {
		t.Errorf("Traverse yielded %v, want ErrNotImplemented", seen)
	}
}

// ---- helpers ----------------------------------------------------------------

type seedEpisode struct {
	uid        string
	kind       string
	confidence float64
	text       string
	// class is the EpisodeClass written into the episodes.class column.
	// Empty defaults to "episodic" (matching the migration's column
	// default) so existing test cases keep working without per-row
	// updates. Type-weight tests set this explicitly.
	class string
}

// openMemoryEpisodes opens a fresh in-memory SQLite with the episodes columns
// the composer reads (uid, kind, create_time, text, payload_json) plus minimal
// relations, mentions, entities, and source_refs tables mirroring migrations
// 0001/0007/0008/0030/0046. The relations / mentions / source_refs tables are
// always seeded empty, so markSuperseded, attachEntities, and attachFreshness
// run their IN-clause lookups and find nothing, the right behaviour for the
// unit suite's tiny stub stores. Tests that want to exercise the supersede or
// entity-anchor paths seed rows directly. The schema is intentionally a minimum
// subset (only the columns each attach-pass reads); we do not pull in the full
// migration runner because the hydrator only touches these episode columns and
// dragging the FTS / vec virtual tables into a unit test would make the test
// dependent on the sqlite-vec extension load.
func openMemoryEpisodes(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), `
        CREATE TABLE episodes (
            uid          TEXT PRIMARY KEY,
            create_time  TEXT NOT NULL,
            kind         TEXT NOT NULL,
            payload_json TEXT NOT NULL,
            text         TEXT NOT NULL DEFAULT '',
            source       TEXT NOT NULL DEFAULT '',
            class        TEXT NOT NULL DEFAULT 'episodic',
            scope        TEXT NOT NULL DEFAULT 'personal'
        );
        CREATE TABLE relations (
            uid               TEXT PRIMARY KEY,
            subject_uid       TEXT NOT NULL DEFAULT '',
            predicate         TEXT NOT NULL DEFAULT '',
            object_uid        TEXT NOT NULL DEFAULT '',
            source_event_uid  TEXT NOT NULL,
            valid_until       TEXT,
            scope             TEXT NOT NULL DEFAULT 'personal'
        );
        CREATE TABLE entities (
            uid          TEXT PRIMARY KEY,
            kind         TEXT NOT NULL,
            display_name TEXT NOT NULL,
            confidence   REAL NOT NULL DEFAULT 0.0,
            create_time  TEXT NOT NULL DEFAULT '',
            scope        TEXT NOT NULL DEFAULT 'personal'
        );
        CREATE TABLE mentions (
            uid              TEXT PRIMARY KEY,
            source_event_uid TEXT NOT NULL,
            entity_uid       TEXT,
            scope            TEXT NOT NULL DEFAULT 'personal'
        );
        CREATE TABLE source_refs (
            uid              TEXT PRIMARY KEY,
            subject_uid      TEXT NOT NULL,
            source_type      TEXT NOT NULL DEFAULT '',
            connector        TEXT,
            external_url     TEXT,
            fetch_time       TEXT NOT NULL DEFAULT '',
            dedup_key        TEXT NOT NULL DEFAULT '',
            source_event_uid TEXT NOT NULL DEFAULT '',
            create_time      TEXT NOT NULL DEFAULT '',
            tombstone        TEXT,
            scope            TEXT NOT NULL DEFAULT 'personal'
        )`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func seedEpisodes(t *testing.T, db *sql.DB, episodes []seedEpisode) {
	t.Helper()
	for _, ep := range episodes {
		payload := fmt.Sprintf(`{"text":%q,"confidence":%v,"source":"test"}`, ep.text, ep.confidence)
		class := ep.class
		if class == "" {
			class = "episodic"
		}
		_, err := db.ExecContext(t.Context(),
			`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source, class) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ep.uid,
			time.Now().UTC().Format(time.RFC3339Nano),
			ep.kind,
			payload,
			ep.text,
			"test",
			class,
		)
		if err != nil {
			t.Fatalf("seed %q: %v", ep.uid, err)
		}
	}
}

func newTestHybrid(t *testing.T, episodes []seedEpisode, vec *stubVector, fts *stubFTS) *HybridQuery {
	t.Helper()
	return newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes)
}

func newTestHybridWithEmbedder(t *testing.T, emb Embedder, vec *stubVector, fts *stubFTS, episodes []seedEpisode, opts ...HybridOption) *HybridQuery {
	t.Helper()
	db := openMemoryEpisodes(t)
	if episodes != nil {
		seedEpisodes(t, db, episodes)
	}
	if vec == nil {
		vec = &stubVector{}
	}
	if fts == nil {
		fts = &stubFTS{}
	}
	h, err := NewHybridQuery(emb, vec, fts, db, opts...)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	return h
}

// drain consumes an iter.Seq2 hit iterator and returns the slice. A non-nil
// error is propagated unchanged.
func drain(it func(yield func(Hit, error) bool)) ([]Hit, error) {
	hits := make([]Hit, 0, recallLimitDefault)
	var err error
	for h, e := range it {
		if e != nil {
			err = e
			break
		}
		hits = append(hits, h)
	}
	return hits, err
}

// approxEqual compares float32 values with a tight epsilon. RRF arithmetic
// uses exact reciprocals of small integers, so the rounding error is
// bounded by float32 precision.
func approxEqual(a, b float32) bool {
	return math.Abs(float64(a-b)) < 1e-6
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ---- stubs ------------------------------------------------------------------

type stubEmbedder struct {
	err error
}

func (s stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	// The composer does not inspect the embedding; any non-empty slice works.
	return []float32{0.1, 0.2, 0.3}, nil
}

type stubVector struct {
	hits       []VectorHit
	err        error
	lastK      int // most recent k passed to Search
	lastFilter VectorFilter
}

func (s *stubVector) Upsert(_ context.Context, _ []VectorItem) error { return nil }
func (s *stubVector) Delete(_ context.Context, _ []string) error     { return nil }
func (s *stubVector) Close() error                                   { return nil }
func (s *stubVector) Search(_ context.Context, _ []float32, k int, f VectorFilter) ([]VectorHit, error) {
	s.lastK = k
	s.lastFilter = f
	if s.err != nil {
		return nil, s.err
	}
	return s.hits, nil
}

type stubFTS struct {
	hits         []FTSHit
	err          error
	lastK        int
	lastRangeK   int       // most recent k passed to SearchInTimeRange
	lastMinTime  time.Time // most recent minBound passed to SearchInTimeRange
	lastMaxTime  time.Time // most recent maxBound passed to SearchInTimeRange
	rangeCalled  bool
	searchCalled bool
}

func (s *stubFTS) Index(_ context.Context, _ []FTSItem) error { return nil }
func (s *stubFTS) Delete(_ context.Context, _ []string) error { return nil }
func (s *stubFTS) Close() error                               { return nil }
func (s *stubFTS) Search(_ context.Context, _ string, k int) ([]FTSHit, error) {
	s.lastK = k
	s.searchCalled = true
	if s.err != nil {
		return nil, s.err
	}
	return s.hits, nil
}

// SearchInTimeRange satisfies the ftsTimeRanger optional interface so tests
// can assert the composer pushes time bounds INTO the FTS lane rather than
// post-filtering. Behaves like Search otherwise.
func (s *stubFTS) SearchInTimeRange(_ context.Context, _ string, k int, minBound, maxBound time.Time) ([]FTSHit, error) {
	s.lastRangeK = k
	s.lastMinTime = minBound
	s.lastMaxTime = maxBound
	s.rangeCalled = true
	if s.err != nil {
		return nil, s.err
	}
	return s.hits, nil
}

// TestHybridQuery_PreFusionSemanticGateFiltersWeakCandidates is the
// load-bearing Phase 1 integration test. A candidate that ranks #1 in
// FTS but is semantically unrelated (raw cosine 0.5) must be DROPPED
// before fusion — keyword overlap can't rescue it. The strongly-related
// candidate (cosine ≈ 0.995) surfaces alone.
//
// Pre-fix behaviour: rank-based fusion at the FTS top-rank elevated
// the off-topic candidate. mem0/utils/scoring.py:102-103 does this
// filter on raw semantic; we now match.
func TestHybridQuery_PreFusionSemanticGateFiltersWeakCandidates(t *testing.T) {
	t.Parallel()

	// L2 distances chosen so cosine similarity straddles the gate:
	//   A: distance 1.0 → cosine 1 - 1.0/2.0 = 0.5  (BELOW the 0.6 gate)
	//   B: distance 0.1 → cosine 1 - 0.01/2  = 0.995 (clears the gate)
	episodes := []seedEpisode{
		{uid: "weak", kind: "remember", confidence: 1.0, text: "off topic"},
		{uid: "strong", kind: "remember", confidence: 1.0, text: "on topic match"},
	}
	vec := &stubVector{hits: []VectorHit{
		{ID: "weak", Score: 1.0},   // cos 0.5
		{ID: "strong", Score: 0.1}, // cos 0.995
	}}
	fts := &stubFTS{hits: []FTSHit{
		{ID: "weak", BM25: 8.0, Snippet: "weak <b>match</b>"}, // strong FTS to test it can't rescue
		{ID: "strong", BM25: 6.0, Snippet: "strong <b>match</b>"},
	}}
	h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
		WithFusionMode("weighted"),
		WithSemanticGateThreshold(0.6),
	)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "match"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want exactly 1 (weak candidate must be gated out); hits=%+v", len(hits), hits)
	}
	if hits[0].UID != "strong" {
		t.Errorf("survivor = %q, want %q", hits[0].UID, "strong")
	}
	if hits[0].Components.RawSemantic < 0.99 {
		t.Errorf("strong hit RawSemantic = %v, want ≈ 0.995", hits[0].Components.RawSemantic)
	}
}

// TestHybridQuery_PreFusionSemanticGateOffByDefault pins the default
// behaviour: without WithSemanticGateThreshold, both candidates
// surface even though one is semantically weak. This guards against
// silent regressions on existing callers that rely on the gate being
// opt-in.
func TestHybridQuery_PreFusionSemanticGateOffByDefault(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "weak", kind: "remember", confidence: 1.0, text: "off topic"},
		{uid: "strong", kind: "remember", confidence: 1.0, text: "on topic match"},
	}
	vec := &stubVector{hits: []VectorHit{
		{ID: "weak", Score: 1.0},
		{ID: "strong", Score: 0.1},
	}}
	fts := &stubFTS{hits: []FTSHit{
		{ID: "weak", BM25: 8.0, Snippet: "weak <b>match</b>"},
		{ID: "strong", BM25: 6.0, Snippet: "strong <b>match</b>"},
	}}
	// fusionMode=weighted but no gate → both candidates kept.
	h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
		WithFusionMode("weighted"),
	)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "match"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (gate off → both survive); hits=%+v", len(hits), hits)
	}
}

// TestRecall_TypeWeightMultipliesFusedScore pins Phase 3 of the
// mem0-parity sweep: three candidates with identical raw semantic
// (cosine 0.995) and different classes must fuse in the ratio
// semantic > episodic > procedural. The default mem0 v3 weights
// (0.6 / 0.3 / 0.1) make the semantic candidate rank first and the
// procedural candidate rank last, even though all three have the
// same vector / FTS / entity contributions.
func TestRecall_TypeWeightMultipliesFusedScore(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "sem", kind: "remember", confidence: 1.0, text: "fact about Berlin", class: "semantic"},
		{uid: "epi", kind: "remember", confidence: 1.0, text: "fact about Berlin", class: "episodic"},
		{uid: "pro", kind: "remember", confidence: 1.0, text: "fact about Berlin", class: "procedural"},
	}
	vec := &stubVector{hits: []VectorHit{
		{ID: "sem", Score: 0.1},
		{ID: "epi", Score: 0.1},
		{ID: "pro", Score: 0.1},
	}}
	fts := &stubFTS{hits: []FTSHit{
		{ID: "sem", BM25: 5.0, Snippet: "fact about <b>Berlin</b>"},
		{ID: "epi", BM25: 5.0, Snippet: "fact about <b>Berlin</b>"},
		{ID: "pro", BM25: 5.0, Snippet: "fact about <b>Berlin</b>"},
	}}
	h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
		WithFusionMode("weighted"),
		WithTypeWeights(map[string]float64{
			"semantic":   0.6,
			"episodic":   0.3,
			"procedural": 0.1,
		}),
	)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Berlin"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3 (no gate, no filter); hits=%+v", len(hits), hits)
	}
	if hits[0].UID != "sem" {
		t.Errorf("rank 1 = %q, want %q (semantic class outweighs)", hits[0].UID, "sem")
	}
	if hits[1].UID != "epi" {
		t.Errorf("rank 2 = %q, want %q (episodic class is middle)", hits[1].UID, "epi")
	}
	if hits[2].UID != "pro" {
		t.Errorf("rank 3 = %q, want %q (procedural class is lowest)", hits[2].UID, "pro")
	}
	// Ratios should approximate the type weights (recency boost may
	// tweak the absolute magnitudes equally across the three; ratios
	// stay anchored). Allow loose tolerance to absorb the recency
	// multiplier — all three episodes were created at the same wall
	// instant by seedEpisodes.
	semScore := hits[0].FusedScore
	epiScore := hits[1].FusedScore
	proScore := hits[2].FusedScore
	ratio := func(a, b float32) float64 { return float64(a) / float64(b) }
	if got, want := ratio(semScore, epiScore), 2.0; got < want*0.9 || got > want*1.1 {
		t.Errorf("semantic/episodic ratio = %.3f, want ≈ %v (±10%%)", got, want)
	}
	if got, want := ratio(epiScore, proScore), 3.0; got < want*0.9 || got > want*1.1 {
		t.Errorf("episodic/procedural ratio = %.3f, want ≈ %v (±10%%)", got, want)
	}
}

// TestRecall_TypeWeightDisabledByDefault pins that without
// WithTypeWeights, every class scores at its raw fused value —
// pre-Phase-3 behaviour for callers that haven't tuned the knob.
func TestRecall_TypeWeightDisabledByDefault(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "sem", kind: "remember", confidence: 1.0, text: "fact about Berlin", class: "semantic"},
		{uid: "pro", kind: "remember", confidence: 1.0, text: "fact about Berlin", class: "procedural"},
	}
	vec := &stubVector{hits: []VectorHit{
		{ID: "sem", Score: 0.1},
		{ID: "pro", Score: 0.1},
	}}
	fts := &stubFTS{hits: []FTSHit{
		{ID: "sem", BM25: 5.0, Snippet: "fact about <b>Berlin</b>"},
		{ID: "pro", BM25: 5.0, Snippet: "fact about <b>Berlin</b>"},
	}}
	h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
		WithFusionMode("weighted"),
	)
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Berlin"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2; hits=%+v", len(hits), hits)
	}
	if hits[0].FusedScore == 0 || hits[1].FusedScore == 0 {
		t.Errorf("both hits should have nonzero fused score when type weights are off; got %v / %v",
			hits[0].FusedScore, hits[1].FusedScore)
	}
	// With type weights off and identical inputs, the two scores must
	// be equal — UID tiebreak orders them deterministically.
	if got, want := hits[0].FusedScore, hits[1].FusedScore; got != want {
		t.Errorf("default behaviour: scores should match without type weights; got %v vs %v", got, want)
	}
}
