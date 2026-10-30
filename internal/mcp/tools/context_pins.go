package tools

// context_pins.go — the brief-steering write surface (#15). Three append-only
// tools let a user steer the cold-start context brief:
//
//   - contextPin     appends a contextPinned event: a free-text steering note
//                    surfaced at the TOP of the brief's "Pinned" section.
//   - contextCorrect appends a contextCorrected event: an overlay that replaces
//                    the rendered text of a surfaced item (a pin, by its
//                    contextPins/{uid}, or an entity, by entities/{uid}).
//   - contextUnpin   appends a contextUnpinned tombstone: retires a pin or
//                    correction so it stops surfacing (convergent, never an
//                    in-place delete).
//
// Each tool mints a fresh ULID for the row's identity (PinUID / CorrectionUID),
// appends through the shared Appender, and maps a write failure through
// mapAppendError so a team-scope write by a non-member surfaces E_PERMISSION.
// Scope rides the event envelope (kernel.Event.Scope), exactly like relate.
//
// These are DISTINCT from `maz context pin <documentUid>` (which toggles
// Document.Pinned on a vault document): a steering note is free text backed by
// the context_pins table, not a vault document. The CLI exposes them as
// `maz context note|correct|unpin` to keep the two pin concepts unambiguous.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// contextPinCollection is the AIP collection segment for a context pin or
// correction: every steering row's resource name is "contextPins/{uid}",
// matching navigate.ContextPin.Name.
const contextPinCollection = "contextPins"

// contextPinReader is the read-side dependency contextUnpin and contextCorrect
// use to confirm a referenced pin exists before writing a tombstone or overlay,
// so an unknown id surfaces as E_NOT_FOUND up front rather than a silently-
// orphaned event. Satisfied by *navigate.Navigator; a nil reader disables the
// check (e.g. a minimal boot where the navigator is unavailable).
type contextPinReader interface {
	GetContextPin(ctx context.Context, uid string) (navigate.ContextPin, error)
}

// ─── contextPin ─────────────────────────────────────────────────────────────

const contextPinInputSchema = `{
  "type": "object",
  "properties": {
    "text":      {"type": "string", "minLength": 1, "description": "The steering note to surface at the top of the cold-start brief, e.g. \"Mid-migration off Postgres — don't suggest new Postgres features\". Required, non-empty."},
    "targetRef": {"type": "string", "description": "Optional resource the note is about, e.g. an entities/{uid}. Omit for a free-floating steering note."},
    "scope":     {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope to write into — a pin steers whoever reads that scope. Defaults to personal (steers only the user). Use \"project\" to steer anyone working in this repo. Use \"team\" to steer the whole team (requires admitted membership, else E_PERMISSION)."}
  },
  "required": ["text"],
  "additionalProperties": false
}`

const contextPinOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^contextPins/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the new pin; pass to contextUnpin or contextCorrect."},
    "status": {"type": "string", "enum": ["pinned"], "description": "Always \"pinned\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// ContextPin appends a contextPinned event: a user's free-text steering note for
// the cold-start brief. The note leads the brief's "Pinned" section (winning a
// budget slot before auto-surfaced documents and entities), so an agent reads it
// first on session open.
type ContextPin struct {
	appender Appender
}

// NewContextPin constructs the contextPin tool bound to appender a.
func NewContextPin(a Appender) *ContextPin { return &ContextPin{appender: a} }

// Name implements mcp.Tool.
func (*ContextPin) Name() string { return "contextPin" }

// Description implements mcp.Tool.
func (*ContextPin) Description() string {
	return "Pin a free-text steering note to the top of the user's cold-start context brief. Use it when the user states a durable preference or constraint they want surfaced first every session (e.g. \"I'm mid-migration off Postgres — don't suggest new Postgres features\"). The note wins a budget slot before auto-surfaced documents and entities. Returns the pin's contextPins/{uid}; retire it with contextUnpin or override it with contextCorrect."
}

// InputSchema implements mcp.Tool.
func (*ContextPin) InputSchema() json.RawMessage { return json.RawMessage(contextPinInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ContextPin) OutputSchema() json.RawMessage { return json.RawMessage(contextPinOutputSchema) }

type contextPinInput struct {
	Text      string `json:"text"`
	TargetRef string `json:"targetRef,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

type contextSteerOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool. It mints a fresh PinUID, appends a contextPinned
// event, and returns the new pin's resource name.
func (c *ContextPin) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in contextPinInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, mcp.NewValidationError("text is required and must be non-empty", nil)
	}
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}
	uid, err := newContextPinULID()
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("generate pin uid: %w", err))
	}
	payload, err := json.Marshal(ontology.ContextPinnedEvent{
		PinUID:    ontology.ULID(uid),
		Text:      text,
		TargetRef: strings.TrimSpace(in.TargetRef),
		PinnedAt:  time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal contextPinned: %w", err))
	}
	if err := c.append(ctx, ontology.KindContextPinned, "mcp:contextPin", scope, payload); err != nil {
		return nil, err
	}
	return marshalTool(contextSteerOutput{Name: contextPinName(uid), Status: "pinned"})
}

// append routes one steering event through the shared appender and maps a
// write failure to a stable tool error. Shared by all three steering tools.
func (c *ContextPin) append(ctx context.Context, kind, source string, scope kernel.Scope, payload json.RawMessage) error {
	return appendSteerEvent(ctx, c.appender, kind, source, scope, payload)
}

// ─── contextCorrect ─────────────────────────────────────────────────────────

const contextCorrectInputSchema = `{
  "type": "object",
  "properties": {
    "targetId": {"type": "string", "minLength": 1, "description": "Id of the item to correct: a contextPins/{uid} (or bare ULID) of a pin shown in the brief, or an entities/{uid} surfaced in it. The brief renders this corrected text in the item's place."},
    "text":     {"type": "string", "minLength": 1, "description": "The corrected text to show in place of the surfaced item. Required, non-empty."},
    "scope":    {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope to write into; match the scope the corrected pin lives in. Defaults to personal (\"project\" = this repo; \"team\" requires admitted membership, else E_PERMISSION)."}
  },
  "required": ["targetId", "text"],
  "additionalProperties": false
}`

const contextCorrectOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^contextPins/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the correction overlay; retire it with contextUnpin."},
    "status": {"type": "string", "enum": ["corrected"], "description": "Always \"corrected\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// ContextCorrect appends a contextCorrected event: an overlay that replaces the
// rendered text of a surfaced brief item. Corrections are append-only — a fresh
// CorrectionUID per event — and the brief resolves the latest overlay per target
// at read time, so a re-correction simply wins without an in-place edit.
type ContextCorrect struct {
	appender Appender
	pins     contextPinReader
}

// NewContextCorrect constructs the contextCorrect tool bound to appender a and
// the reader used to confirm a pin-targeted correction names a real pin. A nil
// reader skips the existence check (minimal boot).
func NewContextCorrect(a Appender, pins contextPinReader) *ContextCorrect {
	return &ContextCorrect{appender: a, pins: pins}
}

// Name implements mcp.Tool.
func (*ContextCorrect) Name() string { return "contextCorrect" }

// Description implements mcp.Tool.
func (*ContextCorrect) Description() string {
	return "Correct a wrong item the cold-start brief surfaced. Pass the item's id (a pin's contextPins/{uid}, or an entities/{uid} the brief showed) and the corrected text; the brief renders your text in its place. The correction is a NEW event, not an in-place edit — history is preserved and the latest correction wins. Returns the overlay's contextPins/{uid}."
}

// InputSchema implements mcp.Tool.
func (*ContextCorrect) InputSchema() json.RawMessage {
	return json.RawMessage(contextCorrectInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*ContextCorrect) OutputSchema() json.RawMessage {
	return json.RawMessage(contextCorrectOutputSchema)
}

type contextCorrectInput struct {
	TargetID string `json:"targetId"`
	Text     string `json:"text"`
	Scope    string `json:"scope,omitempty"`
}

// Invoke implements mcp.Tool. It validates a pin-targeted correction against the
// pin store, mints a CorrectionUID, and appends a contextCorrected overlay.
func (c *ContextCorrect) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in contextCorrectInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, mcp.NewValidationError("text is required and must be non-empty", nil)
	}
	targetID := strings.TrimSpace(in.TargetID)
	if targetID == "" {
		return nil, mcp.NewValidationError("targetId is required and must be non-empty", nil)
	}
	// Normalize a pin-style target ("contextPins/{uid}" or a bare ULID) to its
	// bare uid so the brief's overlay match (correction.TargetRef == pin.UID)
	// lands, and — when a reader is wired — confirm the pin exists. A non-pin
	// target (e.g. "entities/{uid}") is stored verbatim and not existence-checked
	// here: the brief overlays it onto a matching surfaced entity if present.
	if pinUID, isPin := contextPinTargetUID(targetID); isPin {
		if verr := c.requirePinExists(ctx, pinUID); verr != nil {
			return nil, verr
		}
		targetID = pinUID
	}
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}
	uid, err := newContextPinULID()
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("generate correction uid: %w", err))
	}
	payload, err := json.Marshal(ontology.ContextCorrectedEvent{
		CorrectionUID: ontology.ULID(uid),
		TargetID:      targetID,
		Text:          text,
		CorrectedAt:   time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal contextCorrected: %w", err))
	}
	if err := appendSteerEvent(ctx, c.appender, ontology.KindContextCorrected, "mcp:contextCorrect", scope, payload); err != nil {
		return nil, err
	}
	return marshalTool(contextSteerOutput{Name: contextPinName(uid), Status: "corrected"})
}

// requirePinExists confirms uid names a live or retired context pin/correction,
// mapping a miss to E_NOT_FOUND. A nil reader (minimal boot) skips the check.
func (c *ContextCorrect) requirePinExists(ctx context.Context, uid string) error {
	return requireContextPinExists(ctx, c.pins, uid)
}

// ─── contextUnpin ───────────────────────────────────────────────────────────

const contextUnpinInputSchema = `{
  "type": "object",
  "properties": {
    "pin":   {"type": "string", "minLength": 1, "description": "The pin or correction to retire: contextPins/{uid} or a bare ULID, as returned by contextPin/contextCorrect or shown in the brief."},
    "scope": {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope the tombstone is written into; match the pin's scope. Defaults to personal."}
  },
  "required": ["pin"],
  "additionalProperties": false
}`

const contextUnpinOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^contextPins/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the retired pin or correction."},
    "status": {"type": "string", "enum": ["unpinned"], "description": "Always \"unpinned\" on success: the row is retired (a convergent tombstone), not deleted."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// ContextUnpin appends a contextUnpinned tombstone retiring a pin or correction
// so it stops surfacing in the brief. Retirement is a convergent tombstone set
// (the dispatcher folds the pin's active flag), never an in-place delete: the
// row is preserved for audit and the retire converges regardless of replay order.
type ContextUnpin struct {
	appender Appender
	pins     contextPinReader
}

// NewContextUnpin constructs the contextUnpin tool bound to appender a and the
// reader used to confirm the target pin exists before tombstoning it. A nil
// reader skips the existence check (minimal boot).
func NewContextUnpin(a Appender, pins contextPinReader) *ContextUnpin {
	return &ContextUnpin{appender: a, pins: pins}
}

// Name implements mcp.Tool.
func (*ContextUnpin) Name() string { return "contextUnpin" }

// Description implements mcp.Tool.
func (*ContextUnpin) Description() string {
	return "Retire a steering pin or correction so it stops surfacing in the cold-start brief. Pass its contextPins/{uid} (or bare ULID). The row is tombstoned, not deleted — history is preserved and the retire converges across devices. Use when a pinned note or correction is no longer relevant."
}

// InputSchema implements mcp.Tool.
func (*ContextUnpin) InputSchema() json.RawMessage { return json.RawMessage(contextUnpinInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ContextUnpin) OutputSchema() json.RawMessage {
	return json.RawMessage(contextUnpinOutputSchema)
}

type contextUnpinInput struct {
	Pin   string `json:"pin"`
	Scope string `json:"scope,omitempty"`
}

// Invoke implements mcp.Tool. It resolves the target uid, confirms it exists,
// and appends a contextUnpinned tombstone.
func (c *ContextUnpin) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in contextUnpinInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	uid, verr := contextPinUIDFromArg(in.Pin)
	if verr != nil {
		return nil, verr
	}
	if verr := requireContextPinExists(ctx, c.pins, uid); verr != nil {
		return nil, verr
	}
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(ontology.ContextUnpinnedEvent{
		PinUID:     ontology.ULID(uid),
		UnpinnedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal contextUnpinned: %w", err))
	}
	if err := appendSteerEvent(ctx, c.appender, ontology.KindContextUnpinned, "mcp:contextUnpin", scope, payload); err != nil {
		return nil, err
	}
	return marshalTool(contextSteerOutput{Name: contextPinName(uid), Status: "unpinned"})
}

// ─── contextPins (list) ─────────────────────────────────────────────────────

const contextPinsInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const contextPinsOutputSchema = `{
  "type": "object",
  "properties": {
    "pins": {
      "type": "array",
      "description": "Live (active) steering pins and corrections, newest first.",
      "items": {
        "type": "object",
        "properties": {
          "name":       {"type": "string", "description": "contextPins/{uid} — pass to contextUnpin or contextCorrect."},
          "type":       {"type": "string", "enum": ["pin", "correction"], "description": "\"pin\" is a steering note; \"correction\" is an overlay on a surfaced item."},
          "text":       {"type": "string", "description": "The pin's note, or the correction's replacement text."},
          "targetRef":  {"type": "string", "description": "For a correction, the id of the item it overrides; for a pin, an optional resource it is about."},
          "createTime": {"type": "string", "description": "RFC 3339 UTC creation time."},
          "scope":      {"type": "string", "description": "The scope the pin lives in."}
        },
        "required": ["name", "type", "text"]
      }
    },
    "count": {"type": "integer", "description": "Number of live pins and corrections returned."}
  },
  "required": ["pins", "count"],
  "additionalProperties": false
}`

// contextPinLister is the read-side dependency the contextPins list tool uses to
// enumerate the live steering pins + corrections. Satisfied by *navigate.Navigator.
type contextPinLister interface {
	ContextPins(ctx context.Context) ([]navigate.ContextPin, error)
}

// ContextPinList lists the user's live steering pins and corrections so a CLI or
// agent can see what is steering the brief and copy an id to retire (contextUnpin)
// or override (contextCorrect). It is UNLISTED from tools/list: agents read the
// pins inline in the getContext brief (they lead the Pinned section); this tool
// backs `maz context`'s management view, like getIndex backs `maz context index`.
type ContextPinList struct {
	reader contextPinLister
}

// NewContextPinList constructs the contextPins list tool over a pin reader.
func NewContextPinList(reader contextPinLister) *ContextPinList {
	return &ContextPinList{reader: reader}
}

// Name implements mcp.Tool.
func (*ContextPinList) Name() string { return "contextPins" }

// Description implements mcp.Tool.
func (*ContextPinList) Description() string {
	return "List the user's live steering pins and corrections for the cold-start brief, newest first. Each carries its contextPins/{uid} so you can retire it (contextUnpin) or override it (contextCorrect)."
}

// InputSchema implements mcp.Tool.
func (*ContextPinList) InputSchema() json.RawMessage { return json.RawMessage(contextPinsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ContextPinList) OutputSchema() json.RawMessage {
	return json.RawMessage(contextPinsOutputSchema)
}

// Unlisted implements mcp.UnlistedTool: hidden from tools/list (the brief already
// surfaces pins to agents) while remaining dispatchable by name for `maz context`.
func (*ContextPinList) Unlisted() bool { return true }

type contextPinView struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Text       string `json:"text"`
	TargetRef  string `json:"targetRef,omitempty"`
	CreateTime string `json:"createTime,omitempty"`
	Scope      string `json:"scope,omitempty"`
}

type contextPinsOutput struct {
	Pins  []contextPinView `json:"pins"`
	Count int              `json:"count"`
}

// Invoke implements mcp.Tool. It returns the live pins + corrections in the
// navigator's scope span (newest first), each stamped with its resource name.
func (c *ContextPinList) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	pins, err := c.reader.ContextPins(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("list context pins: %w", err))
	}
	views := make([]contextPinView, 0, len(pins))
	for i := range pins {
		views = append(views, contextPinView{
			Name:       pins[i].Name,
			Type:       pins[i].Type,
			Text:       pins[i].Text,
			TargetRef:  pins[i].TargetRef,
			CreateTime: pins[i].CreateTime,
			Scope:      pins[i].Scope,
		})
	}
	return marshalTool(contextPinsOutput{Pins: views, Count: len(views)})
}

// ─── shared helpers ─────────────────────────────────────────────────────────

// appendSteerEvent appends one steering event (a single-element batch) through
// app and maps an Append failure through mapAppendError, so a team-scope write
// by a non-member becomes E_PERMISSION. It mirrors relate's append path.
func appendSteerEvent(ctx context.Context, app Appender, kind, source string, scope kernel.Scope, payload json.RawMessage) error {
	receipts, err := app.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(kind),
		Source:     source,
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	}})
	if err != nil {
		return mapAppendError("append "+kind, err)
	}
	if len(receipts) != 1 {
		return mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}
	return nil
}

// requireContextPinExists confirms uid names a context pin or correction row,
// mapping navigate.ErrNotFound to E_NOT_FOUND and any other read error to
// E_INTERNAL. A nil reader (minimal boot, no navigator) skips the check.
func requireContextPinExists(ctx context.Context, pins contextPinReader, uid string) error {
	if pins == nil {
		return nil
	}
	if _, err := pins.GetContextPin(ctx, uid); err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return mcp.NewNotFoundError(contextPinName(uid) + " not found; nothing to update")
		}
		return mcp.NewInternalError(fmt.Errorf("verify context pin %q: %w", uid, err))
	}
	return nil
}

// contextPinName renders a context-pin uid as its AIP resource name.
func contextPinName(uid string) string { return contextPinCollection + "/" + uid }

// contextPinUIDFromArg normalizes a pin reference — "contextPins/{uid}" or a
// bare ULID — into the bare uid, rejecting anything else as E_VALIDATION. It is
// the strict parser contextUnpin uses: the target MUST be a context pin.
func contextPinUIDFromArg(arg string) (string, error) {
	uid := strings.TrimSpace(arg)
	uid = strings.TrimPrefix(uid, contextPinCollection+"/")
	if _, err := ulid.Parse(uid); err != nil {
		return "", mcp.NewValidationError(
			fmt.Sprintf("pin %q is not a valid context pin reference: expected a 26-char ULID, optionally as contextPins/{uid}", arg), nil)
	}
	return uid, nil
}

// contextPinTargetUID classifies a correction target: a "contextPins/{uid}" ref
// or a bare ULID is a PIN target (returns the bare uid, true); any other form
// (e.g. "entities/{uid}") is a non-pin surfaced-item target (returns "", false)
// stored verbatim for the brief's entity overlay.
func contextPinTargetUID(targetID string) (uid string, isPin bool) {
	t := strings.TrimSpace(targetID)
	if rest, ok := strings.CutPrefix(t, contextPinCollection+"/"); ok {
		if _, err := ulid.Parse(rest); err == nil {
			return rest, true
		}
		return "", false
	}
	if _, err := ulid.Parse(t); err == nil {
		return t, true
	}
	return "", false
}

// newContextPinULID returns a fresh ULID in Crockford base32 for a pin or
// correction row identity, mirroring relate's newRelationULID.
func newContextPinULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return "", fmt.Errorf("ulid: %w", err)
	}
	return id.String(), nil
}
