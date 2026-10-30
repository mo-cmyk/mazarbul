package tools_test

// relate_batch_test.go exercises the relateBatch tool's validation-before-write
// contract and its error-code surface without standing up the full storage stack.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// --- helpers ---

// errorAppender is an Appender stub that accepts all appends until err is set,
// then returns that error. Separate from captureRelateAppender (which has no
// error-inject path) so the two concerns stay independent.
type errorAppender struct {
	events []kernel.Event
	err    error
}

func (e *errorAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	e.events = append(e.events, events...)
	if e.err != nil {
		return nil, e.err
	}
	return []kernel.Receipt{{UID: "rel1"}}, nil
}

// makeRelateBatch constructs a RelateBatch tool backed by the shared
// captureRelateAppender and relateEntityResolver stubs.
func makeRelateBatch(app *captureRelateAppender) *tools.RelateBatch {
	r := tools.NewRelate(app, relateEntityResolver{})
	return tools.NewRelateBatch(r)
}

// --- metadata ---

// TestRelateBatch_Metadata verifies the tool surface contract (Name, Description,
// valid JSON schemas).
func TestRelateBatch_Metadata(t *testing.T) {
	t.Parallel()
	rb := makeRelateBatch(&captureRelateAppender{})
	if got := rb.Name(); got != "relateBatch" {
		t.Errorf("Name() = %q, want relateBatch", got)
	}
	if rb.Description() == "" {
		t.Error("Description() is empty")
	}
	var in, out map[string]any
	if err := json.Unmarshal(rb.InputSchema(), &in); err != nil {
		t.Fatalf("InputSchema is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(rb.OutputSchema(), &out); err != nil {
		t.Fatalf("OutputSchema is not valid JSON: %v", err)
	}
	if in["additionalProperties"] != false {
		t.Error("InputSchema does not forbid additionalProperties")
	}
}

// --- happy path ---

// TestRelateBatch_HappyPath verifies that a valid batch of two relations appends
// exactly two events atomically and returns their names.
func TestRelateBatch_HappyPath(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			{"subject": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "predicate": "knows", "object": "01ARZ3NDEKTSV4RRFFQ69G5FB0"},
			{"subject": "self", "predicate": "lives_in", "object": "01ARZ3NDEKTSV4RRFFQ69G5FB0"},
		},
	})
	raw, err := rb.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// Exactly two events must be in one Append call.
	if len(app.events) != 2 {
		t.Errorf("appended %d events, want 2", len(app.events))
	}
	var out struct {
		Names  []string `json:"names"`
		Count  int      `json:"count"`
		Status string   `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Count != 2 {
		t.Errorf("count = %d, want 2", out.Count)
	}
	if len(out.Names) != 2 {
		t.Errorf("names length = %d, want 2", len(out.Names))
	}
	if out.Status != "asserted" {
		t.Errorf("status = %q, want asserted", out.Status)
	}
}

// TestRelateBatch_SingleRelation verifies a batch with exactly one relation works
// (minItems=1 boundary).
func TestRelateBatch_SingleRelation(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			{"subject": "self", "predicate": "mentions", "object": "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		},
	})
	_, err := rb.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("single-relation batch: %v", err)
	}
	if len(app.events) != 1 {
		t.Errorf("appended %d events, want 1", len(app.events))
	}
}

// --- validation-before-write contract ---

// TestRelateBatch_InvalidSubject_NoWrite verifies that a bogus subject in one
// relation rejects the ENTIRE batch with E_VALIDATION and writes zero events.
func TestRelateBatch_InvalidSubject_NoWrite(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			// Valid edge first.
			{"subject": "self", "predicate": "knows", "object": "01ARZ3NDEKTSV4RRFFQ69G5FB0"},
			// Invalid subject — "not-a-ulid" is neither a ULID, "self", nor a resource name.
			{"subject": "not-a-ulid", "predicate": "knows", "object": "01ARZ3NDEKTSV4RRFFQ69G5FB0"},
		},
	})
	_, err := rb.Invoke(context.Background(), req)
	wantRelateCode(t, err, mcp.CodeValidation)
	// Zero events must have been written (validate-all-before-write).
	if len(app.events) != 0 {
		t.Errorf("partial write detected: %d events appended, want 0", len(app.events))
	}
}

// TestRelateBatch_InvalidObject_NoWrite mirrors the subject case for the object.
func TestRelateBatch_InvalidObject_NoWrite(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			{"subject": "self", "predicate": "knows", "object": "bad-object"},
		},
	})
	_, err := rb.Invoke(context.Background(), req)
	wantRelateCode(t, err, mcp.CodeValidation)
	if len(app.events) != 0 {
		t.Errorf("partial write on invalid object: %d events appended, want 0", len(app.events))
	}
}

// TestRelateBatch_ErrorMessageContainsIndex verifies that the error message
// identifies which relations[N] element caused the failure so agents can
// fix exactly the right row without re-scanning all inputs.
func TestRelateBatch_ErrorMessageContainsIndex(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			{"subject": "self", "predicate": "knows", "object": "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
			{"subject": "self", "predicate": "knows", "object": "01ARZ3NDEKTSV4RRFFQ69G5FB0"},
			// Index 2 is invalid.
			{"subject": "bad!!!", "predicate": "knows", "object": "self"},
		},
	})
	_, err := rb.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error is not *mcp.ToolError: %v", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("error code = %q, want %q", te.Code, mcp.CodeValidation)
	}
	// The error message must contain "relations[2]".
	if msg := te.Message; len(msg) == 0 {
		t.Error("error message is empty")
	} else if !contains(msg, "relations[2]") {
		t.Errorf("error message %q must identify relations[2]", msg)
	}
}

// TestRelateBatch_EmptyRelationsArray verifies that an empty (but present)
// relations array is rejected with E_VALIDATION before any storage touch.
func TestRelateBatch_EmptyRelationsArray(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{},
	})
	_, err := rb.Invoke(context.Background(), req)
	wantRelateCode(t, err, mcp.CodeValidation)
}

// TestRelateBatch_MalformedJSON verifies that a completely unparseable request
// returns E_VALIDATION (not E_INTERNAL).
func TestRelateBatch_MalformedJSON(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	_, err := rb.Invoke(context.Background(), json.RawMessage(`not json`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

// --- storage error surface ---

// TestRelateBatch_AppendError_MapsToInternal verifies that a non-permission
// storage error from Append surfaces as E_INTERNAL (not a raw Go error).
func TestRelateBatch_AppendError_MapsToInternal(t *testing.T) {
	t.Parallel()
	app := &errorAppender{err: errors.New("disk full")}

	r := tools.NewRelate(app, relateEntityResolver{})
	rb := tools.NewRelateBatch(r)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			{"subject": "self", "predicate": "knows", "object": "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		},
	})
	_, err := rb.Invoke(context.Background(), req)
	wantRelateCode(t, err, mcp.CodeInternal)
}

// TestRelateBatch_PermissionError_MapsToPermission verifies that a
// kernel.ErrPermission from Append surfaces as E_PERMISSION so the agent
// knows it is a team-scope access control failure and not a bug.
func TestRelateBatch_PermissionError_MapsToPermission(t *testing.T) {
	t.Parallel()
	app := &errorAppender{err: kernel.ErrPermission}

	r := tools.NewRelate(app, relateEntityResolver{})
	rb := tools.NewRelateBatch(r)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			// Use concrete UIDs so the team-self guard doesn't fire before the
			// append — the permission error must come from the storage layer.
			{"subject": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "predicate": "knows", "object": "01ARZ3NDEKTSV4RRFFQ69G5FB0", "scope": "team"},
		},
	})
	_, err := rb.Invoke(context.Background(), req)
	wantRelateCode(t, err, mcp.CodePermission)
}

// TestRelateBatch_NonSnakeCasePredicate verifies that a predicate with spaces or
// uppercase letters is rejected with E_VALIDATION before any event is written,
// covering the same path that the single-triple relate tool already guards.
func TestRelateBatch_NonSnakeCasePredicate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		predicate string
	}{
		{"spaces", "Has Spaces"},
		{"uppercase", "HasUpperCase"},
		{"leading digit", "1starts_digit"},
		{"mixed case underscore", "Works_At"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := &captureRelateAppender{}
			rb := makeRelateBatch(app)

			req, _ := json.Marshal(map[string]any{
				"relations": []map[string]any{
					{"subject": "self", "predicate": tc.predicate, "object": "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
				},
			})
			_, err := rb.Invoke(context.Background(), req)
			wantRelateCode(t, err, mcp.CodeValidation)
			// Validate-all-before-write: no events must have been appended.
			if len(app.events) != 0 {
				t.Errorf("predicate %q: partial write detected (%d events), want 0", tc.predicate, len(app.events))
			}
		})
	}
}

// TestRelateBatch_EmptyPredicate verifies that a missing predicate gives
// E_VALIDATION with a "required" message, distinct from the snake_case message.
func TestRelateBatch_EmptyPredicate(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rb := makeRelateBatch(app)

	req, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			{"subject": "self", "predicate": "", "object": "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		},
	})
	_, err := rb.Invoke(context.Background(), req)
	wantRelateCode(t, err, mcp.CodeValidation)
	var te *mcp.ToolError
	if errors.As(err, &te) && !contains(te.Message, "required") {
		t.Errorf("empty predicate message %q should mention 'required'", te.Message)
	}
}

// TestRelateBatch_ValidSnakeCasePredicate verifies that a correctly-formed
// snake_case predicate passes the new validation gate.
func TestRelateBatch_ValidSnakeCasePredicate(t *testing.T) {
	t.Parallel()
	cases := []string{"knows", "works_at", "member_of2", "a"}
	for _, pred := range cases {
		t.Run(pred, func(t *testing.T) {
			t.Parallel()
			app := &captureRelateAppender{}
			rb := makeRelateBatch(app)

			req, _ := json.Marshal(map[string]any{
				"relations": []map[string]any{
					{"subject": "self", "predicate": pred, "object": "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
				},
			})
			if _, err := rb.Invoke(context.Background(), req); err != nil {
				t.Errorf("predicate %q should pass validation, got: %v", pred, err)
			}
		})
	}
}

// contains is a small helper to avoid importing strings in table tests.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
