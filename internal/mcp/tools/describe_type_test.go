package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// fakeTypeDescriber stubs the two-method read surface DescribeType consumes.
type fakeTypeDescriber struct {
	kinds    []navigate.RegisteredEntityKind
	counts   []navigate.KindCount
	kindsErr error
}

func (f *fakeTypeDescriber) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	if f.kindsErr != nil {
		return nil, f.kindsErr
	}
	return f.kinds, nil
}

func (f *fakeTypeDescriber) EntityKinds(_ context.Context) ([]navigate.KindCount, error) {
	return f.counts, nil
}

func describeFixture() *fakeTypeDescriber {
	return &fakeTypeDescriber{
		kinds: []navigate.RegisteredEntityKind{
			{Name: "thing", DisplayName: "Thing", Description: "Root.", Provenance: "core", AddedBy: "seed"},
			{Name: "user:vehicle", DisplayName: "Vehicle", Description: "A road vehicle.", Parent: "thing", SameAs: []string{"https://schema.org/Vehicle"}, Provenance: "user", AddedBy: "user"},
			{Name: "user:car", DisplayName: "Car", Description: "A car.", Parent: "user:vehicle", Provenance: "user", AddedBy: "user"},
		},
		counts: []navigate.KindCount{{Kind: "user:vehicle", Count: 7}},
	}
}

// TestDescribeType_Found returns the full definition plus children and usage.
func TestDescribeType_Found(t *testing.T) {
	t.Parallel()
	tool := tools.NewDescribeType(describeFixture())
	req, _ := json.Marshal(map[string]any{"name": "user:vehicle"})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name       string   `json:"name"`
		Parent     string   `json:"parent"`
		Children   []string `json:"children"`
		SameAs     []string `json:"sameAs"`
		Provenance string   `json:"provenance"`
		UsageCount int      `json:"usageCount"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if out.Name != "user:vehicle" || out.Parent != "thing" {
		t.Errorf("name/parent = %q/%q, want user:vehicle/thing", out.Name, out.Parent)
	}
	if len(out.Children) != 1 || out.Children[0] != "user:car" {
		t.Errorf("children = %v, want [user:car]", out.Children)
	}
	if out.UsageCount != 7 {
		t.Errorf("usageCount = %d, want 7", out.UsageCount)
	}
	if len(out.SameAs) != 1 {
		t.Errorf("sameAs = %v, want one IRI", out.SameAs)
	}
}

// TestDescribeType_BareLocalName_Resolves resolves a bare local name to its
// user: kind.
func TestDescribeType_BareLocalName_Resolves(t *testing.T) {
	t.Parallel()
	tool := tools.NewDescribeType(describeFixture())
	req, _ := json.Marshal(map[string]any{"name": "vehicle"})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name string `json:"name"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if out.Name != "user:vehicle" {
		t.Errorf("name = %q, want user:vehicle (bare-name resolution)", out.Name)
	}
}

// TestDescribeType_NotFound_NotFound maps an unknown kind to E_NOT_FOUND.
func TestDescribeType_NotFound_NotFound(t *testing.T) {
	t.Parallel()
	tool := tools.NewDescribeType(describeFixture())
	req, _ := json.Marshal(map[string]any{"name": "spaceship"})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
}

// TestDescribeType_EmptyName_Validation rejects a blank name.
func TestDescribeType_EmptyName_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDescribeType(describeFixture())
	req := json.RawMessage(`{"name":"  "}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestDescribeType_RegistryError_Internal maps a registry listing failure to
// E_INTERNAL.
func TestDescribeType_RegistryError_Internal(t *testing.T) {
	t.Parallel()
	nav := &fakeTypeDescriber{kindsErr: errors.New("db down")}
	tool := tools.NewDescribeType(nav)
	req, _ := json.Marshal(map[string]any{"name": "person"})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}

// TestDescribeType_PropertySchema_Present verifies that a kind with a
// non-trivial attr_schema (propertySchema) surfaces it verbatim in the output,
// and that a kind without a schema omits the field (omitempty).
func TestDescribeType_PropertySchema_Present(t *testing.T) {
	t.Parallel()
	schema := json.RawMessage(`{"type":"object","properties":{"vin":{"type":"string"}}}`)
	nav := &fakeTypeDescriber{
		kinds: []navigate.RegisteredEntityKind{
			{
				Name: "user:vehicle", DisplayName: "Vehicle", Description: "A road vehicle.",
				Provenance: "user", AddedBy: "user",
				PropertySchema: schema,
			},
			{
				Name: "person", DisplayName: "Person", Description: "A human individual.",
				Provenance: "core", AddedBy: "seed",
				// PropertySchema intentionally nil.
			},
		},
	}

	t.Run("kind with schema returns propertySchema", func(t *testing.T) {
		t.Parallel()
		tool := tools.NewDescribeType(nav)
		req, _ := json.Marshal(map[string]any{"name": "user:vehicle"})
		raw, err := tool.Invoke(context.Background(), req)
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var out struct {
			PropertySchema json.RawMessage `json:"propertySchema"`
		}
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			t.Fatalf("unmarshal: %v", uerr)
		}
		if out.PropertySchema == nil {
			t.Fatal("propertySchema = nil, want non-nil JSON object")
		}
		var ps map[string]any
		if err := json.Unmarshal(out.PropertySchema, &ps); err != nil {
			t.Fatalf("propertySchema is not valid JSON: %v", err)
		}
		props, ok := ps["properties"].(map[string]any)
		if !ok {
			t.Fatalf("propertySchema.properties missing; got %v", ps)
		}
		if _, ok := props["vin"]; !ok {
			t.Errorf("propertySchema.properties missing vin; got %v", props)
		}
	})

	t.Run("kind without schema omits propertySchema", func(t *testing.T) {
		t.Parallel()
		tool := tools.NewDescribeType(nav)
		req, _ := json.Marshal(map[string]any{"name": "person"})
		raw, err := tool.Invoke(context.Background(), req)
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var out map[string]json.RawMessage
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			t.Fatalf("unmarshal: %v", uerr)
		}
		if _, present := out["propertySchema"]; present {
			t.Errorf("propertySchema present for kind with nil schema, want omitted; got %s", out["propertySchema"])
		}
	})
}
