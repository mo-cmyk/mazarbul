package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestDefineEntityKind_GovernedMode_ProposedStatus verifies that when governed=true
// a successful defineEntityKind returns status="proposed" and the event payload
// carries Status="proposed".
func TestDefineEntityKind_GovernedMode_ProposedStatus(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, true /* governed */)

	req, _ := json.Marshal(map[string]any{
		"name":        "gadget",
		"displayName": "Gadget",
		"description": "A small electronic consumer device.",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Name != "user:gadget" {
		t.Errorf("name = %q, want user:gadget", out.Name)
	}
	if out.Status != "proposed" {
		t.Errorf("status = %q, want proposed (governed mode)", out.Status)
	}

	// The event payload must carry Status="proposed".
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	var evPayload ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(fa.gotEvents[0].Payload, &evPayload); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	if evPayload.Status != "proposed" {
		t.Errorf("event Status = %q, want proposed", evPayload.Status)
	}
}

// TestDefineEntityKind_GovernedMode_CoreKindRejected verifies that governed mode
// does NOT bypass the core-kind guard: core kinds still return E_VALIDATION.
func TestDefineEntityKind_GovernedMode_CoreKindRejected(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefineEntityKind(&fakeAppender{}, nil, nil, true /* governed */)

	for _, core := range []string{"person", "organization", "place"} {
		t.Run(core, func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"name":        core,
				"displayName": "X",
				"description": "x",
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestDefineEntityKind_PermissiveMode_RegisteredStatus verifies that the
// permissive (governed=false) path still returns status="registered" and that
// the event has no Status field set.
func TestDefineEntityKind_PermissiveMode_RegisteredStatus(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false /* permissive */)

	req, _ := json.Marshal(map[string]any{
		"name":        "widget",
		"displayName": "Widget",
		"description": "A generic user-defined component.",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "registered" {
		t.Errorf("status = %q, want registered (permissive mode)", out.Status)
	}

	// Event payload must NOT carry Status field in permissive mode.
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	var evPayload ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(fa.gotEvents[0].Payload, &evPayload); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	if evPayload.Status != "" {
		t.Errorf("event Status = %q, want empty in permissive mode", evPayload.Status)
	}
}

// TestDefineEntityKind_GovernedMode_AppendError_E_Internal verifies that when
// the Appender returns an error, the tool returns an E_INTERNAL error code.
func TestDefineEntityKind_GovernedMode_AppendError_E_Internal(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("storage unavailable")}
	tool := tools.NewDefineEntityKind(fa, nil, nil, true /* governed */)

	req, _ := json.Marshal(map[string]any{
		"name":        "spaceship",
		"displayName": "Spaceship",
		"description": "An extraterrestrial vehicle.",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}
