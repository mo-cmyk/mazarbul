package composer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// fakeChatProvider is a minimal kernel.ChatProvider for tests. It records the
// system prompt of the most recent Complete call so tests can assert on the
// rendered template body.
type fakeChatProvider struct {
	response   string
	err        error
	lastSystem string
	calls      int
}

func (f *fakeChatProvider) Complete(_ context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	f.calls++
	f.lastSystem = req.System
	if f.err != nil {
		return kernel.CompleteResponse{}, f.err
	}
	return kernel.CompleteResponse{Content: f.response, StopReason: "end_turn"}, nil
}

func (f *fakeChatProvider) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	return kernel.ToolUseResponse{}, errors.New("fakeChatProvider: ToolUse not implemented")
}

// mustRegistry builds a prompts.Registry with the compose-v1 template already
// registered. Fails the test immediately on any setup error.
func mustRegistry(t *testing.T) *prompts.MemoryRegistry {
	t.Helper()
	reg := prompts.New(nil)
	if err := composer.RegisterComposePrompt(context.Background(), reg); err != nil {
		t.Fatalf("RegisterComposePrompt: %v", err)
	}
	return reg
}

// TestRegisterComposePrompt_VersionPinned pins the prompt-registry version
// alongside the constant in prompt.go. Without this pin the constant can
// drift independently of callers that fetch a specific version, and the
// compose lane silently degrades to whatever the registry happened to
// expose. Bump this constant in lockstep with composeTemplateVersion.
func TestRegisterComposePrompt_VersionPinned(t *testing.T) {
	t.Parallel()
	const wantVersion = 12
	reg := prompts.New(nil)
	ctx := context.Background()

	if err := composer.RegisterComposePrompt(ctx, reg); err != nil {
		t.Fatalf("RegisterComposePrompt: %v", err)
	}
	tpl, err := reg.Get(ctx, "compose-v1", wantVersion)
	if err != nil {
		t.Fatalf("Get compose-v1 v%d: %v", wantVersion, err)
	}
	if int(tpl.Version) != wantVersion {
		t.Errorf("registered version = %d, want %d", tpl.Version, wantVersion)
	}
}

func TestCompose_WithHits(t *testing.T) {
	now := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	hits := []kernel.Hit{
		{
			UID:        "01JFRODO00000000000000001",
			Snippet:    "Frodo Baggins works at Erebor Inc.",
			Confidence: 0.9,
			CreateTime: now,
			FusedScore: 0.8,
		},
	}

	reg := mustRegistry(t)
	chat := &fakeChatProvider{response: "Frodo Baggins works at Erebor Inc. [1]."}

	c, err := composer.New(
		composer.Config{Model: "llama3.1:8b"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ans, err := c.Compose(context.Background(), "Who works at Erebor?", hits)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	if ans.Answer == "" {
		t.Error("expected non-empty Answer")
	}
	if len(ans.Citations) != 1 {
		t.Errorf("expected 1 citation, got %d", len(ans.Citations))
	}
	if ans.Citations[0].EpisodeUID != hits[0].UID {
		t.Errorf("citation UID: got %q, want %q", ans.Citations[0].EpisodeUID, hits[0].UID)
	}
	if ans.Citations[0].Index != 1 {
		t.Errorf("citation index: got %d, want 1", ans.Citations[0].Index)
	}
}

func TestCompose_NoHits_Abstain(t *testing.T) {
	reg := mustRegistry(t)
	chat := &fakeChatProvider{response: "I don't have enough information to answer this question."}

	c, err := composer.New(
		composer.Config{Model: "llama3.1:8b"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ans, err := c.Compose(context.Background(), "Who works at Erebor?", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	if ans.Answer == "" {
		t.Error("expected non-empty Answer even on abstain")
	}
	if len(ans.Citations) != 0 {
		t.Errorf("expected 0 citations on empty hits, got %d", len(ans.Citations))
	}
}

// TestCompose_AbstainGate_BelowThresholdSkipsLLM is the Phase 2
// contract test. When AbstainThreshold > 0 and the best hit's
// FusedScore falls below the threshold, Compose returns
// AbstainSentinel verbatim WITHOUT calling the ChatProvider. The
// no-call assertion is the load-bearing part: in production, every
// avoided LLM call is one less chance for qwen2.5:7b to confabulate.
func TestCompose_AbstainGate_BelowThresholdSkipsLLM(t *testing.T) {
	t.Parallel()
	reg := mustRegistry(t)
	chat := &fakeChatProvider{response: "should never be returned"}

	c, err := composer.New(
		composer.Config{Model: "llama3.1:8b", AbstainThreshold: 0.18},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	weak := []kernel.Hit{{
		UID:        "01JWEAK000000000000000001",
		Snippet:    "tangential",
		FusedScore: 0.05,
		CreateTime: time.Now().UTC(),
	}}
	ans, err := c.Compose(context.Background(), "Where do I live?", weak)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if ans.Answer != composer.AbstainSentinel {
		t.Errorf("Answer = %q, want %q", ans.Answer, composer.AbstainSentinel)
	}
	if chat.calls != 0 {
		t.Errorf("ChatProvider called %d times, want 0 (hard floor must skip LLM)", chat.calls)
	}
	if len(ans.Citations) != 0 {
		t.Errorf("expected 0 citations on abstain, got %d", len(ans.Citations))
	}
	// Citations must be a non-nil empty slice (not nil) so the JSON
	// marshalling round-trip emits `"citations": []` rather than
	// `"citations": null`. The MCP ask tool's OutputSchema declares
	// citations as required type=array; null would fail jsonschema
	// validation client-side. Discovered via LongMemEval eval crashing
	// with `None is not of type 'array'` on every question that hit
	// the abstain gate.
	if ans.Citations == nil {
		t.Errorf("Citations is nil; must be empty []Citation{} so JSON marshals as [] not null (MCP ask schema requires array)")
	}
}

// TestCompose_AbstainGate_AboveThresholdCallsLLM is the negative
// half of the contract: when the top score clears the threshold the
// LLM is consulted and the normal compose path runs. Without this
// pin, a misconfigured threshold of 1.0 would silently abstain on
// every query and the abstention "lift" would be a false reading.
func TestCompose_AbstainGate_AboveThresholdCallsLLM(t *testing.T) {
	t.Parallel()
	reg := mustRegistry(t)
	chat := &fakeChatProvider{response: "Lucas [1]."}

	c, err := composer.New(
		composer.Config{Model: "llama3.1:8b", AbstainThreshold: 0.18},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	strong := []kernel.Hit{{
		UID:        "01JSTRONG00000000000000001",
		Snippet:    "My name is Lucas.",
		FusedScore: 0.85,
		CreateTime: time.Now().UTC(),
	}}
	ans, err := c.Compose(context.Background(), "What is my name?", strong)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if chat.calls != 1 {
		t.Errorf("ChatProvider called %d times, want 1 (above-threshold must invoke LLM)", chat.calls)
	}
	if ans.Answer == composer.AbstainSentinel {
		t.Errorf("Answer was the abstain sentinel even though top score 0.85 ≥ threshold 0.18")
	}
}

// TestCompose_AbstainGate_DisabledByZeroThreshold confirms backward
// compatibility: the Phase 0 baseline pipeline (AbstainThreshold == 0)
// must behave identically to v11 — no hard-floor refusals, every query
// reaches the LLM. The ablation row in REPORT.history.jsonl depends on
// being able to toggle this surface off cleanly.
func TestCompose_AbstainGate_DisabledByZeroThreshold(t *testing.T) {
	t.Parallel()
	reg := mustRegistry(t)
	chat := &fakeChatProvider{response: "I don't know."}

	c, err := composer.New(
		composer.Config{Model: "llama3.1:8b"}, // AbstainThreshold default 0
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ans, err := c.Compose(context.Background(), "Where do I live?", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if chat.calls != 1 {
		t.Errorf("ChatProvider called %d times, want 1 (threshold=0 must NOT short-circuit)", chat.calls)
	}
	if ans.Answer == composer.AbstainSentinel {
		t.Errorf("Answer was the abstain sentinel with threshold disabled; gate must not fire")
	}
}

func TestCompose_ChatError(t *testing.T) {
	reg := mustRegistry(t)
	wantErr := errors.New("provider unavailable")
	chat := &fakeChatProvider{err: wantErr}

	c, err := composer.New(
		composer.Config{Model: "llama3.1:8b"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Compose(context.Background(), "Who works at Erebor?", nil)
	if err == nil {
		t.Fatal("expected error from chat provider, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("errors.Is: want %v in chain, got %v", wantErr, err)
	}
}

func TestCompose_MaxContextHitsCap(t *testing.T) {
	now := time.Now().UTC()
	hits := make([]kernel.Hit, 20)
	for i := range hits {
		hits[i] = kernel.Hit{
			UID:        "01JFRODO" + string(rune('A'+i)) + "0000000000000000",
			Snippet:    "some text",
			CreateTime: now,
		}
	}

	reg := mustRegistry(t)

	// The chat provider echoes back nothing; we only care about citation count.
	chat := &fakeChatProvider{response: "answer"}

	c, err := composer.New(
		composer.Config{Model: "m", MaxContextHits: 5},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ans, err := c.Compose(context.Background(), "q", hits)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(ans.Citations) != 5 {
		t.Errorf("expected 5 citations (capped), got %d", len(ans.Citations))
	}
}

// TestCompose_TemporalReasoning_RendersTodaysDateAndArithmeticGuidance is the
// regression guard for the LongMemEval temporal-reasoning ability. The
// majority of these questions are date arithmetic between user events ("how
// many days passed", "how many weeks ago"). Without an explicit "today's
// date" anchor and worked date-arithmetic examples in the compose prompt,
// small models (qwen2.5:7b in particular) silently fall back to either
// guessing or abstaining, which scored 0/5 on the 20q probe. The fix is
// surgical: thread a CurrentDate through Compose, render TODAY'S DATE into
// the system prompt, and add a DATE ARITHMETIC section with two worked
// examples. This test fails before that fix and passes after.
func TestCompose_TemporalReasoning_RendersTodaysDateAndArithmeticGuidance(t *testing.T) {
	hits := []kernel.Hit{
		{
			UID:        "01JCHANDELIER0000000000001",
			Snippet:    "Met my aunt today; she gave me the crystal chandelier she promised.",
			Confidence: 0.9,
			CreateTime: time.Date(2023, 3, 4, 14, 0, 0, 0, time.UTC),
			FusedScore: 0.8,
		},
	}

	reg := mustRegistry(t)
	chat := &fakeChatProvider{response: "About 4 weeks ago [1]."}

	c, err := composer.New(
		composer.Config{Model: "qwen2.5:7b"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	currentDate := time.Date(2023, 4, 1, 8, 9, 0, 0, time.UTC)
	_, err = c.ComposeWithCurrentDate(
		context.Background(),
		"How many weeks ago did I meet up with my aunt and receive the crystal chandelier?",
		hits,
		currentDate,
	)
	if err != nil {
		t.Fatalf("ComposeWithCurrentDate: %v", err)
	}

	// The rendered system prompt must carry today's date so the model knows
	// the anchor for "weeks ago" arithmetic.
	if !strings.Contains(chat.lastSystem, "2023-04-01") {
		t.Errorf("rendered prompt missing TODAY'S DATE 2023-04-01:\n%s", chat.lastSystem)
	}
	// The rendered system prompt must carry an explicit DATE ARITHMETIC
	// section so the model knows it is allowed to subtract dates from the
	// context entries rather than abstain.
	if !strings.Contains(chat.lastSystem, "DATE ARITHMETIC") {
		t.Errorf("rendered prompt missing DATE ARITHMETIC section:\n%s", chat.lastSystem)
	}
	// And at least one worked example tying a "weeks ago" question back to
	// an episode createTime so the model has a pattern to copy.
	if !strings.Contains(chat.lastSystem, "weeks ago") {
		t.Errorf("rendered prompt missing 'weeks ago' worked example:\n%s", chat.lastSystem)
	}
}

// TestCompose_TemporalCurrency_NoSupersededMarkers covers the case the
// user reported on 2026-05-29: two self-statements about the same subject
// ("my name is Steve" then "my name is Lucas") landed in the event log
// but the extractor didn't promote either into a has_name relation, so
// the resolver never set the Superseded flag on the older episode. With
// no [SUPERSEDED] marker the LLM had no signal that Lucas (newer
// createTime) superseded Steve, and qwen2.5:7b answered "Steve" as the
// current name.
//
// The fix is a new prompt clause that says: when the question is
// present-tense and the context has conflicting facts about the same
// subject WITHOUT a [SUPERSEDED] marker, treat the entry with the
// LATEST createTime as the current truth.
//
// This test pins the prompt clause's wording so it can't silently drop
// out of a future rewrite. It fails before the clause lands and passes
// after.
func TestCompose_TemporalCurrency_NoSupersededMarkers(t *testing.T) {
	hits := []kernel.Hit{
		{
			UID:        "01JNAME0000000000000STEVE1",
			Snippet:    "My name is Steve.",
			Confidence: 0.9,
			CreateTime: time.Date(2026, 5, 29, 9, 28, 52, 0, time.UTC),
			FusedScore: 0.42,
		},
		{
			UID:        "01JNAME0000000000000LUCAS1",
			Snippet:    "I changed my name to Lucas.",
			Confidence: 0.9,
			CreateTime: time.Date(2026, 5, 29, 9, 43, 17, 0, time.UTC),
			FusedScore: 0.40,
		},
	}

	reg := mustRegistry(t)
	chat := &fakeChatProvider{response: "Your current name is Lucas [2]."}

	c, err := composer.New(
		composer.Config{Model: "qwen2.5:7b"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Compose(context.Background(), "What is my name?", hits); err != nil {
		t.Fatalf("Compose: %v", err)
	}

	// The rendered system prompt must carry an explicit clause covering
	// the no-marker case so the LLM has a recency rule to apply when the
	// resolver hasn't supersede-flagged anything.
	want := []string{
		"IMPLICIT CURRENCY",
		"LATEST createTime",
		"without",
		"[SUPERSEDED]",
	}
	for _, w := range want {
		if !strings.Contains(chat.lastSystem, w) {
			t.Errorf("rendered prompt missing %q from the implicit-currency clause:\n%s", w, chat.lastSystem)
		}
	}
}

func TestNew_MissingModel(t *testing.T) {
	reg := mustRegistry(t)
	_, err := composer.New(composer.Config{}, &fakeChatProvider{}, reg, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestNew_NilChat(t *testing.T) {
	reg := mustRegistry(t)
	_, err := composer.New(composer.Config{Model: "m"}, nil, reg, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for nil chat")
	}
}

func TestFormatContextBlock_Empty(t *testing.T) {
	block, citations := composer.FormatContextBlock(nil, 400)
	if block == "" {
		t.Error("expected non-empty block for nil hits")
	}
	if len(citations) != 0 {
		t.Errorf("expected 0 citations for nil hits, got %d", len(citations))
	}
}

func TestFormatContextBlock_RendersSupersededMarker(t *testing.T) {
	// Two entries: the first carries a current fact, the second is
	// flagged superseded by the HybridQuery layer (a later remember
	// invalidated one of its derived relations). The render layer must
	// emit composer.SupersededMarker right after the superseded snippet
	// so the compose-v1 CURRENCY rule can demote it on present-tense
	// questions. The marker must NOT appear on the non-superseded entry.
	hits := []kernel.Hit{
		{
			UID:        "01JROSE000000000000000NEW1",
			Snippet:    "Eowyn joined Rivendell in 2025.",
			CreateTime: time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			UID:        "01JROSE000000000000000OLD1",
			Snippet:    "Eowyn works at Erebor Inc. in Berlin.",
			CreateTime: time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC),
			Superseded: true,
		},
	}
	block, citations := composer.FormatContextBlock(hits, 400)
	if !strings.Contains(block, composer.SupersededMarker) {
		t.Errorf("expected %q in rendered block; got:\n%s", composer.SupersededMarker, block)
	}
	if strings.Count(block, composer.SupersededMarker) != 1 {
		t.Errorf("expected exactly 1 %q marker; got %d in block:\n%s",
			composer.SupersededMarker,
			strings.Count(block, composer.SupersededMarker),
			block,
		)
	}
	// The marker must follow the superseded entry's snippet, not the
	// non-superseded one. Concretely: the substring up to "[2]" should
	// not contain the marker (entry 1 is current), and the substring
	// from "[2]" onwards should.
	entry2 := strings.Index(block, "[2]")
	if entry2 < 0 {
		t.Fatalf("expected [2] entry in block:\n%s", block)
	}
	if strings.Contains(block[:entry2], composer.SupersededMarker) {
		t.Errorf("marker leaked into the non-superseded [1] entry:\n%s", block)
	}
	if !strings.Contains(block[entry2:], composer.SupersededMarker) {
		t.Errorf("marker missing from the superseded [2] entry:\n%s", block)
	}
	if len(citations) != 2 {
		t.Errorf("expected 2 citations, got %d", len(citations))
	}
}

// TestFormatContextBlock_MarksLatestEntry reproduces the user-reported
// regression on 2026-05-29: even with the IMPLICIT CURRENCY prompt
// clause, qwen2.5:7b kept reading citation NUMBER as recency
// ("most recently Steve [2]" when Anette was actually newest).
// The LLM needs a stronger positional signal independent of fused
// score. The render layer now appends a literal "[LATEST]" marker
// after the snippet of the entry with the highest createTime. The
// compose-v1 prompt's RECENCY MARKER section keys off this token to
// short-circuit position-based recency guessing.
func TestFormatContextBlock_MarksLatestEntry(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	hits := []kernel.Hit{
		{UID: "01OLD0000000000000000000A1", Snippet: "older fact", CreateTime: now.Add(-2 * time.Hour)},
		{UID: "01NEW0000000000000000000B1", Snippet: "newest fact", CreateTime: now},
		{UID: "01MID0000000000000000000C1", Snippet: "middle-aged fact", CreateTime: now.Add(-1 * time.Hour)},
	}
	block, citations := composer.FormatContextBlock(hits, 400)

	if !strings.Contains(block, "[LATEST]") {
		t.Fatalf("expected [LATEST] marker for the newest hit; got:\n%s", block)
	}
	if strings.Count(block, "[LATEST]") != 1 {
		t.Errorf("expected exactly 1 [LATEST] marker; got %d in:\n%s",
			strings.Count(block, "[LATEST]"), block)
	}
	if len(citations) != 3 {
		t.Errorf("expected 3 citations, got %d", len(citations))
	}

	// The marker must follow entry [2] (the newest hit by createTime),
	// not [1] or [3].
	entry2 := strings.Index(block, "[2]")
	entry3 := strings.Index(block, "[3]")
	latestPos := strings.Index(block, "[LATEST]")
	if entry2 < 0 || entry3 < 0 {
		t.Fatalf("expected entries [2] and [3] in block:\n%s", block)
	}
	if latestPos < entry2 || latestPos > entry3 {
		t.Errorf("[LATEST] should sit between [2] and [3] (i.e. on the newest=B hit); got pos %d, [2]@%d, [3]@%d\nblock:\n%s",
			latestPos, entry2, entry3, block)
	}
}

// TestFormatContextBlock_NoLatestMarkerForSingleHit confirms the
// [LATEST] marker only fires when there are 2+ hits — a single hit
// has no recency competition so the marker adds noise.
func TestFormatContextBlock_NoLatestMarkerForSingleHit(t *testing.T) {
	hits := []kernel.Hit{
		{UID: "01ALONE000000000000000000A", Snippet: "only entry", CreateTime: time.Now().UTC()},
	}
	block, _ := composer.FormatContextBlock(hits, 400)
	if strings.Contains(block, "[LATEST]") {
		t.Errorf("single-hit block should not carry [LATEST]; got:\n%s", block)
	}
}

func TestFormatContextBlock_SnippetCap(t *testing.T) {
	long := make([]rune, 600)
	for i := range long {
		long[i] = 'a'
	}
	hits := []kernel.Hit{
		{UID: "01JFRODO00000000000000001", Snippet: string(long), CreateTime: time.Now().UTC()},
	}
	_, citations := composer.FormatContextBlock(hits, 100)
	if len(citations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(citations))
	}
	// Snippet in citation must be capped.
	if len([]rune(citations[0].Snippet)) > 100 {
		t.Errorf("snippet not capped: got %d runes, want <= 100", len([]rune(citations[0].Snippet)))
	}
}
