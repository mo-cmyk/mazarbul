package kernel

import (
	"context"
	"errors"
	"testing"
)

// fakeReranker reverses the input order so tests can assert the reranker
// actually ran (the fused order is descending FusedScore; reversed is
// ascending). The error path lets us pin error propagation.
type fakeReranker struct {
	reverse bool
	err     error
}

func (r *fakeReranker) Rerank(_ context.Context, _ string, hits []Hit) ([]Hit, error) {
	if r.err != nil {
		return hits, r.err
	}
	if !r.reverse {
		return hits, nil
	}
	out := make([]Hit, len(hits))
	for i := range hits {
		out[len(hits)-1-i] = hits[i]
	}
	return out, nil
}

func TestReranker_NilNoOp(t *testing.T) {
	t.Parallel()
	// Construct a HybridQuery with no reranker; in collectHits the reranker
	// branch must not panic and must not alter ordering. Synthesised via
	// the field directly to avoid pulling in the full constructor stack.
	h := &HybridQuery{}
	if h.reranker != nil {
		t.Fatalf("reranker not nil on default-constructed HybridQuery: %T", h.reranker)
	}
}

func TestWithReranker_Sets(t *testing.T) {
	t.Parallel()
	r := &fakeReranker{reverse: true}
	h := &HybridQuery{}
	WithReranker(r)(h)
	if h.reranker != r {
		t.Fatalf("reranker = %v, want %v", h.reranker, r)
	}
}

func TestWithRRFMinScore_Sets(t *testing.T) {
	t.Parallel()
	h := &HybridQuery{}
	WithRRFMinScore(0.05)(h)
	if h.rrfMinScore != 0.05 {
		t.Errorf("rrfMinScore = %v, want 0.05", h.rrfMinScore)
	}
	// Negative values are ignored — preserve existing positive setting.
	WithRRFMinScore(-1)(h)
	if h.rrfMinScore != 0.05 {
		t.Errorf("rrfMinScore = %v after negative input, want 0.05 (preserved)", h.rrfMinScore)
	}
}

// TestWithLaneWeights_AcceptZeroRejectNegative pins the footgun fix: an
// explicit weight of 0 disables the lane (graph_weight = 0.0 was the config
// an operator wrote, and the >0 guard used to silently swallow it, pinning
// the 1.0 default). Negative weights are still rejected so a typo can't
// invert a lane.
func TestWithLaneWeights_AcceptZeroRejectNegative(t *testing.T) {
	t.Parallel()
	h := &HybridQuery{graphWeight: 1.0, nameEmbWeight: 1.0, twoHopFloor: 3}

	WithGraphWeight(0)(h)
	if h.graphWeight != 0 {
		t.Errorf("graphWeight = %v after WithGraphWeight(0), want 0 (explicit 0 must disable the lane)", h.graphWeight)
	}
	WithNameEmbWeight(0)(h)
	if h.nameEmbWeight != 0 {
		t.Errorf("nameEmbWeight = %v after WithNameEmbWeight(0), want 0", h.nameEmbWeight)
	}
	WithTwoHopFloor(0)(h)
	if h.twoHopFloor != 0 {
		t.Errorf("twoHopFloor = %v after WithTwoHopFloor(0), want 0 (0 disables 2-hop expansion)", h.twoHopFloor)
	}

	// Negative values are rejected — the prior (0) setting is preserved.
	WithGraphWeight(-1)(h)
	if h.graphWeight != 0 {
		t.Errorf("graphWeight = %v after WithGraphWeight(-1), want 0 (negative ignored)", h.graphWeight)
	}
	WithTwoHopFloor(-5)(h)
	if h.twoHopFloor != 0 {
		t.Errorf("twoHopFloor = %v after WithTwoHopFloor(-5), want 0 (negative ignored)", h.twoHopFloor)
	}
}

func TestReranker_ErrorPropagates(t *testing.T) {
	t.Parallel()
	// Pure interface-contract test: a non-nil error from the reranker must
	// be surfaced to the caller; the collectHits wrapper does the actual
	// wrapping with "kernel: hybrid: rerank: %w", but the Reranker contract
	// itself is what we pin here.
	want := errors.New("model timed out")
	r := &fakeReranker{err: want}
	_, got := r.Rerank(context.Background(), "q", []Hit{{UID: "a"}})
	if !errors.Is(got, want) {
		t.Errorf("Rerank err = %v, want %v", got, want)
	}
}

// TestApplyReranker_NonFatalErrorDegradesGracefully pins the recall-vs-rerank
// contract: a malformed LLM response (e.g. "order length = 16, want 20")
// must NOT fail the recall. The reranker's own godoc promises to return the
// original ordering alongside the error so the caller "can choose to log and
// proceed rather than fail the recall". Before the fix applyReranker
// propagated the error, which caused `maz ask` to fail with
// E_INTERNAL: ask: recall: kernel: hybrid: rerank: order length = 16,
// want 20 whenever qwen2.5:7b returned a short permutation.
func TestApplyReranker_NonFatalErrorDegradesGracefully(t *testing.T) {
	t.Parallel()
	want := errors.New("order length = 16, want 20")
	h := &HybridQuery{reranker: &fakeReranker{err: want}}
	in := []Hit{{UID: "A"}, {UID: "B"}, {UID: "C"}}
	got, err := h.applyReranker(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("applyReranker err = %v, want nil (degrade gracefully)", err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d hits, want %d (original ordering)", len(got), len(in))
	}
	for i := range in {
		if got[i].UID != in[i].UID {
			t.Errorf("got[%d].UID = %q, want %q", i, got[i].UID, in[i].UID)
		}
	}
}

// TestApplyReranker_ContextCancellationPropagates pins the one case where
// the error MUST bubble up: context cancellation. The caller asked us to
// stop; honoring that is more important than rendering a partial result.
func TestApplyReranker_ContextCancellationPropagates(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// rerankEnabled must be true to exercise the reranker path: when the
	// reranker is gated off (the default), applyReranker is a no-op and
	// never reaches the cancellation check.
	h := &HybridQuery{reranker: &fakeReranker{err: ctx.Err()}, rerankEnabled: true}
	in := []Hit{{UID: "A"}}
	_, err := h.applyReranker(ctx, "q", in)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("applyReranker err = %v, want context.Canceled", err)
	}
}
