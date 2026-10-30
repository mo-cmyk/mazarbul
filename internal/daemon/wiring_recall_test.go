package daemon

import (
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// ptr returns a pointer to v. The RecallTable knobs whose zero value is
// meaningful (graph_weight, name_emb_weight, two_hop_floor) are pointers so
// an explicit 0 is distinguishable from an absent key.
func ptr[T any](v T) *T { return &v }

func TestApplyRecallTuning_ZeroConfigIsNoOp(t *testing.T) {
	t.Parallel()
	h := &kernel.HybridQuery{}
	// Seed non-zero lane knobs, then apply an empty RecallTable. The
	// empty-config path must be a true no-op — leave prior values intact,
	// not reset them — so an absent [recall] section preserves whatever
	// the kernel constructor (or an earlier apply) established. Asserting
	// the seeded values survive is the real contract; "doesn't panic" is
	// already covered by every other apply test.
	applyRecallTuning(h, config.RecallTable{
		GraphWeight: ptr(1.5), NameEmbWeight: ptr(0.8), TwoHopFloor: ptr(4),
	}, zap.NewNop())
	applyRecallTuning(h, config.RecallTable{}, zap.NewNop())
	if got := h.GraphWeight(); got != 1.5 {
		t.Errorf("GraphWeight() = %v after empty config, want 1.5 (no-op must not reset)", got)
	}
	if got := h.NameEmbWeight(); got != 0.8 {
		t.Errorf("NameEmbWeight() = %v after empty config, want 0.8", got)
	}
	if got := h.TwoHopFloor(); got != 4 {
		t.Errorf("TwoHopFloor() = %v after empty config, want 4", got)
	}
}

func TestApplyRecallTuning_AppliesKnobs(t *testing.T) {
	t.Parallel()
	h := &kernel.HybridQuery{}
	applyRecallTuning(h, config.RecallTable{
		TwoHopFloor:          ptr(4),
		GraphWeight:          ptr(1.5),
		NameEmbWeight:        ptr(0.8),
		RRFMinScore:          ptr(float32(0.01)),
		EdgeAllowlist:        []string{"works_at", "located_in"},
		MMRLambda:            ptr(0.7),
		RecencyBoost:         ptr(0.15),
		RecencyHalflifeDays:  30,
		StrengthBoost:        0.2,
		StrengthHalflifeDays: 45,
		SalienceBase:         0.6,
	}, zap.NewNop())
	if got := h.GraphWeight(); got != 1.5 {
		t.Errorf("GraphWeight() = %v, want 1.5", got)
	}
	if got := h.NameEmbWeight(); got != 0.8 {
		t.Errorf("NameEmbWeight() = %v, want 0.8", got)
	}
	if got := h.TwoHopFloor(); got != 4 {
		t.Errorf("TwoHopFloor() = %v, want 4", got)
	}
	// RRFMinScore/MMRLambda/RecencyBoost/etc. lack exported accessors;
	// kernel-side setter tests cover those field assignments.
}

// TestApplyRecallTuning_ExplicitZeroIsApplied pins the footgun fix: an
// explicit graph_weight = 0.0 (etc.) decodes to a pointer-to-0, distinct
// from nil, so the knob is applied (disabling the lane) rather than silently
// falling through to the kernel default — the bug this migration closed.
// Smoke-level here; the kernel setter tests assert the field lands on 0.
func TestApplyRecallTuning_ExplicitZeroIsApplied(t *testing.T) {
	t.Parallel()
	h := &kernel.HybridQuery{}
	// Seed non-zero first: a zero-value HybridQuery already reads 0, which
	// would make the assertion below vacuous. We need to prove an explicit 0
	// OVERRIDES a prior positive value — i.e. the setter is actually called
	// and accepts 0 (the footgun was that graph_weight = 0.0 was swallowed).
	applyRecallTuning(h, config.RecallTable{
		GraphWeight: ptr(1.5), NameEmbWeight: ptr(0.8), TwoHopFloor: ptr(4),
	}, zap.NewNop())
	applyRecallTuning(h, config.RecallTable{
		GraphWeight: ptr(0.0), NameEmbWeight: ptr(0.0), TwoHopFloor: ptr(0),
	}, zap.NewNop())
	if got := h.GraphWeight(); got != 0 {
		t.Errorf("GraphWeight() = %v after explicit 0.0, want 0 (footgun: explicit 0 must override, not be swallowed)", got)
	}
	if got := h.NameEmbWeight(); got != 0 {
		t.Errorf("NameEmbWeight() = %v after explicit 0.0, want 0", got)
	}
	if got := h.TwoHopFloor(); got != 0 {
		t.Errorf("TwoHopFloor() = %v after explicit 0, want 0", got)
	}
}

func TestApplyRecallTuning_NilHybridIsSafe(t *testing.T) {
	t.Parallel()
	applyRecallTuning(nil, config.RecallTable{TwoHopFloor: ptr(4)}, zap.NewNop())
}
