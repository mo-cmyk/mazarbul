package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// fakeInsightNav is an insightNavigator double for the Insight Inbox tools.
type fakeInsightNav struct {
	list    []navigate.Insight
	listErr error
	get     *navigate.Insight
	getErr  error
}

func (f *fakeInsightNav) ListInsights(context.Context, string, int) ([]navigate.Insight, error) {
	return f.list, f.listErr
}

func (f *fakeInsightNav) GetInsight(_ context.Context, _ string) (navigate.Insight, error) {
	switch {
	case f.getErr != nil:
		return navigate.Insight{}, f.getErr
	case f.get == nil:
		return navigate.Insight{}, fmt.Errorf("insight: %w", navigate.ErrNotFound)
	default:
		return *f.get, nil
	}
}

// captureInsightAppender records appended events so a test can assert the exact
// insightDismissed event (proving dismiss is an append, never an in-place edit).
type captureInsightAppender struct {
	events []kernel.Event
	err    error
}

func (c *captureInsightAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	if c.err != nil {
		return nil, c.err
	}
	c.events = append(c.events, events...)
	return []kernel.Receipt{{UID: "ev_dismiss"}}, nil
}

// --- listInsights ---

// TestListInsights_ListsWithCitations verifies the happy path: rows project with
// a source count and entities/{uid} citations, and the tool name is pinned.
func TestListInsights_ListsWithCitations(t *testing.T) {
	t.Parallel()
	nav := &fakeInsightNav{list: []navigate.Insight{{
		UID:               "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Statement:         "Frodo trusts Sam.",
		InsightType:       "pattern",
		State:             "hot",
		CurrentConfidence: 0.9,
		SourceUIDs:        []string{"ent1", "ent2"},
	}}}
	tool := NewListInsights(nav)
	if tool.Name() != "listInsights" {
		t.Fatalf("Name = %q, want listInsights", tool.Name())
	}
	body, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out listInsightsOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if len(out.Insights) != 1 {
		t.Fatalf("insights len = %d, want 1", len(out.Insights))
	}
	item := out.Insights[0]
	if item.Name != "insights/01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("Name = %q", item.Name)
	}
	if item.SourceCount != 2 {
		t.Errorf("SourceCount = %d, want 2", item.SourceCount)
	}
	if len(item.SourceCitations) != 2 ||
		item.SourceCitations[0] != "entities/ent1" || item.SourceCitations[1] != "entities/ent2" {
		t.Errorf("SourceCitations = %v, want [entities/ent1 entities/ent2]", item.SourceCitations)
	}
}

// TestListInsights_BadState_Validation pins that an out-of-enum state is
// E_VALIDATION (the CLI / in-process path bypasses the JSON-schema enum).
func TestListInsights_BadState_Validation(t *testing.T) {
	t.Parallel()
	tool := NewListInsights(&fakeInsightNav{})
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"state":"bogus"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestListInsights_BadScope_Validation pins that an unknown scope is E_VALIDATION.
func TestListInsights_BadScope_Validation(t *testing.T) {
	t.Parallel()
	tool := NewListInsights(&fakeInsightNav{})
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"scopes":["bogus"]}`))
	wantCode(t, err, mcp.CodeValidation)
}

// --- dismissInsight ---

// TestDismissInsight_AppendsEvent is the core write-path guarantee: dismissing a
// known insight appends exactly one insightDismissed event carrying the right
// uid/reason — and NEVER an in-place UPDATE (the tool's only mutation is Append).
func TestDismissInsight_AppendsEvent(t *testing.T) {
	t.Parallel()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	app := &captureInsightAppender{}
	nav := &fakeInsightNav{get: &navigate.Insight{UID: uid, State: "hot", Scope: "personal"}}
	tool := NewDismissInsight(app, nav)
	if tool.Name() != "dismissInsight" {
		t.Fatalf("Name = %q, want dismissInsight", tool.Name())
	}

	before := time.Now().UTC()
	body, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"name":"insights/`+uid+`","reason":"incorrect inference"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want exactly 1 insightDismissed", len(app.events))
	}
	ev := app.events[0]
	if ev.Kind != kernel.EventKind(ontology.KindInsightDismissed) {
		t.Errorf("event kind = %q, want %q", ev.Kind, ontology.KindInsightDismissed)
	}
	if ev.Scope != kernel.Scope("personal") {
		t.Errorf("event scope = %q, want personal (the insight's scope)", ev.Scope)
	}
	var payload ontology.InsightDismissedEvent
	if uerr := json.Unmarshal(ev.Payload, &payload); uerr != nil {
		t.Fatalf("decode payload: %v", uerr)
	}
	if string(payload.InsightUID) != uid {
		t.Errorf("payload InsightUID = %q, want %q", payload.InsightUID, uid)
	}
	if payload.Reason != "incorrect inference" {
		t.Errorf("payload Reason = %q", payload.Reason)
	}
	if payload.DismissedAt.Before(before) || payload.DismissedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("payload DismissedAt = %v, want a recent UTC instant", payload.DismissedAt)
	}

	var resp dismissInsightOutput
	if uerr := json.Unmarshal(body, &resp); uerr != nil {
		t.Fatalf("decode response: %v", uerr)
	}
	if resp.Name != "insights/"+uid || resp.Status != "dismissed" {
		t.Errorf("response = %+v, want {insights/%s dismissed}", resp, uid)
	}
}

// TestDismissInsight_UnknownUID_NotFound verifies a well-formed but unknown uid
// is E_NOT_FOUND and that NO event is appended for a missing insight.
func TestDismissInsight_UnknownUID_NotFound(t *testing.T) {
	t.Parallel()
	app := &captureInsightAppender{}
	tool := NewDismissInsight(app, &fakeInsightNav{}) // get nil → ErrNotFound
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"name":"insights/01ARZ3NDEKTSV4RRFFQ69G5FAV"}`))
	wantCode(t, err, mcp.CodeNotFound)
	if len(app.events) != 0 {
		t.Errorf("appended %d events for an unknown insight, want 0", len(app.events))
	}
}

// TestDismissInsight_BadArg_Validation pins the bad-input contract: an empty
// name and a wrong-collection resource name are both E_VALIDATION, with no append.
func TestDismissInsight_BadArg_Validation(t *testing.T) {
	t.Parallel()
	for _, req := range []string{
		`{"name":""}`,
		`{"name":"episodes/01ARZ3NDEKTSV4RRFFQ69G5FAV"}`,
	} {
		app := &captureInsightAppender{}
		tool := NewDismissInsight(app, &fakeInsightNav{get: &navigate.Insight{UID: "x", State: "hot"}})
		_, err := tool.Invoke(context.Background(), json.RawMessage(req))
		wantCode(t, err, mcp.CodeValidation)
		if len(app.events) != 0 {
			t.Errorf("req %s appended an event; bad input must not write", req)
		}
	}
}

// TestDismissInsight_AlreadyDismissed_NoDuplicate verifies re-dismissing a
// terminal insight is an idempotent no-op: status dismissed, no second event.
func TestDismissInsight_AlreadyDismissed_NoDuplicate(t *testing.T) {
	t.Parallel()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	app := &captureInsightAppender{}
	nav := &fakeInsightNav{get: &navigate.Insight{UID: uid, State: "dismissed", Scope: "personal"}}
	tool := NewDismissInsight(app, nav)
	body, err := tool.Invoke(context.Background(), json.RawMessage(`{"name":"insights/`+uid+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 0 {
		t.Errorf("re-dismiss appended %d events, want 0 (idempotent)", len(app.events))
	}
	var resp dismissInsightOutput
	if uerr := json.Unmarshal(body, &resp); uerr != nil {
		t.Fatalf("decode response: %v", uerr)
	}
	if resp.Status != "dismissed" {
		t.Errorf("status = %q, want dismissed", resp.Status)
	}
}

// TestDismissInsight_AppendPermission_Mapped verifies a kernel.ErrPermission
// append failure maps to E_PERMISSION through mapAppendError, not E_INTERNAL.
func TestDismissInsight_AppendPermission_Mapped(t *testing.T) {
	t.Parallel()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	app := &captureInsightAppender{err: fmt.Errorf("team write: %w", kernel.ErrPermission)}
	nav := &fakeInsightNav{get: &navigate.Insight{UID: uid, State: "hot", Scope: "team"}}
	tool := NewDismissInsight(app, nav)
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"name":"insights/`+uid+`"}`))
	wantCode(t, err, mcp.CodePermission)
}

// TestInsightUIDFromArg covers the resource-name / bare-uid parsing.
func TestInsightUIDFromArg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg     string
		want    string
		wantErr bool
	}{
		{"insights/abc", "abc", false},
		{"abc", "abc", false},
		{"", "", true},
		{"episodes/abc", "", true},
	}
	for _, c := range cases {
		got, err := insightUIDFromArg(c.arg)
		if c.wantErr {
			if err == nil {
				t.Errorf("insightUIDFromArg(%q): want error", c.arg)
			}
			continue
		}
		if err != nil {
			t.Errorf("insightUIDFromArg(%q): %v", c.arg, err)
		}
		if got != c.want {
			t.Errorf("insightUIDFromArg(%q) = %q, want %q", c.arg, got, c.want)
		}
	}
}

// ensure the fake satisfies the narrow interface (compile-time guard).
var _ insightNavigator = (*fakeInsightNav)(nil)

// ensure the capture appender satisfies Appender.
var _ Appender = (*captureInsightAppender)(nil)
