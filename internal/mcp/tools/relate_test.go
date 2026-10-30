package tools_test

// relate_test.go exercises the relate tool's endpoint-validation logic in the
// black-box package. The critical new case (b) — object="self" accepted — must
// fail before the relateEndpoint fix (isSubject gate dropped) and pass after.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// relateEntityResolver is a minimal entityResolver stub: GetEntity succeeds for
// any uid (including "self"), letting endpoint-existence checks pass without a
// real storage stack.
type relateEntityResolver struct{}

func (relateEntityResolver) GetEntity(_ context.Context, _ string) (navigate.Entity, error) {
	return navigate.Entity{UID: "stub"}, nil
}

// captureRelateAppender records appended events and returns one synthetic receipt.
type captureRelateAppender struct {
	events []kernel.Event
}

func (c *captureRelateAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	c.events = append(c.events, events...)
	return []kernel.Receipt{{UID: "rel1"}}, nil
}

// wantRelateCode asserts err is an *mcp.ToolError carrying the expected code.
func wantRelateCode(t *testing.T, err error, code mcp.ToolErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", code)
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not *mcp.ToolError", err)
	}
	if te.Code != code {
		t.Fatalf("error code = %q, want %q (err: %v)", te.Code, code, err)
	}
}

// --- (a) subject="self" is accepted ---

func TestRelate_SelfSubject_Accepted(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lives_in","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("subject=self should be accepted, got: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	var rel struct {
		SubjectUID string `json:"subjectUid"`
	}
	if err := json.Unmarshal(app.events[0].Payload, &rel); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if rel.SubjectUID != "self" {
		t.Fatalf("subjectUid = %q, want self", rel.SubjectUID)
	}
}

// --- (b) object="self" is accepted (the new behavior fixed in this PR) ---
//
// Before the fix, relateEndpoint only returned "self" when isSubject==true;
// for the object endpoint it fell through to ulid.Parse which rejected "self"
// with E_VALIDATION. After the fix both endpoints accept the "self" sentinel.

func TestRelate_SelfObject_Accepted(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FAV","predicate":"mentions","object":"self"}`))
	if err != nil {
		t.Fatalf("object=self should be accepted (widened schema + data layer allow it), got: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	var rel struct {
		ObjectUID string `json:"objectUid"`
	}
	if err := json.Unmarshal(app.events[0].Payload, &rel); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if rel.ObjectUID != "self" {
		t.Fatalf("objectUid = %q, want self", rel.ObjectUID)
	}
}

// --- (c) bogus non-ULID non-self args are rejected with E_VALIDATION ---

func TestRelate_BogusSubject_Validation(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"bob","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
	if len(app.events) != 0 {
		t.Fatalf("bogus subject must not be appended, got %d events", len(app.events))
	}
}

func TestRelate_BogusObject_Validation(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"mentions","object":"alice"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
	if len(app.events) != 0 {
		t.Fatalf("bogus object must not be appended, got %d events", len(app.events))
	}
}

// --- (d) entities/{uid} resource-name form and bare-ULID form are accepted ---

func TestRelate_ResourceNameSubject_Accepted(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV","predicate":"reports_to","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("entities/{uid} subject should be accepted, got: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	var rel struct {
		SubjectUID string `json:"subjectUid"`
	}
	if err := json.Unmarshal(app.events[0].Payload, &rel); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	// entities/{uid} must be stripped to the bare uid.
	if rel.SubjectUID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatalf("subjectUid = %q, want stripped uid", rel.SubjectUID)
	}
}

func TestRelate_ResourceNameObject_Accepted(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lives_in","object":"entities/01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("entities/{uid} object should be accepted, got: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	var rel struct {
		ObjectUID string `json:"objectUid"`
	}
	if err := json.Unmarshal(app.events[0].Payload, &rel); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if rel.ObjectUID != "01ARZ3NDEKTSV4RRFFQ69G5FB0" {
		t.Fatalf("objectUid = %q, want stripped uid", rel.ObjectUID)
	}
}

func TestRelate_BareULIDEndpoints_Accepted(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FAV","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("bare ULID subject+object should be accepted, got: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
}

// --- (e) explicit source flows through to the appended event ---
//
// The CLI relate command passes "cli:relate" so synced provenance attributes
// the edge to the CLI rather than the generic MCP label.
func TestRelate_Source_FlowsThrough(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FAV","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0","source":"cli:relate"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	if got := app.events[0].Source; got != "cli:relate" {
		t.Errorf("event source = %q, want cli:relate", got)
	}
}

// TestRelate_Source_DefaultsToMcp confirms an omitted source keeps the
// mcp:relate default so existing direct-call provenance is unchanged.
func TestRelate_Source_DefaultsToMcp(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FAV","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := app.events[0].Source; got != "mcp:relate" {
		t.Errorf("event source = %q, want mcp:relate", got)
	}
}
