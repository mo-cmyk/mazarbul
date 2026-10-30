package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// fakeCardBuilder records the CardRequest it received and returns a canned brief,
// so the tool's plumbing (resolution, anchor parsing, include/scope threading)
// is exercised without a live assembler/recall stack.
type fakeCardBuilder struct {
	gotReq contextbrief.CardRequest
	brief  contextbrief.Brief
	err    error
}

func (f *fakeCardBuilder) EntityCard(_ context.Context, _ contextbrief.CardReader, req contextbrief.CardRequest) (contextbrief.Brief, error) {
	f.gotReq = req
	if f.err != nil {
		return contextbrief.Brief{}, f.err
	}
	return f.brief, nil
}

func frodoNav() *fakeNavigator {
	return &fakeNavigator{entities: []navigate.Entity{
		{UID: "frodo", Kind: "person", DisplayName: "Frodo Baggins"},
	}}
}

func TestGetEntityContext_HappyPath_ResolvesAndPassesRequest(t *testing.T) {
	t.Parallel()
	fb := &fakeCardBuilder{brief: contextbrief.Brief{
		Markdown: "# Frodo Baggins\n", Tokens: 5, Budget: 2000, Dropped: 1,
		Sections: []contextbrief.SectionStat{{Section: "Connections", Items: 2}},
	}}
	tool := NewGetEntityContext(fb, frodoNav())

	req, _ := json.Marshal(map[string]any{"entity": "Frodo Baggins", "tokenBudget": 2000})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fb.gotReq.UID != "frodo" {
		t.Errorf("builder got UID %q, want frodo (display name should resolve)", fb.gotReq.UID)
	}
	if fb.gotReq.BudgetTokens != 2000 {
		t.Errorf("builder got budget %d, want 2000", fb.gotReq.BudgetTokens)
	}

	var out contextbrief.Brief
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Markdown != "# Frodo Baggins\n" || out.Tokens != 5 || out.Budget != 2000 || out.Dropped != 1 {
		t.Errorf("brief round-trip mismatch: %+v", out)
	}
	if len(out.Sections) != 1 || out.Sections[0].Section != "Connections" {
		t.Errorf("sections lost in round-trip: %+v", out.Sections)
	}
}

func TestGetEntityContext_ThreadsIncludeAndAt(t *testing.T) {
	t.Parallel()
	fb := &fakeCardBuilder{}
	tool := NewGetEntityContext(fb, frodoNav())
	req, _ := json.Marshal(map[string]any{
		"entity":  "frodo",
		"include": []string{"facts", "connections"},
		"at":      "2024-01-02T03:04:05Z",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fb.gotReq.Include) != 2 || fb.gotReq.Include[0] != "facts" {
		t.Errorf("include not threaded: %+v", fb.gotReq.Include)
	}
	if fb.gotReq.At == nil || fb.gotReq.At.Format("2006-01-02T15:04:05Z07:00") != "2024-01-02T03:04:05Z" {
		t.Errorf("at not threaded as UTC RFC3339: %v", fb.gotReq.At)
	}
}

func TestGetEntityContext_UnknownEntity_NotFound(t *testing.T) {
	t.Parallel()
	tool := NewGetEntityContext(&fakeCardBuilder{}, &fakeNavigator{})
	req, _ := json.Marshal(map[string]any{"entity": "Sauron"})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
}

func TestGetEntityContext_AmbiguousName_Validation(t *testing.T) {
	t.Parallel()
	// Two entities share the exact display name → resolveEntityArg returns
	// E_VALIDATION naming the candidates.
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: "sam1", Kind: "person", DisplayName: "Sam"},
		{UID: "sam2", Kind: "person", DisplayName: "Sam"},
	}}
	tool := NewGetEntityContext(&fakeCardBuilder{}, nav)
	req, _ := json.Marshal(map[string]any{"entity": "Sam"})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

func TestGetEntityContext_BadAt_Validation(t *testing.T) {
	t.Parallel()
	tool := NewGetEntityContext(&fakeCardBuilder{}, frodoNav())
	req, _ := json.Marshal(map[string]any{"entity": "frodo", "at": "not-a-date"})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

func TestGetEntityContext_UnknownIncludeSection_Validation(t *testing.T) {
	t.Parallel()
	tool := NewGetEntityContext(&fakeCardBuilder{}, frodoNav())
	req, _ := json.Marshal(map[string]any{"entity": "frodo", "include": []string{"bogus"}})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

func TestGetEntityContext_EmptyEntity_Validation(t *testing.T) {
	t.Parallel()
	tool := NewGetEntityContext(&fakeCardBuilder{}, frodoNav())
	req, _ := json.Marshal(map[string]any{"entity": ""})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

func TestGetEntityContext_RejectsBadJSON(t *testing.T) {
	t.Parallel()
	tool := NewGetEntityContext(&fakeCardBuilder{}, frodoNav())
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"tokenBudget":"nope"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestGetEntityContext_DrainsBackgroundOnlyWhenMentionsRequested mirrors
// getContext's drain gate: the Mentioned-in lane runs the same HybridQuery as
// recall, so a card that includes it must drain async work first; a
// connections/facts-only card must skip the wait.
func TestGetEntityContext_DrainsBackgroundOnlyWhenMentionsRequested(t *testing.T) {
	t.Parallel()
	t.Run("default include drains", func(t *testing.T) {
		w := &fakeBgWaiter{}
		tool := NewGetEntityContext(&fakeCardBuilder{}, frodoNav()).WithBackgroundWaiter(w)
		req, _ := json.Marshal(map[string]any{"entity": "frodo"})
		if _, err := tool.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if w.calls != 1 {
			t.Errorf("waiter calls = %d, want 1 (mentions in default card)", w.calls)
		}
	})

	t.Run("facts-only card skips drain", func(t *testing.T) {
		w := &fakeBgWaiter{}
		tool := NewGetEntityContext(&fakeCardBuilder{}, frodoNav()).WithBackgroundWaiter(w)
		req, _ := json.Marshal(map[string]any{"entity": "frodo", "include": []string{"facts"}})
		if _, err := tool.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if w.calls != 0 {
			t.Errorf("waiter calls = %d, want 0 (no mentions lane)", w.calls)
		}
	})
}
