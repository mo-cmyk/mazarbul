package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestRecordSourceRef_HappyPath asserts a valid request appends one
// sourceRefRecorded event with a minted uid and returns its resource name.
func TestRecordSourceRef_HappyPath(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRecordSourceRef(app)

	out, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"email","dedupKey":"gmail:msg-1","connector":"gmail"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	ev := app.events[0]
	if ev.Kind != kernel.EventKind(ontology.KindSourceRefRecorded) {
		t.Errorf("event kind = %q, want sourceRefRecorded", ev.Kind)
	}

	var payload ontology.SourceRefRecordedEvent
	if uerr := json.Unmarshal(ev.Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload.SubjectUID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" || payload.SourceType != "email" || payload.DedupKey != "gmail:msg-1" {
		t.Errorf("payload = %+v", payload)
	}
	if payload.UID != ontology.SourceRefUID(payload.DedupKey) {
		t.Errorf("payload.UID = %q, want it derived from dedupKey via SourceRefUID (%q)",
			payload.UID, ontology.SourceRefUID(payload.DedupKey))
	}
	if payload.FetchTime.IsZero() {
		t.Error("fetchTime defaulted to zero, want now")
	}

	var resp struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode response: %v", uerr)
	}
	if resp.Status != "recorded" {
		t.Errorf("status = %q, want recorded", resp.Status)
	}
	if resp.Name != "sourceRefs/"+string(payload.UID) {
		t.Errorf("name = %q, want sourceRefs/%s", resp.Name, payload.UID)
	}
}

// TestRecordSourceRef_PayloadFaithful asserts the write path stamps the input
// fields verbatim onto the appended event payload (the value a later getSourceRef
// surfaces). The full record→getSourceRef roundtrip is exercised white-box in
// TestSourceRef_RecordThenGet_Roundtrip, which can seed a fake navigator.
func TestRecordSourceRef_PayloadFaithful(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	rec := tools.NewRecordSourceRef(app)

	if _, err := rec.Invoke(context.Background(), json.RawMessage(
		`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FB0","sourceType":"slack_message","dedupKey":"slack:c1:ts1","externalUrl":"https://slack.example.com/c1/ts1"}`)); err != nil {
		t.Fatalf("record Invoke: %v", err)
	}
	var payload ontology.SourceRefRecordedEvent
	if err := json.Unmarshal(app.events[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.SourceType != "slack_message" || payload.ExternalURL != "https://slack.example.com/c1/ts1" {
		t.Errorf("recorded payload = %+v", payload)
	}
}

// TestRecordSourceRef_SameDedupKeyStableName asserts that recording the same
// dedupKey twice returns the SAME resource name. This is the contract that the
// random-ULID design broke: with UNIQUE(dedup_key), the second record must not
// hand back an orphan name that getSourceRef can never resolve.
func TestRecordSourceRef_SameDedupKeyStableName(t *testing.T) {
	t.Parallel()
	r := tools.NewRecordSourceRef(&captureRelateAppender{})

	const body = `{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"email","dedupKey":"gmail:msg-7"}`
	first, err := r.Invoke(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	second, err := r.Invoke(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("second Invoke: %v", err)
	}

	name := func(out json.RawMessage) string {
		var resp struct {
			Name string `json:"name"`
		}
		if uerr := json.Unmarshal(out, &resp); uerr != nil {
			t.Fatalf("decode: %v", uerr)
		}
		return resp.Name
	}
	if name(first) != name(second) {
		t.Errorf("same dedupKey produced different names: %q vs %q", name(first), name(second))
	}
	want := "sourceRefs/" + string(ontology.SourceRefUID("gmail:msg-7"))
	if name(first) != want {
		t.Errorf("name = %q, want %q (derived from dedupKey)", name(first), want)
	}
}

// TestRecordSourceRef_ValidationErrors asserts a missing required field, a bad
// scope, and a malformed fetchTime each map to E_VALIDATION, and never append.
func TestRecordSourceRef_ValidationErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing subjectUid":   `{"sourceType":"email","dedupKey":"k"}`,
		"malformed subjectUid": `{"subjectUid":"not-a-ulid","sourceType":"email","dedupKey":"k"}`,
		"missing sourceType":   `{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","dedupKey":"k"}`,
		"missing dedupKey":     `{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"email"}`,
		"bad scope":            `{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"email","dedupKey":"k","scope":"bogus"}`,
		"bad fetchTime":        `{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"email","dedupKey":"k","fetchTime":"not-a-time"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := &captureRelateAppender{}
			r := tools.NewRecordSourceRef(app)
			_, err := r.Invoke(context.Background(), json.RawMessage(body))
			wantRelateCode(t, err, mcp.CodeValidation)
			if len(app.events) != 0 {
				t.Errorf("appended %d events on a validation failure, want 0", len(app.events))
			}
		})
	}
}

// invalidEventAppender mimics the kernel rejecting a payload at pre-write schema
// validation (e.g. a malformed contentHash that bypassed the tool's own checks
// on the CLI/in-process path), returning an ErrInvalidEvent-wrapped error.
type invalidEventAppender struct{}

func (invalidEventAppender) Append(context.Context, []kernel.Event) ([]kernel.Receipt, error) {
	return nil, fmt.Errorf("%w: kind %q: validate failed", kernel.ErrInvalidEvent, ontology.KindSourceRefRecorded)
}

// TestRecordSourceRef_KernelValidationIsE_VALIDATION asserts that a payload the
// kernel rejects at append time (ErrInvalidEvent) surfaces as E_VALIDATION, not
// E_INTERNAL — so a malformed field that slipped past the tool's own checks is
// reported to the caller as their error (exit 1), matching the wire schema path.
func TestRecordSourceRef_KernelValidationIsE_VALIDATION(t *testing.T) {
	t.Parallel()
	r := tools.NewRecordSourceRef(invalidEventAppender{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"email","dedupKey":"k","contentHash":"NOT-A-HASH"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

// TestRecordSourceRef_TeamWriteByNonMemberIsE_PERMISSION asserts a team-scope
// recordSourceRef whose append is rejected with ErrPermission surfaces as
// E_PERMISSION, not the opaque E_INTERNAL — so an agent knows to request
// membership rather than blind-retry. Sibling of the remember/create_entity guard.
func TestRecordSourceRef_TeamWriteByNonMemberIsE_PERMISSION(t *testing.T) {
	t.Parallel()
	r := tools.NewRecordSourceRef(permissionDeniedAppender{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"email","dedupKey":"k","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodePermission)
}

// TestRecordSourceRef_ConnectorProjectScope_Refused asserts that combining a
// connector with project scope is refused as E_VALIDATION before any append.
// Connector-sourced provenance is sensitive (the same class as captureExternal
// and connect, which already refuse project scope), so it must not reach the
// shared git tree via the project shard's source-refs.jsonl render.
func TestRecordSourceRef_ConnectorProjectScope_Refused(t *testing.T) {
	t.Parallel()

	t.Run("connector with project scope is E VALIDATION", func(t *testing.T) {
		t.Parallel()
		app := &captureRelateAppender{}
		r := tools.NewRecordSourceRef(app)
		_, err := r.Invoke(context.Background(), json.RawMessage(
			`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"notion_page","dedupKey":"notion:p-1","connector":"notion","scope":"project"}`))
		wantRelateCode(t, err, mcp.CodeValidation)
		if len(app.events) != 0 {
			t.Errorf("appended %d events on a validation failure, want 0", len(app.events))
		}
	})

	t.Run("connector with personal scope succeeds", func(t *testing.T) {
		t.Parallel()
		app := &captureRelateAppender{}
		r := tools.NewRecordSourceRef(app)
		_, err := r.Invoke(context.Background(), json.RawMessage(
			`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"notion_page","dedupKey":"notion:p-2","connector":"notion","scope":"personal"}`))
		if err != nil {
			t.Fatalf("connector with personal scope should succeed, got: %v", err)
		}
		if len(app.events) != 1 {
			t.Errorf("appended %d events, want 1", len(app.events))
		}
	})

	t.Run("no connector with project scope is allowed", func(t *testing.T) {
		t.Parallel()
		app := &captureRelateAppender{}
		r := tools.NewRecordSourceRef(app)
		_, err := r.Invoke(context.Background(), json.RawMessage(
			`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","sourceType":"web_page","dedupKey":"web:p-3","scope":"project"}`))
		if err != nil {
			t.Fatalf("no-connector project scope should be allowed, got: %v", err)
		}
		if len(app.events) != 1 {
			t.Errorf("appended %d events, want 1", len(app.events))
		}
	})
}
