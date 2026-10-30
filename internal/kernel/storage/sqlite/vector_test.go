package sqlite_test

import (
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

func openVector(t *testing.T) (*sqlite.Store, *sqlite.Vector) {
	t.Helper()
	store := openStore(t)
	v, err := sqlite.OpenVector(t.Context(), store)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return store, v
}

func randVec(seed uint64) []float32 {
	r := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
	out := make([]float32, storage.VectorDim)
	for i := range out {
		out[i] = r.Float32()
	}
	return out
}

func TestOpenVector_ExtensionLoaded(t *testing.T) {
	store := openStore(t)

	var ver string
	if err := store.Writer().QueryRowContext(t.Context(), "SELECT vec_version()").Scan(&ver); err != nil {
		t.Fatalf("vec_version(): %v (extension not loaded?)", err)
	}
	if ver == "" {
		t.Errorf("vec_version() returned empty string")
	}
}

func TestOpenVector_ProbesSchema(t *testing.T) {
	_, v := openVector(t)
	if v == nil {
		t.Fatal("OpenVector returned nil Vector")
	}
}

func TestOpenVector_FailsWhenSchemaMissing(t *testing.T) {
	store := openStore(t)
	// Drop the vector tables to simulate the case where migration 0003 was
	// not applied. OpenVector's schema probe must surface a clear error
	// rather than silently returning a Vector wired to non-existent tables.
	for _, table := range []string{"episodes_vec", "entities_vec", "episodes_vec_ids", "entities_vec_ids"} {
		if _, err := store.Writer().ExecContext(t.Context(), "DROP TABLE "+table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if _, err := sqlite.OpenVector(t.Context(), store); err == nil {
		t.Fatal("OpenVector succeeded with missing schema")
	} else if !strings.Contains(err.Error(), "migration 0003") {
		t.Errorf("error = %v, want substring %q", err, "migration 0003")
	}
}

func TestOpenVector_RejectsNilStore(t *testing.T) {
	if _, err := sqlite.OpenVector(t.Context(), nil); err == nil {
		t.Fatal("OpenVector accepted nil store")
	}
}

func TestVector_UpsertSearchReturnsKNearest(t *testing.T) {
	_, v := openVector(t)

	const n = 64
	items := make([]kernel.VectorItem, 0, n)
	for i := range n {
		items = append(items, kernel.VectorItem{
			ID:        "ep-" + strconv.Itoa(i),
			Embedding: randVec(uint64(i + 1)),
		})
	}
	if err := v.Upsert(t.Context(), items); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	query := items[7].Embedding
	hits, err := v.Search(t.Context(), query, 3, kernel.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	if hits[0].ID != "ep-7" {
		t.Errorf("top hit ID = %q, want %q", hits[0].ID, "ep-7")
	}
	if hits[0].Score > 0.001 {
		t.Errorf("top distance = %v, want ~0", hits[0].Score)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score < hits[i-1].Score {
			t.Errorf("distance not monotonic: hits[%d]=%v < hits[%d]=%v", i, hits[i].Score, i-1, hits[i-1].Score)
		}
	}
}

func TestVector_UpsertReplacesExistingID(t *testing.T) {
	_, v := openVector(t)

	id := "ep-replace"
	first := randVec(1)
	if err := v.Upsert(t.Context(), []kernel.VectorItem{{ID: id, Embedding: first}}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	second := randVec(2)
	if err := v.Upsert(t.Context(), []kernel.VectorItem{{ID: id, Embedding: second}}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	hits, err := v.Search(t.Context(), second, 1, kernel.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != id {
		t.Fatalf("hits=%+v, want one hit with ID %q", hits, id)
	}
	if hits[0].Score > 0.001 {
		t.Errorf("distance to replacement = %v, want ~0 (vec0 row should hold the new embedding)", hits[0].Score)
	}
}

func TestVector_DeleteRemoves(t *testing.T) {
	_, v := openVector(t)

	const n = 8
	items := make([]kernel.VectorItem, 0, n)
	ids := make([]string, 0, n)
	for i := range n {
		id := "ep-del-" + strconv.Itoa(i)
		ids = append(ids, id)
		items = append(items, kernel.VectorItem{ID: id, Embedding: randVec(uint64(100 + i))})
	}
	if err := v.Upsert(t.Context(), items); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := v.Delete(t.Context(), ids[:3]); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	hits, err := v.Search(t.Context(), randVec(999), n, kernel.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != n-3 {
		t.Errorf("hits=%d, want %d (3 deleted)", len(hits), n-3)
	}
	for _, h := range hits {
		for _, gone := range ids[:3] {
			if h.ID == gone {
				t.Errorf("deleted ID %q still appears in search", gone)
			}
		}
	}
}

func TestVector_DeleteMissingIDIsNoop(t *testing.T) {
	_, v := openVector(t)
	if err := v.Delete(t.Context(), []string{"nope"}); err != nil {
		t.Fatalf("Delete missing id should be a no-op, got %v", err)
	}
}

func TestVector_UpsertRejectsEmptyID(t *testing.T) {
	_, v := openVector(t)
	err := v.Upsert(t.Context(), []kernel.VectorItem{{ID: "", Embedding: randVec(1)}})
	if err == nil {
		t.Fatal("Upsert accepted empty id")
	}
	if !strings.Contains(err.Error(), "empty id") {
		t.Errorf("error = %v, want substring %q", err, "empty id")
	}
}

func TestVector_DeleteLargeBatchChunked(t *testing.T) {
	// Exercise the chunked-Delete path: more than SQLite's 999-bind limit
	// must round-trip cleanly across two domains in one transaction. We
	// upsert ~1.5K items, delete them all, and confirm the index is empty.
	const n = 1500
	_, v := openVector(t)

	items := make([]kernel.VectorItem, 0, n)
	ids := make([]string, 0, n)
	for i := range n {
		id := "ep-large-" + strconv.Itoa(i)
		ids = append(ids, id)
		items = append(items, kernel.VectorItem{ID: id, Embedding: randVec(uint64(2000 + i))})
	}
	if err := v.Upsert(t.Context(), items); err != nil {
		t.Fatalf("Upsert seed: %v", err)
	}
	if err := v.Delete(t.Context(), ids); err != nil {
		t.Fatalf("Delete large batch: %v", err)
	}
	hits, err := v.Search(t.Context(), randVec(9999), 10, kernel.VectorFilter{})
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("post-delete hits = %d, want 0", len(hits))
	}
}

func TestVector_UpsertRejectsWrongDim(t *testing.T) {
	_, v := openVector(t)

	bad := make([]float32, storage.VectorDim-1)
	err := v.Upsert(t.Context(), []kernel.VectorItem{{ID: "bad", Embedding: bad}})
	if err == nil {
		t.Fatal("Upsert accepted wrong-dim embedding")
	}
	wantSub := "refusing to mix dimensions"
	if !strings.Contains(err.Error(), wantSub) {
		t.Errorf("error = %v, want substring %q", err, wantSub)
	}
}

func TestVector_SearchRejectsWrongDim(t *testing.T) {
	_, v := openVector(t)
	bad := make([]float32, storage.VectorDim+1)
	_, err := v.Search(t.Context(), bad, 1, kernel.VectorFilter{})
	if err == nil {
		t.Fatal("Search accepted wrong-dim query")
	}
}

func TestVector_SearchRejectsHugeK(t *testing.T) {
	_, v := openVector(t)
	_, err := v.Search(t.Context(), randVec(1), 1_000_000, kernel.VectorFilter{})
	if err == nil {
		t.Fatal("Search accepted k above ceiling")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("error = %v, want substring %q", err, "ceiling")
	}
}

func TestVector_TwoDomainsAreIndependent(t *testing.T) {
	_, v := openVector(t)

	episode := kernel.VectorItem{
		ID:        "shared-id",
		Embedding: randVec(11),
		Metadata:  map[string]any{"domain": sqlite.DomainEpisodes},
	}
	entity := kernel.VectorItem{
		ID:        "shared-id",
		Embedding: randVec(22),
		Metadata:  map[string]any{"domain": sqlite.DomainEntities},
	}
	if err := v.Upsert(t.Context(), []kernel.VectorItem{episode, entity}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	epHits, err := v.SearchByDomain(t.Context(), sqlite.DomainEpisodes, episode.Embedding, 1)
	if err != nil {
		t.Fatalf("SearchByDomain episodes: %v", err)
	}
	if len(epHits) != 1 || epHits[0].ID != "shared-id" || epHits[0].Score > 0.001 {
		t.Errorf("episodes hit = %+v, want exact match", epHits)
	}

	enHits, err := v.SearchByDomain(t.Context(), sqlite.DomainEntities, entity.Embedding, 1)
	if err != nil {
		t.Fatalf("SearchByDomain entities: %v", err)
	}
	if len(enHits) != 1 || enHits[0].ID != "shared-id" || enHits[0].Score > 0.001 {
		t.Errorf("entities hit = %+v, want exact match", enHits)
	}

	crossHits, err := v.SearchByDomain(t.Context(), sqlite.DomainEntities, episode.Embedding, 1)
	if err != nil {
		t.Fatalf("SearchByDomain cross: %v", err)
	}
	if len(crossHits) == 1 && crossHits[0].Score < 0.001 {
		t.Errorf("entities table returned exact match for the episodes embedding; domains are leaking")
	}
}

func TestVector_SearchRejectsUnknownDomain(t *testing.T) {
	_, v := openVector(t)
	_, err := v.SearchByDomain(t.Context(), "bogus", randVec(1), 1)
	if err == nil {
		t.Fatal("SearchByDomain accepted unknown domain")
	}
}

func TestVector_UpsertRejectsUnknownDomain(t *testing.T) {
	_, v := openVector(t)
	item := kernel.VectorItem{
		ID:        "x",
		Embedding: randVec(1),
		Metadata:  map[string]any{"domain": "bogus"},
	}
	if err := v.Upsert(t.Context(), []kernel.VectorItem{item}); err == nil {
		t.Fatal("Upsert accepted unknown domain")
	}
}

func TestVector_SearchKindsFilterIsNotImplemented(t *testing.T) {
	_, v := openVector(t)
	filter := kernel.VectorFilter{Kinds: []kernel.EventKind{"remember"}}
	_, err := v.Search(t.Context(), randVec(1), 1, filter)
	if !errors.Is(err, kernel.ErrNotImplemented) {
		t.Errorf("Search with Kinds filter err = %v, want kernel.ErrNotImplemented", err)
	}
}

// TestVector_SearchTimeWindowFiltersOutOfBandRows exercises the time-bound
// push-down: episodes are inserted across a year, then Search with a tight
// MinTime/MaxTime window returns only the rows whose create_time falls in
// that window. The vec0 top-k is computed within the time-allowed candidate
// set so no in-window match can be crowded out by an out-of-window neighbour.
func TestVector_SearchTimeWindowFiltersOutOfBandRows(t *testing.T) {
	store, v := openVector(t)

	// Three rows: one in 2024, one in 2025, one in 2026. Identical embeddings
	// so the vec0 distance is meaningless — only the time predicate decides
	// the survivor set.
	emb := randVec(42)
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
			r.uid, r.ts, "remember", `{}`, "any", "src",
		); err != nil {
			t.Fatalf("seed episodes row %q: %v", r.uid, err)
		}
	}
	items := make([]kernel.VectorItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, kernel.VectorItem{ID: r.uid, Embedding: emb})
	}
	if err := v.Upsert(t.Context(), items); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	// Window covers only 2025: MinTime inclusive, MaxTime exclusive.
	minT := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	maxT := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hits, err := v.Search(t.Context(), emb, 10, kernel.VectorFilter{MinTime: minT, MaxTime: maxT})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "ep-2025" {
		t.Fatalf("hits = %+v, want exactly ep-2025", hits)
	}

	// Open-ended lower bound: everything from 2025 forward.
	hits, err = v.Search(t.Context(), emb, 10, kernel.VectorFilter{MinTime: minT})
	if err != nil {
		t.Fatalf("Search lower-only: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got["ep-2025"] || !got["ep-2026"] || got["ep-2024"] {
		t.Errorf("lower-only window hits = %v, want {ep-2025, ep-2026}", got)
	}

	// Open-ended upper bound: everything strictly before 2026.
	hits, err = v.Search(t.Context(), emb, 10, kernel.VectorFilter{MaxTime: maxT})
	if err != nil {
		t.Fatalf("Search upper-only: %v", err)
	}
	got = map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got["ep-2024"] || !got["ep-2025"] || got["ep-2026"] {
		t.Errorf("upper-only window hits = %v, want {ep-2024, ep-2025}", got)
	}
}

// TestVector_SearchTimeWindowSubsecondPrecision guards against the
// julianday() rounding bug: two timestamps that differ only by a small
// number of nanoseconds must not be conflated by the time predicate.
// We seed two rows in the same second separated by 100 microseconds and
// place an exclusive Until bound between them, then assert the later
// row is dropped while the earlier survives.
func TestVector_SearchTimeWindowSubsecondPrecision(t *testing.T) {
	store, v := openVector(t)

	emb := randVec(42)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []struct {
		uid string
		t   time.Time
	}{
		// 100 microseconds apart, same wall second.
		{"ep-early", base.Add(100 * time.Microsecond)},
		{"ep-late", base.Add(400 * time.Microsecond)},
	}
	for _, r := range rows {
		if _, err := store.Writer().ExecContext(t.Context(),
			`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
             VALUES (?, ?, ?, ?, ?, ?)`,
			r.uid, r.t.UTC().Format(time.RFC3339Nano), "remember", `{}`, "any", "src",
		); err != nil {
			t.Fatalf("seed %q: %v", r.uid, err)
		}
	}
	items := make([]kernel.VectorItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, kernel.VectorItem{ID: r.uid, Embedding: emb})
	}
	if err := v.Upsert(t.Context(), items); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	// Exclusive Until at +250us: ep-early (+100us) is in, ep-late (+400us) is out.
	until := base.Add(250 * time.Microsecond)
	hits, err := v.Search(t.Context(), emb, 10, kernel.VectorFilter{MaxTime: until})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got["ep-early"] || got["ep-late"] {
		t.Errorf("sub-second window hits = %v, want only ep-early (julianday rounding bug?)", got)
	}
}

// TestVector_SearchTimeWindowSecondBoundary guards against the
// strftime('%s', col) rounding bug: SQLite rounds a fractional-second
// timestamp like .999999999Z up to the next whole second BEFORE the
// helper adds the fractional nanos back, double-counting that second
// and shoving the row out of any window that ends on the next second.
// We seed a row at exactly .999999999Z and bound the window exclusively
// at the next whole second , the row must survive.
func TestVector_SearchTimeWindowSecondBoundary(t *testing.T) {
	store, v := openVector(t)

	emb := randVec(42)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Row at base + 999999999 nanos (one nanosecond before base+1s).
	rowTime := base.Add(time.Second - 1)
	if _, err := store.Writer().ExecContext(t.Context(),
		`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
         VALUES (?, ?, ?, ?, ?, ?)`,
		"ep-edge", rowTime.UTC().Format(time.RFC3339Nano), "remember", `{}`, "any", "src",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := v.Upsert(t.Context(), []kernel.VectorItem{{ID: "ep-edge", Embedding: emb}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Exclusive Until at base+1s: ep-edge at base+999999999ns must survive.
	until := base.Add(time.Second)
	hits, err := v.Search(t.Context(), emb, 10, kernel.VectorFilter{MaxTime: until})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "ep-edge" {
		t.Errorf("hits = %+v, want ep-edge to survive exclusive Until at +1s (strftime rounding bug?)", hits)
	}
}

func TestVector_SearchEmptyDBReturnsNoHits(t *testing.T) {
	_, v := openVector(t)
	hits, err := v.Search(t.Context(), randVec(1), 3, kernel.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits=%d, want 0 on empty index", len(hits))
	}
}

func TestVector_ConcurrentReadersDuringWrite(t *testing.T) {
	_, v := openVector(t)

	// Seed the index so the readers have something to traverse.
	seed := make([]kernel.VectorItem, 0, 16)
	for i := range 16 {
		seed = append(seed, kernel.VectorItem{
			ID:        "ep-seed-" + strconv.Itoa(i),
			Embedding: randVec(uint64(1000 + i)),
		})
	}
	if err := v.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	var wg sync.WaitGroup
	readerErrs := make(chan error, 8)
	for r := range 8 {
		wg.Add(1)
		go func(rid int) {
			defer wg.Done()
			for j := range 4 {
				_, err := v.Search(t.Context(), randVec(uint64(rid*100+j)), 3, kernel.VectorFilter{})
				if err != nil {
					readerErrs <- err
					return
				}
			}
		}(r)
	}

	writerErr := make(chan error, 1)
	go func() {
		batch := make([]kernel.VectorItem, 0, 16)
		for k := range 16 {
			batch = append(batch, kernel.VectorItem{
				ID:        "ep-write-" + strconv.Itoa(k),
				Embedding: randVec(uint64(5000 + k)),
			})
		}
		writerErr <- v.Upsert(t.Context(), batch)
	}()

	wg.Wait()
	close(readerErrs)
	for err := range readerErrs {
		t.Errorf("reader error: %v", err)
	}
	if err := <-writerErr; err != nil {
		t.Errorf("writer error: %v", err)
	}
}

func TestVector_CloseIsIdempotent(t *testing.T) {
	_, v := openVector(t)
	if err := v.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestVector_KNN_ScopeIsolation verifies that the AND scope IN (...) predicate
// in every KNN CTE prevents cross-scope leakage. The test seeds personal and
// team vectors into the episodes domain (identical embeddings so distance alone
// cannot explain exclusion), then asserts both directions of isolation:
//
//   - scopes=[team]     → ZERO personal hits
//   - scopes=[personal] → ZERO team hits
//
// This is defense-in-depth; the authoritative boundary is the relational guard
// in scoped_reader.go. These assertions catch any regression where the scope
// predicate is accidentally dropped from a KNN CTE.
func TestVector_KNN_ScopeIsolation(t *testing.T) {
	_, v := openVector(t)

	// All items share the same embedding vector so vec0 distance is identical
	// for every candidate. Without the scope predicate, any top-k query would
	// return both personal and team items; with the predicate only the requested
	// scope's items appear.
	sharedEmb := randVec(77)

	const nPerScope = 8

	personalItems := make([]kernel.VectorItem, 0, nPerScope)
	for i := range nPerScope {
		personalItems = append(personalItems, kernel.VectorItem{
			ID:        "scope-personal-" + strconv.Itoa(i),
			Embedding: sharedEmb,
			// No explicit scope → defaults to kernel.DefaultScope ("personal").
		})
	}
	teamItems := make([]kernel.VectorItem, 0, nPerScope)
	for i := range nPerScope {
		teamItems = append(teamItems, kernel.VectorItem{
			ID:        "scope-team-" + strconv.Itoa(i),
			Embedding: sharedEmb,
			Metadata:  map[string]any{"scope": "team"},
		})
	}

	if err := v.Upsert(t.Context(), personalItems); err != nil {
		t.Fatalf("Upsert personal: %v", err)
	}
	if err := v.Upsert(t.Context(), teamItems); err != nil {
		t.Fatalf("Upsert team: %v", err)
	}

	teamScopes := kernel.NewScopeSet(kernel.ScopeTeam)
	personalScopes := kernel.NewScopeSet(kernel.ScopePersonal)

	t.Run("team scope returns only team items", func(t *testing.T) {
		hits, err := v.Search(t.Context(), sharedEmb, nPerScope*2, kernel.VectorFilter{
			Scopes: teamScopes,
		})
		if err != nil {
			t.Fatalf("Search team: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("expected team hits, got zero")
		}
		for _, h := range hits {
			if !strings.HasPrefix(h.ID, "scope-team-") {
				t.Errorf("team-scoped search returned personal item %q", h.ID)
			}
		}
		if len(hits) > nPerScope {
			t.Errorf("team search returned %d hits, want at most %d", len(hits), nPerScope)
		}
	})

	t.Run("personal scope returns only personal items", func(t *testing.T) {
		hits, err := v.Search(t.Context(), sharedEmb, nPerScope*2, kernel.VectorFilter{
			Scopes: personalScopes,
		})
		if err != nil {
			t.Fatalf("Search personal: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("expected personal hits, got zero")
		}
		for _, h := range hits {
			if !strings.HasPrefix(h.ID, "scope-personal-") {
				t.Errorf("personal-scoped search returned team item %q", h.ID)
			}
		}
		if len(hits) > nPerScope {
			t.Errorf("personal search returned %d hits, want at most %d", len(hits), nPerScope)
		}
	})

	t.Run("empty scope set is all pass", func(t *testing.T) {
		hits, err := v.Search(t.Context(), sharedEmb, nPerScope*2, kernel.VectorFilter{})
		if err != nil {
			t.Fatalf("Search all-pass: %v", err)
		}
		// With both scopes seeded and all-pass, we expect 2*nPerScope items back.
		if len(hits) < nPerScope*2 {
			t.Errorf("all-pass search returned %d hits, want at least %d", len(hits), nPerScope*2)
		}
	})
}
