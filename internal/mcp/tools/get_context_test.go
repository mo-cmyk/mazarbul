package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
)

type fakeBriefBuilder struct {
	gotReq contextbrief.Request
	brief  contextbrief.Brief
}

func (f *fakeBriefBuilder) Build(_ context.Context, req contextbrief.Request) (contextbrief.Brief, error) {
	f.gotReq = req
	return f.brief, nil
}

func TestGetContext_PassesRequestAndReturnsBrief(t *testing.T) {
	fb := &fakeBriefBuilder{brief: contextbrief.Brief{
		Markdown: "# Context\n", Tokens: 3, Budget: 1000, Dropped: 2,
		Sections: []contextbrief.SectionStat{{Section: "Pinned", Items: 1}},
	}}
	tool := NewGetContext(fb)
	req, _ := json.Marshal(map[string]any{"query": "Q3 launch", "budgetTokens": 1000})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if fb.gotReq.Query != "Q3 launch" || fb.gotReq.BudgetTokens != 1000 {
		t.Errorf("builder got %+v, want query=Q3 launch budget=1000", fb.gotReq)
	}

	var out contextbrief.Brief
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Markdown != "# Context\n" || out.Tokens != 3 || out.Budget != 1000 || out.Dropped != 2 {
		t.Errorf("brief round-trip mismatch: %+v", out)
	}
	if len(out.Sections) != 1 || out.Sections[0].Section != "Pinned" {
		t.Errorf("sections lost in round-trip: %+v", out.Sections)
	}
}

func TestGetContext_RejectsBadJSON(t *testing.T) {
	tool := NewGetContext(&fakeBriefBuilder{})
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"budgetTokens": "nope"}`))
	if err == nil {
		t.Fatal("expected a validation error for a non-integer budgetTokens")
	}
}

type fakeBgWaiter struct{ calls int }

func (f *fakeBgWaiter) WaitForBackgroundWork(_ context.Context) error {
	f.calls++
	return nil
}

func TestGetContext_DrainsBackgroundOnlyForQueriedBrief(t *testing.T) {
	// A queried brief reads the recall lane, so it must drain async extraction +
	// resolution first (same gate recall uses); a query-less brief reads only
	// kept projections and must skip the wait.
	t.Run("queried brief drains", func(t *testing.T) {
		w := &fakeBgWaiter{}
		tool := NewGetContext(&fakeBriefBuilder{}).WithBackgroundWaiter(w)
		req, _ := json.Marshal(map[string]any{"query": "Rosie"})
		if _, err := tool.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if w.calls != 1 {
			t.Errorf("waiter calls = %d, want 1 for a queried brief", w.calls)
		}
	})

	t.Run("query-less brief skips drain", func(t *testing.T) {
		w := &fakeBgWaiter{}
		tool := NewGetContext(&fakeBriefBuilder{}).WithBackgroundWaiter(w)
		req, _ := json.Marshal(map[string]any{})
		if _, err := tool.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if w.calls != 0 {
			t.Errorf("waiter calls = %d, want 0 for a query-less brief", w.calls)
		}
	})
}
