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

// TestAddIdentifier_HappyPath_DefaultAlias verifies that omitting kind attaches
// the identifier as an alias (the rename-retention case) with source=user, and
// appends exactly one entityIdentifierAdded event.
func TestAddIdentifier_HappyPath_DefaultAlias(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAddIdentifier(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "Erebor Global",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "added" {
		t.Errorf("status = %q, want added", out.Status)
	}
	if out.Name != "entities/"+testEntityUID {
		t.Errorf("name = %q, want entities/%s", out.Name, testEntityUID)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if string(fa.gotEvents[0].Kind) != ontology.KindEntityIdentifierAdded {
		t.Errorf("event kind = %q, want %s", fa.gotEvents[0].Kind, ontology.KindEntityIdentifierAdded)
	}
	var payload map[string]any
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload["kind"] != string(ontology.IdentifierKindAlias) {
		t.Errorf("payload kind = %v, want alias", payload["kind"])
	}
	if payload["source"] != string(ontology.IdentifierSourceUser) {
		t.Errorf("payload source = %v, want user", payload["source"])
	}
	if payload["identifier"] != "Erebor Global" {
		t.Errorf("payload identifier = %v, want Erebor Global", payload["identifier"])
	}
}

// TestAddIdentifier_HappyPath_ExplicitKind verifies a non-default kind passes
// through to the event.
func TestAddIdentifier_HappyPath_ExplicitKind(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAddIdentifier(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "rosie@erebor.example",
		"kind":       "email",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(fa.gotEvents[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["kind"] != "email" {
		t.Errorf("payload kind = %v, want email", payload["kind"])
	}
}

// TestAddIdentifier_NotFound returns E_NOT_FOUND and appends nothing.
func TestAddIdentifier_NotFound(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAddIdentifier(fa, missingEntity())

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  "01HZZZZZZZZZZZZZZZZZZZGONE1",
		"identifier": "Ghost",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on not-found", len(fa.gotEvents))
	}
}

// TestAddIdentifier_Validation_MissingUID rejects an empty entityUid.
func TestAddIdentifier_Validation_MissingUID(t *testing.T) {
	t.Parallel()
	tool := tools.NewAddIdentifier(&fakeAppender{}, nil)
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"identifier":"x"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestAddIdentifier_Validation_EmptyIdentifier rejects a blank identifier.
func TestAddIdentifier_Validation_EmptyIdentifier(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAddIdentifier(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "   ",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on validation failure", len(fa.gotEvents))
	}
}

// TestAddIdentifier_Validation_BadKind rejects a non-snake_case kind.
func TestAddIdentifier_Validation_BadKind(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAddIdentifier(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "x",
		"kind":       "Not Snake",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on validation failure", len(fa.gotEvents))
	}
}

// TestAddIdentifier_AppendError_Internal maps a storage failure to E_INTERNAL.
func TestAddIdentifier_AppendError_Internal(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("disk full")}
	tool := tools.NewAddIdentifier(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "x",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}

// TestAddIdentifier_NilNav_SkipsExistenceCheck appends without a navigator.
func TestAddIdentifier_NilNav_SkipsExistenceCheck(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAddIdentifier(fa, nil)

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "x",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke with nil nav: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
}

// --- B5 scope inheritance tests ---

// TestAddIdentifier_TeamEntity_EventInheritsScope verifies that when the target
// entity lives in team scope (Scope="team" on the navigate.Entity returned by
// GetEntity), the appended entityIdentifierAdded event carries Scope=ScopeTeam
// so team-scope dedup and the AuthorizeTeamWrite gate both fire correctly.
func TestAddIdentifier_TeamEntity_EventInheritsScope(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	getter := &fakeEntityGetter{entity: &navigate.Entity{
		UID:   testEntityUID,
		Kind:  "person",
		Scope: string(kernel.ScopeTeam), // B5: entity lives in team scope
	}}
	tool := tools.NewAddIdentifier(fa, getter)

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "Alias Name",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if fa.gotEvents[0].Scope != kernel.ScopeTeam {
		t.Errorf("event scope = %q, want %q (B5: identifier event must inherit entity scope)",
			fa.gotEvents[0].Scope, kernel.ScopeTeam)
	}
}

// TestAddIdentifier_PersonalEntity_EventScopeEmpty verifies that a personal-
// scope entity (Scope="" or "personal") leaves the event Scope empty so the
// event log backfills personal — the default behaviour is preserved.
func TestAddIdentifier_PersonalEntity_EventScopeEmpty(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	// existingEntity() returns Scope="" → personal
	tool := tools.NewAddIdentifier(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":  testEntityUID,
		"identifier": "Old Name",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fa.gotEvents[0].Scope != "" {
		t.Errorf("event scope = %q, want empty (log backfills personal)", fa.gotEvents[0].Scope)
	}
}
