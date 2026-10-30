package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const refTestURI = "https://github.com/org/repo/issues/42"

// referenceResponse mirrors the reference tool's output schema.
type referenceResponse struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	URI     string `json:"uri"`
	Scope   string `json:"scope"`
	Created bool   `json:"created"`
}

// TestReference_MintsExternalRef asserts a fresh URI mints an external_ref
// entity carrying a {type:"uri"} identifier, defaults to personal scope, and
// reports created=true with the deterministic resource name.
func TestReference_MintsExternalRef(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewReference(app, &fakeNavigator{}) // empty nav → GetEntity ErrNotFound → mint

	out, err := r.Invoke(context.Background(), json.RawMessage(`{"uri":"`+refTestURI+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// KindEntity + one entityIdentifierAdded (the {type:"uri"} anchor).
	if len(app.events) != 2 {
		t.Fatalf("appended %d events, want 2 (entity + identifier)", len(app.events))
	}
	if app.events[0].Kind != kernel.EventKind(ontology.KindEntity) {
		t.Errorf("event[0] kind = %q, want %s", app.events[0].Kind, ontology.KindEntity)
	}
	var ent ontology.Entity
	if uerr := json.Unmarshal(app.events[0].Payload, &ent); uerr != nil {
		t.Fatalf("unmarshal entity: %v", uerr)
	}
	if ent.Kind != ontology.EntityKindExternalRef {
		t.Errorf("entity kind = %q, want external_ref", ent.Kind)
	}
	if len(ent.Identifiers) != 1 || ent.Identifiers[0].Type != "uri" || ent.Identifiers[0].Value != refTestURI {
		t.Errorf("identifiers = %+v, want one {uri, %s}", ent.Identifiers, refTestURI)
	}
	if ent.DisplayName != refTestURI {
		t.Errorf("displayName = %q, want the URI (no --label)", ent.DisplayName)
	}

	resp := decodeReference(t, out)
	wantUID := string(ontology.ExternalRefUID("personal", refTestURI))
	if !resp.Created {
		t.Error("created = false, want true on first mint")
	}
	if resp.UID != wantUID {
		t.Errorf("uid = %q, want %q (personal-derived)", resp.UID, wantUID)
	}
	if resp.Name != "entities/"+wantUID {
		t.Errorf("name = %q, want entities/%s", resp.Name, wantUID)
	}
	if resp.Scope != "personal" {
		t.Errorf("scope = %q, want personal (default)", resp.Scope)
	}
	if resp.URI != refTestURI {
		t.Errorf("uri = %q, want %q", resp.URI, refTestURI)
	}
}

// TestReference_LabelOverridesDisplayName asserts an explicit label becomes the
// entity's display name instead of the raw URI.
func TestReference_LabelOverridesDisplayName(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewReference(app, &fakeNavigator{})

	if _, err := r.Invoke(context.Background(), json.RawMessage(
		`{"uri":"`+refTestURI+`","label":"issue #42"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var ent ontology.Entity
	if uerr := json.Unmarshal(app.events[0].Payload, &ent); uerr != nil {
		t.Fatalf("unmarshal entity: %v", uerr)
	}
	if ent.DisplayName != "issue #42" {
		t.Errorf("displayName = %q, want the label", ent.DisplayName)
	}
}

// TestReference_BadURI_Validation asserts a missing, blank, or scheme-less URI
// is E_VALIDATION and appends nothing.
func TestReference_BadURI_Validation(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing":   `{}`,
		"blank":     `{"uri":"   "}`,
		"no scheme": `{"uri":"github.com/org/repo/issues/42"}`,
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := &captureAppender{}
			r := NewReference(app, &fakeNavigator{})
			_, err := r.Invoke(context.Background(), json.RawMessage(req))
			wantReferenceCode(t, err, mcp.CodeValidation)
			if len(app.events) != 0 {
				t.Errorf("appended %d events on invalid input, want 0", len(app.events))
			}
		})
	}
}

// TestReference_ReturnsExistingWithinScope asserts a repeat call for the same
// URI in the same scope returns the existing entity (created=false) and appends
// nothing — the mint-or-return contract.
func TestReference_ReturnsExistingWithinScope(t *testing.T) {
	t.Parallel()
	uid := string(ontology.ExternalRefUID("personal", refTestURI))
	app := &captureAppender{}
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: uid, Kind: "external_ref", DisplayName: refTestURI},
	}}
	r := NewReference(app, nav)

	out, err := r.Invoke(context.Background(), json.RawMessage(`{"uri":"`+refTestURI+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 0 {
		t.Errorf("appended %d events, want 0 (returned existing)", len(app.events))
	}
	resp := decodeReference(t, out)
	if resp.Created {
		t.Error("created = true, want false (existing entity returned)")
	}
	if resp.Name != "entities/"+uid {
		t.Errorf("name = %q, want entities/%s", resp.Name, uid)
	}
}

// TestReference_CrossScopeDistinct asserts the same URI in personal vs project
// scope derives two distinct entities, so a personal pointer never collapses
// into the project-scope entity that renders to git.
func TestReference_CrossScopeDistinct(t *testing.T) {
	t.Parallel()
	personalUID := string(ontology.ExternalRefUID("personal", refTestURI))
	projectUID := string(ontology.ExternalRefUID("project", refTestURI))
	if personalUID == projectUID {
		t.Fatalf("personal and project derived the same uid %q", personalUID)
	}

	app := &captureAppender{}
	r := NewReference(app, &fakeNavigator{})
	out, err := r.Invoke(context.Background(), json.RawMessage(
		`{"uri":"`+refTestURI+`","scope":"project"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	resp := decodeReference(t, out)
	if resp.Scope != "project" {
		t.Errorf("scope = %q, want project", resp.Scope)
	}
	if resp.Name != "entities/"+projectUID {
		t.Errorf("name = %q, want entities/%s (project-derived)", resp.Name, projectUID)
	}
}

// TestReference_HasPrimarySourceRelateUnchanged proves the issue's zero-diff
// claim functionally: relate(localEntity, has_primary_source, externalRef)
// succeeds because the external_ref is a real local ULID — no relate.go change.
func TestReference_HasPrimarySourceRelateUnchanged(t *testing.T) {
	t.Parallel()
	const localUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	refUID := string(ontology.ExternalRefUID("personal", refTestURI))

	app := &captureAppender{}
	// fakeNavigator with a configured entity resolves GetEntity for any uid, so
	// both relate endpoints pass checkEndpointExists.
	rel := NewRelate(app, &fakeNavigator{entity: &navigate.Entity{UID: "stub"}})
	if _, err := rel.Invoke(context.Background(), json.RawMessage(
		`{"subject":"`+localUID+`","predicate":"has_primary_source","object":"`+refUID+`"}`)); err != nil {
		t.Fatalf("relate(localEntity, has_primary_source, externalRef): %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1 relation", len(app.events))
	}
	var r ontology.Relation
	if uerr := json.Unmarshal(app.events[0].Payload, &r); uerr != nil {
		t.Fatalf("unmarshal relation: %v", uerr)
	}
	if r.Predicate != "has_primary_source" {
		t.Errorf("predicate = %q, want has_primary_source", r.Predicate)
	}
	if r.ObjectUID != refUID {
		t.Errorf("objectUid = %q, want the external_ref %q", r.ObjectUID, refUID)
	}
}

// permissionDeniedAppender simulates the event log refusing a team-scope write
// by a non-member: the TeamWriteAuthority gate returns kernel.ErrPermission.
type permissionDeniedAppender struct{}

func (permissionDeniedAppender) Append(_ context.Context, _ []kernel.Event) ([]kernel.Receipt, error) {
	return nil, kernel.ErrPermission
}

// TestReference_NonMemberTeamWrite_Permission pins that an append-boundary
// kernel.ErrPermission (a team-scope reference by a non-member) maps to
// E_PERMISSION via mapAppendError — matching relate, not a misleading
// E_INTERNAL. A nil navigator skips the existence lookup so the call reaches
// the denying append.
func TestReference_NonMemberTeamWrite_Permission(t *testing.T) {
	t.Parallel()
	r := NewReference(permissionDeniedAppender{}, nil)
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"uri":"`+refTestURI+`","scope":"team"}`))
	wantReferenceCode(t, err, mcp.CodePermission)
}

func decodeReference(t *testing.T, out json.RawMessage) referenceResponse {
	t.Helper()
	var resp referenceResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// wantReferenceCode asserts err is an *mcp.ToolError carrying the expected code.
func wantReferenceCode(t *testing.T, err error, code mcp.ToolErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with code %s, got nil", code)
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("want *mcp.ToolError, got %T: %v", err, err)
	}
	if te.Code != code {
		t.Errorf("error code = %s, want %s", te.Code, code)
	}
}
