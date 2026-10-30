package extractor_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// stubProvider is a minimal ChatProvider that returns a canned ToolUse response.
type stubProvider struct {
	response kernel.ToolUseResponse
	err      error
}

func (s *stubProvider) Complete(_ context.Context, _ kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	return kernel.CompleteResponse{}, nil
}

func (s *stubProvider) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	return s.response, s.err
}

// buildRegistry returns a prompts.Registry with the extract-v1 template registered.
func buildRegistry(t *testing.T) prompts.Registry {
	t.Helper()
	reg := prompts.New(nil)
	if err := extractor.RegisterExtractPrompt(context.Background(), reg); err != nil {
		t.Fatalf("RegisterExtractPrompt: %v", err)
	}
	return reg
}

// toolUseInput marshals a rawToolOutput shape for the stub provider.
func toolUseInput(t *testing.T, mentions []map[string]any, relations []map[string]any) json.RawMessage {
	t.Helper()
	payload := map[string]any{
		"mentions":  mentions,
		"relations": relations,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	return raw
}

func TestExtract_SinglePerson_ExactGround(t *testing.T) {
	t.Parallel()
	input := toolUseInput(t,
		[]map[string]any{
			{"kind": "person", "displayName": "Frodo", "extractionText": "Frodo"},
		},
		[]map[string]any{},
	)
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    &kernel.ToolInvocation{Name: "extract_entities_and_relations", Input: input},
			StopReason: "tool_use",
		},
	}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{Model: "test-model"})

	source := "Met with Frodo yesterday to plan the sprint."
	mentions, candidates, err := e.Extract(context.Background(), "EPISODEUID01", source)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(mentions) != 1 {
		t.Fatalf("expected 1 mention, got %d", len(mentions))
	}
	if mentions[0].GroundingScore != 1.0 {
		t.Errorf("expected groundingScore 1.0, got %v", mentions[0].GroundingScore)
	}
	if mentions[0].Kind != ontology.EntityKindPerson {
		t.Errorf("expected kind person, got %v", mentions[0].Kind)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(candidates))
	}
}

func TestExtract_ConfabulatedMention_Dropped(t *testing.T) {
	t.Parallel()
	// The model invents "Gandalf" which does not appear in the source text.
	input := toolUseInput(t,
		[]map[string]any{
			{"kind": "person", "displayName": "Gandalf", "extractionText": "Gandalf"},
		},
		[]map[string]any{},
	)
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    &kernel.ToolInvocation{Name: "extract_entities_and_relations", Input: input},
			StopReason: "tool_use",
		},
	}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{
		Model:              "test-model",
		GroundingThreshold: 0.6,
	})

	source := "The quarterly report looks good. Revenue is up."
	mentions, _, err := e.Extract(context.Background(), "EPISODEUID02", source)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(mentions) != 0 {
		t.Errorf("expected confabulated mention to be dropped, got %d mentions", len(mentions))
	}
}

func TestExtract_NoToolUseResponse(t *testing.T) {
	t.Parallel()
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    nil,
			StopReason: "end_turn",
		},
	}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{Model: "test-model"})
	mentions, candidates, err := e.Extract(context.Background(), "UID", "some text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mentions) != 0 || len(candidates) != 0 {
		t.Errorf("expected empty result when model does not invoke tool")
	}
}

func TestExtract_WithRelations(t *testing.T) {
	t.Parallel()
	input := toolUseInput(t,
		[]map[string]any{
			{"kind": "person", "displayName": "Eowyn", "extractionText": "Eowyn"},
			{"kind": "organization", "displayName": "Rivendell", "extractionText": "Rivendell"},
		},
		[]map[string]any{
			{"subjectMentionIdx": 0, "predicate": "works_at", "objectMentionIdx": 1},
		},
	)
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    &kernel.ToolInvocation{Name: "extract_entities_and_relations", Input: input},
			StopReason: "tool_use",
		},
	}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{Model: "test-model"})

	source := "Eowyn is a software engineer at Rivendell."
	mentions, candidates, err := e.Extract(context.Background(), "EPISODEUID03", source)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(mentions) != 2 {
		t.Fatalf("expected 2 mentions, got %d", len(mentions))
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Predicate != "works_at" {
		t.Errorf("expected predicate works_at, got %q", candidates[0].Predicate)
	}
}

// countingStubProvider counts ToolUse calls so a test can prove that the
// extractor did NOT hit the provider on a short-text skip.
type countingStubProvider struct {
	stubProvider
	calls int
}

func (c *countingStubProvider) ToolUse(ctx context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	c.calls++
	return c.stubProvider.ToolUse(ctx, req)
}

// TestExtractFrom_ShortEpisode_SkipsLLM pins the short-text guard. Episodes
// below cfg.MinEpisodeChars must short-circuit before any ChatProvider call:
// the resolver and the dispatcher have no use for a Mention extracted from
// "thanks!" or "ok cool" and the LLM round-trip is pure waste. A
// LongMemEval-style haystack ingests many such filler turns; the saving is
// material at fan-out.
func TestExtractFrom_ShortEpisode_SkipsLLM(t *testing.T) {
	t.Parallel()
	stub := &countingStubProvider{}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{
		Model:           "test-model",
		MinEpisodeChars: 20,
	})

	epPayload, _ := json.Marshal(map[string]any{"text": "ok thanks", "source": "test"})
	ev := kernel.Event{
		UID:     "01HZSHORTEPISODE0000000001",
		Kind:    kernel.EventKind("episode"),
		Payload: epPayload,
	}
	evts, err := e.ExtractFrom(context.Background(), ev)
	if err != nil {
		t.Fatalf("ExtractFrom: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("ChatProvider.ToolUse called %d times for short episode; expected 0 (short-text skip)", stub.calls)
	}
	if len(evts) != 0 {
		t.Errorf("expected zero events from short-text skip, got %d", len(evts))
	}
}

func TestExtractFrom_NonEpisodeEvent_Skipped(t *testing.T) {
	t.Parallel()
	stub := &stubProvider{}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{Model: "test-model"})

	ev := kernel.Event{
		Kind:    kernel.EventKind("mention"),
		Payload: json.RawMessage(`{"kind":"person","displayName":"Frodo"}`),
	}
	evts, err := e.ExtractFrom(context.Background(), ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 0 {
		t.Errorf("expected no events for non-episode kind, got %d", len(evts))
	}
}

func TestExtractFrom_EpisodeEvent_ProducesMentionEvents(t *testing.T) {
	t.Parallel()
	input := toolUseInput(t,
		[]map[string]any{
			{"kind": "person", "displayName": "Ana", "extractionText": "Ana"},
		},
		[]map[string]any{},
	)
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    &kernel.ToolInvocation{Name: "extract_entities_and_relations", Input: input},
			StopReason: "tool_use",
		},
	}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{Model: "test-model"})

	// Text deliberately exceeds the default MinEpisodeChars guard so the
	// extractor runs the LLM path instead of short-circuiting.
	epPayload, _ := json.Marshal(map[string]any{"text": "Call with Ana about the new project kickoff.", "source": "test"})
	ev := kernel.Event{
		UID:     "01HZ0000000000000000000001",
		Kind:    kernel.EventKind("episode"),
		Payload: epPayload,
	}
	evts, err := e.ExtractFrom(context.Background(), ev)
	if err != nil {
		t.Fatalf("ExtractFrom: %v", err)
	}
	if len(evts) == 0 {
		t.Fatal("expected at least one extraction event")
	}
	// First event should be a mention event.
	if evts[0].Kind != kernel.EventKind(ontology.KindMention) {
		t.Errorf("expected kind %q, got %q", ontology.KindMention, evts[0].Kind)
	}
}

func TestExtract_LongEpisode_Chunks(t *testing.T) {
	t.Parallel()
	// Build a text > 6000 bytes where named entities appear exactly once.
	// The text is: "Arwen works at Mithril Corp. " followed by neutral filler so
	// the episode crosses the chunk threshold but entities are not repeated.
	header := "Arwen works at Mithril Corp. "
	filler := strings.Repeat("This is a filler sentence about nothing in particular. ", 120)
	long := header + filler // ~6630 bytes total

	// Stub always returns Arwen + Mithril Corp from each chunk. After AdjustSpans
	// the first occurrence (chunk 0, offset 0) gives span [0,4) and [15,24).
	// The second chunk's AdjustedSpans give different offsets; Ground finds
	// "Arwen" and "Mithril Corp" again inside the overlapping region.
	// After DeduplicateMentions by (displayName, kind, span), identical spans
	// from the overlap window collapse to a single entry each, but any
	// additional occurrence at a unique span is kept.
	// The assertion: at minimum 2 unique (displayName, kind) entities survive.
	input := toolUseInput(t,
		[]map[string]any{
			{"kind": "person", "displayName": "Arwen", "extractionText": "Arwen"},
			{"kind": "organization", "displayName": "Mithril Corp", "extractionText": "Mithril Corp"},
		},
		[]map[string]any{
			{"subjectMentionIdx": 0, "predicate": "works_at", "objectMentionIdx": 1},
		},
	)
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    &kernel.ToolInvocation{Name: "extract_entities_and_relations", Input: input},
			StopReason: "tool_use",
		},
	}

	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{
		Model:          "test-model",
		ChunkThreshold: 6000,
		ChunkOverlap:   200,
	})
	mentions, _, err := e.Extract(context.Background(), "UID_LONG", long)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// At least 2 mentions must survive (Arwen and Mithril Corp).
	if len(mentions) < 2 {
		t.Errorf("expected >= 2 mentions, got %d", len(mentions))
	}
	// All surviving mentions must be above the default grounding threshold.
	for _, m := range mentions {
		if m.GroundingScore < 0.6 {
			t.Errorf("mention %q has score %v below threshold", m.DisplayName, m.GroundingScore)
		}
	}
}

// TestExtractFrom_DraftScope_PropagatesToDerivedEvents pins that Mention and
// RelationCandidatesProposed events emitted from a draft-scoped episode inherit
// the draft scope. This is the first half of the privacy-guarantee fix: if the
// scope were dropped here, entities and relations would default to personal and
// bypass the draft staging boundary.
func TestExtractFrom_DraftScope_PropagatesToDerivedEvents(t *testing.T) {
	t.Parallel()
	input := toolUseInput(t,
		[]map[string]any{
			{"kind": "person", "displayName": "Sauron", "extractionText": "Sauron"},
		},
		[]map[string]any{
			{"subjectMentionIdx": 0, "predicate": "rules", "objectMentionIdx": 0},
		},
	)
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    &kernel.ToolInvocation{Name: "extract_entities_and_relations", Input: input},
			StopReason: "tool_use",
		},
	}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{Model: "test-model"})

	epPayload, _ := json.Marshal(map[string]any{"text": "Sauron rules Mordor with an iron fist."})
	ev := kernel.Event{
		UID:     "01HZDRAFT0000000000000001",
		Kind:    kernel.EventKind("episode"),
		Scope:   kernel.ScopeDraft,
		Payload: epPayload,
	}
	evts, err := e.ExtractFrom(context.Background(), ev)
	if err != nil {
		t.Fatalf("ExtractFrom: %v", err)
	}
	if len(evts) == 0 {
		t.Fatal("expected at least one derived event from a draft episode")
	}
	for _, derived := range evts {
		if derived.Scope != kernel.ScopeDraft {
			t.Errorf("derived event kind=%q has scope=%q, want %q",
				derived.Kind, derived.Scope, kernel.ScopeDraft)
		}
	}
}

// TestExtractFrom_PersonalScope_PropagatesToDerivedEvents pins that personal-
// scoped episodes produce personal-scoped derived events (regression guard for
// the default path after the draft-scope fix).
func TestExtractFrom_PersonalScope_PropagatesToDerivedEvents(t *testing.T) {
	t.Parallel()
	input := toolUseInput(t,
		[]map[string]any{
			{"kind": "person", "displayName": "Gandalf", "extractionText": "Gandalf"},
		},
		[]map[string]any{},
	)
	stub := &stubProvider{
		response: kernel.ToolUseResponse{
			ToolUse:    &kernel.ToolInvocation{Name: "extract_entities_and_relations", Input: input},
			StopReason: "tool_use",
		},
	}
	reg := buildRegistry(t)
	e := extractor.New(stub, reg, extractor.Config{Model: "test-model"})

	epPayload, _ := json.Marshal(map[string]any{"text": "Gandalf arrived just in time at Helm's Deep."})
	ev := kernel.Event{
		UID:     "01HZPERSONAL000000000001",
		Kind:    kernel.EventKind("episode"),
		Scope:   kernel.ScopePersonal,
		Payload: epPayload,
	}
	evts, err := e.ExtractFrom(context.Background(), ev)
	if err != nil {
		t.Fatalf("ExtractFrom: %v", err)
	}
	if len(evts) == 0 {
		t.Fatal("expected at least one derived event from a personal episode")
	}
	for _, derived := range evts {
		if derived.Scope != kernel.ScopePersonal {
			t.Errorf("derived event kind=%q has scope=%q, want %q",
				derived.Kind, derived.Scope, kernel.ScopePersonal)
		}
	}
}
