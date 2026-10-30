//go:build integration

package orchestrator_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestOrchestrator_EndToEnd_TenEvents is the acceptance criterion for
// issue #55: with a real EventLog + real four-layer Dispatcher + fake
// Embedder wired together, Append 10 episode events and assert every
// layer (JSONL, SQLite relational, sqlite-vec, FTS5, LadybugDB)
// reflects them.
func TestOrchestrator_EndToEnd_TenEvents(t *testing.T) {
	ctx := testutil.TestContext(t)
	dir := t.TempDir()

	o, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  dir,
		Ontology: newOntology(t),
		Embedder: &fakeEmbedder{model: "fake-bge-m3", dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })

	const n = 10
	events := make([]kernel.Event, n)
	for i := range n {
		events[i] = sampleEpisode(t, fmt.Sprintf("orchestrator e2e text number %d carries UNIQE2E%03d", i, i))
	}

	receipts, err := o.Log.Append(ctx, events)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != n {
		t.Fatalf("Append receipts: got %d, want %d", len(receipts), n)
	}

	t.Run("jsonl has 10 lines", func(t *testing.T) {
		count := jsonlLineCount(t, dir)
		if count != n {
			t.Errorf("jsonl line count = %d, want %d", count, n)
		}
	})

	t.Run("jsonl uids match receipts", func(t *testing.T) {
		want := map[string]struct{}{}
		for _, r := range receipts {
			want[r.UID] = struct{}{}
		}
		for _, uid := range jsonlUIDs(t, dir) {
			delete(want, uid)
		}
		if len(want) != 0 {
			t.Errorf("receipts missing from JSONL: %v", want)
		}
	})

	t.Run("sqlite relational has 10 rows", func(t *testing.T) {
		db, derr := o.Storage.OpenSQLite(ctx)
		if derr != nil {
			t.Fatalf("OpenSQLite: %v", derr)
		}
		var got int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&got); err != nil {
			t.Fatalf("count episodes: %v", err)
		}
		if got != n {
			t.Errorf("episodes count = %d, want %d", got, n)
		}
	})

	t.Run("sqlite vec has 10 sidecar rows", func(t *testing.T) {
		db, derr := o.Storage.OpenSQLite(ctx)
		if derr != nil {
			t.Fatalf("OpenSQLite: %v", derr)
		}
		var got int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM episodes_vec_ids`).Scan(&got); err != nil {
			t.Fatalf("count episodes_vec_ids: %v", err)
		}
		if got != n {
			t.Errorf("episodes_vec_ids count = %d, want %d", got, n)
		}
	})

	t.Run("fts5 hits a unique token", func(t *testing.T) {
		fts, derr := o.Storage.OpenFTS(ctx)
		if derr != nil {
			t.Fatalf("OpenFTS: %v", derr)
		}
		hits, herr := fts.Search(ctx, "UNIQE2E007", 5)
		if herr != nil {
			t.Fatalf("fts search: %v", herr)
		}
		if len(hits) != 1 {
			t.Fatalf("fts hits = %d, want 1", len(hits))
		}
		if hits[0].ID != receipts[7].UID {
			t.Errorf("fts hit ID = %q, want %q", hits[0].ID, receipts[7].UID)
		}
	})

	t.Run("ladybug graph has 10 episode nodes", func(t *testing.T) {
		g, gerr := o.Storage.OpenGraph(ctx)
		if gerr != nil {
			t.Fatalf("OpenGraph: %v", gerr)
		}
		r, rerr := g.Execute(ctx, "MATCH (e:Episode) RETURN count(e)", nil)
		if rerr != nil {
			t.Fatalf("graph count execute: %v", rerr)
		}
		defer func() { _ = r.Close() }()
		if !r.Next() {
			if err := r.Err(); err != nil {
				t.Fatalf("graph iter: %v", err)
			}
			t.Fatal("graph count: zero rows")
		}
		var got int64
		if err := r.Scan(&got); err != nil {
			t.Fatalf("scan count: %v", err)
		}
		if got != int64(n) {
			t.Errorf("episode node count = %d, want %d", got, n)
		}
	})
}

// TestOrchestrator_EndToEnd_StorageFailureDoesNotPoisonLog asserts the
// warn-and-continue contract under the live wiring: if the wrapped
// dispatcher would somehow fail after the JSONL fsync, the receipts
// still come back and the JSONL line is durable. We simulate the
// failure by closing the dispatcher mid-flight: Apply after Close
// returns ErrClosed.
func TestOrchestrator_EndToEnd_StorageFailureDoesNotPoisonLog(t *testing.T) {
	ctx := testutil.TestContext(t)
	dir := t.TempDir()

	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	o, err := orchestrator.New(logging.WithLogger(ctx, logger), orchestrator.Config{
		DataDir:  dir,
		Ontology: newOntology(t),
		Embedder: &fakeEmbedder{model: "fake-bge-m3", dim: storage.VectorDim},
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	// Close the dispatcher so subsequent Apply calls error.
	if err := o.Storage.Close(); err != nil {
		t.Fatalf("pre-close Storage: %v", err)
	}

	receipts, err := o.Log.Append(logging.WithLogger(ctx, logger), []kernel.Event{
		sampleEpisode(t, "even with a dead storage, the log must remain durable"),
	})
	if err != nil {
		t.Fatalf("Append must succeed even with closed Storage: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("receipts: got %d, want 1", len(receipts))
	}

	// JSONL must hold the line.
	if got := jsonlLineCount(t, dir); got != 1 {
		t.Errorf("jsonl line count = %d, want 1 (the log must be durable)", got)
	}

	// At least one warn-level log entry must mention the failure.
	entries := logs.FilterMessageSnippet("storage apply failed").All()
	if len(entries) == 0 {
		t.Errorf("expected a warn log about storage apply failure, none found (all entries: %#v)", logs.All())
	}
}

// jsonlLineCount walks dir/events/**/*.jsonl and counts the total
// non-empty lines across every day file.
func jsonlLineCount(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "events", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	count := 0
	for _, m := range matches {
		data, rerr := os.ReadFile(m)
		if rerr != nil {
			t.Fatalf("ReadFile %s: %v", m, rerr)
		}
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				count++
			}
		}
	}
	return count
}

// jsonlUIDs walks dir/events/**/*.jsonl and returns the UID of every
// event line, decoding just enough JSON to read the field.
func jsonlUIDs(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "events", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	var out []string
	for _, m := range matches {
		data, rerr := os.ReadFile(m)
		if rerr != nil {
			t.Fatalf("ReadFile %s: %v", m, rerr)
		}
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var hdr struct {
				UID string `json:"uid"`
			}
			if err := json.Unmarshal([]byte(line), &hdr); err != nil {
				t.Fatalf("decode JSONL header %s: %v", m, err)
			}
			out = append(out, hdr.UID)
		}
	}
	return out
}
