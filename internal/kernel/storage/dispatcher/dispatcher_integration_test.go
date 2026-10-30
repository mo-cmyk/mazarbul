//go:build integration

package dispatcher_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestDispatcherApply20 exercises the full four-layer write path with a
// real SQLite + sqlite-vec + FTS5 + LadybugDB. Acceptance criterion for
// issue #32: Apply 20 Episode events, assert all four layers are coherent.
//
// Each event carries a unique-token text so the FTS5 assertion can pin
// the round-trip to the exact uid. The fake embedder produces
// deterministic per-uid vectors so the vector layer's KNN search is
// reproducible across runs.
func TestDispatcherApply20(t *testing.T) {
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const n = 20
	events := make([]kernel.Event, n)
	for i := range n {
		uid := fmt.Sprintf("01HZZZZZZZZZZZZZZZZZZZZ%03d", i)
		text := fmt.Sprintf("episode body number %d carrying unique token UNIQ%03d", i, i)
		events[i] = makeEpisodeEventAt(t, uid, text, "cli:test", time.Unix(int64(1_700_000_000+i), 0).UTC())
	}

	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	t.Run("sqlite has all 20 rows", func(t *testing.T) {
		assertEpisodeCount(t, ctx, d, n)
	})

	t.Run("sqlite text and source columns populated", func(t *testing.T) {
		db, err := d.OpenSQLite(ctx)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		var emptyText, emptySource int
		if err := db.QueryRowContext(ctx,
			`SELECT
				sum(CASE WHEN text   = '' THEN 1 ELSE 0 END),
				sum(CASE WHEN source = '' THEN 1 ELSE 0 END)
			FROM episodes`,
		).Scan(&emptyText, &emptySource); err != nil {
			t.Fatalf("scan empties: %v", err)
		}
		if emptyText != 0 || emptySource != 0 {
			t.Errorf("empty cells: text=%d source=%d, want 0/0", emptyText, emptySource)
		}
	})

	t.Run("vector has 20 sidecar rows", func(t *testing.T) {
		assertVectorCount(t, ctx, d, n)
	})

	t.Run("vector knn finds the right uid", func(t *testing.T) {
		// The fakeEmbedder seeds vectors by string length + index. Re-embed
		// the text of event index 7 to get a query vector that should rank
		// event 7 best (distance 0).
		emb := &fakeEmbedder{dim: storage.VectorDim}
		q, err := emb.Embed(ctx, eventText(events[7]))
		if err != nil {
			t.Fatalf("embed query: %v", err)
		}
		vec, err := d.OpenVector(ctx)
		if err != nil {
			t.Fatalf("OpenVector: %v", err)
		}
		hits, err := vec.Search(ctx, q, 1, kernel.VectorFilter{})
		if err != nil {
			t.Fatalf("vector search: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("vector search returned %d hits, want 1", len(hits))
		}
		if hits[0].ID != events[7].UID {
			t.Errorf("vector top hit = %q, want %q", hits[0].ID, events[7].UID)
		}
	})

	t.Run("fts hits the right uid via unique token", func(t *testing.T) {
		fts, err := d.OpenFTS(ctx)
		if err != nil {
			t.Fatalf("OpenFTS: %v", err)
		}
		hits, err := fts.Search(ctx, "UNIQ012", 5)
		if err != nil {
			t.Fatalf("fts search: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("fts hits = %d, want 1", len(hits))
		}
		if hits[0].ID != events[12].UID {
			t.Errorf("fts hit = %q, want %q", hits[0].ID, events[12].UID)
		}
	})

	t.Run("ladybug has 20 Episode nodes", func(t *testing.T) {
		assertEpisodeNodeCount(t, ctx, d, n)
	})

	t.Run("ladybug node properties round-trip", func(t *testing.T) {
		g, err := d.OpenGraph(ctx)
		if err != nil {
			t.Fatalf("OpenGraph: %v", err)
		}
		r, err := g.Execute(ctx,
			`MATCH (e:Episode {uid: $uid}) RETURN e.kind, e.source, e.confidence`,
			map[string]any{"uid": events[3].UID},
		)
		if err != nil {
			t.Fatalf("match episode: %v", err)
		}
		defer func() { _ = r.Close() }()
		if !r.Next() {
			t.Fatal("no Episode node returned for uid")
		}
		var (
			kind   string
			source string
			conf   float64
		)
		if err := r.Scan(&kind, &source, &conf); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if kind != string(ontology.KindEpisode) {
			t.Errorf("kind = %q, want %q", kind, ontology.KindEpisode)
		}
		if source != "cli:test" {
			t.Errorf("source = %q, want cli:test", source)
		}
		if conf != 0.9 {
			t.Errorf("confidence = %v, want 0.9", conf)
		}
	})
}

// TestDispatcherApply_LadybugFailureRollsBackSQLite covers the
// cross-store rollback path called out in the acceptance criteria:
// when LadybugDB fails, the SQLite tx must roll back so no orphan
// episodes row, vector slot, or FTS entry is left behind.
//
// The failure is provoked via the test-only InjectGraphClose helper
// (white-box, dispatcher_internal_test.go) which closes the inner
// LadybugDB connection so the next MERGE returns ErrClosed.
func TestDispatcherApply_LadybugFailureRollsBackSQLite(t *testing.T) {
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	if err := dispatcher.CloseInnerGraphForTest(d); err != nil {
		t.Fatalf("close inner graph: %v", err)
	}

	ev := makeEpisodeEventAt(t, "01HZBADBADBADBADBADBADBADD", "rollback target", "cli:test", time.Unix(1_700_000_000, 0).UTC())
	if err := d.Apply(ctx, []kernel.Event{ev}); err == nil {
		t.Fatal("Apply: expected error from closed graph, got nil")
	}

	// All three SQLite-resident layers must be empty: episodes, vector
	// sidecar, and FTS5 (which mirrors episodes via triggers).
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	for _, table := range []string{"episodes", "episodes_vec_ids"} {
		var got int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != 0 {
			t.Errorf("%s count = %d after rollback, want 0", table, got)
		}
	}
}

// TestDispatcherApply_Idempotent verifies that replaying the same event
// twice (the Rebuild path) does not duplicate rows or nodes.
func TestDispatcherApply_Idempotent(t *testing.T) {
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	ev := makeEpisodeEventAt(t, "01HIDEMPOTENTIDEMPOTENT001", "idempotent payload", "cli:test", time.Unix(1_700_000_000, 0).UTC())

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 1)
	assertVectorCount(t, ctx, d, 1)
	assertEpisodeNodeCount(t, ctx, d, 1)
}

// makeEpisodeEventAt is the integration-test variant of makeEpisodeEvent
// that lets the caller pin the CreateTime (so vector / FTS / graph
// assertions are reproducible).
func makeEpisodeEventAt(t *testing.T, uid, text, source string, createTime time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"text":       text,
		"source":     source,
		"confidence": 0.9,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		CreateTime: createTime,
		Kind:       kernel.EventKind(ontology.KindEpisode),
		Source:     source,
		Confidence: 0.9,
		Payload:    payload,
	}
}

// eventText extracts the text field from an event's JSON payload so the
// vector-search assertion can re-embed the same string the dispatcher
// stored.
func eventText(ev kernel.Event) string {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Payload, &body); err != nil {
		panic(fmt.Sprintf("eventText: %v", err))
	}
	return body.Text
}

// (verify the Embedder interface is satisfied so the test compiles even
// when no test calls into the interface directly)
var _ dispatcher.Embedder = (*fakeEmbedder)(nil)

// ensure ctx import is used; helper kept for symmetry with sibling tests
var _ = context.Background
