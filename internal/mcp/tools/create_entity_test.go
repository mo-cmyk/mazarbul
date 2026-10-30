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

// okAppender returns a single receipt so CreateEntity's "no receipt" guard does
// not fire on the happy path.
func okAppender() *fakeAppender {
	return &fakeAppender{receipts: []kernel.Receipt{{}}}
}

// TestCreateEntity_HappyPath_Created verifies a clean mint appends exactly one
// KindEntity event and returns status=created with an entities/{uid} name.
func TestCreateEntity_HappyPath_Created(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Eowyn",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name   string `json:"name"`
		UID    string `json:"uid"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "created" {
		t.Errorf("status = %q, want created", out.Status)
	}
	if out.Kind != "person" {
		t.Errorf("kind = %q, want person", out.Kind)
	}
	if out.Name != "entities/"+out.UID {
		t.Errorf("name = %q, want entities/%s", out.Name, out.UID)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if string(fa.gotEvents[0].Kind) != "entity" {
		t.Errorf("event kind = %q, want entity", fa.gotEvents[0].Kind)
	}
}

// TestCreateEntity_Scope_FlowsThrough verifies an explicit scope is stamped
// onto the appended KindEntity event so a team-scope mint lands in the team's
// envelope; an omitted scope leaves the event-log default empty.
func TestCreateEntity_Scope_FlowsThrough(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Rosie Cotton",
		"scope":       "team",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if got := fa.gotEvents[0].Scope; got != kernel.ScopeTeam {
		t.Errorf("event scope = %q, want %q", got, kernel.ScopeTeam)
	}

	def := okAppender()
	defTool := tools.NewCreateEntity(def, nil)
	reqDef, _ := json.Marshal(map[string]any{"kind": "person", "displayName": "Rosie Cotton"})
	if _, err := defTool.Invoke(context.Background(), reqDef); err != nil {
		t.Fatalf("Invoke(default): %v", err)
	}
	if got := def.gotEvents[0].Scope; got != "" {
		t.Errorf("default event scope = %q, want empty", got)
	}
}

// TestCreateEntity_BadScope_Validation rejects an unknown scope value with
// E_VALIDATION before any event is appended.
func TestCreateEntity_BadScope_Validation(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)
	req := json.RawMessage(`{"kind":"person","displayName":"X","scope":"nope"}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on validation failure", len(fa.gotEvents))
	}
}

// TestCreateEntity_Identifiers_AnchorEvents verifies one entityIdentifierAdded
// event is appended per identifier alongside the entity event.
func TestCreateEntity_Identifiers_AnchorEvents(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "user:vehicle",
		"displayName": "Eowyn's Tesla",
		"identifiers": []map[string]string{
			{"type": "id", "value": "VIN-123"},
			{"type": "email", "value": "eowyn@mithril.example"},
		},
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// 1 entity + 2 identifier events.
	if len(fa.gotEvents) != 3 {
		t.Fatalf("appended %d events, want 3 (entity + 2 identifiers)", len(fa.gotEvents))
	}
	if string(fa.gotEvents[1].Kind) != "entityIdentifierAdded" {
		t.Errorf("event[1] kind = %q, want entityIdentifierAdded", fa.gotEvents[1].Kind)
	}
}

// TestCreateEntity_InvalidKind_Validation rejects a malformed kind.
func TestCreateEntity_InvalidKind_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewCreateEntity(okAppender(), nil)

	for _, bad := range []string{"Vehicle", "1vehicle", "user:", ":deal", ""} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"kind":        bad,
				"displayName": "X",
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestCreateEntity_DocumentKind_Validation rejects the reserved core kind
// "document" with E_VALIDATION and appends nothing — minting a bare document
// entity here would strand a body-less ghost document. A namespaced
// "user:document" is a distinct extension kind and must NOT be caught by this
// guard.
func TestCreateEntity_DocumentKind_Validation(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)
	req, _ := json.Marshal(map[string]any{"kind": "document", "displayName": "Some Spec"})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 (document kind must be refused)", len(fa.gotEvents))
	}

	// A namespaced extension kind that merely contains "document" is allowed.
	fa2 := okAppender()
	tool2 := tools.NewCreateEntity(fa2, nil)
	req2, _ := json.Marshal(map[string]any{"kind": "user:document", "displayName": "My Doc Kind"})
	if _, err := tool2.Invoke(context.Background(), req2); err != nil {
		t.Fatalf("user:document should be allowed, got %v", err)
	}
	if len(fa2.gotEvents) == 0 {
		t.Error("user:document minted no events, want at least the entity event")
	}
}

// TestCreateEntity_EmptyDisplayName_Validation rejects a missing displayName
// (direct dispatch bypasses the JSON-schema required check).
func TestCreateEntity_EmptyDisplayName_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewCreateEntity(okAppender(), nil)
	req := json.RawMessage(`{"kind":"person","displayName":""}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestCreateEntity_BadConfidence_Validation rejects out-of-range confidence.
func TestCreateEntity_BadConfidence_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewCreateEntity(okAppender(), nil)
	req := json.RawMessage(`{"kind":"person","displayName":"X","confidence":1.5}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestCreateEntity_NonObjectProperties_Validation rejects a scalar properties.
func TestCreateEntity_NonObjectProperties_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewCreateEntity(okAppender(), nil)
	req := json.RawMessage(`{"kind":"person","displayName":"X","properties":"not-an-object"}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestCreateEntity_AppendError_Internal maps an appender failure to E_INTERNAL.
func TestCreateEntity_AppendError_Internal(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("storage unavailable")}
	tool := tools.NewCreateEntity(fa, nil)
	req := json.RawMessage(`{"kind":"person","displayName":"X"}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}

// fakeKindLister stubs the kind-existence read surface CreateEntity uses.
type fakeKindLister struct {
	kinds []navigate.RegisteredEntityKind
}

func (f *fakeKindLister) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	return f.kinds, nil
}

// TestCreateEntity_UnknownKind_NotFound rejects an unregistered extension kind
// when a registry is wired (a typo must not mint under a phantom type).
func TestCreateEntity_UnknownKind_NotFound(t *testing.T) {
	t.Parallel()
	lister := &fakeKindLister{kinds: []navigate.RegisteredEntityKind{{Name: "user:vehicle"}}}
	tool := tools.NewCreateEntity(okAppender(), lister)
	req := json.RawMessage(`{"kind":"user:vehcile","displayName":"typo"}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
}

// TestCreateEntity_RegisteredKind_Created accepts a registered extension kind,
// and core kinds are accepted without consulting the registry.
func TestCreateEntity_RegisteredKind_Created(t *testing.T) {
	t.Parallel()
	lister := &fakeKindLister{kinds: []navigate.RegisteredEntityKind{{Name: "user:vehicle"}}}
	tool := tools.NewCreateEntity(okAppender(), lister)
	req := json.RawMessage(`{"kind":"user:vehicle","displayName":"Eowyn's Tesla"}`)
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("registered kind should mint: %v", err)
	}
	// A core kind is exempt from the registry lookup.
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"kind":"person","displayName":"X"}`)); err != nil {
		t.Fatalf("core kind should mint without registry hit: %v", err)
	}
}

// --- link-reconciler tests (G6: wikilink reconcile on createEntity) ---

// fakeLinkReconciler stubs the linkReconciler interface so tests can assert
// that ReconcileForEntity is called with the correct entity UID and names,
// and that a reconcile error does not propagate to the caller.
type fakeLinkReconciler struct {
	calledWithUID   string
	calledWithNames []string
	err             error
	callCount       int
}

func (f *fakeLinkReconciler) ReconcileForEntity(_ context.Context, entityUID string, names []string) (int, error) {
	f.callCount++
	f.calledWithUID = entityUID
	f.calledWithNames = names
	return 0, f.err
}

// TestCreateEntity_ReconcilerCalled verifies that after a successful mint the
// wired linkReconciler is invoked with the new entity UID and a names slice
// containing at least the displayName.
func TestCreateEntity_ReconcilerCalled(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	rec := &fakeLinkReconciler{}
	tool := tools.NewCreateEntity(fa, nil).WithLinkReconciler(rec)

	req, _ := json.Marshal(map[string]any{
		"kind":           "person",
		"displayName":    "Gandalf",
		"alternateNames": []string{"Mithrandir"},
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		UID string `json:"uid"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if rec.callCount != 1 {
		t.Fatalf("ReconcileForEntity called %d times, want 1", rec.callCount)
	}
	if rec.calledWithUID != out.UID {
		t.Errorf("ReconcileForEntity uid = %q, want %q", rec.calledWithUID, out.UID)
	}
	// names must include both displayName and alternateNames.
	nameSet := make(map[string]bool, len(rec.calledWithNames))
	for _, n := range rec.calledWithNames {
		nameSet[n] = true
	}
	if !nameSet["Gandalf"] {
		t.Errorf("names %v missing displayName %q", rec.calledWithNames, "Gandalf")
	}
	if !nameSet["Mithrandir"] {
		t.Errorf("names %v missing alternateName %q", rec.calledWithNames, "Mithrandir")
	}
}

// TestCreateEntity_ReconcilerError_DoesNotFail verifies that a reconcile error
// does NOT propagate to the caller — the entity is already created and the
// result must be returned successfully.
func TestCreateEntity_ReconcilerError_DoesNotFail(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	rec := &fakeLinkReconciler{err: errors.New("reconcile boom")}
	tool := tools.NewCreateEntity(fa, nil).WithLinkReconciler(rec)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Saruman",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke returned error on reconcile failure: %v (must be best-effort)", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "created" {
		t.Errorf("status = %q, want created even when reconcile errors", out.Status)
	}
}

// TestCreateEntity_NoReconciler_NilSafe verifies that a tool with no reconciler
// wired (nil) still operates normally — nil reconciler is the documented safe
// degradation for minimal boot or tests that don't need reconcile.
func TestCreateEntity_NoReconciler_NilSafe(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil) // no WithLinkReconciler call

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Frodo",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke with nil reconciler: %v", err)
	}
}

// --- identifier-kind preservation tests (Greptile fix: identifierKindFor) ---

// TestCreateEntity_Identifier_DomainTypePreserved verifies that an unrecognised
// but valid snake_case identifier type (hrb) is preserved verbatim as the
// IdentifierKind in the entityIdentifierAdded event, so the resolver's lookup
// lane is not collapsed into the generic "id" kind.
func TestCreateEntity_Identifier_DomainTypePreserved(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "organization",
		"displayName": "Mithril Corp",
		"identifiers": []map[string]string{
			{"type": "hrb", "value": "HRB 12345 B"},
		},
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// event[0] = entity, event[1] = entityIdentifierAdded
	if len(fa.gotEvents) != 2 {
		t.Fatalf("expected 2 events (entity + identifier), got %d", len(fa.gotEvents))
	}
	var payload ontology.EntityIdentifierAddedEvent
	if uerr := json.Unmarshal(fa.gotEvents[1].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal entityIdentifierAdded payload: %v", uerr)
	}
	if payload.Kind != ontology.IdentifierKind("hrb") {
		t.Errorf("identifier Kind = %q, want hrb", payload.Kind)
	}
}

// TestCreateEntity_Identifier_CanonicalTypeNormalizes verifies that a
// canonical type supplied in a non-canonical casing (EMAIL) is normalised to
// its IdentifierKind constant (email).
func TestCreateEntity_Identifier_CanonicalTypeNormalizes(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Eowyn",
		"identifiers": []map[string]string{
			{"type": "EMAIL", "value": "eowyn@mithril.example"},
		},
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 2 {
		t.Fatalf("expected 2 events, got %d", len(fa.gotEvents))
	}
	var payload ontology.EntityIdentifierAddedEvent
	if uerr := json.Unmarshal(fa.gotEvents[1].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload.Kind != ontology.IdentifierKindEmail {
		t.Errorf("identifier Kind = %q, want %q", payload.Kind, ontology.IdentifierKindEmail)
	}
}

// TestCreateEntity_Identifier_MalformedTypeRejected verifies that an identifier
// type that does not normalise to snake_case (Bad-Type!) is rejected with
// E_VALIDATION before any event is appended.
func TestCreateEntity_Identifier_MalformedTypeRejected(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Eowyn",
		"identifiers": []map[string]string{
			{"type": "Bad-Type!", "value": "some-value"},
		},
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// --- B5 scope inheritance tests ---

// TestCreateEntity_TeamScope_IdentifierEventsInheritScope verifies that when
// createEntity is called with scope=team and one or more identifiers, every
// appended event — the entity event AND all entityIdentifierAdded events —
// carries Scope=kernel.ScopeTeam. Before the B5 fix the identifier events
// were appended without a Scope field, routing them to the personal scope
// envelope and breaking team-scope dedup + the AuthorizeTeamWrite gate.
func TestCreateEntity_TeamScope_IdentifierEventsInheritScope(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Eowyn",
		"scope":       "team",
		"identifiers": []map[string]string{
			{"type": "email", "value": "eowyn@mithril.example"},
			{"type": "alias", "value": "Shield-Maiden"},
		},
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// 1 entity event + 2 identifier events.
	if len(fa.gotEvents) != 3 {
		t.Fatalf("expected 3 events (entity + 2 identifiers), got %d", len(fa.gotEvents))
	}
	for i, ev := range fa.gotEvents {
		if ev.Scope != kernel.ScopeTeam {
			t.Errorf("event[%d] scope = %q, want %q (B5: all events must carry team scope)",
				i, ev.Scope, kernel.ScopeTeam)
		}
	}
}

// TestCreateEntity_PersonalScope_IdentifierEventsEmptyScope verifies that a
// personal-scope entity mint leaves all event Scope fields empty so the event
// log backfills personal — the default behaviour is preserved.
func TestCreateEntity_PersonalScope_IdentifierEventsEmptyScope(t *testing.T) {
	t.Parallel()
	fa := okAppender()
	tool := tools.NewCreateEntity(fa, nil)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Merry",
		"identifiers": []map[string]string{
			{"type": "alias", "value": "Master Brandybuck"},
		},
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 2 {
		t.Fatalf("expected 2 events, got %d", len(fa.gotEvents))
	}
	for i, ev := range fa.gotEvents {
		if ev.Scope != "" {
			t.Errorf("event[%d] scope = %q, want empty (log backfills personal)", i, ev.Scope)
		}
	}
}
