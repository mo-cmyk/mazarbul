//go:build integration

package dispatcher_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// rebuildOntology is a permissive Ontology for the integration test:
// every kind validates as long as the payload is JSON. The real ontology
// is exercised elsewhere; this test pins Rebuild's end-to-end behavior
// against the four-layer dispatcher, not schema enforcement.
type rebuildOntology struct{}

func (rebuildOntology) Schema(_ context.Context, _ string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}

func (rebuildOntology) Register(_ context.Context, _ kernel.Schema) error {
	return kernel.ErrNotImplemented
}

func (rebuildOntology) List(_ context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}

func (rebuildOntology) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}

// TestFileEventLog_Rebuild_FromJSONLEndToEnd seeds 1000 episode events
// through the real four-layer dispatcher, captures a recall baseline,
// drops the derived stores, calls Rebuild on the FileEventLog against a
// fresh dispatcher rooted at the same DataDir, and asserts every layer
// matches the baseline. Second invocation of Rebuild must skip all
// 1000 events via HasEvent.
//
// Acceptance criterion from issue #27: "Integration test: write 1000
// events, drop derived stores, call Rebuild, assert recall returns
// the same hits."
func TestFileEventLog_Rebuild_FromJSONLEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: 1000-event rebuild end-to-end (~60s)")
	}
	ctx := testutil.TestContext(t)

	dataDir := t.TempDir()
	const n = 1000

	// Phase 1: seed 1000 events through Append → dispatcher fanout. The
	// dispatcher and FileEventLog share dataDir; the JSONL log lives at
	// dataDir/events/, the derived stores at dataDir/index.db and
	// dataDir/graph.ladybug.
	dis1, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open (phase 1): %v", err)
	}

	log1 := kernel.NewFileEventLog(dataDir, rebuildOntology{}, kernel.WithStorage(dis1))
	events := make([]kernel.Event, n)
	for i := range events {
		payload, err := json.Marshal(map[string]any{
			"text":       fmt.Sprintf("rebuild event %d carrying unique token TOKEN%04d", i, i),
			"source":     "integration:rebuild",
			"confidence": 0.9,
		})
		if err != nil {
			t.Fatalf("marshal payload %d: %v", i, err)
		}
		events[i] = kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEpisode),
			Source:     "integration:rebuild",
			Confidence: 0.9,
			Payload:    payload,
			// CreateTime is left zero so the writer's clock assigns it;
			// ULIDs are auto-generated. Chronological order via Read
			// matches Append order.
		}
	}

	// Append in batches of 50 so a single failure surfaces close to the
	// offending event, and so the per-event Apply timeout (30s) is not
	// stressed by sub-millisecond timestamp collisions inside one batch.
	const batchSize = 50
	for start := 0; start < n; start += batchSize {
		end := start + batchSize
		if end > n {
			end = n
		}
		if _, err := log1.Append(ctx, events[start:end]); err != nil {
			t.Fatalf("Append batch [%d:%d]: %v", start, end, err)
		}
	}

	// Snapshot the baseline. Row counts are asserted to equal n via the
	// shared assert helpers (dispatcher_test.go); FTS hits are snapshotted
	// for round-trip comparison after the rebuild.
	assertEpisodeCount(t, ctx, dis1, n)
	assertVectorCount(t, ctx, dis1, n)
	assertEpisodeNodeCount(t, ctx, dis1, n)
	baseFTSHits := snapshotFTSHits(t, ctx, dis1, []string{"TOKEN0042", "TOKEN0500", "TOKEN0999"})

	// Phase 2: drop the derived stores. JSONL log stays put.
	if err := dis1.Close(); err != nil {
		t.Fatalf("dispatcher.Close (phase 1): %v", err)
	}
	for _, p := range []string{
		filepath.Join(dataDir, "index.db"),
		filepath.Join(dataDir, "index.db-wal"),
		filepath.Join(dataDir, "index.db-shm"),
		filepath.Join(dataDir, "graph.ladybug"),
	} {
		// Files may or may not exist (WAL/SHM only after writes).
		_ = os.RemoveAll(p)
	}

	// Confirm dataDir/events/ is still there.
	if _, err := os.Stat(filepath.Join(dataDir, "events")); err != nil {
		t.Fatalf("JSONL events dir vanished: %v", err)
	}

	// Phase 3: open a fresh dispatcher rooted at the same DataDir; the
	// derived stores are now empty.
	dis2, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open (phase 3): %v", err)
	}
	t.Cleanup(func() { _ = dis2.Close() })

	assertEpisodeCount(t, ctx, dis2, 0)
	assertVectorCount(t, ctx, dis2, 0)
	assertEpisodeNodeCount(t, ctx, dis2, 0)

	// Phase 4: rebuild from the JSONL log. Capture progress output so
	// the contract for io.Writer streaming is exercised.
	var progress bytes.Buffer
	applyErrs, err := log1.Rebuild(ctx, dis2,
		kernel.WithProgress(&progress),
		kernel.WithProgressEvery(250),
	)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(applyErrs) != 0 {
		t.Fatalf("Rebuild ApplyError slice: got %d entries, want 0; first: %v", len(applyErrs), applyErrs[0])
	}

	// Progress must include the terminal summary mentioning 1000.
	progressOut := progress.String()
	if !strings.Contains(progressOut, "rebuilt 1000 events") {
		t.Errorf("progress output missing terminal summary; got: %q", progressOut)
	}

	// Phase 5: assert the rebuilt state matches the baseline.
	assertEpisodeCount(t, ctx, dis2, n)
	assertVectorCount(t, ctx, dis2, n)
	assertEpisodeNodeCount(t, ctx, dis2, n)
	postFTSHits := snapshotFTSHits(t, ctx, dis2, []string{"TOKEN0042", "TOKEN0500", "TOKEN0999"})
	if !equalFTSHits(baseFTSHits, postFTSHits) {
		t.Errorf("FTS hits drift after rebuild\nbase: %#v\npost: %#v", baseFTSHits, postFTSHits)
	}

	// Phase 6: second Rebuild must skip every event (HasEvent returns
	// true for all UIDs after phase 4).
	var progress2 bytes.Buffer
	applyErrs2, err := log1.Rebuild(ctx, dis2,
		kernel.WithProgress(&progress2),
		kernel.WithProgressEvery(250),
	)
	if err != nil {
		t.Fatalf("Rebuild (second pass): %v", err)
	}
	if len(applyErrs2) != 0 {
		t.Fatalf("Rebuild (second) ApplyError slice: got %d entries, want 0", len(applyErrs2))
	}
	// Idempotency: counts unchanged after the second pass.
	assertEpisodeCount(t, ctx, dis2, n)
	assertVectorCount(t, ctx, dis2, n)
	assertEpisodeNodeCount(t, ctx, dis2, n)
	// The second progress output reports skipped=1000.
	if !strings.Contains(progress2.String(), "skipped=1000") {
		t.Errorf("second rebuild progress should report skipped=1000; got: %q", progress2.String())
	}
}

// TestFileEventLog_Rebuild_RestoresGraphAfterPartialCorruption proves
// the HasEvent contract under partial derived-store corruption: when
// only the LadybugDB graph is deleted (index.db with episodes + vec
// rows intact), the next Rebuild must detect the missing layer and
// re-Apply every event so the graph layer is reconstructed.
//
// Before the multi-layer HasEvent fix, this scenario silently no-op'd:
// HasEvent saw the episodes row, returned true, Apply was skipped, and
// the graph stayed empty forever. The only escape hatch was a manual
// `rm -rf data/` covering ALL derived stores — fragile and unforgiving.
//
// Acceptance: Rebuild applies 10 events, graph has 10 Episode nodes,
// progress reports applied=10 (not skipped=10).
func TestFileEventLog_Rebuild_RestoresGraphAfterPartialCorruption(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	const n = 10

	// Phase 1: seed events through the full four-layer dispatcher.
	dis1, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open (phase 1): %v", err)
	}
	log1 := kernel.NewFileEventLog(dataDir, rebuildOntology{}, kernel.WithStorage(dis1))
	events := make([]kernel.Event, n)
	for i := range events {
		payload, err := json.Marshal(map[string]any{
			"text":       fmt.Sprintf("partial-corruption probe %d", i),
			"source":     "integration:partial-corruption",
			"confidence": 0.9,
		})
		if err != nil {
			t.Fatalf("marshal payload %d: %v", i, err)
		}
		events[i] = kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEpisode),
			Source:     "integration:partial-corruption",
			Confidence: 0.9,
			Payload:    payload,
		}
	}
	if _, err := log1.Append(ctx, events); err != nil {
		t.Fatalf("Append: %v", err)
	}
	assertEpisodeCount(t, ctx, dis1, n)
	assertVectorCount(t, ctx, dis1, n)
	assertEpisodeNodeCount(t, ctx, dis1, n)

	// Phase 2: drop ONLY the graph layer. SQLite index.db (with episodes
	// + episodes_vec_ids rows) and the JSONL events directory survive.
	if err := dis1.Close(); err != nil {
		t.Fatalf("dispatcher.Close (phase 1): %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "graph.ladybug")); err != nil {
		t.Fatalf("RemoveAll graph.ladybug: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "index.db")); err != nil {
		t.Fatalf("index.db must survive partial corruption: %v", err)
	}

	// Phase 3: reopen dispatcher. SQLite layers carry over; graph layer
	// is empty (LadybugDB recreated on Open).
	dis2, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open (phase 3): %v", err)
	}
	t.Cleanup(func() { _ = dis2.Close() })
	assertEpisodeCount(t, ctx, dis2, n)
	assertVectorCount(t, ctx, dis2, n)
	assertEpisodeNodeCount(t, ctx, dis2, 0)

	// Phase 4: Rebuild. HasEvent must report false for every UID because
	// the graph node is missing; Apply re-MERGE-s every Episode node.
	var progress bytes.Buffer
	applyErrs, err := log1.Rebuild(ctx, dis2, kernel.WithProgress(&progress))
	if err != nil {
		t.Fatalf("Rebuild after partial corruption: %v", err)
	}
	if len(applyErrs) != 0 {
		t.Fatalf("Rebuild ApplyErrors: got %d, want 0", len(applyErrs))
	}

	// Phase 5: graph is now restored. The episodes / vector layers were
	// idempotently overwritten (no-op or identical upsert), still n rows.
	assertEpisodeCount(t, ctx, dis2, n)
	assertVectorCount(t, ctx, dis2, n)
	assertEpisodeNodeCount(t, ctx, dis2, n)

	// Progress line must report applied=n, not skipped=n: this is the
	// machine-readable proof of the fix.
	progressOut := progress.String()
	if !strings.Contains(progressOut, fmt.Sprintf("rebuilt %d events", n)) {
		t.Errorf("progress: expected applied=%d after partial-corruption rebuild; got: %q", n, progressOut)
	}
}

// TestFileEventLog_Rebuild_RestoresVec0AfterCorruption proves the
// HasEvent contract under vec0-only corruption: the sidecar mapping
// (episodes_vec_ids) survives, but the actual vec0 rows that hold
// embeddings are gone. Recall depends on the vec0 row, not the sidecar,
// so HasEvent must detect this asymmetry and trigger re-Apply.
//
// Before the join-against-vec0 fix, HasEvent treated a sidecar row as
// proof of vector presence — incorrect. The sidecar is just an
// id<->rowid mapping; the vec0 row is the real payload. Recall would
// silently return empty for affected UIDs until the next full wipe.
//
// Acceptance: vec0 rows wiped → Rebuild applies n events (not skips
// them) → vec0 row count back to n.
func TestFileEventLog_Rebuild_RestoresVec0AfterCorruption(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	const n = 8

	dis, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = dis.Close() })

	log := kernel.NewFileEventLog(dataDir, rebuildOntology{}, kernel.WithStorage(dis))
	events := make([]kernel.Event, n)
	for i := range events {
		payload, err := json.Marshal(map[string]any{
			"text":       fmt.Sprintf("vec0-corruption probe %d", i),
			"source":     "integration:vec0-corruption",
			"confidence": 0.9,
		})
		if err != nil {
			t.Fatalf("marshal payload %d: %v", i, err)
		}
		events[i] = kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEpisode),
			Source:     "integration:vec0-corruption",
			Confidence: 0.9,
			Payload:    payload,
		}
	}
	if _, err := log.Append(ctx, events); err != nil {
		t.Fatalf("Append: %v", err)
	}

	db, err := dis.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	// Sanity: both sidecar and vec0 currently hold n rows.
	assertSQLCount(t, ctx, db, "episodes_vec_ids", n)
	assertSQLCount(t, ctx, db, "episodes_vec", n)

	// Corrupt only vec0: wipe the virtual-table rows while leaving the
	// sidecar id<->rowid mapping intact. This is the exact asymmetry
	// the Greptile finding called out.
	if _, err := db.ExecContext(ctx, `DELETE FROM episodes_vec`); err != nil {
		t.Fatalf("corrupt vec0: %v", err)
	}
	assertSQLCount(t, ctx, db, "episodes_vec_ids", n)
	assertSQLCount(t, ctx, db, "episodes_vec", 0)

	// Rebuild. HasEvent must report false for every UID because the
	// vec0 row is missing; Apply re-INSERT-s every embedding.
	var progress bytes.Buffer
	applyErrs, err := log.Rebuild(ctx, dis, kernel.WithProgress(&progress))
	if err != nil {
		t.Fatalf("Rebuild after vec0 corruption: %v", err)
	}
	if len(applyErrs) != 0 {
		t.Fatalf("Rebuild ApplyErrors: got %d, want 0; first: %v", len(applyErrs), applyErrs[0])
	}

	// vec0 layer is restored.
	assertSQLCount(t, ctx, db, "episodes_vec_ids", n)
	assertSQLCount(t, ctx, db, "episodes_vec", n)

	progressOut := progress.String()
	if !strings.Contains(progressOut, fmt.Sprintf("rebuilt %d events", n)) {
		t.Errorf("progress: expected applied=%d after vec0 corruption; got: %q", n, progressOut)
	}
}

// assertSQLCount fails the test if SELECT count(*) FROM <table> != want.
// Localised here because the wider dispatcher_test.go helpers all wrap
// d.OpenSQLite themselves; this test owns the db handle for the full
// scenario and reuses it.
func assertSQLCount(t *testing.T, ctx context.Context, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	q := "SELECT count(*) FROM " + table
	if err := db.QueryRowContext(ctx, q).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Errorf("%s count = %d, want %d", table, got, want)
	}
}

// snapshotFTSHits returns a map from query -> hit UIDs for the listed
// tokens. The token vocabulary is built into the seeded event text, so
// each token resolves to exactly one UID. Stable ordering relies on the
// FTS5 result determinism for distinct BM25 scores.
func snapshotFTSHits(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, queries []string) map[string][]string {
	t.Helper()
	fts, err := d.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	out := make(map[string][]string, len(queries))
	for _, q := range queries {
		hits, err := fts.Search(ctx, q, 5)
		if err != nil {
			t.Fatalf("fts search %q: %v", q, err)
		}
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		out[q] = ids
	}
	return out
}

// equalFTSHits compares two snapshots produced by snapshotFTSHits.
// Equality is per-key slice equality (order matters: same query against
// the same data must produce the same ranking).
func equalFTSHits(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		for i := range va {
			if va[i] != vb[i] {
				return false
			}
		}
	}
	return true
}
