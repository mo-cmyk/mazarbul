package kernel

import (
	"math"
	"slices"
	"testing"
	"time"
)

// TestReinforcementScore_MatchesIteratedRule pins the closed form against the
// order-dependent iterated rule it replaces. The competitor reinforces with
// r <- r + 0.1*(1-r) per access; our closed form r_n = 1 - (1-r0)*0.9^n must
// equal that loop for every count n in 0..50, proving the two agree on value
// while ours is reorder-invariant.
func TestReinforcementScore_MatchesIteratedRule(t *testing.T) {
	t.Parallel()
	const base = salienceBaseDefault
	for n := range 51 {
		iter := base
		for range n {
			iter += 0.1 * (1 - iter)
		}
		got := ReinforcementScore(n, base)
		if math.Abs(got-iter) > 1e-9 {
			t.Errorf("n=%d: closed form %v != iterated %v", n, got, iter)
		}
	}
}

// TestReinforcementScore_MonotoneBounded asserts the curve rises with each
// access and never reaches 1 (bounded above), and starts at the base for n=0.
func TestReinforcementScore_MonotoneBounded(t *testing.T) {
	t.Parallel()
	const base = 0.5
	if got := ReinforcementScore(0, base); math.Abs(got-base) > 1e-12 {
		t.Errorf("n=0 score = %v, want base %v", got, base)
	}
	prev := -1.0
	for n := range 101 {
		got := ReinforcementScore(n, base)
		if got <= prev {
			t.Errorf("n=%d: score %v not strictly greater than prev %v", n, got, prev)
		}
		if got >= 1.0 {
			t.Errorf("n=%d: score %v reached/exceeded 1.0 (must stay < 1)", n, got)
		}
		prev = got
	}
}

// TestReinforcementScore_NegativeCountClamped guards the floor: a negative count
// folds to n=0 (the base), never a >1 blowup from a negative exponent.
func TestReinforcementScore_NegativeCountClamped(t *testing.T) {
	t.Parallel()
	if got := ReinforcementScore(-5, 0.5); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("negative count score = %v, want base 0.5", got)
	}
}

// TestStrengthMultiplier pins the query-time composition: no accesses → neutral
// multiplier 1; accesses with a recent last-access → multiplier > 1; the same
// access count decayed far in the past → smaller boost than a recent one.
func TestStrengthMultiplier(t *testing.T) {
	t.Parallel()
	h := &HybridQuery{
		strengthBoost:        strengthBoostDefault,
		strengthHalflifeDays: strengthHalflifeDaysDefault,
		salienceBase:         salienceBaseDefault,
	}
	anchor := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// No accesses → neutral.
	if mult, st := h.strengthMultiplier(strengthRow{}, anchor); mult != 1 || st != 0 {
		t.Errorf("zero accesses: mult=%v strength=%v, want 1, 0", mult, st)
	}

	recent := strengthRow{accessCount: 5, lastAccessedAt: anchor.AddDate(0, 0, -1), salienceBase: 0.5}
	stale := strengthRow{accessCount: 5, lastAccessedAt: anchor.AddDate(0, 0, -120), salienceBase: 0.5}

	multRecent, stRecent := h.strengthMultiplier(recent, anchor)
	multStale, stStale := h.strengthMultiplier(stale, anchor)

	if multRecent <= 1 {
		t.Errorf("recent multiplier = %v, want > 1", multRecent)
	}
	if stRecent <= stStale {
		t.Errorf("recent strength %v should exceed stale strength %v (Gaussian decay)", stRecent, stStale)
	}
	if multRecent <= multStale {
		t.Errorf("recent multiplier %v should exceed stale multiplier %v", multRecent, multStale)
	}
}

// TestStrengthMultiplier_DisabledBoost asserts a zero boost disables the lane
// entirely (always neutral), so the cognitive ranking can be turned fully off.
func TestStrengthMultiplier_DisabledBoost(t *testing.T) {
	t.Parallel()
	h := &HybridQuery{strengthBoost: 0, strengthHalflifeDays: strengthHalflifeDaysDefault, salienceBase: 0.5}
	mult, st := h.strengthMultiplier(strengthRow{accessCount: 10, lastAccessedAt: time.Now()}, time.Now())
	if mult != 1 || st != 0 {
		t.Errorf("disabled boost: mult=%v strength=%v, want 1, 0", mult, st)
	}
}

// TestStrengthOptionSetters pins the config-tuning surface: WithStrengthBoost
// clamps to [0, 1]; WithStrengthHalflife and WithSalienceBase ignore
// out-of-range values, leaving the prior value intact (so an absent/invalid
// config key falls through to the kernel default).
func TestStrengthOptionSetters(t *testing.T) {
	t.Parallel()

	// Boost clamps high and low.
	h := &HybridQuery{}
	WithStrengthBoost(2.0)(h)
	if h.strengthBoost != 1 {
		t.Errorf("WithStrengthBoost(2.0) = %v, want clamped to 1", h.strengthBoost)
	}
	WithStrengthBoost(-1)(h)
	if h.strengthBoost != 0 {
		t.Errorf("WithStrengthBoost(-1) = %v, want clamped to 0", h.strengthBoost)
	}
	WithStrengthBoost(0.3)(h)
	if h.strengthBoost != 0.3 {
		t.Errorf("WithStrengthBoost(0.3) = %v, want 0.3", h.strengthBoost)
	}

	// Halflife ignores non-positive, accepts positive.
	h2 := &HybridQuery{strengthHalflifeDays: strengthHalflifeDaysDefault}
	WithStrengthHalflife(0)(h2)
	if h2.strengthHalflifeDays != strengthHalflifeDaysDefault {
		t.Errorf("WithStrengthHalflife(0) changed σ to %v, want default %v", h2.strengthHalflifeDays, strengthHalflifeDaysDefault)
	}
	WithStrengthHalflife(45)(h2)
	if h2.strengthHalflifeDays != 45 {
		t.Errorf("WithStrengthHalflife(45) = %v, want 45", h2.strengthHalflifeDays)
	}

	// SalienceBase ignores out-of-(0,1), accepts in-range.
	h3 := &HybridQuery{salienceBase: salienceBaseDefault}
	for _, bad := range []float64{0, 1, -0.5, 1.5} {
		WithSalienceBase(bad)(h3)
		if h3.salienceBase != salienceBaseDefault {
			t.Errorf("WithSalienceBase(%v) changed base to %v, want default %v", bad, h3.salienceBase, salienceBaseDefault)
		}
	}
	WithSalienceBase(0.6)(h3)
	if h3.salienceBase != 0.6 {
		t.Errorf("WithSalienceBase(0.6) = %v, want 0.6", h3.salienceBase)
	}
}

// TestDerivedKindWanted pins the per-lane Kinds gate (issue #231): an empty
// filter runs every derived lane (no restriction), while a non-empty filter
// runs a lane only when its own kind is listed. The prior code bailed out of
// all derived lanes whenever any Kinds filter was set, hiding insights and
// consolidated memories from a kind-scoped recall.
func TestDerivedKindWanted(t *testing.T) {
	t.Parallel()
	const insight = EventKind("insightSynthesized")
	const consolidated = EventKind("memoryConsolidated")

	cases := []struct {
		name  string
		kinds []EventKind
		want  EventKind
		ok    bool
	}{
		{"empty filter runs every lane", nil, insight, true},
		{"empty slice runs every lane", []EventKind{}, consolidated, true},
		{"listed kind runs its lane", []EventKind{insight}, insight, true},
		{"unlisted kind skips its lane", []EventKind{insight}, consolidated, false},
		{"mixed filter runs each listed lane", []EventKind{insight, consolidated}, consolidated, true},
		{"unrelated kind skips both lanes", []EventKind{"episodeRecorded"}, insight, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := derivedKindWanted(tc.kinds, tc.want); got != tc.ok {
				t.Errorf("derivedKindWanted(%v, %q) = %v, want %v", tc.kinds, tc.want, got, tc.ok)
			}
		})
	}
}

// TestPrefixCitations pins the provenance typing (issue #231): bare source
// ULIDs gain the lane's AIP collection prefix so an agent can pivot straight
// into get/neighbors, while already-typed entries pass through and empty
// entries are dropped. An all-empty input yields nil, not an empty slice.
func TestPrefixCitations(t *testing.T) {
	t.Parallel()
	const a = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const b = "01BX5ZZKBKACTAV9WEVGEMMVRZ"

	cases := []struct {
		name   string
		prefix string
		uids   []string
		want   []string
	}{
		{"nil input yields nil", "entities/", nil, nil},
		{"bare ulids gain prefix", "entities/", []string{a, b}, []string{"entities/" + a, "entities/" + b}},
		{"episode prefix", "episodes/", []string{a}, []string{"episodes/" + a}},
		{"already-typed passes through", "entities/", []string{"episodes/" + a}, []string{"episodes/" + a}},
		{"empty entries dropped", "entities/", []string{"", a, ""}, []string{"entities/" + a}},
		{"all-empty yields nil", "entities/", []string{"", ""}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := prefixCitations(tc.prefix, tc.uids)
			if !slices.Equal(got, tc.want) {
				t.Errorf("prefixCitations(%q, %v) = %v, want %v", tc.prefix, tc.uids, got, tc.want)
			}
		})
	}
}

// TestWithinRequestedTimeRange pins the derived-lane transaction-time filter
// (issue #231 regression): a derived memory is kept only when its timestamp
// falls within the request's Since/Until/AsOf bounds, matching the episode and
// bare-entity lanes. Before this, the derived lanes ignored these bounds and
// leaked insights/consolidated memories across the bi-temporal boundary.
func TestWithinRequestedTimeRange(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	before := base.AddDate(0, 0, -10)
	after := base.AddDate(0, 0, 10)
	asOfPast := base.AddDate(0, 0, -5)

	cases := []struct {
		name string
		ct   time.Time
		req  RecallRequest
		want bool
	}{
		{"no bounds keeps all", base, RecallRequest{}, true},
		{"since keeps on-or-after", base, RecallRequest{Since: before}, true},
		{"since drops before", before, RecallRequest{Since: base}, false},
		{"until keeps strictly-before", before, RecallRequest{Until: base}, true},
		{"until drops on-boundary", base, RecallRequest{Until: base}, false},
		{"until drops after", after, RecallRequest{Until: base}, false},
		{"asOf keeps on-or-before", before, RecallRequest{AsOf: &base}, true},
		{"asOf drops after", after, RecallRequest{AsOf: &asOfPast}, false},
		{"combined window keeps inside", base, RecallRequest{Since: before, Until: after}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := withinRequestedTimeRange(tc.ct, tc.req); got != tc.want {
				t.Errorf("withinRequestedTimeRange(%v, %+v) = %v, want %v", tc.ct, tc.req, got, tc.want)
			}
		})
	}
}

// TestDecodeSourceUids pins the source-provenance decode: a well-formed JSON
// array decodes to its elements, an empty column yields nil, and a malformed
// value yields nil (the derived hit still surfaces; only its provenance is
// unavailable) rather than failing the whole recall.
func TestDecodeSourceUids(t *testing.T) {
	t.Parallel()
	const a = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const b = "01BX5ZZKBKACTAV9WEVGEMMVRZ"

	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty column yields nil", "", nil},
		{"single element", `["` + a + `"]`, []string{a}},
		{"two elements", `["` + a + `","` + b + `"]`, []string{a, b}},
		{"malformed yields nil", `{not json`, nil},
		{"wrong-type yields nil", `42`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decodeSourceUids(tc.raw)
			if !slices.Equal(got, tc.want) {
				t.Errorf("decodeSourceUids(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
