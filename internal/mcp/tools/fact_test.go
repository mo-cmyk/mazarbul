package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// factValidULID is an arbitrary well-formed ULID used as a subject entity.
const factValidULID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// factAppenderStub captures the events the fact tool dispatches. (The
// tools_test package's fakeAppender is not visible from this white-box test
// package, which is where fakeNavigator + wantCode live.)
type factAppenderStub struct {
	called    bool
	gotEvents []kernel.Event
	err       error
}

func (f *factAppenderStub) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	f.called = true
	f.gotEvents = append(f.gotEvents, events...)
	return nil, f.err
}

// TestFact_AssertsKindFactEvent pins the happy path: the tool builds a Fact
// payload and dispatches it as a KindFact event, returning a facts/{uid} name.
func TestFact_AssertsKindFactEvent(t *testing.T) {
	t.Parallel()
	app := &factAppenderStub{}
	// entity present → subject existence check passes.
	f := NewFact(app, &fakeNavigator{entity: &navigate.Entity{UID: factValidULID}})

	req, _ := json.Marshal(map[string]any{
		"subject":  factValidULID,
		"stateKey": "state.task.status",
		"value":    "pending",
	})
	out, err := f.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !app.called || len(app.gotEvents) != 1 {
		t.Fatalf("expected one appended event, got called=%v n=%d", app.called, len(app.gotEvents))
	}
	if got := app.gotEvents[0].Kind; got != kernel.EventKind(ontology.KindFact) {
		t.Errorf("event kind = %q, want %q", got, ontology.KindFact)
	}
	var fact ontology.Fact
	if uerr := json.Unmarshal(app.gotEvents[0].Payload, &fact); uerr != nil {
		t.Fatalf("decode payload: %v", uerr)
	}
	if fact.SubjectUID != factValidULID || fact.StateKey != "state.task.status" || fact.Value != "pending" {
		t.Errorf("fact payload = %+v", fact)
	}
	if fact.SourceEventUID != fact.UID {
		t.Errorf("fact should be self-sourced: SourceEventUID=%q UID=%q", fact.SourceEventUID, fact.UID)
	}
	// The outer event UID must be pinned to the fact UID so get(facts/{uid})
	// can walk SourceEventUID for provenance.
	if app.gotEvents[0].UID != fact.UID {
		t.Errorf("event UID = %q, want fact UID %q (pin required for provenance)", app.gotEvents[0].UID, fact.UID)
	}
	var resp struct{ Name, Status string }
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode response: %v", uerr)
	}
	if resp.Status != "asserted" || resp.Name != fact.Name {
		t.Errorf("response = %+v, want asserted %s", resp, fact.Name)
	}
}

// TestFact_SourceFlowsThrough confirms an explicit source overrides the
// mcp:fact default on the appended event (the CLI passes "cli:fact"), and that
// omitting it keeps the default.
func TestFact_SourceFlowsThrough(t *testing.T) {
	t.Parallel()
	app := &factAppenderStub{}
	f := NewFact(app, &fakeNavigator{entity: &navigate.Entity{UID: factValidULID}})
	req, _ := json.Marshal(map[string]any{
		"subject":  factValidULID,
		"stateKey": "state.task.status",
		"value":    "pending",
		"source":   "cli:fact",
	})
	if _, err := f.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.gotEvents))
	}
	if got := app.gotEvents[0].Source; got != "cli:fact" {
		t.Errorf("event source = %q, want cli:fact", got)
	}

	def := &factAppenderStub{}
	fd := NewFact(def, &fakeNavigator{entity: &navigate.Entity{UID: factValidULID}})
	reqDef, _ := json.Marshal(map[string]any{
		"subject":  factValidULID,
		"stateKey": "state.task.status",
		"value":    "pending",
	})
	if _, err := fd.Invoke(context.Background(), reqDef); err != nil {
		t.Fatalf("Invoke(default): %v", err)
	}
	if got := def.gotEvents[0].Source; got != "mcp:fact" {
		t.Errorf("default event source = %q, want mcp:fact", got)
	}
}

// TestFact_SelfSubjectSkipsExistenceCheck confirms the "self" sentinel writes
// without an entity lookup (mirrors relate's self handling).
func TestFact_SelfSubjectSkipsExistenceCheck(t *testing.T) {
	t.Parallel()
	app := &factAppenderStub{}
	f := NewFact(app, &fakeNavigator{}) // no entity seeded
	req, _ := json.Marshal(map[string]any{
		"subject":  "self",
		"stateKey": "preference.coffee.milk",
		"value":    "oat",
	})
	if _, err := f.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke(self): %v", err)
	}
	if !app.called {
		t.Fatal("expected append for self subject")
	}
}

// TestFact_RejectsBadStateKey checks a malformed slot address is E_VALIDATION
// (not an opaque schema rejection downstream) and never reaches the appender.
func TestFact_RejectsBadStateKey(t *testing.T) {
	t.Parallel()
	app := &factAppenderStub{}
	f := NewFact(app, &fakeNavigator{entity: &navigate.Entity{UID: factValidULID}})
	req, _ := json.Marshal(map[string]any{
		"subject":  factValidULID,
		"stateKey": "Bad Key", // spaces + uppercase
		"value":    "x",
	})
	_, err := f.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if app.called {
		t.Error("appender must not be called on a validation failure")
	}
}

// TestFact_RejectsUnknownSubject checks a non-self subject that does not exist
// is E_NOT_FOUND.
func TestFact_RejectsUnknownSubject(t *testing.T) {
	t.Parallel()
	app := &factAppenderStub{}
	f := NewFact(app, &fakeNavigator{}) // GetEntity → ErrNotFound
	req, _ := json.Marshal(map[string]any{
		"subject":  factValidULID,
		"stateKey": "state.task.status",
		"value":    "pending",
	})
	_, err := f.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
	if app.called {
		t.Error("appender must not be called when the subject is missing")
	}
}

// TestGet_FactByName routes facts/{uid} to GetFact and tags the union "fact",
// hydrating provenance via the fact's SourceEventUID (mirrors the relation arm).
func TestGet_FactByName(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{
		fact: &navigate.Fact{
			UID: "f1", Name: "facts/f1", SubjectUID: "alice",
			StateKey: "state.task.status", Value: "pending",
			SourceEventUID: "ev1",
		},
		prov: []navigate.Provenance{
			{EventUID: "ev1", Source: "cli:fact", Confidence: 1.0},
		},
	})
	req, _ := json.Marshal(map[string]any{"name": "facts/f1"})
	out, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var resp struct {
		Type       string                `json:"type"`
		Fact       *navigate.Fact        `json:"fact"`
		Provenance []navigate.Provenance `json:"provenance"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.Type != "fact" || resp.Fact == nil || resp.Fact.Value != "pending" {
		t.Errorf("response = %+v", resp)
	}
	if len(resp.Provenance) != 1 || resp.Provenance[0].EventUID != "ev1" {
		t.Errorf("provenance = %+v, want one ev1 record walked via SourceEventUID", resp.Provenance)
	}
}

// TestGet_FactWithoutSourceSkipsProvenance confirms a fact with no
// SourceEventUID returns no provenance and never calls EventProvenance for an
// empty key.
func TestGet_FactWithoutSourceSkipsProvenance(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{
		fact: &navigate.Fact{
			UID: "f1", Name: "facts/f1", SubjectUID: "alice",
			StateKey: "state.task.status", Value: "pending",
		},
		// prov is seeded but must be ignored: EventProvenance is not called
		// when SourceEventUID is empty.
		prov: []navigate.Provenance{{EventUID: "ev1", Source: "cli:fact", Confidence: 1.0}},
	})
	req, _ := json.Marshal(map[string]any{"name": "facts/f1"})
	out, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var resp struct {
		Type       string                `json:"type"`
		Provenance []navigate.Provenance `json:"provenance"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.Type != "fact" || len(resp.Provenance) != 0 {
		t.Errorf("response = %+v, want fact with no provenance", resp)
	}
}

// TestGet_FactSyntheticProvenance confirms that a direct-assertion fact (no
// episode provenance row) surfaces synthetic provenance built from the fact
// row's own source + confidence columns (migration 0017).  This path fires
// when EventProvenance returns empty but the fact carries a non-empty Source.
func TestGet_FactSyntheticProvenance(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{
		fact: &navigate.Fact{
			UID: "f1", Name: "facts/f1", SubjectUID: "alice",
			StateKey: "state.task.status", Value: "pending",
			SourceEventUID: "f1",       // pinned to fact UID
			Source:         "mcp:fact", // written by applyFact (migration 0017)
			Confidence:     1.0,
		},
		// prov is nil → EventProvenance returns empty → synthetic fallback fires.
	})
	req, _ := json.Marshal(map[string]any{"name": "facts/f1"})
	out, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var resp struct {
		Type       string                `json:"type"`
		Provenance []navigate.Provenance `json:"provenance"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.Type != "fact" {
		t.Errorf("type = %q, want fact", resp.Type)
	}
	if len(resp.Provenance) != 1 {
		t.Fatalf("provenance len = %d, want 1 (synthetic)", len(resp.Provenance))
	}
	p := resp.Provenance[0]
	if p.Source != "mcp:fact" || p.Confidence != 1.0 || p.EventUID != "f1" {
		t.Errorf("synthetic provenance = %+v, want {EventUID:f1, Source:mcp:fact, Confidence:1.0}", p)
	}
}

// TestGet_EntityIncludesOpenFacts confirms get(entities/{uid}) surfaces the
// entity's open facts in the sibling facts[] array.
func TestGet_EntityIncludesOpenFacts(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{
		entity: &navigate.Entity{UID: "alice", Kind: "person", DisplayName: "Merry"},
		facts: []navigate.Fact{
			{UID: "f1", Name: "facts/f1", StateKey: "state.task.status", Value: "pending"},
		},
	})
	req, _ := json.Marshal(map[string]any{"name": "entities/alice"})
	out, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var resp struct {
		Type  string          `json:"type"`
		Facts []navigate.Fact `json:"facts"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.Type != "entity" || len(resp.Facts) != 1 || resp.Facts[0].StateKey != "state.task.status" {
		t.Errorf("response = %+v", resp)
	}
}

// TestGet_EntityEmptyFactsStillPresent pins the present-and-empty contract: an
// entity with zero open facts still serializes "facts": [] (not absent, not
// null), so clients can distinguish a facts-aware server with no open facts
// from an old server that never emits the field.
func TestGet_EntityEmptyFactsStillPresent(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{
		entity: &navigate.Entity{UID: "alice", Kind: "person", DisplayName: "Merry"},
		// facts left nil → navigator returns no rows.
	})
	req, _ := json.Marshal(map[string]any{"name": "entities/alice"})
	out, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// Inspect the raw JSON: the key must be present and an empty array, never
	// "null" and never omitted.
	var raw map[string]json.RawMessage
	if uerr := json.Unmarshal(out, &raw); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	f, ok := raw["facts"]
	if !ok {
		t.Fatalf("entity response missing facts key: %s", out)
	}
	if string(f) != "[]" {
		t.Errorf("facts = %s, want []", f)
	}
}

// TestGet_EpisodeOmitsFactsKey confirms the facts key is absent (not null) on
// a non-entity arm, so the pointer never leaks "facts": null off the entity arm.
func TestGet_EpisodeOmitsFactsKey(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{episode: &navigate.Episode{UID: "ep1"}})
	req, _ := json.Marshal(map[string]any{"name": "episodes/ep1"})
	out, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var raw map[string]json.RawMessage
	if uerr := json.Unmarshal(out, &raw); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if _, ok := raw["facts"]; ok {
		t.Errorf("episode response must omit facts key, got: %s", out)
	}
}
