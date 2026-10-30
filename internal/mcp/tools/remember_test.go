package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// fakeAppender is a tools.Appender stub that captures the events the
// handler dispatches and returns a configurable receipt list / error.
// It lets the unit tests assert (a) the Episode payload shape the
// handler builds and (b) that errors from Append surface as the right
// MCP error code, without standing up the four-layer storage stack.
type fakeAppender struct {
	receipts []kernel.Receipt
	err      error

	gotEvents []kernel.Event
	called    bool
}

func (f *fakeAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	f.called = true
	f.gotEvents = append(f.gotEvents, events...)
	if f.err != nil {
		return nil, f.err
	}
	return f.receipts, nil
}

// TestRemember_Metadata pins the tool's surface metadata. Name and
// Description appear in tools/list, so they are part of the
// agent-facing contract; the schemas drive validation, so a typo breaks
// discovery silently if not asserted.
func TestRemember_Metadata(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{})
	if got, want := r.Name(), "remember"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if r.Description() == "" {
		t.Error("Description() is empty")
	}
	var in map[string]any
	if err := json.Unmarshal(r.InputSchema(), &in); err != nil {
		t.Fatalf("InputSchema unparseable: %v", err)
	}
	if in["additionalProperties"] != false {
		t.Error("InputSchema does not forbid additionalProperties")
	}
	required, _ := in["required"].([]any)
	if len(required) != 1 || required[0] != "text" {
		t.Errorf("InputSchema.required = %v, want [text] (source defaults to mcp:remember)", required)
	}
	var out map[string]any
	if err := json.Unmarshal(r.OutputSchema(), &out); err != nil {
		t.Fatalf("OutputSchema unparseable: %v", err)
	}
	outRequired, _ := out["required"].([]any)
	if len(outRequired) != 1 || outRequired[0] != "name" {
		t.Errorf("OutputSchema.required = %v, want [name]", outRequired)
	}
}

// TestRemember_HappyPath proves the handler builds the Episode the way
// the dispatcher expects (Kind="episode", payload carrying the three
// required fields) and that the response wraps the receipt UID in an
// AIP resource name.
func TestRemember_HappyPath(t *testing.T) {
	t.Parallel()
	const uid = "01HX0000000000000000000001"
	fake := &fakeAppender{
		receipts: []kernel.Receipt{{UID: uid, CreateTime: time.Now().UTC()}},
	}
	r := tools.NewRemember(fake)

	resp, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"the cat sat on the mat","source":"chat","confidence":0.8}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Response shape: single AIP `name` field, no bare uid.
	var got map[string]any
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got["name"] != "episodes/"+uid {
		t.Errorf("response name = %v, want %q", got["name"], "episodes/"+uid)
	}
	if _, hasUID := got["uid"]; hasUID {
		t.Error("response carries bare `uid`; should be name only")
	}

	// Dispatched event shape.
	if !fake.called {
		t.Fatal("Append was never called")
	}
	if len(fake.gotEvents) != 1 {
		t.Fatalf("Append received %d events, want 1", len(fake.gotEvents))
	}
	ev := fake.gotEvents[0]
	if ev.Kind != ontology.KindEpisode {
		t.Errorf("Event.Kind = %q, want %q", ev.Kind, ontology.KindEpisode)
	}
	if ev.Source != "chat" {
		t.Errorf("Event.Source = %q, want %q", ev.Source, "chat")
	}
	if ev.Confidence != 0.8 {
		t.Errorf("Event.Confidence = %v, want 0.8", ev.Confidence)
	}
	// The handler must leave UID + CreateTime zero so FileEventLog.prepare
	// mints them. A regression here would strip the writer's monotonic-
	// ULID guarantee and produce duplicate UIDs under concurrent Append.
	if ev.UID != "" {
		t.Errorf("Event.UID = %q, handler must leave it for the writer", ev.UID)
	}
	if !ev.CreateTime.IsZero() {
		t.Errorf("Event.CreateTime = %v, handler must leave it for the writer", ev.CreateTime)
	}

	var ep ontology.Episode
	if err := json.Unmarshal(ev.Payload, &ep); err != nil {
		t.Fatalf("unmarshal episode payload: %v", err)
	}
	if ep.Text != "the cat sat on the mat" {
		t.Errorf("Episode.Text = %q", ep.Text)
	}
	if ep.Source != "chat" {
		t.Errorf("Episode.Source = %q", ep.Source)
	}
	if ep.Confidence != 0.8 {
		t.Errorf("Episode.Confidence = %v, want 0.8", ep.Confidence)
	}
}

// TestRemember_DefaultSource pins the contract the tool description
// promises agents — "just call remember({text})": with `source` omitted,
// the schema admits the body and the handler defaults Source to
// "mcp:remember" in both the dispatched Event and the Episode payload.
// A regression that re-adds `source` to the schema's required set would
// reject a fresh agent's very first write.
func TestRemember_DefaultSource(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{
		receipts: []kernel.Receipt{{UID: "01HX0000000000000000000009", CreateTime: time.Now().UTC()}},
	}
	r := tools.NewRemember(fake)

	if _, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"a note with no source"}`)); err != nil {
		t.Fatalf("Invoke with source omitted: %v", err)
	}
	if len(fake.gotEvents) != 1 {
		t.Fatalf("Append received %d events, want 1", len(fake.gotEvents))
	}
	if got := fake.gotEvents[0].Source; got != "mcp:remember" {
		t.Errorf("Event.Source = %q, want default %q", got, "mcp:remember")
	}
	var ep ontology.Episode
	if err := json.Unmarshal(fake.gotEvents[0].Payload, &ep); err != nil {
		t.Fatalf("unmarshal episode payload: %v", err)
	}
	if ep.Source != "mcp:remember" {
		t.Errorf("Episode.Source = %q, want default %q", ep.Source, "mcp:remember")
	}
}

// TestRemember_DefaultConfidence pins that omitting `confidence`
// produces a 1.0 default in both the dispatched Event and the Episode
// payload. The default belongs in the handler, not the schema, so a
// schema-only validator (no defaulting) reaches the right value.
func TestRemember_DefaultConfidence(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{
		receipts: []kernel.Receipt{{UID: "01HX0000000000000000000002"}},
	}
	r := tools.NewRemember(fake)

	if _, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"hi","source":"chat"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.gotEvents) != 1 {
		t.Fatalf("Append got %d events", len(fake.gotEvents))
	}
	ev := fake.gotEvents[0]
	if ev.Confidence != 1.0 {
		t.Errorf("Event.Confidence = %v, want 1.0 default", ev.Confidence)
	}
	var ep ontology.Episode
	if err := json.Unmarshal(ev.Payload, &ep); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if ep.Confidence != 1.0 {
		t.Errorf("Episode.Confidence = %v, want 1.0 default", ep.Confidence)
	}
}

// TestRemember_AppendError pins the failure path: Append returning a
// non-nil error surfaces as an mcp.ToolError with CodeInternal, and the
// underlying cause stays reachable via errors.Is.
func TestRemember_AppendError(t *testing.T) {
	t.Parallel()
	boom := errors.New("eventlog: disk full")
	r := tools.NewRemember(&fakeAppender{err: boom})

	_, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"hi","source":"chat"}`))
	if err == nil {
		t.Fatal("Invoke succeeded, want error")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeInternal {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeInternal)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err does not wrap the appender's boom: %v", err)
	}
}

// TestRemember_TeamWriteByNonMemberPermissionDenied pins the authz-mapping
// contract: a team-scope Episode whose Append is rejected with
// kernel.ErrPermission (the writer is not an admitted team member) must
// surface as E_PERMISSION, not the opaque E_INTERNAL an agent cannot act on.
// Regression guard for the "scope=team surfaced as E_INTERNAL" report; mirrors
// create_entity's mapAppendError routing.
func TestRemember_TeamWriteByNonMemberPermissionDenied(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{err: kernel.ErrPermission})

	_, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"quarterly numbers","source":"chat","scope":"team"}`))
	if err == nil {
		t.Fatal("Invoke succeeded, want permission error")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodePermission {
		t.Errorf("err.Code = %q, want %q (team write by a non-member must be E_PERMISSION)", te.Code, mcp.CodePermission)
	}
	if !errors.Is(err, kernel.ErrPermission) {
		t.Errorf("err does not wrap kernel.ErrPermission: %v", err)
	}
}

// TestRemember_AppendInvalidEventIsValidation pins the schema-rejection arm of
// the same mapping: an ErrInvalidEvent from Append (e.g. a team-scope write
// attempted under the public local-default signer) is a caller-fixable
// E_VALIDATION, not E_INTERNAL.
func TestRemember_AppendInvalidEventIsValidation(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{err: kernel.ErrInvalidEvent})

	_, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"x","source":"chat","scope":"team"}`))
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
	}
}

// TestRemember_SchemaRejection proves the tool's declared input schema
// is well-formed and rejects every malformed argument shape the
// adapter would forward to Invoke. The mcp package's own server tests
// cover the adapter -> validator -> handler wiring; this test pins the
// schema content itself so a typo can't silently disable validation.
func TestRemember_SchemaRejection(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{})
	compiled := compileTestSchema(t, r.InputSchema())

	cases := []struct {
		name string
		body string
	}{
		{name: "missing text", body: `{"source":"chat"}`},
		{name: "empty text", body: `{"text":"","source":"chat"}`},
		{name: "empty source", body: `{"text":"hi","source":""}`},
		{name: "wrong type for confidence", body: `{"text":"hi","source":"chat","confidence":"high"}`},
		{name: "confidence below range", body: `{"text":"hi","source":"chat","confidence":-0.1}`},
		{name: "confidence above range", body: `{"text":"hi","source":"chat","confidence":1.1}`},
		{name: "extra property", body: `{"text":"hi","source":"chat","unknown":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(tc.body), &doc); err != nil {
				t.Fatalf("test body unparseable: %v", err)
			}
			if err := compiled.Validate(doc); err == nil {
				t.Errorf("schema accepted %q, expected rejection", tc.body)
			}
		})
	}
}

// TestRemember_SchemaAccepts pairs with SchemaRejection: the schema
// must admit the canonical valid bodies, including the
// confidence-omitted case the agent will most often send.
func TestRemember_SchemaAccepts(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{})
	compiled := compileTestSchema(t, r.InputSchema())

	cases := []string{
		`{"text":"hi"}`, // source omitted: the server defaults it to "mcp:remember"
		`{"text":"hi","source":"chat"}`,
		`{"text":"hi","source":"chat","confidence":0.5}`,
		`{"text":"hi","source":"chat","confidence":0}`,
		`{"text":"hi","source":"chat","confidence":1}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("test body unparseable: %v", err)
			}
			if err := compiled.Validate(doc); err != nil {
				t.Errorf("schema rejected %q: %v", body, err)
			}
		})
	}
}

// TestRemember_OutputMatchesPattern pins the OutputSchema's
// resource-name pattern against the handler's actual response. If the
// handler's UID-prefix logic ever drifts from "episodes/", the
// declared schema would mark the response invalid and clients honoring
// output schemas would reject it.
func TestRemember_OutputMatchesPattern(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{
		receipts: []kernel.Receipt{{UID: "01HX0000000000000000000003"}},
	}
	r := tools.NewRemember(fake)
	compiled := compileTestSchema(t, r.OutputSchema())

	resp, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"hi","source":"chat"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var doc any
	if err := json.Unmarshal(resp, &doc); err != nil {
		t.Fatalf("response unparseable: %v", err)
	}
	if err := compiled.Validate(doc); err != nil {
		t.Errorf("response %s fails its declared OutputSchema: %v", resp, err)
	}
}

// TestRemember_ActorRoundtrip is the Phase 1 contract test: the actor
// field on tools/call arguments must reach the persisted Episode payload
// verbatim so the extractor can branch on it (v6 ACTOR CONTEXT). When
// actor is absent, the handler defaults to "user"; when actor is
// "assistant" or "system", that value persists.
func TestRemember_ActorRoundtrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		body      string
		wantActor ontology.ActorKind
	}{
		{"default user", `{"text":"hi","source":"chat"}`, ontology.ActorUser},
		{"explicit user", `{"text":"hi","source":"chat","actor":"user"}`, ontology.ActorUser},
		{"assistant", `{"text":"I booked your flight.","source":"agent","actor":"assistant"}`, ontology.ActorAssistant},
		{"system", `{"text":"ingest scheduled","source":"daemon","actor":"system"}`, ontology.ActorSystem},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAppender{
				receipts: []kernel.Receipt{{UID: "01HX0000000000000000000004"}},
			}
			r := tools.NewRemember(fake)
			if _, err := r.Invoke(context.Background(), json.RawMessage(tc.body)); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if len(fake.gotEvents) != 1 {
				t.Fatalf("Append got %d events", len(fake.gotEvents))
			}
			var ep ontology.Episode
			if err := json.Unmarshal(fake.gotEvents[0].Payload, &ep); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if ep.Actor != tc.wantActor {
				t.Errorf("Episode.Actor = %q, want %q", ep.Actor, tc.wantActor)
			}
		})
	}
}

// TestRemember_ActorSchemaRejection pins that the schema rejects invalid
// actor values (typos, unknown personas) at the validation layer before
// they ever reach Invoke. Catching it early keeps the persisted Episode
// payload's enum invariant intact.
func TestRemember_ActorSchemaRejection(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{})
	compiled := compileTestSchema(t, r.InputSchema())

	cases := []string{
		`{"text":"hi","source":"chat","actor":"agent"}`,
		`{"text":"hi","source":"chat","actor":"User"}`,
		`{"text":"hi","source":"chat","actor":""}`,
		`{"text":"hi","source":"chat","actor":42}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("body unparseable: %v", err)
			}
			if err := compiled.Validate(doc); err == nil {
				t.Errorf("schema accepted %q, expected rejection", body)
			}
		})
	}
}

// TestRemember_ScopePlumbedOntoEvent proves an explicit scope reaches the
// dispatched Event so #206 scope isolation applies to remember writes
// (the fs-watch path relies on this to land project-scoped memory).
func TestRemember_ScopePlumbedOntoEvent(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{receipts: []kernel.Receipt{{UID: "01J0000000000000000000PROJ"}}}
	r := tools.NewRemember(fake)

	if _, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"edited main.go","source":"fs-watch:repo:file_change","scope":"project"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.gotEvents) != 1 {
		t.Fatalf("Append got %d events", len(fake.gotEvents))
	}
	if got := fake.gotEvents[0].Scope; got != kernel.ScopeProject {
		t.Errorf("Event.Scope = %q, want %q", got, kernel.ScopeProject)
	}
}

// TestRemember_ScopeDefaultsEmpty proves an omitted scope leaves the
// Event.Scope zero so FileEventLog.prepare backfills the personal
// default, preserving the prior remember() behaviour.
func TestRemember_ScopeDefaultsEmpty(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{receipts: []kernel.Receipt{{UID: "01J0000000000000000000PERS"}}}
	r := tools.NewRemember(fake)

	if _, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"hi","source":"chat"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := fake.gotEvents[0].Scope; got != "" {
		t.Errorf("Event.Scope = %q, want empty (backfilled to personal)", got)
	}
}

// TestRemember_InvalidScopeIsValidationError is the required error-code
// test: an unrecognised scope reaching Invoke surfaces E_VALIDATION (the
// schema enum is the first gate; this pins the handler's own guard).
func TestRemember_InvalidScopeIsValidationError(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{})

	_, err := r.Invoke(context.Background(),
		json.RawMessage(`{"text":"hi","source":"chat","scope":"galaxy"}`))
	if err == nil {
		t.Fatal("Invoke succeeded, want validation error")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
	}
}

// TestRemember_ScopeSchemaRejection pins that the declared schema rejects
// an out-of-enum scope before it ever reaches Invoke.
func TestRemember_ScopeSchemaRejection(t *testing.T) {
	t.Parallel()
	r := tools.NewRemember(&fakeAppender{})
	compiled := compileTestSchema(t, r.InputSchema())

	var doc any
	if err := json.Unmarshal([]byte(`{"text":"hi","source":"chat","scope":"galaxy"}`), &doc); err != nil {
		t.Fatalf("body unparseable: %v", err)
	}
	if err := compiled.Validate(doc); err == nil {
		t.Error("schema accepted scope=galaxy, expected rejection")
	}
}
