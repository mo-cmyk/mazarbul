package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// memStore is a faithful in-memory projection: it is both an Appender and a
// Reader, so an item appended on one Run is deduped on the next (what the real
// source_refs + connector_watermarks projections do). It lets the dedup and
// resume paths be exercised end-to-end without the CGO sqlite stack.
type memStore struct {
	mu     sync.Mutex
	hashes map[string]string // dedupKey -> contentHash
	wms    map[string]Watermark
}

func newMemStore() *memStore {
	return &memStore{hashes: map[string]string{}, wms: map[string]Watermark{}}
}

func (m *memStore) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range events {
		switch string(events[i].Kind) {
		case ontology.KindSourceRefRecorded:
			var sr ontology.SourceRefRecordedEvent
			if err := json.Unmarshal(events[i].Payload, &sr); err != nil {
				return nil, err
			}
			m.hashes[sr.DedupKey] = sr.ContentHash
		case ontology.KindConnectorWatermarkAdvanced:
			var adv ontology.ConnectorWatermarkAdvancedEvent
			if err := json.Unmarshal(events[i].Payload, &adv); err != nil {
				return nil, err
			}
			m.wms[adv.Connector+"|"+adv.Stream] = Watermark{
				Connector: adv.Connector, Stream: adv.Stream, Cursor: adv.Cursor, Sequence: adv.Sequence,
			}
		}
	}
	return make([]kernel.Receipt, len(events)), nil
}

func (m *memStore) SourceRefHash(_ context.Context, dedupKey string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hashes[dedupKey]
	return h, ok, nil
}

func (m *memStore) Watermark(_ context.Context, connector, stream string) (Watermark, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if wm, ok := m.wms[connector+"|"+stream]; ok {
		return wm, nil
	}
	return Watermark{Connector: connector, Stream: stream}, nil
}

// resetWatermarks drops resume state while keeping the source-ref hashes, so a
// re-ingest re-pulls the whole repo and exercises the content-hash dedup path
// (the second line of defence when a watermark is lost or reset).
func (m *memStore) resetWatermarks() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wms = map[string]Watermark{}
}

// TestOrchestrator_ReingestIsIdempotent is the AC-2 round-trip: a second ingest
// of an unchanged repo applies nothing because the watermark resumes past every
// already-seen commit (efficient delta).
func TestOrchestrator_ReingestIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := initRepo(t)
	for i := range 5 {
		_ = commit(t, dir, fmt.Sprintf("commit number %d", i))
	}
	store := newMemStore()
	src := NewLocalGitSource(dir, nil, nil)

	first, err := newOrchestratorOver(store).Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.ItemsApplied != 5 {
		t.Fatalf("first run applied = %d, want 5", first.ItemsApplied)
	}

	second, err := newOrchestratorOver(store).Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.ItemsApplied != 0 {
		t.Errorf("second run applied = %d, want 0 (idempotent)", second.ItemsApplied)
	}
}

// TestOrchestrator_FullRepullDedupsByContentHash exercises the dedup path that
// the watermark normally short-circuits: with resume state lost but the
// source-ref hashes intact, a full re-pull skips every unchanged item by content
// hash rather than re-appending it.
func TestOrchestrator_FullRepullDedupsByContentHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := initRepo(t)
	for i := range 5 {
		_ = commit(t, dir, fmt.Sprintf("commit number %d", i))
	}
	store := newMemStore()
	src := NewLocalGitSource(dir, nil, nil)

	if _, err := newOrchestratorOver(store).Run(ctx, src, kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	store.resetWatermarks() // simulate a lost/reset watermark -> full re-pull

	repull, err := newOrchestratorOver(store).Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("repull: %v", err)
	}
	if repull.ItemsApplied != 0 {
		t.Errorf("repull applied = %d, want 0 (all deduped by content hash)", repull.ItemsApplied)
	}
	if repull.ItemsSkipped != 5 {
		t.Errorf("repull skipped = %d, want 5 (content-hash dedup)", repull.ItemsSkipped)
	}
}

func newOrchestratorOver(store *memStore) *Orchestrator {
	return NewOrchestrator(store, store, jobs.NewRegistry(), nil)
}

// BenchmarkIngestDedup reports dedup recall (fraction of an already-ingested
// repo correctly skipped on re-ingest) and throughput. Env-gated: set
// INGEST_BENCH=1 to run, since it builds a real git repo. nameEmbMatch
// precision/recall needs the live extractor+embedder and is deferred to a
// live-LLM bench; this bench covers the deterministic dedup correlation.
func BenchmarkIngestDedup(b *testing.B) {
	if os.Getenv("INGEST_BENCH") == "" {
		b.Skip("set INGEST_BENCH=1 to run the ingest correlation bench")
	}
	ctx := context.Background()
	dir := benchInitRepo(b)
	const n = 100
	for i := range n {
		benchCommit(b, dir, fmt.Sprintf("bench commit %d: add feature %d", i, i))
	}
	store := newMemStore()
	src := NewLocalGitSource(dir, nil, nil)
	if _, err := newOrchestratorOver(store).Run(ctx, src, kernel.ScopePersonal, Bounds{MaxItems: n, MaxExtractionCalls: n}, false); err != nil {
		b.Fatalf("seed run: %v", err)
	}

	b.ResetTimer()
	var skipped int
	for range b.N {
		// Reset resume state each iteration so the whole repo is re-pulled and the
		// content-hash dedup path (not the watermark short-circuit) is measured.
		store.resetWatermarks()
		rep, err := newOrchestratorOver(store).Run(ctx, src, kernel.ScopePersonal, Bounds{MaxItems: n, MaxExtractionCalls: n}, false)
		if err != nil {
			b.Fatalf("reingest: %v", err)
		}
		skipped = rep.ItemsSkipped
	}
	b.StopTimer()
	b.ReportMetric(float64(skipped)/float64(n), "dedup_recall")
	b.ReportMetric(float64(n*b.N)/b.Elapsed().Seconds(), "items/sec")
}

func benchInitRepo(b *testing.B) string {
	b.Helper()
	dir := b.TempDir()
	if _, err := runGit(context.Background(), dir, "init", "-b", "main"); err != nil {
		b.Fatalf("git init: %v", err)
	}
	return dir
}

func benchCommit(b *testing.B, dir, msg string) {
	b.Helper()
	_, err := runGit(context.Background(), dir,
		"-c", "user.email=bench@example.com", "-c", "user.name=Bench",
		"-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", msg)
	if err != nil {
		b.Fatalf("git commit: %v", err)
	}
}
