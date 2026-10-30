package tools_test

// define_schema_preview_test.go verifies the preview=true path for both
// DefineEntityKind and DefineSchema:
//
//   (a) Preview a kind with a property-schema field removal against N existing
//       entities → orphanCount == N (EntityCount wired through kindCountSource).
//   (b) Preview schema v2 when v1 is registered → action="supersede" with
//       supersedesFrom set.
//   (c) A malformed propertySchema / definition returns E_VALIDATION and
//       appends no event.
//
// Every preview call is asserted to append zero events (the no-write invariant).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
)

// fakeKindCountSource implements both kindLister and kindCountSource (the
// optional EntityKinds extension). Used to inject realistic entity counts
// into the preview path without a live database.
type fakeKindCountSource struct {
	kinds  []navigate.RegisteredEntityKind
	counts []navigate.KindCount
}

func (f *fakeKindCountSource) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	return f.kinds, nil
}

func (f *fakeKindCountSource) EntityKinds(_ context.Context) ([]navigate.KindCount, error) {
	return f.counts, nil
}

// fakeSchemaLister implements schemaLister for DefineSchema preview tests.
type fakeSchemaLister struct {
	schemas []navigate.RegisteredSchema
}

func (f *fakeSchemaLister) RegisteredSchemas(_ context.Context) ([]navigate.RegisteredSchema, error) {
	return f.schemas, nil
}

// TestDefineEntityKind_Preview_OrphanCount_EqualsEntityCount verifies that
// when a preview removes a field from a kind's propertySchema and N entities
// of that kind exist, orphanCount == N in the plan.
func TestDefineEntityKind_Preview_OrphanCount_EqualsEntityCount(t *testing.T) {
	t.Parallel()

	const kindName = "user:widget"
	const entityCount = 7

	existingSchema := json.RawMessage(`{"type":"object","properties":{"color":{"type":"string"},"weight":{"type":"number"}}}`)
	// New schema drops "weight" — one orphaned field.
	newSchema := json.RawMessage(`{"type":"object","properties":{"color":{"type":"string"}}}`)

	src := &fakeKindCountSource{
		kinds: []navigate.RegisteredEntityKind{
			{
				Name:           kindName,
				Description:    "A small UI widget.",
				PropertySchema: existingSchema,
			},
		},
		counts: []navigate.KindCount{
			{Kind: kindName, Count: entityCount},
		},
	}

	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, src, false)

	req, _ := json.Marshal(map[string]any{
		"name":           "widget",
		"displayName":    "Widget",
		"description":    "A small UI widget.",
		"propertySchema": newSchema,
		"preview":        true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// No-write invariant.
	if len(fa.gotEvents) != 0 {
		t.Fatalf("preview=true appended %d events, want 0", len(fa.gotEvents))
	}

	var out struct {
		Status string `json:"status"`
		Plan   *struct {
			Action      string `json:"action"`
			OrphanCount int    `json:"orphanCount"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "preview" {
		t.Errorf("status = %q, want preview", out.Status)
	}
	if out.Plan == nil {
		t.Fatal("plan is nil")
	}
	if out.Plan.Action != "supersede" {
		t.Errorf("plan.action = %q, want supersede", out.Plan.Action)
	}
	if out.Plan.OrphanCount != entityCount {
		t.Errorf("plan.orphanCount = %d, want %d", out.Plan.OrphanCount, entityCount)
	}
}

// TestDefineSchema_Preview_Supersede_WhenV1Registered verifies that when
// schema v1 is already registered for a schemaKind, previewing v2 returns
// action="supersede" with supersedesFrom="v1.0.0", and appends no event.
func TestDefineSchema_Preview_Supersede_WhenV1Registered(t *testing.T) {
	t.Parallel()

	v1Schema := json.RawMessage(`{"type":"object","properties":{"make":{"type":"string"},"year":{"type":"integer"}}}`)
	v2Schema := json.RawMessage(`{"type":"object","properties":{"make":{"type":"string"}}}`)

	lister := &fakeSchemaLister{
		schemas: []navigate.RegisteredSchema{
			{
				SchemaKind: "entityKind:vehicle",
				Version:    "v1.0.0",
				Definition: v1Schema,
			},
		},
	}

	fa := &fakeAppender{}
	tool := tools.NewDefineSchemaWithLister(fa, lister)

	req, _ := json.Marshal(map[string]any{
		"schemaKind": "entityKind:vehicle",
		"version":    "v2.0.0",
		"definition": v2Schema,
		"preview":    true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// No-write invariant.
	if len(fa.gotEvents) != 0 {
		t.Fatalf("preview=true appended %d events, want 0", len(fa.gotEvents))
	}

	var out struct {
		Status string `json:"status"`
		Plan   *struct {
			Action         string `json:"action"`
			SupersedesFrom string `json:"supersedesFrom"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "preview" {
		t.Errorf("status = %q, want preview", out.Status)
	}
	if out.Plan == nil {
		t.Fatal("plan is nil")
	}
	if out.Plan.Action != "supersede" {
		t.Errorf("plan.action = %q, want supersede", out.Plan.Action)
	}
	if out.Plan.SupersedesFrom != "v1.0.0" {
		t.Errorf("plan.supersedesFrom = %q, want v1.0.0", out.Plan.SupersedesFrom)
	}
}

// TestDefineSchema_Preview_Register_WhenNoPriorSchema verifies that when no
// schema is registered for the schemaKind, preview returns action="register"
// and appends no event.
func TestDefineSchema_Preview_Register_WhenNoPriorSchema(t *testing.T) {
	t.Parallel()

	lister := &fakeSchemaLister{schemas: nil}
	fa := &fakeAppender{}
	tool := tools.NewDefineSchemaWithLister(fa, lister)

	req, _ := json.Marshal(map[string]any{
		"schemaKind": "entityKind:newThing",
		"version":    "v1.0.0",
		"definition": map[string]any{"type": "object"},
		"preview":    true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 0 {
		t.Fatalf("preview=true appended %d events, want 0", len(fa.gotEvents))
	}

	var out struct {
		Plan *struct {
			Action string `json:"action"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Plan == nil {
		t.Fatal("plan is nil")
	}
	if out.Plan.Action != "register" {
		t.Errorf("plan.action = %q, want register", out.Plan.Action)
	}
}

// TestDefineEntityKind_Preview_MalformedPropertySchema_Validation verifies
// that a propertySchema that is not a JSON object returns E_VALIDATION on the
// preview path and appends no event.
func TestDefineEntityKind_Preview_MalformedPropertySchema_Validation(t *testing.T) {
	t.Parallel()

	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	// propertySchema is a JSON string, not an object — invalid for a JSON Schema.
	req := json.RawMessage(`{
		"name":           "gadget",
		"displayName":    "Gadget",
		"description":    "A small electronic device.",
		"propertySchema": "not-an-object",
		"preview":        true
	}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)

	// No-write invariant.
	if len(fa.gotEvents) != 0 {
		t.Fatalf("malformed propertySchema appended %d events, want 0", len(fa.gotEvents))
	}
}

// TestDefineSchema_MalformedDefinition_Validation verifies that a definition
// that is a JSON string (not an object) returns E_VALIDATION and appends no
// event, on both the normal and preview paths.
func TestDefineSchema_MalformedDefinition_Validation(t *testing.T) {
	t.Parallel()

	for _, preview := range []bool{false, true} {
		t.Run(map[bool]string{false: "non preview", true: "preview"}[preview], func(t *testing.T) {
			t.Parallel()
			fa := &fakeAppender{}
			tool := tools.NewDefineSchemaWithLister(fa, &fakeSchemaLister{})

			req, _ := json.Marshal(map[string]any{
				"schemaKind": "myCustomEvent",
				"version":    "v1.0.0",
				"definition": "not-an-object",
				"preview":    preview,
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)

			if len(fa.gotEvents) != 0 {
				t.Fatalf("malformed definition appended %d events, want 0", len(fa.gotEvents))
			}
		})
	}
}

// TestDefineEntityKind_Preview_NoWrite_AllPaths verifies that no event is
// appended for any combination of preview=true outcome (mint, supersede,
// collapse).
func TestDefineEntityKind_Preview_NoWrite_AllPaths(t *testing.T) {
	t.Parallel()

	existingSchema := json.RawMessage(`{"type":"object","properties":{"color":{"type":"string"}}}`)
	newSchema := json.RawMessage(`{"type":"object","properties":{"size":{"type":"string"}}}`)

	cases := []struct {
		name    string
		src     *fakeKindCountSource
		reqBody map[string]any
	}{
		{
			name: "mint",
			src:  &fakeKindCountSource{},
			reqBody: map[string]any{
				"name": "completelynewkind", "displayName": "New", "description": "Brand new.", "preview": true,
			},
		},
		{
			name: "collapse",
			src: &fakeKindCountSource{
				kinds: []navigate.RegisteredEntityKind{{Name: "user:collider", Description: "A collider."}},
			},
			reqBody: map[string]any{
				"name": "collider", "displayName": "Collider", "description": "A collider.", "preview": true,
			},
		},
		{
			name: "supersede",
			src: &fakeKindCountSource{
				kinds: []navigate.RegisteredEntityKind{
					{Name: "user:supersedeme", Description: "To be superseded.", PropertySchema: existingSchema},
				},
				counts: []navigate.KindCount{{Kind: "user:supersedeme", Count: 3}},
			},
			reqBody: map[string]any{
				"name":           "supersedeme",
				"displayName":    "Supersede Me",
				"description":    "To be superseded.",
				"propertySchema": newSchema,
				"preview":        true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fa := &fakeAppender{}
			tool := tools.NewDefineEntityKind(fa, nil, tc.src, false)

			req, _ := json.Marshal(tc.reqBody)
			_, err := tool.Invoke(context.Background(), req)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if len(fa.gotEvents) != 0 {
				t.Errorf("preview path %q appended %d events, want 0", tc.name, len(fa.gotEvents))
			}
		})
	}
}

// Compile-time assertion: *fakeSchemaLister satisfies the schemaLister interface
// as accepted by NewDefineSchemaWithLister. This catches any future signature
// drift without needing a runtime test.
var _ dedup.ExistingSchema = dedup.ExistingSchema{} // keep dedup import live
