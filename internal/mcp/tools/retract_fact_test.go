package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const testFactUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// retractNav is a configurable Navigator for the retractFact tests. It embeds
// the package's no-op *fakeNav for the long tail of interface methods and
// overrides the three retractFact actually touches: GetEntity (any subject
// resolves), GetFact (single configured fact), and FactsForSubject (slot list).
type retractNav struct {
	*fakeNav
	fact  *navigate.Fact
	facts []navigate.Fact
}

func (n *retractNav) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	return navigate.Entity{UID: uid}, nil
}

func (n *retractNav) GetFact(_ context.Context, _ string) (navigate.Fact, error) {
	if n.fact == nil {
		return navigate.Fact{}, navigate.ErrNotFound
	}
	return *n.fact, nil
}

func (n *retractNav) FactsForSubject(_ context.Context, _ string, _ navigate.FactFilter) ([]navigate.Fact, error) {
	return n.facts, nil
}

// TestRetractFact_ByUID_HappyPath retracts the open fact addressed by uid and
// asserts a single factRetracted event with the resolved factUid.
func TestRetractFact_ByUID_HappyPath(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	nav := &retractNav{fakeNav: &fakeNav{}, fact: &navigate.Fact{UID: testFactUID, Name: "facts/" + testFactUID}}
	tool := tools.NewRetractFact(fa, nav)

	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"fact":"facts/`+testFactUID+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name        string `json:"name"`
		Status      string `json:"status"`
		RetractedAt string `json:"retractedAt"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if out.Status != "retracted" {
		t.Errorf("status = %q, want retracted", out.Status)
	}
	if out.Name != "facts/"+testFactUID {
		t.Errorf("name = %q, want facts/%s", out.Name, testFactUID)
	}
	if out.RetractedAt == "" {
		t.Error("retractedAt should be stamped")
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if string(fa.gotEvents[0].Kind) != ontology.KindFactRetracted {
		t.Errorf("event kind = %q, want %s", fa.gotEvents[0].Kind, ontology.KindFactRetracted)
	}
	var payload ontology.FactRetractedEvent
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload.FactUID != testFactUID {
		t.Errorf("payload factUid = %q, want %s", payload.FactUID, testFactUID)
	}
	if payload.RetractedAt.IsZero() {
		t.Error("payload retractedAt is zero")
	}
}

// TestRetractFact_BySlot_HappyPath resolves the slot's open value by (subject,
// stateKey) and retracts it.
func TestRetractFact_BySlot_HappyPath(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	nav := &retractNav{fakeNav: &fakeNav{}, facts: []navigate.Fact{
		{UID: "01HZZZZZZZZZZZZZZZZZZOTHER1", StateKey: "state.other", Value: "x"},
		{UID: testFactUID, StateKey: "state.task.status", Value: "active"},
	}}
	tool := tools.NewRetractFact(fa, nav)

	req := mustMarshalJSON(t, map[string]any{
		"subject":  testEntityUID,
		"stateKey": "state.task.status",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	var payload ontology.FactRetractedEvent
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload.FactUID != testFactUID {
		t.Errorf("resolved factUid = %q, want %s (the matching open slot)", payload.FactUID, testFactUID)
	}
}

// TestRetractFact_ByUID_NotFound returns E_NOT_FOUND and appends nothing.
func TestRetractFact_ByUID_NotFound(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	nav := &retractNav{fakeNav: &fakeNav{}} // fact nil ⇒ GetFact returns ErrNotFound
	tool := tools.NewRetractFact(fa, nav)

	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"fact":"facts/`+testFactUID+`"}`))
	wantCode(t, err, mcp.CodeNotFound)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0", len(fa.gotEvents))
	}
}

// TestRetractFact_ByUID_AlreadyClosed refuses to re-close a value that is
// already superseded or retracted (E_VALIDATION), appending nothing.
func TestRetractFact_ByUID_AlreadyClosed(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	nav := &retractNav{fakeNav: &fakeNav{}, fact: &navigate.Fact{UID: testFactUID, EventEnd: "2026-01-01T00:00:00Z"}}
	tool := tools.NewRetractFact(fa, nav)

	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"fact":"facts/`+testFactUID+`"}`))
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on already-closed", len(fa.gotEvents))
	}
}

// TestRetractFact_BySlot_Empty returns E_NOT_FOUND when no open value matches.
func TestRetractFact_BySlot_Empty(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	nav := &retractNav{fakeNav: &fakeNav{}, facts: []navigate.Fact{
		{UID: "01HZZZZZZZZZZZZZZZZZZOTHER1", StateKey: "state.other"},
	}}
	tool := tools.NewRetractFact(fa, nav)

	req := mustMarshalJSON(t, map[string]any{"subject": testEntityUID, "stateKey": "state.task.status"})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
}

// TestRetractFact_Validation covers the mutually-exclusive / missing-mode arms.
func TestRetractFact_Validation(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	nav := &retractNav{fakeNav: &fakeNav{}}
	tool := tools.NewRetractFact(fa, nav)

	cases := map[string]string{
		"neither":       `{}`,
		"both":          `{"fact":"facts/` + testFactUID + `","subject":"` + testEntityUID + `","stateKey":"state.x"}`,
		"partial slot":  `{"subject":"` + testEntityUID + `"}`,
		"bad fact name": `{"fact":"relations/` + testFactUID + `"}`,
		"bad fact ulid": `{"fact":"not-a-ulid"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := tool.Invoke(context.Background(), json.RawMessage(body))
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestRetractFact_AppendError maps a storage failure to E_INTERNAL.
func TestRetractFact_AppendError(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("disk full")}
	nav := &retractNav{fakeNav: &fakeNav{}, fact: &navigate.Fact{UID: testFactUID}}
	tool := tools.NewRetractFact(fa, nav)

	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"fact":"facts/`+testFactUID+`"}`))
	wantCode(t, err, mcp.CodeInternal)
}
