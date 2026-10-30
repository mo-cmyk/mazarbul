package tools_test

// context_pins_test.go exercises the three brief-steering write tools and the
// contextPins list tool: their error codes (empty text → E_VALIDATION, unknown
// pin id → E_NOT_FOUND via GetContextPin), the event Kind each appends, and the
// list tool's row projection + Unlisted opt-out.

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

const validPinULID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// captureSteerAppender records appended events and returns one synthetic receipt.
type captureSteerAppender struct {
	events []kernel.Event
}

func (c *captureSteerAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	c.events = append(c.events, events...)
	return []kernel.Receipt{{UID: "evt1"}}, nil
}

// permissionDeniedAppender rejects every append with kernel.ErrPermission, so a
// team-scope write by a non-member is exercised: each steering tool must route
// it through mapAppendError to E_PERMISSION, never a bare E_INTERNAL.
type permissionDeniedAppender struct{}

func (permissionDeniedAppender) Append(_ context.Context, _ []kernel.Event) ([]kernel.Receipt, error) {
	return nil, kernel.ErrPermission
}

// notFoundPinReader implements the context-pin read surface and always reports
// the pin missing, so the E_NOT_FOUND path is exercised without storage.
type notFoundPinReader struct{}

func (notFoundPinReader) GetContextPin(_ context.Context, _ string) (navigate.ContextPin, error) {
	return navigate.ContextPin{}, navigate.ErrNotFound
}

// foundPinReader reports every queried pin as existing.
type foundPinReader struct{}

func (foundPinReader) GetContextPin(_ context.Context, uid string) (navigate.ContextPin, error) {
	return navigate.ContextPin{UID: uid, Name: "contextPins/" + uid, Type: "pin", Text: "x", Active: true}, nil
}

// listPinReader returns a fixed set of pins for the contextPins list tool.
type listPinReader struct{ pins []navigate.ContextPin }

func (l listPinReader) ContextPins(_ context.Context) ([]navigate.ContextPin, error) {
	return l.pins, nil
}

func wantSteerCode(t *testing.T, err error, code mcp.ToolErrorCode) {
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

// ─── contextPin ─────────────────────────────────────────────────────────────

func TestContextPin_EmptyText_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewContextPin(&captureSteerAppender{})
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"text":"   "}`))
	wantSteerCode(t, err, mcp.CodeValidation)
}

func TestContextPin_AppendsContextPinnedEvent(t *testing.T) {
	t.Parallel()
	app := &captureSteerAppender{}
	tool := tools.NewContextPin(app)
	raw, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"text":"Ship the wave Friday","scope":"project"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	if got := string(app.events[0].Kind); got != ontology.KindContextPinned {
		t.Errorf("event kind = %q, want %q", got, ontology.KindContextPinned)
	}
	if app.events[0].Scope != kernel.ScopeProject {
		t.Errorf("event scope = %q, want project", app.events[0].Scope)
	}
	var pin ontology.ContextPinnedEvent
	if uerr := json.Unmarshal(app.events[0].Payload, &pin); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if pin.Text != "Ship the wave Friday" {
		t.Errorf("pin text = %q, want the note", pin.Text)
	}
	if string(pin.PinUID) == "" {
		t.Error("pinUid was not minted")
	}
	var out struct {
		Name, Status string
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "pinned" {
		t.Errorf("status = %q, want pinned", out.Status)
	}
	if want := "contextPins/" + string(pin.PinUID); out.Name != want {
		t.Errorf("name = %q, want %q", out.Name, want)
	}
}

// ─── contextCorrect ─────────────────────────────────────────────────────────

func TestContextCorrect_EmptyText_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewContextCorrect(&captureSteerAppender{}, foundPinReader{})
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"targetId":"`+validPinULID+`","text":""}`))
	wantSteerCode(t, err, mcp.CodeValidation)
}

func TestContextCorrect_UnknownPinID_NotFound(t *testing.T) {
	t.Parallel()
	// A bare-ULID target is treated as a pin id; an unknown one resolves to
	// E_NOT_FOUND via GetContextPin.
	tool := tools.NewContextCorrect(&captureSteerAppender{}, notFoundPinReader{})
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"targetId":"`+validPinULID+`","text":"corrected"}`))
	wantSteerCode(t, err, mcp.CodeNotFound)
}

func TestContextCorrect_EntityTarget_SkipsPinCheck(t *testing.T) {
	t.Parallel()
	// An entities/{uid} target is a surfaced-item overlay, not a pin, so the
	// existence check is skipped even with a not-found pin reader.
	app := &captureSteerAppender{}
	tool := tools.NewContextCorrect(app, notFoundPinReader{})
	raw, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"targetId":"entities/`+validPinULID+`","text":"Left Acme in May"}`))
	if err != nil {
		t.Fatalf("entity-targeted correction should succeed, got: %v", err)
	}
	if len(app.events) != 1 || string(app.events[0].Kind) != ontology.KindContextCorrected {
		t.Fatalf("want 1 contextCorrected event, got %d (%v)", len(app.events), app.events)
	}
	var corr ontology.ContextCorrectedEvent
	if uerr := json.Unmarshal(app.events[0].Payload, &corr); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if corr.TargetID != "entities/"+validPinULID {
		t.Errorf("targetId = %q, want the entity ref verbatim", corr.TargetID)
	}
	var out struct{ Status string }
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "corrected" {
		t.Errorf("status = %q, want corrected", out.Status)
	}
}

func TestContextCorrect_PinTarget_NormalizesToBareUID(t *testing.T) {
	t.Parallel()
	app := &captureSteerAppender{}
	tool := tools.NewContextCorrect(app, foundPinReader{})
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"targetId":"contextPins/`+validPinULID+`","text":"new text"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var corr ontology.ContextCorrectedEvent
	if uerr := json.Unmarshal(app.events[0].Payload, &corr); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	// A contextPins/{uid} target is normalized to the bare uid so the brief's
	// overlay match (correction.TargetRef == pin.UID) lands.
	if corr.TargetID != validPinULID {
		t.Errorf("targetId = %q, want bare uid %q", corr.TargetID, validPinULID)
	}
}

// ─── contextUnpin ───────────────────────────────────────────────────────────

func TestContextUnpin_InvalidID_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewContextUnpin(&captureSteerAppender{}, foundPinReader{})
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"pin":"not-a-ulid"}`))
	wantSteerCode(t, err, mcp.CodeValidation)
}

func TestContextUnpin_UnknownPinID_NotFound(t *testing.T) {
	t.Parallel()
	tool := tools.NewContextUnpin(&captureSteerAppender{}, notFoundPinReader{})
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"pin":"contextPins/`+validPinULID+`"}`))
	wantSteerCode(t, err, mcp.CodeNotFound)
}

func TestContextUnpin_AppendsTombstone(t *testing.T) {
	t.Parallel()
	app := &captureSteerAppender{}
	tool := tools.NewContextUnpin(app, foundPinReader{})
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"pin":"`+validPinULID+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 1 || string(app.events[0].Kind) != ontology.KindContextUnpinned {
		t.Fatalf("want 1 contextUnpinned event, got %d (%v)", len(app.events), app.events)
	}
	var unpin ontology.ContextUnpinnedEvent
	if uerr := json.Unmarshal(app.events[0].Payload, &unpin); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if string(unpin.PinUID) != validPinULID {
		t.Errorf("pinUid = %q, want %q", unpin.PinUID, validPinULID)
	}
	var out struct{ Status string }
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "unpinned" {
		t.Errorf("status = %q, want unpinned", out.Status)
	}
}

// ─── contextPins (list) ─────────────────────────────────────────────────────

func TestContextPinList_ProjectsRowsAndCount(t *testing.T) {
	t.Parallel()
	reader := listPinReader{pins: []navigate.ContextPin{
		{UID: "P1", Name: "contextPins/P1", Type: "pin", Text: "note one", Active: true, Scope: "personal"},
		{UID: "C1", Name: "contextPins/C1", Type: "correction", Text: "fixed", TargetRef: "entities/E1", Active: true},
	}}
	tool := tools.NewContextPinList(reader)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Pins []struct {
			Name, Type, Text, TargetRef string
		} `json:"pins"`
		Count int `json:"count"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if out.Count != 2 || len(out.Pins) != 2 {
		t.Fatalf("count/pins = %d/%d, want 2/2", out.Count, len(out.Pins))
	}
	if out.Pins[0].Name != "contextPins/P1" || out.Pins[0].Type != "pin" {
		t.Errorf("pin[0] = %+v, want contextPins/P1 pin", out.Pins[0])
	}
	if out.Pins[1].Type != "correction" || out.Pins[1].TargetRef != "entities/E1" {
		t.Errorf("pin[1] = %+v, want correction → entities/E1", out.Pins[1])
	}
}

func TestContextPinList_Unlisted(t *testing.T) {
	t.Parallel()
	tool := tools.NewContextPinList(listPinReader{})
	if !tool.Unlisted() {
		t.Error("contextPins list should be Unlisted (CLI-driven; brief surfaces pins to agents)")
	}
}

// ─── append error-code mapping (E_PERMISSION) ───────────────────────────────
// Every steering write tool must route an Append permission denial through
// mapAppendError to E_PERMISSION (see mcp-append-error-mapping-symmetry), not a
// bare E_INTERNAL — a team-scope write by a non-member is a well-formed request
// the actor simply lacks authority for.

func TestContextPin_AppendPermission_Mapped(t *testing.T) {
	t.Parallel()
	tool := tools.NewContextPin(permissionDeniedAppender{})
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"text":"Ship the wave Friday","scope":"team"}`))
	wantSteerCode(t, err, mcp.CodePermission)
}

func TestContextCorrect_AppendPermission_Mapped(t *testing.T) {
	t.Parallel()
	// foundPinReader lets the existence check pass so the Append is reached.
	tool := tools.NewContextCorrect(permissionDeniedAppender{}, foundPinReader{})
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"targetId":"`+validPinULID+`","text":"corrected","scope":"team"}`))
	wantSteerCode(t, err, mcp.CodePermission)
}

func TestContextUnpin_AppendPermission_Mapped(t *testing.T) {
	t.Parallel()
	tool := tools.NewContextUnpin(permissionDeniedAppender{}, foundPinReader{})
	_, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"pin":"`+validPinULID+`","scope":"team"}`))
	wantSteerCode(t, err, mcp.CodePermission)
}
