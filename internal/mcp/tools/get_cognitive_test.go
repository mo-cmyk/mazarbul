package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// TestGet_Insight_HappyPath verifies that get(insights/{uid}) returns type=insight
// with the insight fields and source citations typed as entities/{uid}.
func TestGet_Insight_HappyPath(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{insight: &navigate.Insight{
		UID:                   "ins1",
		Statement:             "Merry prefers the Shire.",
		InsightType:           "preference",
		SourceUIDs:            []string{"ent1", "ent2"},
		SeedEntityUID:         "ent1",
		SynthesizedConfidence: 0.9,
		CurrentConfidence:     0.9,
		State:                 "hot",
		SynthesizedAt:         "2026-06-17T10:00:00Z",
	}})
	body, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"insights/ins1"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if out.Type != "insight" {
		t.Fatalf("type = %q, want insight", out.Type)
	}
	if out.Insight == nil {
		t.Fatal("insight arm is nil")
	}
	if out.Insight.Statement != "Merry prefers the Shire." {
		t.Errorf("Statement = %q", out.Insight.Statement)
	}
	if out.Insight.State != "hot" {
		t.Errorf("State = %q, want hot", out.Insight.State)
	}
	if len(out.Insight.SourceCitations) != 2 {
		t.Fatalf("SourceCitations len = %d, want 2", len(out.Insight.SourceCitations))
	}
	if out.Insight.SourceCitations[0] != "entities/ent1" || out.Insight.SourceCitations[1] != "entities/ent2" {
		t.Errorf("SourceCitations = %v, want [entities/ent1 entities/ent2]", out.Insight.SourceCitations)
	}
	// No other arms populated.
	if out.Entity != nil || out.Episode != nil || out.Relation != nil || out.ConsolidatedMemory != nil {
		t.Fatalf("non-insight arms must be nil: %s", body)
	}
}

// TestGet_Insight_DemotedStillFetchable confirms that a demoted insight (state=demoted)
// is returned by get — get is not the hot-recall index.
func TestGet_Insight_DemotedStillFetchable(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{insight: &navigate.Insight{
		UID:         "ins_demoted",
		Statement:   "Sam is a gardener.",
		InsightType: "pattern",
		SourceUIDs:  []string{"ent3"},
		State:       "demoted",
	}})
	body, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"insights/ins_demoted"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if out.Type != "insight" || out.Insight == nil {
		t.Fatalf("expected type=insight, got %s", body)
	}
	if out.Insight.State != "demoted" {
		t.Errorf("State = %q, want demoted", out.Insight.State)
	}
}

// TestGet_Insight_UnknownUID_NotFound verifies that a well-formed insights/{uid}
// name whose uid does not exist returns E_NOT_FOUND.
func TestGet_Insight_UnknownUID_NotFound(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{}) // insight nil → ErrNotFound
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"insights/ghost"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestGet_Insight_OutOfScope_NotFound verifies scope fail-closed: an insight whose
// navigator returns ErrNotFound (simulating an out-of-scope row) surfaces E_NOT_FOUND,
// never leaking a row that exists in a different scope.
func TestGet_Insight_OutOfScope_NotFound(t *testing.T) {
	t.Parallel()
	// fakeNavigator with insight=nil models an out-of-scope row (scope predicate
	// excluded it, same as ErrNotFound from the real navigator).
	g := NewGet(&fakeNavigator{})
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"insights/ins_team","scopes":["personal"]}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestGet_ConsolidatedMemory_HappyPath verifies that get(consolidatedMemories/{uid})
// returns type=consolidatedMemory with source citations typed as episodes/{uid}.
func TestGet_ConsolidatedMemory_HappyPath(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{consolidatedMemory: &navigate.ConsolidatedMemory{
		UID:            "cm1",
		Summary:        "Merry's activities in the Shire.",
		MemoryType:     "semantic",
		SourceUIDs:     []string{"ep1", "ep2"},
		CitedEntityUID: "ent1",
		Confidence:     0.85,
		State:          "hot",
		ConsolidatedAt: "2026-06-17T10:00:00Z",
	}})
	body, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"consolidatedMemories/cm1"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if out.Type != "consolidatedMemory" {
		t.Fatalf("type = %q, want consolidatedMemory", out.Type)
	}
	if out.ConsolidatedMemory == nil {
		t.Fatal("consolidatedMemory arm is nil")
	}
	if out.ConsolidatedMemory.Summary != "Merry's activities in the Shire." {
		t.Errorf("Summary = %q", out.ConsolidatedMemory.Summary)
	}
	if len(out.ConsolidatedMemory.SourceCitations) != 2 {
		t.Fatalf("SourceCitations len = %d, want 2", len(out.ConsolidatedMemory.SourceCitations))
	}
	if out.ConsolidatedMemory.SourceCitations[0] != "episodes/ep1" || out.ConsolidatedMemory.SourceCitations[1] != "episodes/ep2" {
		t.Errorf("SourceCitations = %v, want [episodes/ep1 episodes/ep2]", out.ConsolidatedMemory.SourceCitations)
	}
	// No other arms populated.
	if out.Entity != nil || out.Episode != nil || out.Relation != nil || out.Insight != nil {
		t.Fatalf("non-consolidatedMemory arms must be nil: %s", body)
	}
}

// TestGet_ConsolidatedMemory_UnknownUID_NotFound verifies E_NOT_FOUND for
// a well-formed consolidatedMemories/{uid} name that does not exist.
func TestGet_ConsolidatedMemory_UnknownUID_NotFound(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{}) // consolidatedMemory nil → ErrNotFound
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"consolidatedMemories/ghost"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestGet_ConsolidatedMemory_OutOfScope_NotFound verifies scope fail-closed for
// consolidated memories: an out-of-scope uid returns E_NOT_FOUND.
func TestGet_ConsolidatedMemory_OutOfScope_NotFound(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{})
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"consolidatedMemories/cm_team","scopes":["personal"]}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestGet_InsightName_ValidationPattern verifies that the updated input schema
// pattern accepts insights/{uid} and consolidatedMemories/{uid} names, and
// still rejects unknown collections.
func TestGet_InsightName_ValidationPattern(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{})

	// Valid collections that must pass schema validation (may return E_NOT_FOUND
	// from the fake, not E_VALIDATION).
	validNames := []string{
		`{"name":"insights/01ARZ3NDEKTSV4RRFFQ69G5FAV"}`,
		`{"name":"consolidatedMemories/01ARZ3NDEKTSV4RRFFQ69G5FAV"}`,
	}
	for _, req := range validNames {
		_, err := g.Invoke(context.Background(), json.RawMessage(req))
		// E_NOT_FOUND is expected (uid not in fake store). E_VALIDATION means the
		// collection prefix was rejected, which must not happen.
		var te *mcp.ToolError
		if errors.As(err, &te) && te.Code == mcp.CodeValidation {
			t.Errorf("name %s must not be E_VALIDATION (schema should accept it): %v", req, err)
		}
	}

	// A string whose prefix is not a known collection ("widgets/abc") is treated
	// as a display name now (the slash-in-name fix), so an absent one is
	// E_NOT_FOUND rather than the old E_VALIDATION "unknown collection".
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"widgets/abc"}`))
	wantCode(t, err, mcp.CodeNotFound)
}
