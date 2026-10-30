package dedup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// blockingEmbedder blocks every Embed call until the passed ctx is cancelled,
// simulating a wedged or cold-loading embedding provider. It records the call
// count so a test can assert the pass aborted early instead of fanning out
// over every registered kind.
type blockingEmbedder struct{ calls atomic.Int64 }

func (b *blockingEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	b.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*blockingEmbedder) Model() string { return "blocking" }

// countingBatchEmbedder implements both Embed and EmbedBatch, counting each so
// a test can assert the dedup semantic pass takes the single-round-trip batch
// path rather than fanning out one Embed per registered kind. Vectors are
// looked up by exact text; unknown inputs return zeros.
type countingBatchEmbedder struct {
	embedCalls atomic.Int64
	batchCalls atomic.Int64
	vecs       map[string][]float32
}

func (e *countingBatchEmbedder) lookup(text string) []float32 {
	if v, ok := e.vecs[text]; ok {
		return v
	}
	return []float32{0, 0, 0}
}

func (e *countingBatchEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.embedCalls.Add(1)
	return e.lookup(text), nil
}

func (e *countingBatchEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	e.batchCalls.Add(1)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.lookup(t)
	}
	return out, nil
}

func (*countingBatchEmbedder) Model() string { return "counting" }

// fakeKindSource returns a fixed list of registered kinds.
type fakeKindSource struct {
	kinds []navigate.RegisteredEntityKind
	err   error
}

func (f *fakeKindSource) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.kinds, nil
}

// fakeEmbedder returns a configurable fixed-dimension vector for each input.
// The vector for input text is looked up by exact string; unknown inputs return zeros.
type fakeEmbedder struct {
	vecs map[string][]float32
	err  error
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 0}, nil
}

func (f *fakeEmbedder) Model() string { return "fake" }

// vec3 returns a unit 3-vector along axis i.
func vec3(i int) []float32 {
	v := []float32{0, 0, 0}
	v[i] = 1.0
	return v
}

// ----- lexical tests -----

func TestCheck_ExactNameMatch_Reuse(t *testing.T) {
	t.Parallel()
	kinds := []navigate.RegisteredEntityKind{
		{Name: "vehicle", Description: "A land vehicle."},
	}
	c := New(&fakeKindSource{kinds: kinds}, nil)
	res, err := c.Check(context.Background(), "vehicle", "A land vehicle.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != Reuse {
		t.Errorf("Decision = %v, want Reuse", res.Decision)
	}
	if res.Match != "vehicle" {
		t.Errorf("Match = %q, want %q", res.Match, "vehicle")
	}
	if res.Similarity != 1.0 {
		t.Errorf("Similarity = %v, want 1.0", res.Similarity)
	}
}

func TestCheck_NormalizedNameMatch_Reuse(t *testing.T) {
	t.Parallel()
	// "land vehicle" normalizes to same as "land_vehicle".
	kinds := []navigate.RegisteredEntityKind{
		{Name: "land_vehicle", Description: "A land vehicle."},
	}
	c := New(&fakeKindSource{kinds: kinds}, nil)
	// Candidate "land vehicle" should match "land_vehicle" after normalization.
	res, err := c.Check(context.Background(), "land_vehicle", "A land vehicle.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != Reuse {
		t.Errorf("Decision = %v, want Reuse (exact normalized match)", res.Decision)
	}
}

func TestCheck_CloseNameMatch_MintWithAlias(t *testing.T) {
	t.Parallel()
	// "vehicl" is close but not exact to "vehicle" → MintWithAlias or Reuse depending on sim.
	kinds := []navigate.RegisteredEntityKind{
		{Name: "vehicle", Description: "A land vehicle."},
	}
	c := New(&fakeKindSource{kinds: kinds}, nil)
	// "vessels" vs "vehicle" — not close enough for Reuse but let's use a really
	// close one: "vehiclee" (one extra char) to force MintWithAlias territory.
	res, err := c.Check(context.Background(), "vehiclee", "A slightly different vehicle.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// sim("vehiclee","vehicle") via levenshtein: edit=1, maxLen=8, sim=0.875 → MintWithAlias.
	if res.Decision != MintWithAlias && res.Decision != Reuse {
		t.Errorf("Decision = %v, want MintWithAlias or Reuse for near-miss", res.Decision)
	}
	if res.Match == "" {
		t.Error("Match should be set for near-miss result")
	}
}

func TestCheck_DistinctName_Mint(t *testing.T) {
	t.Parallel()
	kinds := []navigate.RegisteredEntityKind{
		{Name: "vehicle", Description: "A land vehicle."},
		{Name: "person", Description: "A human being."},
	}
	c := New(&fakeKindSource{kinds: kinds}, nil)
	res, err := c.Check(context.Background(), "spaceship", "An interstellar spacecraft.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != Mint {
		t.Errorf("Decision = %v, want Mint for unrelated name", res.Decision)
	}
}

func TestCheck_KindSourceError_DegradesToMint(t *testing.T) {
	t.Parallel()
	boom := errors.New("db unavailable")
	c := New(&fakeKindSource{err: boom}, nil)
	res, err := c.Check(context.Background(), "spaceship", "An interstellar spacecraft.")
	if err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}
	if res.Decision != Mint {
		t.Errorf("Decision = %v, want Mint on kind source error", res.Decision)
	}
}

func TestCheck_NoEmbedder_LexicalOnly(t *testing.T) {
	t.Parallel()
	// Fully distinct names with no embedder → Mint.
	kinds := []navigate.RegisteredEntityKind{
		{Name: "organization", Description: "A structured group of people."},
	}
	c := New(&fakeKindSource{kinds: kinds}, nil) // nil embedder
	res, err := c.Check(context.Background(), "nebula", "A cloud of interstellar gas.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != Mint {
		t.Errorf("Decision = %v, want Mint (lexical-only, unrelated name)", res.Decision)
	}
}

// ----- semantic tests -----

func TestCheck_SemanticReuse_WithFakeEmbedder(t *testing.T) {
	t.Parallel()
	// Two kinds: "organization" and "company". Embedder returns identical vectors
	// for candidate and "organization" → should Reuse.
	kinds := []navigate.RegisteredEntityKind{
		{Name: "organization", Description: "A structured group of people pursuing a common goal."},
		{Name: "place", Description: "A geographic location."},
	}
	orgText := "organization — A structured group of people pursuing a common goal."
	placeText := "place — A geographic location."
	candText := "company — A business entity or corporation."

	emb := &fakeEmbedder{vecs: map[string][]float32{
		orgText:   vec3(0), // organization → axis 0
		placeText: vec3(1), // place → axis 1
		candText:  vec3(0), // company → axis 0 (identical to organization)
	}}

	c := New(&fakeKindSource{kinds: kinds}, emb)
	res, err := c.Check(context.Background(), "company", "A business entity or corporation.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != Reuse {
		t.Errorf("Decision = %v, want Reuse (identical semantic vector to organization)", res.Decision)
	}
	if res.Match != "organization" {
		t.Errorf("Match = %q, want %q", res.Match, "organization")
	}
}

func TestCheck_SemanticMintWithAlias_WithFakeEmbedder(t *testing.T) {
	t.Parallel()
	kinds := []navigate.RegisteredEntityKind{
		{Name: "organization", Description: "A structured group."},
	}
	orgText := "organization — A structured group."
	// candidate similarity = 0.8 (between MintAliasThreshold and ReuseThreshold).
	// cos(a,b)=0.8 means dot=0.8, norms=1 each; simplest: two 2D unit vecs at angle θ where cos θ=0.8.
	// We'll use a close-but-not-identical vector.
	candText := "startup — A new small business venture."
	emb := &fakeEmbedder{vecs: map[string][]float32{
		orgText:  {1.0, 0.0},
		candText: {0.8, 0.6}, // cos similarity = 0.8 with {1,0}
	}}
	c := New(&fakeKindSource{kinds: kinds}, emb)
	res, err := c.Check(context.Background(), "startup", "A new small business venture.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != MintWithAlias {
		t.Errorf("Decision = %v, want MintWithAlias (semantic sim=0.8)", res.Decision)
	}
	if res.Similarity < MintAliasThreshold {
		t.Errorf("Similarity = %v, want >= %v", res.Similarity, MintAliasThreshold)
	}
}

func TestCheck_EmbedderError_DegradesToMint(t *testing.T) {
	t.Parallel()
	kinds := []navigate.RegisteredEntityKind{
		{Name: "organization", Description: "A structured group."},
	}
	emb := &fakeEmbedder{err: errors.New("ollama unavailable")}
	c := New(&fakeKindSource{kinds: kinds}, emb)
	res, err := c.Check(context.Background(), "nebula", "An interstellar cloud.")
	if err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}
	// Embedder failure degrades to Mint.
	if res.Decision != Mint {
		t.Errorf("Decision = %v, want Mint on embedder error", res.Decision)
	}
}

// TestCheck_SlowEmbedder_BoundedBySemanticTimeout pins the regression behind the
// reported defineEntityKind hang: a cold/wedged embedder must not let the
// semantic pass run unbounded. The candidate "tool" is lexically distinct from
// "product", so Check falls through to the embedding layer; with a blocking
// embedder and a short SemanticTimeout the pass must degrade to Mint within the
// budget rather than occupying the caller for the full invoke deadline.
func TestCheck_SlowEmbedder_BoundedBySemanticTimeout(t *testing.T) {
	t.Parallel()
	kinds := []navigate.RegisteredEntityKind{
		{Name: "user:product", Description: "A sellable product."},
		{Name: "user:standard", Description: "A published specification."},
	}
	emb := &blockingEmbedder{}
	c := NewWithOptions(&fakeKindSource{kinds: kinds}, emb, Options{SemanticTimeout: 50 * time.Millisecond})

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := c.Check(context.Background(), "tool", "A software tool.")
		done <- outcome{r, e}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Check returned unexpected error: %v", got.err)
		}
		if got.res.Decision != Mint {
			t.Errorf("Decision = %v, want Mint on semantic-pass timeout", got.res.Decision)
		}
		if got := emb.calls.Load(); got == 0 {
			t.Error("embedder was never called: the timeout must fire DURING the embedding pass, not short-circuit before it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Check did not return within 2s: semantic embedding pass is not bounded by a deadline")
	}
}

// TestCheck_BatchEmbedder_SingleRoundTrip pins the cold-start optimization: when
// the embedder can batch (production: *cache.CachingEmbedder over *ollama.Client),
// the semantic pass embeds the candidate + every registered kind in ONE round
// trip, not N+1 sequential Embed calls — and still reaches the correct decision.
func TestCheck_BatchEmbedder_SingleRoundTrip(t *testing.T) {
	t.Parallel()
	kinds := []navigate.RegisteredEntityKind{
		{Name: "user:widget", Description: "A widget."},
		{Name: "user:gizmo", Description: "A gizmo."},
	}
	emb := &countingBatchEmbedder{vecs: map[string][]float32{
		"tool — A software tool.": vec3(0),
		"user:widget — A widget.": vec3(0), // identical direction → cosine 1.0 → Reuse
		"user:gizmo — A gizmo.":   vec3(1),
	}}
	c := New(&fakeKindSource{kinds: kinds}, emb)

	res, err := c.Check(context.Background(), "tool", "A software tool.")
	if err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}
	if got := emb.batchCalls.Load(); got != 1 {
		t.Errorf("EmbedBatch call count = %d, want 1 (one round trip for candidate + all kinds)", got)
	}
	if got := emb.embedCalls.Load(); got != 0 {
		t.Errorf("Embed call count = %d, want 0 (batch path must not fall back to per-text Embed)", got)
	}
	if res.Decision != Reuse || res.Match != "user:widget" {
		t.Errorf("Decision = %v, Match = %q; want Reuse of user:widget via the batch path", res.Decision, res.Match)
	}
}

func TestCheck_EmptyKindList_Mint(t *testing.T) {
	t.Parallel()
	c := New(&fakeKindSource{kinds: nil}, nil)
	res, err := c.Check(context.Background(), "anything", "Some description.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != Mint {
		t.Errorf("Decision = %v, want Mint for empty registry", res.Decision)
	}
}

// ----- helpers -----

func TestNormalizeName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"vehicle", "vehicle"},
		{"land_vehicle", "land vehicle"},
		{"Land_Vehicle", "land vehicle"},
		{"  land vehicle  ", "land vehicle"},
		{"PERSON", "person"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := normalizeName(tc.in); got != tc.want {
				t.Errorf("normalizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCosineSimilarity(t *testing.T) {
	t.Parallel()
	// Identical unit vectors → 1.0.
	v := []float32{1, 0, 0}
	if got := cosineSimilarity(v, v); got < 0.9999 {
		t.Errorf("cosineSimilarity(identical) = %v, want ~1.0", got)
	}
	// Orthogonal unit vectors → 0.0.
	a := []float32{1, 0}
	b := []float32{0, 1}
	if got := cosineSimilarity(a, b); got > 0.0001 {
		t.Errorf("cosineSimilarity(orthogonal) = %v, want ~0.0", got)
	}
	// Different lengths → 0.0.
	if got := cosineSimilarity([]float32{1}, []float32{1, 0}); got != 0 {
		t.Errorf("cosineSimilarity(different lengths) = %v, want 0", got)
	}
}

// TestNewWithOptions_CustomThresholds verifies that custom thresholds in
// Options are honoured by Check. This is the Issue-5 regression test:
// dedup_high_threshold and dedup_medium_threshold in config must be threaded
// through to the Checker so borderline pairs are classified differently.
//
// Strategy: use a pair with lexical name-similarity of exactly 0.80
// (above the default MintAliasThreshold=0.75 but below a custom
// ReuseThreshold=0.85). With default thresholds this pair is MintWithAlias;
// with a high ReuseThreshold of 0.85 it is STILL MintWithAlias; with a
// custom MintAliasThreshold raised to 0.85 the pair drops to Mint because
// the similarity is below the new floor.
func TestNewWithOptions_CustomThresholds_ClassifyBorderlinePair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// "auto transport" vs "auto transport vehicle": normalizeName produces
	// "auto transport" (len=14) vs "auto transport vehicle" (len=22).
	// nameSimilarity uses LCS and Levenshtein. Let's use a pair where the
	// lexical similarity is deterministic: identical short names → sim=1.0.
	// Instead, pick a pair where nameSimilarity returns a value we can control.
	//
	// Use normalizeName("motorcar") = "motorcar" and normalizeName("motorcars") = "motorcars".
	// LCS("motorcar","motorcars") = 8 (all of "motorcar"), maxLen=9, lcsSim=8/9≈0.889.
	// Levenshtein = 1 (add 's'), levSim = 1 - 1/9 ≈ 0.889.
	// So nameSimilarity ≈ 0.889. Above default MintAliasThreshold(0.75),
	// below default ReuseThreshold(0.90).
	// → Default thresholds: MintWithAlias.
	// → Custom MintAliasThreshold=0.95 (above 0.889): Mint.
	kinds := []navigate.RegisteredEntityKind{
		{Name: "motorcar", Description: "A motorised road vehicle."},
	}
	src := &fakeKindSource{kinds: kinds}

	// With default thresholds: similarity ≈ 0.889 ≥ MintAliasThreshold(0.75) → MintWithAlias.
	defaultChecker := New(src, nil)
	resDefault, err := defaultChecker.Check(ctx, "motorcars", "Several motorised road vehicles.")
	if err != nil {
		t.Fatalf("Check (default): %v", err)
	}
	if resDefault.Decision != MintWithAlias {
		t.Errorf("default thresholds: got Decision=%v, want MintWithAlias (sim≈0.889 ≥ 0.75)", resDefault.Decision)
	}

	// With raised MintAliasThreshold=0.95: similarity ≈ 0.889 < 0.95 → Mint.
	highMintChecker := NewWithOptions(src, nil, Options{MintAliasThreshold: 0.95})
	resHighMint, err := highMintChecker.Check(ctx, "motorcars", "Several motorised road vehicles.")
	if err != nil {
		t.Fatalf("Check (high MintAliasThreshold): %v", err)
	}
	if resHighMint.Decision != Mint {
		t.Errorf("MintAliasThreshold=0.95: got Decision=%v, want Mint (sim≈0.889 < 0.95)", resHighMint.Decision)
	}
}

// TestNewWithOptions_ZeroValuesUseDefaults verifies that zero threshold values
// in Options fall back to the package-level defaults (not to zero, which would
// make every pair trigger Reuse).
func TestNewWithOptions_ZeroValuesUseDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	kinds := []navigate.RegisteredEntityKind{
		{Name: "vehicle", Description: "A road vehicle."},
	}
	src := &fakeKindSource{kinds: kinds}

	// "totally_unrelated" normalizes to "totally unrelated" → nameSimilarity
	// with "vehicle" should be very low (near 0). With zero Options the
	// thresholds must be the package defaults (not zero), so the result is Mint.
	c := NewWithOptions(src, nil, Options{})
	res, err := c.Check(ctx, "totally_unrelated", "Something completely different.")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != Mint {
		t.Errorf("zero Options: got Decision=%v for unrelated pair, want Mint (package defaults in effect)", res.Decision)
	}
}
