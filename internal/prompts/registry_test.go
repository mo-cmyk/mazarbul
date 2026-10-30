package prompts_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// -----------------------------------------------------------------------------
// fakeEventLog captures Append calls for assertion in tests.
// -----------------------------------------------------------------------------

type fakeEventLog struct {
	mu     sync.Mutex
	events []kernel.Event
}

func (f *fakeEventLog) Append(_ context.Context, evs []kernel.Event) ([]kernel.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evs...)
	receipts := make([]kernel.Receipt, len(evs))
	for i := range evs {
		receipts[i] = kernel.Receipt{UID: "test-uid", CreateTime: time.Now().UTC()}
	}
	return receipts, nil
}

func (f *fakeEventLog) Read(_ context.Context, _ kernel.ReadOpts) iter.Seq2[kernel.Event, error] { //nolint:gocritic // ReadOpts is part of the kernel.EventLog interface; cannot change signature
	return func(yield func(kernel.Event, error) bool) {}
}

func (f *fakeEventLog) At(_ context.Context, _ string) (kernel.Event, error) {
	return kernel.Event{}, kernel.ErrNotFound
}

func (f *fakeEventLog) Rebuild(_ context.Context, _ kernel.Storage, _ ...kernel.RebuildOption) ([]kernel.ApplyError, error) {
	return nil, nil
}

func (f *fakeEventLog) Provenance(_ context.Context, _ string) ([]kernel.ProvenanceRecord, error) {
	return nil, nil
}

func (f *fakeEventLog) captured() []kernel.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]kernel.Event, len(f.events))
	copy(out, f.events)
	return out
}

// waitForEvent blocks until at least one event is captured, or the deadline
// elapses. Used to synchronise the test against the background goroutine that
// emitRendered spawns.
func (f *fakeEventLog) waitForEvent(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(f.captured()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for audit event")
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func mustTemplate(name string, version prompts.Version, body string) prompts.Template {
	return prompts.Template{
		Name:        name,
		Version:     version,
		Body:        body,
		Description: "test template",
	}
}

// -----------------------------------------------------------------------------
// Register tests
// -----------------------------------------------------------------------------

func TestRegister_HappyPath(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	tmpl := mustTemplate("greet", 1, "Hello, {{.name}}!")
	if err := r.Register(ctx, tmpl); err != nil {
		t.Fatalf("Register: unexpected error: %v", err)
	}
}

func TestRegister_Idempotent(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	tmpl := mustTemplate("greet", 1, "Hello, {{.name}}!")
	if err := r.Register(ctx, tmpl); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Second identical call must succeed (idempotent).
	if err := r.Register(ctx, tmpl); err != nil {
		t.Fatalf("second Register (idempotent): unexpected error: %v", err)
	}
}

func TestRegister_VersionConflict(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	if err := r.Register(ctx, mustTemplate("greet", 1, "Hello, {{.name}}!")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(ctx, mustTemplate("greet", 1, "Hi, {{.name}}!"))
	if !errors.Is(err, prompts.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got: %v", err)
	}
}

func TestRegister_ZeroVersion(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	err := r.Register(ctx, mustTemplate("greet", 0, "Hello"))
	if err == nil {
		t.Fatal("expected error for version 0, got nil")
	}
}

func TestRegister_EmptyName(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	err := r.Register(ctx, prompts.Template{Version: 1, Body: "x"})
	if err == nil {
		t.Fatal("expected error for empty Name, got nil")
	}
}

func TestRegister_InvalidTemplate(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	err := r.Register(ctx, mustTemplate("bad", 1, "{{.unclosed"))
	if err == nil {
		t.Fatal("expected error for malformed template body, got nil")
	}
}

// -----------------------------------------------------------------------------
// Get tests
// -----------------------------------------------------------------------------

func TestGet_HappyPath(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	tmpl := mustTemplate("recall.query", 2, "Find: {{.query}}")
	if err := r.Register(ctx, tmpl); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := r.Get(ctx, "recall.query", 2)
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if got.Body != tmpl.Body {
		t.Errorf("Get: body mismatch: got %q, want %q", got.Body, tmpl.Body)
	}
	if got.CreateTime.IsZero() {
		t.Error("Get: CreateTime not set by registry")
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	_, err := r.Get(ctx, "missing", 1)
	if !errors.Is(err, prompts.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Render tests
// -----------------------------------------------------------------------------

func TestRender_HappyPath(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	body := "Hello, {{index . \"name\"}}!"
	if err := r.Register(ctx, mustTemplate("greet", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	out, err := r.Render(ctx, "greet", 1, prompts.RenderVars{"name": "World"})
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	if out != "Hello, World!" {
		t.Errorf("Render: got %q, want %q", out, "Hello, World!")
	}
}

func TestRender_NotFound(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	_, err := r.Render(ctx, "missing", 1, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var renderErr *prompts.RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected *RenderError, got: %T %v", err, err)
	}
	if !errors.Is(err, prompts.ErrNotFound) {
		t.Fatalf("expected ErrNotFound in chain, got: %v", err)
	}
}

func TestRender_NilVars(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	if err := r.Register(ctx, mustTemplate("static", 1, "no vars here")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	out, err := r.Render(ctx, "static", 1, nil)
	if err != nil {
		t.Fatalf("Render with nil vars: %v", err)
	}
	if out != "no vars here" {
		t.Errorf("got %q, want %q", out, "no vars here")
	}
}

// -----------------------------------------------------------------------------
// Audit event tests
// -----------------------------------------------------------------------------

func TestRender_AuditEventEmitted(t *testing.T) {
	t.Parallel()
	fake := &fakeEventLog{}
	r := prompts.New(fake)
	ctx := context.Background()

	body := "Query: {{index . \"q\"}}"
	if err := r.Register(ctx, mustTemplate("recall.query", 3, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	vars := prompts.RenderVars{"q": "who is Merry"}
	out, err := r.Render(ctx, "recall.query", 3, vars)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	fake.waitForEvent(t, 2*time.Second)

	evs := fake.captured()
	if len(evs) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(evs))
	}

	ev := evs[0]
	if ev.Kind != prompts.KindRendered {
		t.Errorf("event Kind: got %q, want %q", ev.Kind, prompts.KindRendered)
	}
	if ev.Source != "prompts/registry" {
		t.Errorf("event Source: got %q, want %q", ev.Source, "prompts/registry")
	}
	if ev.Confidence != 1.0 {
		t.Errorf("event Confidence: got %v, want 1.0", ev.Confidence)
	}

	var payload prompts.RenderedPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if payload.TemplateName != "recall.query" {
		t.Errorf("payload.TemplateName: got %q, want %q", payload.TemplateName, "recall.query")
	}
	if payload.TemplateVersion != 3 {
		t.Errorf("payload.TemplateVersion: got %d, want 3", payload.TemplateVersion)
	}
	if len(payload.InputHash) != 64 {
		t.Errorf("payload.InputHash: expected 64 hex chars, got %d", len(payload.InputHash))
	}
	if payload.OutputLen != len(out) {
		t.Errorf("payload.OutputLen: got %d, want %d", payload.OutputLen, len(out))
	}

	// Privacy: raw variable value must not appear in the serialised payload.
	raw := string(ev.Payload)
	if strings.Contains(raw, "who is Merry") {
		t.Error("audit payload contains raw var value; privacy violation")
	}
}

func TestRender_AuditNilLog(t *testing.T) {
	t.Parallel()
	// nil log must not panic.
	r := prompts.New(nil)
	ctx := context.Background()

	if err := r.Register(ctx, mustTemplate("x", 1, "hello")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := r.Render(ctx, "x", 1, nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
}

// -----------------------------------------------------------------------------
// InputHash determinism
// -----------------------------------------------------------------------------

func TestInputHash_Deterministic(t *testing.T) {
	t.Parallel()
	// Two maps with the same content but different insertion order must
	// produce the same hash (canonical JSON must sort keys).
	fake := &fakeEventLog{}
	r := prompts.New(fake)
	ctx := context.Background()

	body := "{{index . \"a\"}} {{index . \"b\"}}"
	if err := r.Register(ctx, mustTemplate("det", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	vars1 := prompts.RenderVars{"a": "foo", "b": "bar"}
	vars2 := prompts.RenderVars{"b": "bar", "a": "foo"}

	if _, err := r.Render(ctx, "det", 1, vars1); err != nil {
		t.Fatalf("Render vars1: %v", err)
	}
	fake.waitForEvent(t, 2*time.Second)

	if _, err := r.Render(ctx, "det", 1, vars2); err != nil {
		t.Fatalf("Render vars2: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.captured()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	evs := fake.captured()
	if len(evs) < 2 {
		t.Fatalf("expected 2 audit events, got %d", len(evs))
	}

	var p1, p2 prompts.RenderedPayload
	if err := json.Unmarshal(evs[0].Payload, &p1); err != nil {
		t.Fatalf("unmarshal p1: %v", err)
	}
	if err := json.Unmarshal(evs[1].Payload, &p2); err != nil {
		t.Fatalf("unmarshal p2: %v", err)
	}
	if p1.InputHash != p2.InputHash {
		t.Errorf("hashes differ: %s vs %s", p1.InputHash, p2.InputHash)
	}
}

// -----------------------------------------------------------------------------
// Missing key and context cancellation tests
// -----------------------------------------------------------------------------

func TestRender_MissingKeyReturnsError(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	// Template references .name via dot-notation; missingkey=error must surface
	// this as a RenderError instead of silently rendering an empty string.
	body := "Hello, {{.name}}!"
	if err := r.Register(ctx, mustTemplate("greet.strict", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := r.Render(ctx, "greet.strict", 1, prompts.RenderVars{})
	if err == nil {
		t.Fatal("expected error for missing dot-notation key, got nil")
	}
	var renderErr *prompts.RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected *RenderError, got: %T %v", err, err)
	}
}

func TestRender_MissingKeyIndexReturnsError(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	// Template uses {{index . "key"}} syntax; the built-in index function returns
	// the zero string for missing keys. Our checked replacement must return an
	// error instead so both template authoring styles fail fast on missing vars.
	body := `Hello, {{index . "name"}}!`
	if err := r.Register(ctx, mustTemplate("greet.index", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := r.Render(ctx, "greet.index", 1, prompts.RenderVars{})
	if err == nil {
		t.Fatal("expected error for missing index key, got nil")
	}
	var renderErr *prompts.RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected *RenderError, got: %T %v", err, err)
	}
}

func TestRender_MissingKeyInControlPipeReturnsError(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	// Key referenced in the condition pipe of {{if}} must be captured.
	// Without walking IfNode.Pipe, the required-key set misses "flag" and
	// rendering with missing "flag" would silently succeed via the built-in
	// index zero value.
	body := `{{if index . "flag"}}enabled{{else}}disabled{{end}}`
	if err := r.Register(ctx, mustTemplate("ctrl.pipe", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := r.Render(ctx, "ctrl.pipe", 1, prompts.RenderVars{})
	if err == nil {
		t.Fatal("expected error for missing key in control-flow pipe, got nil")
	}
	var renderErr *prompts.RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected *RenderError, got: %T %v", err, err)
	}
}

func TestRender_MissingKeyInInvokedTemplateReturnsError(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	// Key referenced inside a {{define}} block that IS invoked via {{template}}
	// must be captured. Without following TemplateNode invocations, "name"
	// would be missed and rendering would silently succeed.
	body := `{{define "inner"}}Hello, {{index . "name"}}{{end}}{{template "inner" .}}`
	if err := r.Register(ctx, mustTemplate("named.invoked", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := r.Render(ctx, "named.invoked", 1, prompts.RenderVars{})
	if err == nil {
		t.Fatal("expected error for missing key in invoked sub-template, got nil")
	}
	var renderErr *prompts.RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected *RenderError, got: %T %v", err, err)
	}
}

func TestRender_UnreachableDefineDoesNotRequireKey(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	// A {{define}} block that is never invoked must NOT contribute to the
	// required-key set. Rendering the root with empty vars must succeed.
	body := `Hello{{define "unused"}}{{index . "secret"}}{{end}}`
	if err := r.Register(ctx, mustTemplate("named.unreachable", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	out, err := r.Render(ctx, "named.unreachable", 1, prompts.RenderVars{})
	if err != nil {
		t.Fatalf("expected success for unreachable define, got: %v", err)
	}
	if out != "Hello" {
		t.Errorf("got %q, want %q", out, "Hello")
	}
}

func TestRender_MissingKeyDynamicIndexReturnsError(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	// Dynamic computed-key form: {{$k := "name"}}{{index . $k}}.
	// The key arrives as a reflect.Value at execution time; checkedIndex must
	// still error when it is absent from vars.
	body := `{{$k := "name"}}{{index . $k}}`
	if err := r.Register(ctx, mustTemplate("greet.dynamic", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := r.Render(ctx, "greet.dynamic", 1, prompts.RenderVars{})
	if err == nil {
		t.Fatal("expected error for missing dynamic-index key, got nil")
	}
	var renderErr *prompts.RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected *RenderError, got: %T %v", err, err)
	}
}

func TestRender_CancelledContextReturnsError(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	if err := r.Register(context.Background(), mustTemplate("any", 1, "body")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := r.Render(ctx, "any", 1, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
	var renderErr *prompts.RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected *RenderError, got: %T %v", err, err)
	}
}

func TestRender_VarsSnapshotted(t *testing.T) {
	t.Parallel()
	// Mutating the original RenderVars map after Render returns must not race
	// with the audit goroutine. Run with -race to detect.
	fake := &fakeEventLog{}
	r := prompts.New(fake)
	ctx := context.Background()

	body := "{{index . \"k\"}}"
	if err := r.Register(ctx, mustTemplate("snap", 1, body)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	vars := prompts.RenderVars{"k": "original"}
	if _, err := r.Render(ctx, "snap", 1, vars); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Mutate the original map immediately after Render returns; the audit
	// goroutine must operate on its own copy.
	vars["k"] = "mutated"

	fake.waitForEvent(t, 2*time.Second)
	// No assertion on the hash value -- the race detector is the guard here.
}

// -----------------------------------------------------------------------------
// List tests
// -----------------------------------------------------------------------------

func TestList(t *testing.T) {
	t.Parallel()
	r := prompts.New(nil)
	ctx := context.Background()

	templates := []prompts.Template{
		mustTemplate("a", 1, "body a"),
		mustTemplate("b", 1, "body b"),
		mustTemplate("a", 2, "body a v2"),
	}
	for _, tmpl := range templates {
		if err := r.Register(ctx, tmpl); err != nil {
			t.Fatalf("Register %s@%d: %v", tmpl.Name, tmpl.Version, err)
		}
	}

	list, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(templates) {
		t.Errorf("List length: got %d, want %d", len(list), len(templates))
	}
}
