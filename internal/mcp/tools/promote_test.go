package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// stubRelationReader is a relationReader the promote tool pre-checks against. It
// returns a fixed relation/status or a fixed error (e.g. navigate.ErrNotFound).
type stubRelationReader struct {
	rel navigate.Relation
	err error
}

func (s stubRelationReader) GetRelation(_ context.Context, _ string) (navigate.Relation, error) {
	return s.rel, s.err
}

// promoteOutput is the decoded promoteRelation response.
type promoteOutput struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Promoted bool   `json:"promoted"`
}

func invokePromote(t *testing.T, tool *tools.PromoteRelation, name string) (promoteOutput, error) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	raw, ierr := tool.Invoke(context.Background(), body)
	if ierr != nil {
		return promoteOutput{}, ierr
	}
	var out promoteOutput
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode output: %v", uerr)
	}
	return out, nil
}

// TestPromoteRelation_FlipsProposed pins the happy path: a proposed edge yields
// one relationPromoted event targeting its uid, and the output reports the flip.
func TestPromoteRelation_FlipsProposed(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	app := &fakeAppender{receipts: []kernel.Receipt{{UID: "evt1"}}}
	reader := stubRelationReader{rel: navigate.Relation{UID: uid, Status: ontology.RelationStatusProposed}}
	tool := tools.NewPromoteRelation(app, reader)

	out, err := invokePromote(t, tool, "relations/"+uid)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Promoted {
		t.Error("promoted = false, want true")
	}
	if out.Status != ontology.RelationStatusAsserted {
		t.Errorf("status = %q, want asserted", out.Status)
	}
	if out.Name != "relations/"+uid {
		t.Errorf("name = %q, want relations/%s", out.Name, uid)
	}
	if len(app.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.gotEvents))
	}
	ev := app.gotEvents[0]
	if ev.Kind != kernel.EventKind(ontology.KindRelationPromoted) {
		t.Errorf("event kind = %q, want relationPromoted", ev.Kind)
	}
	var payload ontology.RelationPromotedEvent
	if uerr := json.Unmarshal(ev.Payload, &payload); uerr != nil {
		t.Fatalf("decode payload: %v", uerr)
	}
	if string(payload.RelationUID) != uid {
		t.Errorf("payload.relationUid = %q, want %q", payload.RelationUID, uid)
	}
	if payload.PromotedAt.IsZero() {
		t.Error("payload.promotedAt is zero")
	}
}

// TestPromoteRelation_AlreadyAsserted_NoEvent pins the idempotent short-circuit:
// promoting an asserted edge appends nothing and reports promoted=false.
func TestPromoteRelation_AlreadyAsserted_NoEvent(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	app := &fakeAppender{receipts: []kernel.Receipt{{UID: "evt1"}}}
	reader := stubRelationReader{rel: navigate.Relation{UID: uid, Status: ontology.RelationStatusAsserted}}
	tool := tools.NewPromoteRelation(app, reader)

	out, err := invokePromote(t, tool, "relations/"+uid)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Promoted {
		t.Error("promoted = true, want false (already asserted)")
	}
	if out.Status != ontology.RelationStatusAsserted {
		t.Errorf("status = %q, want asserted", out.Status)
	}
	if app.called {
		t.Errorf("appender called for an already-asserted edge; want no event (%d appended)", len(app.gotEvents))
	}
}

// TestPromoteRelation_NotFound maps an absent relation to E_NOT_FOUND and
// appends nothing.
func TestPromoteRelation_NotFound(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	app := &fakeAppender{}
	reader := stubRelationReader{err: navigate.ErrNotFound}
	tool := tools.NewPromoteRelation(app, reader)

	_, err := invokePromote(t, tool, "relations/"+uid)
	wantRelateCode(t, err, mcp.CodeNotFound)
	if app.called {
		t.Error("appender called for a not-found relation")
	}
}

// TestPromoteRelation_BadName rejects a non-relation collection and a malformed
// uid as E_VALIDATION before any append.
func TestPromoteRelation_BadName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"entities/" + ulid.Make().String(), "relations/not-a-ulid", ""} {
		app := &fakeAppender{}
		tool := tools.NewPromoteRelation(app, stubRelationReader{})
		_, err := invokePromote(t, tool, name)
		wantRelateCode(t, err, mcp.CodeValidation)
		if app.called {
			t.Errorf("appender called for bad name %q", name)
		}
	}
}

// TestPromoteRelation_BareULIDAndNilReader confirms a bare ULID is accepted and,
// with no reader wired, the tool appends unconditionally (the dispatcher
// projection is idempotent regardless).
func TestPromoteRelation_BareULIDAndNilReader(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	app := &fakeAppender{receipts: []kernel.Receipt{{UID: "evt1"}}}
	tool := tools.NewPromoteRelation(app, nil)

	out, err := invokePromote(t, tool, uid) // bare ULID, no relations/ prefix
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Promoted {
		t.Error("promoted = false, want true (nil reader appends unconditionally)")
	}
	if out.Name != "relations/"+uid {
		t.Errorf("name = %q, want relations/%s", out.Name, uid)
	}
	if len(app.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.gotEvents))
	}
}

// TestPromoteRelation_AppendError surfaces an event-log append failure rather
// than reporting a phantom promotion.
func TestPromoteRelation_AppendError(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	app := &fakeAppender{err: errors.New("event log down")}
	reader := stubRelationReader{rel: navigate.Relation{UID: uid, Status: ontology.RelationStatusProposed}}
	tool := tools.NewPromoteRelation(app, reader)

	if _, err := invokePromote(t, tool, "relations/"+uid); err == nil {
		t.Fatal("expected an error when the append fails")
	}
}

// TestPromoteRelation_Metadata pins the agent-facing surface contract.
func TestPromoteRelation_Metadata(t *testing.T) {
	t.Parallel()
	tool := tools.NewPromoteRelation(&fakeAppender{}, nil)
	if tool.Name() != "promoteRelation" {
		t.Errorf("Name() = %q, want promoteRelation", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	var in map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &in); err != nil {
		t.Fatalf("InputSchema unparseable: %v", err)
	}
	if in["additionalProperties"] != false {
		t.Error("InputSchema does not forbid additionalProperties")
	}
}
