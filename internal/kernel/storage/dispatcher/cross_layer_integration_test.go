//go:build integration

package dispatcher_test

// cross_layer_integration_test.go covers three scenarios that are distinct
// from the existing dispatcher_integration_test.go:
//
//  1. Apply 200 events in one call; UID sets must be equal across all four layers.
//  2. 4 concurrent writers + 2 concurrent readers; final counts must be 200.
//  3. Partial apply failure (inner graph closed) followed by reopen + re-apply;
//     no orphan rows, no duplicates.
//
// Acceptance criterion for issue #33.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestCrossLayer_Apply200_UIDAgreementAcrossAllFourLayers seeds 200 Episode
// events in a single Apply call and asserts that the UID set returned by
// every layer is identical.  A 5-UID spot-check via vec.Search and a 10-token
// spot-check via fts.Search confirm that the layers are not only populated
// but queryable.
func TestCrossLayer_Apply200_UIDAgreementAcrossAllFourLayers(t *testing.T) {
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const n = 200
	events := makeIndexedEvents(t, "CL2000", n, 0)
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// -- UID-set agreement ---------------------------------------------------

	wantUIDs := uidSliceFrom(events)

	sqliteUIDs := uidSetFromSQLite(t, ctx, d)
	assertUIDSetEqual(t, "sqlite", wantUIDs, sqliteUIDs)

	graphUIDs := uidSetFromGraph(t, ctx, d)
	assertUIDSetEqual(t, "ladybug", wantUIDs, graphUIDs)

	vecUIDs := uidSetFromVec(t, ctx, d)
	assertUIDSetEqual(t, "sqlite-vec", wantUIDs, vecUIDs)

	// FTS: check every unique token -- 200 events, each with a distinct
	// XUID<n> token, so we can confirm full coverage at this scale.
	allIndices := make([]int, n)
	for i := range allIndices {
		allIndices[i] = i
	}
	checkFTSTokens(t, ctx, d, events, allIndices)

	// -- vec.Search spot-check for 5 UIDs ------------------------------------
	vec, err := d.OpenVector(ctx)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	emb := &fakeEmbedder{dim: storage.VectorDim}
	for _, idx := range spotIndices(n, 5) {
		q, err := emb.Embed(ctx, eventText(events[idx]))
		if err != nil {
			t.Fatalf("embed query idx=%d: %v", idx, err)
		}
		hits, err := vec.Search(ctx, q, 1, kernel.VectorFilter{})
		if err != nil {
			t.Fatalf("vec.Search idx=%d: %v", idx, err)
		}
		if len(hits) != 1 {
			t.Fatalf("vec.Search idx=%d: got %d hits, want 1", idx, len(hits))
		}
		if hits[0].ID != events[idx].UID {
			t.Errorf("vec top hit idx=%d = %q, want %q", idx, hits[0].ID, events[idx].UID)
		}
	}
}

// TestCrossLayer_ConcurrentApply4Writers2Readers_NoLostEvents runs 4
// goroutines each writing 50 Episode events (200 distinct UIDs total) while
// 2 reader goroutines query all four layers throughout.  After all writers
// finish, the test asserts that every layer holds exactly 200 events.
//
// Run with -race (enforced by mise run test:integration) to catch any
// data races introduced by the concurrent writes.
func TestCrossLayer_ConcurrentApply4Writers2Readers_NoLostEvents(t *testing.T) {
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		writers         = 4
		eventsPerWriter = 50
		total           = writers * eventsPerWriter
	)

	// Build writer event slices with globally-unique UIDs per writer.
	writerEvents := make([][]kernel.Event, writers)
	for w := range writers {
		writerEvents[w] = makeIndexedEvents(t, fmt.Sprintf("CW%04d", w), eventsPerWriter, w*eventsPerWriter)
	}

	// Start gate: all goroutines start writing at the same instant.
	start := make(chan struct{})
	var wg sync.WaitGroup

	// Writers.
	writeErrs := make([]error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			writeErrs[w] = d.Apply(ctx, writerEvents[w])
		}()
	}

	// Readers: fire SELECT count queries against SQLite and LadybugDB while
	// writers are in flight. SQLite is in WAL mode so readers never block
	// writers. Unexpected errors (anything other than ErrClosed, which can
	// race on the tail of the test) are captured atomically and surfaced
	// after the writers finish.
	stopReaders := make(chan struct{})
	var readerErrCount atomic.Int64
	var readerWG sync.WaitGroup
	for range 2 {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				db, err := d.OpenSQLite(ctx)
				if err != nil {
					if !errors.Is(err, dispatcher.ErrClosed) {
						readerErrCount.Add(1)
					}
					return
				}
				var c int
				if scanErr := db.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&c); scanErr != nil {
					readerErrCount.Add(1)
					return
				}

				g, err := d.OpenGraph(ctx)
				if err != nil {
					if !errors.Is(err, dispatcher.ErrClosed) {
						readerErrCount.Add(1)
					}
					return
				}
				r, err := g.Execute(ctx, "MATCH (e:Episode) RETURN count(e)", nil)
				if err != nil {
					readerErrCount.Add(1)
					return
				}
				var gc int64
				if r.Next() {
					if scanErr := r.Scan(&gc); scanErr != nil {
						readerErrCount.Add(1)
						_ = r.Close()
						return
					}
				} else if iterErr := r.Err(); iterErr != nil {
					readerErrCount.Add(1)
					_ = r.Close()
					return
				}
				if closeErr := r.Close(); closeErr != nil {
					readerErrCount.Add(1)
					return
				}

				// Yield so writers make progress.
				time.Sleep(time.Millisecond)
			}
		}()
	}

	close(start)
	wg.Wait()
	// Signal readers to stop and wait for them to drain.
	close(stopReaders)
	readerWG.Wait()

	if n := readerErrCount.Load(); n > 0 {
		t.Errorf("readers encountered %d unexpected error(s) during concurrent writes", n)
	}

	// Check no writer returned an error.
	for w, err := range writeErrs {
		if err != nil {
			t.Fatalf("writer %d: %v", w, err)
		}
	}

	// -- Final counts across all four layers ---------------------------------

	assertEpisodeCount(t, ctx, d, total)

	assertEpisodeNodeCount(t, ctx, d, total)

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	// Count only rows where both the sidecar mapping and the vec0 slot
	// exist -- same join HasEvent uses; sidecar-only would hide a missing
	// embedding row.
	var vecCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM   episodes_vec_ids AS ids
		JOIN   episodes_vec     AS v ON v.rowid = ids.rowid
	`).Scan(&vecCount); err != nil {
		t.Fatalf("count episodes_vec join: %v", err)
	}
	if vecCount != total {
		t.Errorf("episodes_vec (joined) count = %d, want %d", vecCount, total)
	}

	// FTS spot-check: pick 10 unique tokens spread across all writers.
	allEvents := make([]kernel.Event, 0, total)
	for _, evs := range writerEvents {
		allEvents = append(allEvents, evs...)
	}
	spot := spotIndices(total, 10)
	checkFTSTokens(t, ctx, d, allEvents, spot)
}

// TestCrossLayer_PartialApplyFailure_NoOrphanAcrossLayers seeds one event A,
// then closes the inner graph to provoke a failure on event B.  After
// reopening a second dispatcher on the same DataDir and applying event B
// successfully, every layer must contain exactly the two UIDs A and B with
// no duplicates.
func TestCrossLayer_PartialApplyFailure_NoOrphanAcrossLayers(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	// Open first dispatcher and write event A.
	d1, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open (d1): %v", err)
	}

	evA := makeEpisodeEventAt(t, "01HCRSLYRA0000000000000000", "event alpha carries XUIDALPHA", "cli:test", time.Unix(1_700_000_001, 0).UTC())
	if err := d1.Apply(ctx, []kernel.Event{evA}); err != nil {
		t.Fatalf("Apply evA: %v", err)
	}

	// Close the inner graph so the next MERGE fails; event B must roll back.
	if err := dispatcher.CloseInnerGraphForTest(d1); err != nil {
		t.Fatalf("CloseInnerGraphForTest: %v", err)
	}
	evB := makeEpisodeEventAt(t, "01HCRSLYRA0000000000000001", "event beta carries XUIDBETA", "cli:test", time.Unix(1_700_000_002, 0).UTC())
	if err := d1.Apply(ctx, []kernel.Event{evB}); err == nil {
		t.Fatal("Apply evB with closed graph: expected error, got nil")
	}

	// Assert rollback: evB must be absent from every SQLite-resident layer
	// before we reopen. The graph is already closed, so we check the three
	// SQLite layers only. This is the only window where residue is visible:
	// after reopen + successful re-Apply, idempotent upserts would overwrite
	// any leaked row and mask the bug.
	d1db, err := d1.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite (pre-reopen check): %v", err)
	}
	// episodes uses the "uid" key column.
	var episodeResidue int
	if err := d1db.QueryRowContext(ctx,
		`SELECT count(*) FROM episodes WHERE uid = ?`, evB.UID,
	).Scan(&episodeResidue); err != nil {
		t.Fatalf("pre-reopen count episodes for evB: %v", err)
	}
	if episodeResidue != 0 {
		t.Errorf("rollback left %d row(s) for evB in episodes before reopen, want 0", episodeResidue)
	}
	// episodes_vec_ids uses "id" (not "uid") as its natural key.
	var vecResidue int
	if err := d1db.QueryRowContext(ctx,
		`SELECT count(*) FROM episodes_vec_ids WHERE id = ?`, evB.UID,
	).Scan(&vecResidue); err != nil {
		t.Fatalf("pre-reopen count episodes_vec_ids for evB: %v", err)
	}
	if vecResidue != 0 {
		t.Errorf("rollback left %d sidecar row(s) for evB before reopen, want 0", vecResidue)
	}
	// FTS5 (episodes_fts) is a separate SQLite-resident layer backed by
	// the episodes content table via triggers. The SQLite tx rolled back,
	// so the INSERT trigger never fired and no FTS entry should exist for
	// XUIDBETA.
	//
	// We query episodes_fts directly rather than going through
	// FTSStore.Search: that method JOINs episodes on rowid before
	// returning hits, so an orphan FTS row whose episodes row rolled back
	// would be silently filtered out -- exactly the residue this check
	// is meant to catch.
	var ftsResidue int
	if err := d1db.QueryRowContext(ctx,
		`SELECT count(*) FROM episodes_fts WHERE episodes_fts MATCH ?`, "XUIDBETA",
	).Scan(&ftsResidue); err != nil {
		t.Fatalf("pre-reopen direct FTS count XUIDBETA: %v", err)
	}
	if ftsResidue != 0 {
		t.Errorf("rollback left %d FTS row(s) for XUIDBETA in episodes_fts before reopen, want 0", ftsResidue)
	}

	// Close d1 (its SQLite pools are still healthy; only the graph broke).
	if err := d1.Close(); err != nil {
		// It's acceptable for Close to surface the already-closed graph error.
		_ = err
	}

	// Reopen a second dispatcher on the same DataDir.
	d2, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open (d2): %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })

	// Apply event B successfully on d2.
	if err := d2.Apply(ctx, []kernel.Event{evB}); err != nil {
		t.Fatalf("Apply evB on d2: %v", err)
	}

	// -- Assert exactly 2 events in every layer, no duplicates ---------------

	assertEpisodeCount(t, ctx, d2, 2)
	assertEpisodeNodeCount(t, ctx, d2, 2)
	assertVectorCount(t, ctx, d2, 2)

	// FTS: both unique tokens must be retrievable.
	fts, err := d2.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	for _, tc := range []struct{ token, wantUID string }{
		{"XUIDALPHA", evA.UID},
		{"XUIDBETA", evB.UID},
	} {
		hits, err := fts.Search(ctx, tc.token, 5)
		if err != nil {
			t.Fatalf("fts.Search %q: %v", tc.token, err)
		}
		if len(hits) != 1 {
			t.Fatalf("fts.Search %q: got %d hits, want 1", tc.token, len(hits))
		}
		if hits[0].ID != tc.wantUID {
			t.Errorf("fts.Search %q: got UID %q, want %q", tc.token, hits[0].ID, tc.wantUID)
		}
	}
}

// -- helpers -----------------------------------------------------------------

// makeIndexedEvents builds n Episode events with UIDs derived from prefix and
// a monotone index starting at offset.  Each event text embeds a unique token
// XUID<n> so the FTS layer can pin results to a specific event.
//
// UID format: prefix (6 chars) + %020d (20 digits) -> 26 chars total, matching
// the ULID alphabet length used throughout the test suite.
func makeIndexedEvents(t *testing.T, prefix string, n, offset int) []kernel.Event {
	t.Helper()
	evs := make([]kernel.Event, n)
	for i := range n {
		idx := offset + i
		uid := fmt.Sprintf("%s%020d", prefix, idx)
		if len(uid) != 26 {
			t.Fatalf("makeIndexedEvents: UID %q has length %d, want 26", uid, len(uid))
		}
		text := fmt.Sprintf("cross-layer probe %d token XUID%06d", idx, idx)
		evs[i] = makeEpisodeEventAt(t, uid, text, "cli:cross-layer",
			time.Unix(int64(1_700_000_000+idx), 0).UTC())
	}
	return evs
}

// uidSliceFrom extracts the UID field from every event into a slice.
func uidSliceFrom(events []kernel.Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.UID
	}
	return out
}

// uidSetFromSQLite returns the set of UIDs stored in the episodes table.
func uidSetFromSQLite(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher) map[string]struct{} {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("uidSetFromSQLite: OpenSQLite: %v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT uid FROM episodes`)
	if err != nil {
		t.Fatalf("uidSetFromSQLite: query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			t.Fatalf("uidSetFromSQLite: scan: %v", err)
		}
		out[uid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("uidSetFromSQLite: rows.Err: %v", err)
	}
	return out
}

// uidSetFromGraph returns the set of UIDs stored as Episode nodes in LadybugDB.
func uidSetFromGraph(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher) map[string]struct{} {
	t.Helper()
	g, err := d.OpenGraph(ctx)
	if err != nil {
		t.Fatalf("uidSetFromGraph: OpenGraph: %v", err)
	}
	r, err := g.Execute(ctx, "MATCH (e:Episode) RETURN e.uid", nil)
	if err != nil {
		t.Fatalf("uidSetFromGraph: Execute: %v", err)
	}
	defer func() { _ = r.Close() }()
	out := make(map[string]struct{})
	for r.Next() {
		var uid string
		if err := r.Scan(&uid); err != nil {
			t.Fatalf("uidSetFromGraph: Scan: %v", err)
		}
		out[uid] = struct{}{}
	}
	if err := r.Err(); err != nil {
		t.Fatalf("uidSetFromGraph: Err: %v", err)
	}
	return out
}

// uidSetFromVec returns the set of IDs for which BOTH the sidecar row in
// episodes_vec_ids AND the actual embedding row in episodes_vec exist.
// Checking only the sidecar would miss a write path that inserts the mapping
// but fails to write the vec0 slot -- the same join that HasEvent uses.
func uidSetFromVec(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher) map[string]struct{} {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("uidSetFromVec: OpenSQLite: %v", err)
	}
	const q = `
		SELECT ids.id
		FROM   episodes_vec_ids AS ids
		JOIN   episodes_vec     AS v ON v.rowid = ids.rowid
	`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("uidSetFromVec: query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("uidSetFromVec: scan: %v", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("uidSetFromVec: rows.Err: %v", err)
	}
	return out
}

// assertUIDSetEqual fails the test if the layer's UID set does not exactly
// match the expected slice.  layer is a label for error messages.
func assertUIDSetEqual(t *testing.T, layer string, want []string, got map[string]struct{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: UID count = %d, want %d", layer, len(got), len(want))
		return
	}
	for _, uid := range want {
		if _, ok := got[uid]; !ok {
			t.Errorf("%s: UID %q missing", layer, uid)
		}
	}
}

// spotIndices picks n distinct indices in [0, total) using a deterministic
// source so tests are reproducible across runs.
func spotIndices(total, n int) []int {
	if n > total {
		n = total
	}
	// Fisher-Yates partial shuffle over a seeded source.
	rng := rand.New(rand.NewPCG(0xdeadbeef, 0xcafebabe))
	indices := make([]int, total)
	for i := range indices {
		indices[i] = i
	}
	for i := range n {
		j := i + rng.IntN(total-i)
		indices[i], indices[j] = indices[j], indices[i]
	}
	return indices[:n]
}

// checkFTSTokens verifies that the FTS5 layer returns exactly one hit for each
// of the spot indices, and that the returned UID matches the event at that
// index.  Token format mirrors makeIndexedEvents: "XUID%06d".
func checkFTSTokens(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, events []kernel.Event, indices []int) {
	t.Helper()
	fts, err := d.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("checkFTSTokens: OpenFTS: %v", err)
	}
	// Derive the absolute index from the event's text to reconstruct the token.
	for _, idx := range indices {
		ev := events[idx]
		// Token format from makeIndexedEvents: XUID followed by the
		// absolute (offset-aware) index baked into the text field.
		// Extract the offset index: parse from the event UID suffix isn't
		// robust across prefix lengths; instead extract from the text.
		// makeIndexedEvents writes "token XUID%06d" so we can recover it.
		var absIdx int
		if _, err := fmt.Sscanf(eventText(ev), "cross-layer probe %d", &absIdx); err != nil {
			t.Fatalf("checkFTSTokens: parse absIdx from %q: %v", eventText(ev), err)
		}
		token := fmt.Sprintf("XUID%06d", absIdx)
		hits, err := fts.Search(ctx, token, 5)
		if err != nil {
			t.Fatalf("checkFTSTokens: fts.Search %q: %v", token, err)
		}
		if len(hits) != 1 {
			t.Fatalf("checkFTSTokens: fts.Search %q: got %d hits, want 1", token, len(hits))
		}
		if hits[0].ID != ev.UID {
			t.Errorf("checkFTSTokens: fts.Search %q: got UID %q, want %q", token, hits[0].ID, ev.UID)
		}
	}
}
