package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// TestDefineSchema_HappyPath_Registered verifies a clean registration appends
// one schemaRegistered event and returns status=registered.
func TestDefineSchema_HappyPath_Registered(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewDefineSchema(fa)

	req, _ := json.Marshal(map[string]any{
		"schemaKind": "entityKind:vehicle",
		"version":    "v0.1.0",
		"definition": map[string]any{"type": "object"},
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if len(fa.gotEvents) != 1 || string(fa.gotEvents[0].Kind) != "schemaRegistered" {
		t.Fatalf("want 1 schemaRegistered event, got %d (%v)", len(fa.gotEvents), fa.gotEvents)
	}
}

// TestDefineSchema_CoreKind_Validation rejects overwriting a protected core
// schema kind.
func TestDefineSchema_CoreKind_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefineSchema(&fakeAppender{})
	for _, core := range []string{"entity", "episode", "relation", "predicateRegistered"} {
		t.Run(core, func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"schemaKind": core,
				"version":    "v0.1.0",
				"definition": map[string]any{"type": "object"},
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestDefineSchema_BadKind_Validation rejects a malformed schemaKind.
func TestDefineSchema_BadKind_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefineSchema(&fakeAppender{})
	req, _ := json.Marshal(map[string]any{
		"schemaKind": "Bad Kind!",
		"version":    "v0.1.0",
		"definition": map[string]any{"type": "object"},
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestDefineSchema_NonObjectDefinition_Validation rejects a scalar definition.
func TestDefineSchema_NonObjectDefinition_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefineSchema(&fakeAppender{})
	req := json.RawMessage(`{"schemaKind":"myCustomEvent","version":"v0.1.0","definition":"not-an-object"}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestDefineSchema_Uncompilable_Validation rejects a definition that is a JSON
// object but not a compilable JSON Schema, so callers get E_VALIDATION instead
// of a "registered" no-op that applySchemaRegistered would later skip.
func TestDefineSchema_Uncompilable_Validation(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewDefineSchema(fa)
	req := json.RawMessage(`{"schemaKind":"myCustomEvent","version":"v0.1.0","definition":{"type":12}}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("an uncompilable schema must not append an event, got %d", len(fa.gotEvents))
	}
}

// TestDefineSchema_AppendError_Internal maps an appender failure to E_INTERNAL.
func TestDefineSchema_AppendError_Internal(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("storage unavailable")}
	tool := tools.NewDefineSchema(fa)
	req, _ := json.Marshal(map[string]any{
		"schemaKind": "myCustomEvent",
		"version":    "v0.1.0",
		"definition": map[string]any{"type": "object"},
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}
