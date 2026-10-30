package sqlite_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// insertEpisode is a thin helper for FTS tests: builds a deterministic uid
// and writes a row with explicit text and source values so the AFTER INSERT
// trigger can copy them into episodes_fts.
func insertEpisode(t *testing.T, store *sqlite.Store, uid, text, source string) {
	t.Helper()
	_, err := store.Writer().ExecContext(t.Context(),
		`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
         VALUES (?, ?, ?, ?, ?, ?)`,
		uid, time.Now().UTC().Format(time.RFC3339Nano), "remember", `{}`, text, source,
	)
	if err != nil {
		t.Fatalf("insert episode %q: %v", uid, err)
	}
}

func search(t *testing.T, store *sqlite.Store, query string, k int) []searchHit {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := store.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	hits, err := c.Search(ctx, query, k)
	if err != nil {
		t.Fatalf("Search(%q, %d): %v", query, k, err)
	}
	out := make([]searchHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHit{ID: h.ID, BM25: h.BM25, Snippet: h.Snippet})
	}
	return out
}

type searchHit struct {
	ID      string
	BM25    float32
	Snippet string
}

func TestFTS_VirtualTableExists(t *testing.T) {
	store := openStore(t)

	rows, err := store.Reader().QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name LIKE 'episodes_fts%' ORDER BY name`,
	)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	want := []string{
		"episodes_fts",
		"episodes_fts_data",
		"episodes_fts_idx",
		"episodes_fts_docsize",
		"episodes_fts_config",
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("FTS table %q missing; got %v", n, got)
		}
	}
}

func TestFTS_RankIsBM25(t *testing.T) {
	store := openStore(t)

	// FTS5 stores the rank in the config shadow table as a blob; the
	// human-readable form is recovered by quoting the value into text.
	var rank string
	err := store.Reader().QueryRowContext(t.Context(),
		`SELECT quote(v) FROM episodes_fts_config WHERE k = 'rank'`,
	).Scan(&rank)
	if err != nil {
		t.Fatalf("read rank from episodes_fts_config: %v", err)
	}
	want := "'bm25(10.0, 5.0)'"
	if rank != want {
		t.Errorf("rank = %s, want %s", rank, want)
	}
}

func TestFTS_InsertTriggerSyncs(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-insert-1", "hippopotamus on the lawn", "src-a")

	hits := search(t, store, "hippopotamus", 10)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d (%v)", len(hits), hits)
	}
	if hits[0].ID != "ep-insert-1" {
		t.Errorf("hit ID = %q, want ep-insert-1", hits[0].ID)
	}
}

func TestFTS_UpdateTriggerSyncs(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-update-1", "kangaroo at dawn", "src-a")

	if got := search(t, store, "kangaroo", 10); len(got) != 1 {
		t.Fatalf("pre-update: expected 1 hit for kangaroo, got %d", len(got))
	}

	if _, err := store.Writer().ExecContext(t.Context(),
		`UPDATE episodes SET text = ? WHERE uid = ?`,
		"narwhal under ice", "ep-update-1",
	); err != nil {
		t.Fatalf("update episode: %v", err)
	}

	if got := search(t, store, "kangaroo", 10); len(got) != 0 {
		t.Errorf("post-update: kangaroo should be gone, got %d hits", len(got))
	}
	if got := search(t, store, "narwhal", 10); len(got) != 1 {
		t.Errorf("post-update: narwhal should be present, got %d hits", len(got))
	}
}

func TestFTS_DeleteTriggerSyncs(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-delete-1", "platypus at sunset", "src-a")

	if got := search(t, store, "platypus", 10); len(got) != 1 {
		t.Fatalf("pre-delete: expected 1 hit, got %d", len(got))
	}

	if _, err := store.Writer().ExecContext(t.Context(),
		`DELETE FROM episodes WHERE uid = ?`, "ep-delete-1",
	); err != nil {
		t.Fatalf("delete episode: %v", err)
	}

	if got := search(t, store, "platypus", 10); len(got) != 0 {
		t.Errorf("post-delete: expected 0 hits, got %d", len(got))
	}
}

func TestFTS_BM25WeightFavorsText(t *testing.T) {
	store := openStore(t)

	// Same keyword appears once each. The bm25(10.0, 5.0) weights give text
	// twice the influence of source, so the text-bearing row must outrank
	// the source-bearing row.
	insertEpisode(t, store, "ep-text", "axolotl swimming", "filler-a")
	insertEpisode(t, store, "ep-source", "filler-b filler-c", "axolotl")

	hits := search(t, store, "axolotl", 10)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d (%v)", len(hits), hits)
	}
	if hits[0].ID != "ep-text" {
		t.Errorf("top hit = %q, want ep-text (text weight should outrank source)", hits[0].ID)
	}
	if hits[0].BM25 <= hits[1].BM25 {
		t.Errorf("BM25 ordering: top=%v second=%v; expected top>second after sign-flip", hits[0].BM25, hits[1].BM25)
	}
}

func TestFTS_SnippetFormat(t *testing.T) {
	store := openStore(t)

	long := strings.Repeat("alpha ", 40) + "needle " + strings.Repeat("omega ", 40)
	insertEpisode(t, store, "ep-snippet", long, "src")

	hits := search(t, store, "needle", 10)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	snip := hits[0].Snippet
	if !strings.Contains(snip, "<b>needle</b>") {
		t.Errorf("snippet missing <b>needle</b> wrap: %q", snip)
	}
	if !strings.Contains(snip, "...") {
		t.Errorf("snippet missing ellipsis marker: %q", snip)
	}
}

func TestFTS_Unicode61RemovesDiacritics(t *testing.T) {
	store := openStore(t)

	insertEpisode(t, store, "ep-cafe", "we met at the café in Berlin", "src")

	hits := search(t, store, "cafe", 10)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for diacritic-folded match, got %d", len(hits))
	}
	if hits[0].ID != "ep-cafe" {
		t.Errorf("hit ID = %q, want ep-cafe", hits[0].ID)
	}
}

func TestFTS_TokencharsUnderscore(t *testing.T) {
	store := openStore(t)

	// With tokenchars '_' the underscore is part of a token, so foo_bar
	// indexes as one unit. A search for foo_bar matches the row; a search
	// for the bare token foo does not, because the underscore did not act
	// as a delimiter.
	insertEpisode(t, store, "ep-snake", "the symbol foo_bar appears once", "src")

	if got := search(t, store, "foo_bar", 10); len(got) != 1 {
		t.Errorf("foo_bar should match (tokenchars '_'): got %d hits", len(got))
	}
	if got := search(t, store, "foo", 10); len(got) != 0 {
		t.Errorf("foo should NOT match (underscore is a tokenchar, not a delimiter): got %d hits %v", len(got), got)
	}
}

func TestFTS_TopHitsForDistinctiveTokens(t *testing.T) {
	store := openStore(t)

	// 20 episodes with mixed text; three rows carry a unique token
	// "zebra-token-N" to exercise top-hit assertions.
	for i := range 20 {
		uid := fmt.Sprintf("ep-corpus-%02d", i)
		text := fmt.Sprintf("background filler row %d talking about ordinary subjects", i)
		source := "src-bulk"
		switch i {
		case 3:
			text = "zebra_token_a introducing a unique marker"
		case 7:
			text = "another row mentioning zebra_token_b only once"
		case 11:
			text = "this row zebra_token_c alone stands out"
		}
		insertEpisode(t, store, uid, text, source)
	}

	cases := []struct {
		token  string
		wantID string
	}{
		{"zebra_token_a", "ep-corpus-03"},
		{"zebra_token_b", "ep-corpus-07"},
		{"zebra_token_c", "ep-corpus-11"},
	}
	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			hits := search(t, store, tc.token, 5)
			if len(hits) == 0 {
				t.Fatalf("no hits for %q", tc.token)
			}
			if hits[0].ID != tc.wantID {
				t.Errorf("top hit for %q = %q, want %q", tc.token, hits[0].ID, tc.wantID)
			}
			if !strings.Contains(hits[0].Snippet, "<b>") {
				t.Errorf("top hit snippet missing highlight: %q", hits[0].Snippet)
			}
		})
	}
}

// TestFTS_RaceClean fires concurrent reader queries while the writer
// stream-inserts new rows. With -race this catches any pool sharing or
// trigger reentrancy bug that would corrupt the shadow tables.
func TestFTS_RaceClean(t *testing.T) {
	store := openStore(t)

	// Seed a baseline so readers always have something to hit.
	for i := range 10 {
		insertEpisode(t, store, fmt.Sprintf("ep-seed-%02d", i),
			fmt.Sprintf("seed token-%02d content", i), "src-seed")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Writer goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			if ctx.Err() != nil {
				return
			}
			uid := fmt.Sprintf("ep-race-%02d", i)
			_, err := store.Writer().ExecContext(ctx,
				`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
                 VALUES (?, ?, ?, ?, ?, ?)`,
				uid, time.Now().UTC().Format(time.RFC3339Nano), "remember", `{}`,
				fmt.Sprintf("race row %02d content", i), "src-race",
			)
			if err != nil && ctx.Err() == nil {
				t.Errorf("writer insert %q: %v", uid, err)
				return
			}
		}
	}()

	// Reader goroutines.
	const readers = 8
	for r := range readers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			fts, err := store.OpenFTS(ctx)
			if err != nil {
				t.Errorf("reader %d: OpenFTS: %v", id, err)
				return
			}
			for range 30 {
				if ctx.Err() != nil {
					return
				}
				if _, err := fts.Search(ctx, "content", 5); err != nil && ctx.Err() == nil {
					t.Errorf("reader %d: search: %v", id, err)
					return
				}
			}
		}(r)
	}

	wg.Wait()
}

func TestFTS_OpenAfterCloseFails(t *testing.T) {
	store := openStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := store.OpenFTS(t.Context()); err == nil {
		t.Fatal("OpenFTS on closed store returned nil error")
	}
}

func TestFTS_SearchZeroKReturnsNothing(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-zero-k", "anything", "src")

	if got := search(t, store, "anything", 0); len(got) != 0 {
		t.Errorf("k=0 should return no hits, got %d", len(got))
	}
}

func TestFTS_SearchEmptyQueryReturnsNothing(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-empty", "anything", "src")

	cases := []string{"", "   ", "\t\n  "}
	for _, q := range cases {
		t.Run("q="+strconv.Quote(q), func(t *testing.T) {
			if got := search(t, store, q, 10); len(got) != 0 {
				t.Errorf("empty/whitespace query should return no hits, got %d", len(got))
			}
		})
	}
}

func TestFTS_SearchMalformedQueryReturnsError(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-malformed", "anything", "src")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := store.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}

	// Unclosed quote and a bare AND are FTS5 syntax errors; the wrapped
	// error surfaces from sqlite at iterate time. We accept either an
	// immediate query error or a wrapped iterate error.
	bad := []string{`"unclosed`, `AND`, `()`}
	for _, q := range bad {
		t.Run("q="+strconv.Quote(q), func(t *testing.T) {
			if _, err := conn.Search(ctx, q, 10); err == nil {
				t.Errorf("expected error for malformed FTS5 query %q, got nil", q)
			}
		})
	}
}

// TestFTS_RebuildIndexesExistingRows guards the migration's 'rebuild'
// insert: any pre-existing episode rows (added with non-empty text after
// the schema change but before the FTS vtable existed, for example via a
// future schema migration that backfills) must be searchable immediately
// after migration, not only on the next trigger fire. Regression for
// Greptile P1 PR #83.
func TestFTS_RebuildIndexesExistingRows(t *testing.T) {
	store := openStore(t)

	// Drop the trigger to simulate "row written without firing the AFTER
	// INSERT trigger" (this is the closest in-process simulation of the
	// pre-migration content-table state). Then rebuild and assert search
	// still finds it.
	if _, err := store.Writer().ExecContext(t.Context(),
		`DROP TRIGGER episodes_ai`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	_, err := store.Writer().ExecContext(t.Context(),
		`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
         VALUES (?, ?, ?, ?, ?, ?)`,
		"ep-rebuild", time.Now().UTC().Format(time.RFC3339Nano), "remember", `{}`,
		"distinctive_rebuild_token here", "src",
	)
	if err != nil {
		t.Fatalf("insert without trigger: %v", err)
	}

	// Without the trigger and without rebuild, the row is invisible to FTS.
	if got := search(t, store, "distinctive_rebuild_token", 10); len(got) != 0 {
		t.Fatalf("setup precondition broken: row visible before rebuild (got %d hits)", len(got))
	}

	if _, err := store.Writer().ExecContext(t.Context(),
		`INSERT INTO episodes_fts(episodes_fts) VALUES('rebuild')`); err != nil {
		t.Fatalf("rebuild fts: %v", err)
	}

	hits := search(t, store, "distinctive_rebuild_token", 10)
	if len(hits) != 1 {
		t.Fatalf("after rebuild: expected 1 hit, got %d", len(hits))
	}
	if hits[0].ID != "ep-rebuild" {
		t.Errorf("rebuild hit ID = %q, want ep-rebuild", hits[0].ID)
	}
}

// TestFTS_SearchInTimeRangeFiltersOutOfBandRows guards the FTS push-down
// path: rows whose create_time falls outside [minUnix, maxUnix) must not
// appear in the top-k, regardless of BM25 rank. Without push-down, top-K
// out-of-window matches would crowd out the in-window long tail when the
// query is generic (e.g., a common term).
func TestFTS_SearchInTimeRangeFiltersOutOfBandRows(t *testing.T) {
	store := openStore(t)

	rows := []struct {
		uid string
		ts  string
	}{
		{"ep-2024", "2024-06-01T00:00:00Z"},
		{"ep-2025", "2025-06-01T00:00:00Z"},
		{"ep-2026", "2026-06-01T00:00:00Z"},
	}
	for _, r := range rows {
		if _, err := store.Writer().ExecContext(t.Context(),
			`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
             VALUES (?, ?, ?, ?, ?, ?)`,
			r.uid, r.ts, "remember", `{}`, "shared_token "+r.uid, "src",
		); err != nil {
			t.Fatalf("seed row %q: %v", r.uid, err)
		}
	}

	conn, err := store.OpenFTS(t.Context())
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	fts, ok := conn.(*sqlite.FTSStore)
	if !ok {
		t.Fatalf("OpenFTS did not return *sqlite.FTSStore, got %T", conn)
	}

	minT := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	maxT := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	hits, err := fts.SearchInTimeRange(t.Context(), "shared_token", 10, minT, maxT)
	if err != nil {
		t.Fatalf("SearchInTimeRange: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "ep-2025" {
		t.Fatalf("hits = %+v, want exactly ep-2025 (in-window only)", hits)
	}

	// Lower-only window: zero maxT means no upper bound.
	hits, err = fts.SearchInTimeRange(t.Context(), "shared_token", 10, minT, time.Time{})
	if err != nil {
		t.Fatalf("SearchInTimeRange lower-only: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got["ep-2025"] || !got["ep-2026"] || got["ep-2024"] {
		t.Errorf("lower-only hits = %v, want {ep-2025, ep-2026}", got)
	}

	// Upper-only window: zero minT means no lower bound.
	hits, err = fts.SearchInTimeRange(t.Context(), "shared_token", 10, time.Time{}, maxT)
	if err != nil {
		t.Fatalf("SearchInTimeRange upper-only: %v", err)
	}
	got = map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got["ep-2024"] || !got["ep-2025"] || got["ep-2026"] {
		t.Errorf("upper-only hits = %v, want {ep-2024, ep-2025}", got)
	}
}

// TestFTS_PrefixMatch verifies the quoted-phrase-prefix form that
// escapeFTSQuery emits (`"tok"*`) matches morphological variants: a query
// "peanut" recalls a stored "peanuts". The double quotes neutralise FTS5
// keywords (AND/OR/NOT), and the trailing star turns the final token into a
// prefix token. Without the star the match is exact, so "peanut" alone does
// NOT find "peanuts" — that gap is exactly what prefix matching closes.
func TestFTS_PrefixMatch(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-peanut", "Merry is allergic to peanuts", "src")

	if got := search(t, store, `"peanut"*`, 10); len(got) != 1 || got[0].ID != "ep-peanut" {
		t.Errorf(`"peanut"* should prefix-match stored "peanuts": got %+v`, got)
	}
	if got := search(t, store, `"peanut"`, 10); len(got) != 0 {
		t.Errorf(`exact "peanut" must NOT match "peanuts" (proves the star is load-bearing): got %d`, len(got))
	}

	// The exact-mode direction: escapeFTSQuery leaves 1-2 rune tokens without
	// a star, so a short token must match exactly, not as a prefix. A bare
	// "hi" must NOT match "hint" — the star's ABSENCE is load-bearing too
	// (short function words must not explode into broad prefix scans).
	insertEpisode(t, store, "ep-hint", "take the hint please", "src")
	if got := search(t, store, `"hi"`, 10); len(got) != 0 {
		t.Errorf(`exact "hi" must NOT match "hint" (short tokens stay exact, no prefix): got %+v`, got)
	}
}

func TestFTS_SearchMultiWordPhrase(t *testing.T) {
	store := openStore(t)
	insertEpisode(t, store, "ep-phrase", "hippopotamus on the lawn at dawn", "src")
	insertEpisode(t, store, "ep-reversed", "lawn hippopotamus reversed order", "src")

	// Phrase query: the words must appear adjacent in this order.
	hits := search(t, store, `"hippopotamus on"`, 10)
	if len(hits) != 1 {
		t.Fatalf("phrase query: expected 1 hit, got %d", len(hits))
	}
	if hits[0].ID != "ep-phrase" {
		t.Errorf("phrase query top hit = %q, want ep-phrase", hits[0].ID)
	}

	// Reversed phrase must not match the first row.
	for _, h := range search(t, store, `"on hippopotamus"`, 10) {
		if h.ID == "ep-phrase" {
			t.Errorf("reversed phrase should not match ep-phrase; FTS5 phrase semantics broken")
		}
	}
}
