package dispatcher_test

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fixedEmbedder returns one of two L2-normalized vectors based on a
// substring match in the input text. This lets us drive the semantic
// dedup branch deterministically: "twin" texts produce the same
// vector (cosine = 1.0), "other" texts produce an orthogonal vector
// (cosine = 0.0). All test embeddings are unit-normalized so the
// CosineSimilarityFromDistance(L2)=1-L2²/2 identity holds.
type fixedEmbedder struct {
	twinKeyword string
}

func (f *fixedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	out := make([]float32, storage.VectorDim)
	if containsSubstring(text, f.twinKeyword) {
		// Unit vector along axis 0 → twin candidates land on this point.
		out[0] = 1
		return out, nil
	}
	// Unit vector along axis 1 → orthogonal to twin (cosine = 0).
	out[1] = 1
	return out, nil
}

func containsSubstring(s, sub string) bool {
	if sub == "" {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// newDedupDispatcher builds a Dispatcher with sync-dedup wired against
// a fixedEmbedder. The cosine threshold is `threshold`; the stats sink
// is a real in-memory store so IncDedup observations are inspectable
// via Snapshot.
func newDedupDispatcher(t *testing.T, ctx context.Context, threshold float64) (*dispatcher.Dispatcher, stats.Store) {
	t.Helper()
	st := stats.NewInMemoryStore()
	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:                t.TempDir(),
		Embedder:               &fixedEmbedder{twinKeyword: "twin"},
		Stats:                  st,
		DedupSemanticThreshold: threshold,
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, st
}

// TestApplyEpisode_SemanticDuplicateIsSkipped is the headline Phase 2
// test. Two Episodes carry near-identical content; the fixedEmbedder
// emits identical vectors (cosine 1.0). With threshold 0.85, the
// second Apply must NOT persist a new episode row and MUST bump the
// stats counter dedup_skips["semantic"] by exactly 1. Matches mem0
// v3's NOOP path semantics.
func TestApplyEpisode_SemanticDuplicateIsSkipped(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d, st := newDedupDispatcher(t, ctx, 0.85)

	first := makeEpisodeEvent(t, "01HFIRST00000000000000000A", "I love twin sister oat milk", "test")
	second := makeEpisodeEvent(t, "01HSECOND0000000000000000B", "twin sister favorite is oat milk", "test")

	if err := d.Apply(ctx, []kernel.Event{first}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{second}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 1) // second was deduped

	snap, err := st.Snapshot(ctx, stats.Lifetime)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.DedupSkipsByReason["semantic"]; got != 1 {
		t.Errorf("dedup_skips[\"semantic\"] = %d, want 1; full map = %+v", got, snap.DedupSkipsByReason)
	}
}

// TestApplyEpisode_BelowThresholdNotSkipped pins the inverse: when
// the candidate is semantically orthogonal to existing episodes
// (cosine 0 in the fixedEmbedder), both episodes must persist and
// the dedup counter must NOT increment.
func TestApplyEpisode_BelowThresholdNotSkipped(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d, st := newDedupDispatcher(t, ctx, 0.85)

	first := makeEpisodeEvent(t, "01HFIRST00000000000000000A", "I love twin sister oat milk", "test")
	second := makeEpisodeEvent(t, "01HSECOND0000000000000000B", "completely different subject", "test")

	if err := d.Apply(ctx, []kernel.Event{first}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{second}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 2)

	snap, err := st.Snapshot(ctx, stats.Lifetime)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.DedupSkipsByReason["semantic"]; got != 0 {
		t.Errorf("dedup_skips[\"semantic\"] = %d, want 0", got)
	}
}

// TestApplyEpisode_DedupDisabledKeepsLegacyAsync pins the
// "threshold = 0 disables the branch" default. Without
// DedupSemanticThreshold set, the dispatcher uses its legacy
// async-embed path and the second Apply persists even though the
// fixedEmbedder would have emitted a duplicate vector. Backward
// compat for every caller that doesn't opt in.
func TestApplyEpisode_DedupDisabledKeepsLegacyAsync(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	st := stats.NewInMemoryStore()
	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  t.TempDir(),
		Embedder: &fixedEmbedder{twinKeyword: "twin"},
		Stats:    st,
		// DedupSemanticThreshold deliberately zero.
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	first := makeEpisodeEvent(t, "01HFIRST00000000000000000A", "I love twin sister oat milk", "test")
	second := makeEpisodeEvent(t, "01HSECOND0000000000000000B", "twin sister favorite is oat milk", "test")

	if err := d.Apply(ctx, []kernel.Event{first, second}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 2)

	snap, err := st.Snapshot(ctx, stats.Lifetime)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.DedupSkipsByReason) != 0 {
		t.Errorf("dedup_skips should be empty when knob is off; got %+v", snap.DedupSkipsByReason)
	}
}

// TestApplyEpisode_DedupSelfMatchIgnored pins that re-Applying the
// SAME event UID (e.g. an Apply retry) does NOT trigger the dedup
// path against itself. The dispatcher already has idempotent
// ON CONFLICT(uid) DO UPDATE semantics for the episode row; the
// dedup branch must not regress that into a spurious skip.
func TestApplyEpisode_DedupSelfMatchIgnored(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d, st := newDedupDispatcher(t, ctx, 0.85)

	ev := makeEpisodeEvent(t, "01HDUPSELF000000000000000A", "I love twin sister oat milk", "test")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Re-apply the same event. The candidate UID matches the existing
	// row, so checkSemanticDuplicate must NOT classify it as a dup.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 1)
	snap, err := st.Snapshot(ctx, stats.Lifetime)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.DedupSkipsByReason["semantic"]; got != 0 {
		t.Errorf("self-match must not trigger dedup; got skips=%d", got)
	}
}

// TestStore_IncDedup pins the counter contract directly: an empty
// reason is a no-op (don't pollute the map with ""), non-empty
// reasons aggregate per-key. Guard against future regressions that
// e.g. forget the empty-reason guard.
func TestStore_IncDedup(t *testing.T) {
	t.Parallel()
	st := stats.NewInMemoryStore()
	st.IncDedup("semantic")
	st.IncDedup("semantic")
	st.IncDedup("")
	st.IncDedup("exact-hash")

	snap, err := st.Snapshot(context.Background(), stats.Lifetime)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := snap.DedupSkipsByReason["semantic"]; got != 2 {
		t.Errorf("semantic count = %d, want 2", got)
	}
	if got := snap.DedupSkipsByReason["exact-hash"]; got != 1 {
		t.Errorf("exact-hash count = %d, want 1", got)
	}
	if _, ok := snap.DedupSkipsByReason[""]; ok {
		t.Errorf("empty reason must not appear in map; got %+v", snap.DedupSkipsByReason)
	}
}
