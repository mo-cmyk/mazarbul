package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Valid relation ULIDs used across the unrelate tests.
const (
	unrelRelUID   = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	unrelNewerUID = "01ARZ3NDEKTSV4RRFFQ69G5FB9"
)

func TestUnrelate_Metadata(t *testing.T) {
	t.Parallel()
	u := NewUnrelate(&captureAppender{}, &fakeNavigator{})
	if got := u.Name(); got != "unrelate" {
		t.Errorf("Name() = %q, want %q", got, "unrelate")
	}
	if u.Description() == "" {
		t.Error("Description() is empty")
	}
	for _, raw := range []json.RawMessage{u.InputSchema(), u.OutputSchema()} {
		if len(raw) == 0 {
			t.Fatal("schema is empty")
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("schema is not valid JSON: %v", err)
		}
	}
}

func TestUnrelate_RequiresTarget(t *testing.T) {
	t.Parallel()
	u := NewUnrelate(&captureAppender{}, &fakeNavigator{})
	_, err := u.Invoke(context.Background(), json.RawMessage(`{}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestUnrelate_RejectsBothModes(t *testing.T) {
	t.Parallel()
	u := NewUnrelate(&captureAppender{}, &fakeNavigator{})
	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"relation":"relations/`+unrelRelUID+`","subject":"entities/SUBJ"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestUnrelate_RelationNotFound(t *testing.T) {
	t.Parallel()
	// relation is nil on the fake → GetRelation returns ErrNotFound.
	u := NewUnrelate(&captureAppender{}, &fakeNavigator{})
	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"relation":"relations/`+unrelRelUID+`"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

func TestUnrelate_BadEndedAt(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID}}
	u := NewUnrelate(&captureAppender{}, nav)
	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"relation":"relations/`+unrelRelUID+`","endedAt":"last tuesday"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestUnrelate_RejectsAlreadyClosedRelation(t *testing.T) {
	t.Parallel()
	// GetRelation returns an edge whose valid_until is already in the past →
	// re-closing it would silently backdate the close via the earliest-wins fold;
	// reject instead, mirroring the triple path's open-only contract. The bound
	// carries explicit fractional seconds (as the dispatcher's Format(RFC3339Nano)
	// writes them) so this is a clock-independent proof the guard parses the
	// nanosecond column — not reliant on time.Now()'s resolution.
	past := "2020-01-01T00:00:00.123456789Z"
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID, ValidUntil: past}}
	u := NewUnrelate(&captureAppender{}, nav)
	_, err := u.Invoke(context.Background(), json.RawMessage(`{"relation":"relations/`+unrelRelUID+`"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestUnrelate_RejectsSupersededRelation(t *testing.T) {
	t.Parallel()
	// A superseded edge is already closed → not re-closable by uid.
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID, SupersededByUID: unrelNewerUID}}
	u := NewUnrelate(&captureAppender{}, nav)
	_, err := u.Invoke(context.Background(), json.RawMessage(`{"relation":"relations/`+unrelRelUID+`"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestUnrelate_ClosesFutureBoundedRelation(t *testing.T) {
	t.Parallel()
	// An edge with a FUTURE valid_until is still active now, so the triple path
	// would close it (IncludeInactive:false). The uid path must agree: closing
	// earlier is allowed (the fold moves the end earlier).
	future := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339Nano)
	app := &captureAppender{}
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID, ValidUntil: future}}
	u := NewUnrelate(app, nav)
	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"relation":"relations/`+unrelRelUID+`","endedAt":"2026-05-22T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("a future-bounded (still active) edge must be closable: %v", err)
	}
	if got := string(unrelPayload(t, app).RelationUID); got != unrelRelUID {
		t.Errorf("RelationUID = %q, want %q", got, unrelRelUID)
	}
}

func TestUnrelate_AcceptsFractionalEndedAt(t *testing.T) {
	t.Parallel()
	// A programmatic caller may pass a sub-second instant (e.g.
	// time.Now().Format(RFC3339Nano)); it must be honoured, not rejected, and
	// the fractional precision must survive into the retraction payload.
	app := &captureAppender{}
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID}}
	u := NewUnrelate(app, nav)
	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"relation":"relations/`+unrelRelUID+`","endedAt":"2026-05-22T00:00:00.123456789Z"}`))
	if err != nil {
		t.Fatalf("a fractional-second endedAt must be accepted: %v", err)
	}
	want := time.Date(2026, 5, 22, 0, 0, 0, 123456789, time.UTC)
	if got := unrelPayload(t, app).RetractedAt; !got.Equal(want) {
		t.Errorf("RetractedAt = %v, want %v (fractional precision preserved)", got, want)
	}
}

func TestUnrelate_PartialTriple(t *testing.T) {
	t.Parallel()
	// Two of three triple fields, no relation → a distinct, precise message.
	u := NewUnrelate(&captureAppender{}, &fakeNavigator{})
	_, err := u.Invoke(context.Background(), json.RawMessage(`{"subject":"entities/SUBJ","predicate":"works_at"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestUnrelate_ClosesByRelationUID(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID}}
	u := NewUnrelate(app, nav)

	body, err := u.Invoke(context.Background(), json.RawMessage(
		`{"relation":"relations/`+unrelRelUID+`","endedAt":"2026-05-22T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct{ Name, Status, ValidUntil string }
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "retracted" || out.Name != "relations/"+unrelRelUID {
		t.Fatalf("output = %s, want status=retracted name=relations/%s", body, unrelRelUID)
	}

	payload := unrelPayload(t, app)
	if string(payload.RelationUID) != unrelRelUID {
		t.Errorf("RelationUID = %q, want %q", payload.RelationUID, unrelRelUID)
	}
	// A pure temporal close carries NO superseder — the relation is closed by
	// valid_until alone, preserving history.
	if payload.SupersededByUID != "" {
		t.Errorf("SupersededByUID = %q, want empty (pure temporal close)", payload.SupersededByUID)
	}
	want := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	if !payload.RetractedAt.Equal(want) {
		t.Errorf("RetractedAt = %v, want %v", payload.RetractedAt, want)
	}
}

func TestUnrelate_SupersededByCarriedThrough(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID}}
	u := NewUnrelate(app, nav)

	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"relation":"relations/`+unrelRelUID+`","supersededBy":"relations/`+unrelNewerUID+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := string(unrelPayload(t, app).SupersededByUID); got != unrelNewerUID {
		t.Errorf("SupersededByUID = %q, want %q", got, unrelNewerUID)
	}
}

func TestUnrelate_DefaultEndedAtIsNow(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	nav := &fakeNavigator{relation: &navigate.Relation{UID: unrelRelUID}}
	u := NewUnrelate(app, nav)
	before := time.Now().UTC()

	_, err := u.Invoke(context.Background(), json.RawMessage(`{"relation":"relations/`+unrelRelUID+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	ra := unrelPayload(t, app).RetractedAt
	if ra.Before(before) || ra.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("RetractedAt = %v, want ~now", ra)
	}
}

func TestUnrelate_ClosesByTriple(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	// The single open works_at edge from SUBJ to OBJ is the one to close; the
	// SUBJ->OTHER edge must be ignored.
	nav := &fakeNavigator{relations: []navigate.Relation{
		{UID: unrelRelUID, SubjectUID: "SUBJ", ObjectUID: "OBJ", Predicate: "works_at"},
		{UID: "01ARZ3NDEKTSV4RRFFQ69G5FCC", SubjectUID: "SUBJ", ObjectUID: "OTHER", Predicate: "works_at"},
	}}
	u := NewUnrelate(app, nav)

	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"subject":"entities/SUBJ","predicate":"works_at","object":"entities/OBJ","endedAt":"2026-05-22T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := string(unrelPayload(t, app).RelationUID); got != unrelRelUID {
		t.Errorf("RelationUID = %q, want the SUBJ->OBJ edge %q", got, unrelRelUID)
	}
}

func TestUnrelate_TripleNoMatch(t *testing.T) {
	t.Parallel()
	// Edge exists only for a different object → no SUBJ->OBJ match.
	nav := &fakeNavigator{relations: []navigate.Relation{
		{UID: unrelRelUID, SubjectUID: "SUBJ", ObjectUID: "OTHER", Predicate: "works_at"},
	}}
	u := NewUnrelate(&captureAppender{}, nav)
	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"subject":"entities/SUBJ","predicate":"works_at","object":"entities/OBJ"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

func TestUnrelate_TripleAmbiguous(t *testing.T) {
	t.Parallel()
	// Two open edges share (SUBJ, works_at, OBJ) → ambiguous, must not guess.
	nav := &fakeNavigator{relations: []navigate.Relation{
		{UID: unrelRelUID, SubjectUID: "SUBJ", ObjectUID: "OBJ", Predicate: "works_at"},
		{UID: "01ARZ3NDEKTSV4RRFFQ69G5FDD", SubjectUID: "SUBJ", ObjectUID: "OBJ", Predicate: "works_at"},
	}}
	u := NewUnrelate(&captureAppender{}, nav)
	_, err := u.Invoke(context.Background(), json.RawMessage(
		`{"subject":"entities/SUBJ","predicate":"works_at","object":"entities/OBJ"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// unrelPayload asserts exactly one relationRetracted event was appended and
// returns its decoded payload.
func unrelPayload(t *testing.T, app *captureAppender) ontology.RelationRetractedEvent {
	t.Helper()
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	ev := app.events[0]
	if ev.Kind != kernel.EventKind(ontology.KindRelationRetracted) {
		t.Fatalf("event kind = %q, want %q", ev.Kind, ontology.KindRelationRetracted)
	}
	var payload ontology.RelationRetractedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}
