package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// fakeNavigator is a configurable Navigator for the tool projection +
// error-mapping tests. Any field left nil returns navigate.ErrNotFound (for the
// getters) or an empty slice (for the listers), so a test sets only what it
// exercises.
type fakeNavigator struct {
	entity             *navigate.Entity
	episode            *navigate.Episode
	episodes           []navigate.Episode
	relation           *navigate.Relation
	relations          []navigate.Relation
	neighbors          []navigate.Neighbor
	entities           []navigate.Entity
	prov               []navigate.Provenance
	preds              []navigate.Predicate
	kinds              []navigate.KindCount
	registeredKinds    []navigate.RegisteredEntityKind
	fact               *navigate.Fact
	facts              []navigate.Fact
	sourceRef          *navigate.SourceRef
	insight            *navigate.Insight
	consolidatedMemory *navigate.ConsolidatedMemory
	identifiers        []navigate.EntityIdentifier
	count              int64
	groups             []navigate.GroupCount
	err                error // when set, every method returns it
}

func (f *fakeNavigator) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	if f.err != nil {
		return navigate.Entity{}, f.err
	}
	if f.entity != nil {
		return *f.entity, nil // single configured entity wins for any uid
	}
	for _, e := range f.entities {
		if e.UID == uid {
			return e, nil
		}
	}
	return navigate.Entity{}, navigate.ErrNotFound
}

func (f *fakeNavigator) GetEpisode(_ context.Context, _ string) (navigate.Episode, error) {
	if f.err != nil {
		return navigate.Episode{}, f.err
	}
	if f.episode == nil {
		return navigate.Episode{}, navigate.ErrNotFound
	}
	return *f.episode, nil
}

func (f *fakeNavigator) GetRelation(_ context.Context, _ string) (navigate.Relation, error) {
	if f.err != nil {
		return navigate.Relation{}, f.err
	}
	if f.relation == nil {
		return navigate.Relation{}, navigate.ErrNotFound
	}
	return *f.relation, nil
}

func (f *fakeNavigator) EntityRelations(_ context.Context, _ string, _ navigate.RelationFilter) ([]navigate.Relation, error) {
	return f.relations, f.err
}

func (f *fakeNavigator) Neighbors(_ context.Context, _ string, _ int, _ navigate.RelationFilter) ([]navigate.Neighbor, error) {
	return f.neighbors, f.err
}

func (f *fakeNavigator) ListEntities(_ context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	if f.err != nil {
		return nil, f.err
	}
	// Mirror the real Navigator's filter semantics so the resolution paths
	// (exact vs substring, with a presentation cap) are exercised honestly.
	out := make([]navigate.Entity, 0, len(f.entities))
	for _, e := range f.entities {
		switch {
		case opts.NameExact != "":
			if !strings.EqualFold(e.DisplayName, opts.NameExact) {
				continue
			}
		case opts.NameLike != "":
			if !strings.Contains(strings.ToLower(e.DisplayName), strings.ToLower(opts.NameLike)) {
				continue
			}
		}
		if opts.Kind != "" && e.Kind != opts.Kind {
			continue
		}
		if opts.MinConfidence > 0 && e.Confidence < opts.MinConfidence {
			continue
		}
		out = append(out, e)
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (f *fakeNavigator) EpisodesCitingEntity(_ context.Context, _ string, _ int) ([]navigate.Episode, error) {
	return nil, f.err
}

func (f *fakeNavigator) EventProvenance(_ context.Context, _ string) ([]navigate.Provenance, error) {
	return f.prov, f.err
}

func (f *fakeNavigator) EntityProvenance(_ context.Context, _ string) ([]navigate.Provenance, error) {
	return f.prov, f.err
}

func (f *fakeNavigator) EntityIdentifiers(_ context.Context, _ string) ([]navigate.EntityIdentifier, error) {
	return f.identifiers, f.err
}

func (f *fakeNavigator) Predicates(_ context.Context) ([]navigate.Predicate, error) {
	return f.preds, f.err
}

func (f *fakeNavigator) EntityKinds(_ context.Context) ([]navigate.KindCount, error) {
	return f.kinds, f.err
}

func (f *fakeNavigator) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	return f.registeredKinds, f.err
}

func (f *fakeNavigator) GetFact(_ context.Context, _ string) (navigate.Fact, error) {
	if f.err != nil {
		return navigate.Fact{}, f.err
	}
	if f.fact == nil {
		return navigate.Fact{}, navigate.ErrNotFound
	}
	return *f.fact, nil
}

func (f *fakeNavigator) FactsForSubject(_ context.Context, _ string, _ navigate.FactFilter) ([]navigate.Fact, error) {
	return f.facts, f.err
}

func (f *fakeNavigator) GetSourceRef(_ context.Context, _ string) (navigate.SourceRef, error) {
	if f.err != nil {
		return navigate.SourceRef{}, f.err
	}
	if f.sourceRef == nil {
		return navigate.SourceRef{}, navigate.ErrNotFound
	}
	return *f.sourceRef, nil
}

func (f *fakeNavigator) GetInsight(_ context.Context, _ string) (navigate.Insight, error) {
	if f.err != nil {
		return navigate.Insight{}, f.err
	}
	if f.insight == nil {
		return navigate.Insight{}, navigate.ErrNotFound
	}
	return *f.insight, nil
}

func (f *fakeNavigator) GetConsolidatedMemory(_ context.Context, _ string) (navigate.ConsolidatedMemory, error) {
	if f.err != nil {
		return navigate.ConsolidatedMemory{}, f.err
	}
	if f.consolidatedMemory == nil {
		return navigate.ConsolidatedMemory{}, navigate.ErrNotFound
	}
	return *f.consolidatedMemory, nil
}

func (f *fakeNavigator) CountEntities(_ context.Context, _ navigate.EntityFilter) (int64, error) {
	return f.count, f.err
}

func (f *fakeNavigator) AggregateEntities(_ context.Context, _ navigate.AggregateSpec) ([]navigate.GroupCount, error) {
	return f.groups, f.err
}

func (f *fakeNavigator) ListEpisodes(_ context.Context, _ navigate.EpisodeFilter) ([]navigate.Episode, error) {
	return f.episodes, f.err
}

// wantCode asserts err is an *mcp.ToolError carrying code.
func wantCode(t *testing.T, err error, code mcp.ToolErrorCode) {
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

// TestAskIsUnlisted_NavigationVerbsAreListed pins the discovery contract: ask
// opts out of tools/list (so external agents stay on the LLM-free verbs) while
// every navigation verb stays discoverable.
func TestAskIsUnlisted_NavigationVerbsAreListed(t *testing.T) {
	t.Parallel()
	if u, ok := mcp.Tool(&Ask{}).(mcp.UnlistedTool); !ok || !u.Unlisted() {
		t.Fatalf("ask must implement UnlistedTool and return true (ok=%v)", ok)
	}
	listed := []mcp.Tool{
		NewGet(&fakeNavigator{}),
		NewNeighbors(&fakeNavigator{}),
		NewRelations(&fakeNavigator{}),
		NewEntities(&fakeNavigator{}, nil),
		NewIntrospect(&fakeNavigator{}, nil),
		NewRelate(&captureAppender{}, relateNav()),
	}
	for _, tool := range listed {
		if u, ok := tool.(mcp.UnlistedTool); ok && u.Unlisted() {
			t.Errorf("%s must stay listed (discoverable), but reports Unlisted()=true", tool.Name())
		}
	}
}

// ---- get ----

func TestGet_MalformedName_Validation(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{})
	// A resource name with a KNOWN collection but an empty uid is malformed →
	// E_VALIDATION. A bare token (no "/") is NOT malformed — it is an entity
	// name/uid resolution attempt (TestGet_BareName_* below). A string whose
	// prefix is NOT a known collection (e.g. "widgets/abc") is likewise no longer
	// malformed: it is treated as a display name so slashed names like
	// "Ring Mk 4000 / RFC 1149" resolve — see TestGet_UnknownCollectionPrefix_AsName.
	for _, name := range []string{`{"name":"entities/"}`, `{"name":"relations/"}`} {
		_, err := g.Invoke(context.Background(), json.RawMessage(name))
		wantCode(t, err, mcp.CodeValidation)
	}
}

// TestGet_UnknownCollectionPrefix_AsName pins the slash-in-name fix: a string
// whose segment before "/" is not a recognized collection is a display name, not
// a malformed resource name, so an absent one is E_NOT_FOUND (not the old
// E_VALIDATION "unknown collection"). This is what lets entities named e.g.
// "Ring Mk 4000 / RFC 1149 Integration" be reachable by name.
func TestGet_UnknownCollectionPrefix_AsName(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{})
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"widgets/abc"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestGet_BareName_ResolvesEntity pins the consistency fix: `get <display name>`
// and `get <bare uid>` resolve to the entity, exactly like show/relations/
// neighbors, instead of demanding the entities/{uid} resource-name form.
func TestGet_BareName_ResolvesEntity(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: "alice", Kind: "person", DisplayName: "Mira Stonebrook"},
	}}
	for _, name := range []string{"Mira Stonebrook", "mira stonebrook", "alice"} {
		g := NewGet(nav)
		req, _ := json.Marshal(map[string]string{"name": name})
		body, err := g.Invoke(context.Background(), req)
		if err != nil {
			t.Fatalf("Invoke(%q): %v", name, err)
		}
		var out getOutput
		if uerr := json.Unmarshal(body, &out); uerr != nil {
			t.Fatalf("decode: %v", uerr)
		}
		if out.Type != "entity" || out.Entity == nil || out.Entity.Name != "entities/alice" {
			t.Fatalf("get %q = %s, want type=entity name=entities/alice", name, body)
		}
	}
}

// TestGet_BareName_Unknown_NotFound pins that an unknown bare name resolves to a
// pointed E_NOT_FOUND (naming the lookup), not the old "must be entities/{uid}"
// validation error.
func TestGet_BareName_Unknown_NotFound(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{}) // no entities → name resolves to nothing
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"Nobody At All"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

func TestGet_UnknownUID_NotFound(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{}) // entity nil → ErrNotFound
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"entities/ghost"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestGet_BadTimeAnchor_Validation pins that a malformed --at / --as-of anchor
// is rejected as E_VALIDATION (the existing shared time-anchor path), the same
// failure the relations/neighbors tools return, before any entity lookup.
func TestGet_BadTimeAnchor_Validation(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{entity: &navigate.Entity{UID: "alice", Kind: "person", DisplayName: "Merry"}})
	for _, req := range []string{
		`{"name":"entities/alice","at":"not-a-time"}`,
		`{"name":"entities/alice","asOf":"yesterday"}`,
	} {
		_, err := g.Invoke(context.Background(), json.RawMessage(req))
		wantCode(t, err, mcp.CodeValidation)
	}
}

func TestGet_Entity_HappyPath(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{entity: &navigate.Entity{UID: "alice", Kind: "person", DisplayName: "Merry"}})
	body, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"entities/alice"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if out.Type != "entity" || out.Entity == nil || out.Entity.Name != "entities/alice" {
		t.Fatalf("get entity = %s, want type=entity name=entities/alice", body)
	}
	if out.Episode != nil || out.Relation != nil {
		t.Fatalf("non-entity arms must be nil: %s", body)
	}
}

func TestGet_Episode_HappyPath(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{episode: &navigate.Episode{UID: "ep1", Text: "Merry lives in Zurich.", Source: "cli:test"}})
	body, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"episodes/ep1"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if out.Type != "episode" || out.Episode == nil || out.Episode.Name != "episodes/ep1" {
		t.Fatalf("get episode = %s, want type=episode name=episodes/ep1", body)
	}
	if out.Entity != nil || out.Relation != nil {
		t.Fatalf("non-episode arms must be nil: %s", body)
	}
}

func TestGet_AttachesProvenance(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{
		entity: &navigate.Entity{UID: "alice", Kind: "person", DisplayName: "Merry"},
		prov:   []navigate.Provenance{{EventUID: "ep1", Source: "cli:remember", Confidence: 0.9, DecidedAt: "2026-06-03T00:00:00Z"}},
	})
	body, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"entities/alice"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if len(out.Provenance) != 1 || out.Provenance[0].EventUID != "ep1" || out.Provenance[0].Source != "cli:remember" {
		t.Fatalf("get provenance = %s, want one ep1/cli:remember record", body)
	}
}

func TestGet_Relation_CarriesEndpointNames(t *testing.T) {
	t.Parallel()
	g := NewGet(&fakeNavigator{relation: &navigate.Relation{UID: "r1", SubjectUID: "alice", Predicate: "reports_to", ObjectUID: "bob"}})
	body, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"relations/r1"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	_ = json.Unmarshal(body, &out)
	if out.Type != "relation" || out.Relation == nil {
		t.Fatalf("want relation arm: %s", body)
	}
	if out.Relation.Subject != "entities/alice" || out.Relation.Object != "entities/bob" {
		t.Fatalf("endpoint resource names = %s/%s, want entities/alice and entities/bob", out.Relation.Subject, out.Relation.Object)
	}
}

// ---- relations ----

func TestRelations_WrongCollection_Validation(t *testing.T) {
	t.Parallel()
	r := NewRelations(&fakeNavigator{})
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"entity":"episodes/ep1"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestRelations_HappyPath_AcceptsResourceName(t *testing.T) {
	t.Parallel()
	r := NewRelations(&fakeNavigator{relations: []navigate.Relation{
		{UID: "r1", SubjectUID: "alice", Predicate: "reports_to", ObjectUID: "bob", SubjectName: "Merry", ObjectName: "Sam"},
		{UID: "r2", SubjectUID: "carol", Predicate: "reports_to", ObjectUID: "alice", SubjectName: "Pippin", ObjectName: "Merry"},
	}})
	body, err := r.Invoke(context.Background(), json.RawMessage(`{"entity":"entities/alice","predicates":["reports_to"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out relationsOutput
	_ = json.Unmarshal(body, &out)
	if len(out.Relations) != 2 || out.Relations[0].Name != "relations/r1" || out.Relations[0].Subject != "entities/alice" {
		t.Fatalf("relations = %s, want two hydrated reports_to edges with resource names", body)
	}
	// Direction is stamped relative to the queried entity (alice): she is the
	// subject of r1 (outgoing) and the object of r2 (incoming), so an agent reads
	// who-reports-to-whom without diffing subjectUid against the queried uid.
	if out.Relations[0].Direction != directionOutgoing {
		t.Errorf("r1 direction = %q, want outgoing (alice is subject)", out.Relations[0].Direction)
	}
	if out.Relations[1].Direction != directionIncoming {
		t.Errorf("r2 direction = %q, want incoming (alice is object)", out.Relations[1].Direction)
	}
}

func TestRelations_BadTimeAnchor_Validation(t *testing.T) {
	t.Parallel()
	r := NewRelations(&fakeNavigator{})
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"entity":"alice","at":"not-a-time"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// ---- neighbors ----

func TestNeighbors_HappyPath_HydratesHops(t *testing.T) {
	t.Parallel()
	n := NewNeighbors(&fakeNavigator{neighbors: []navigate.Neighbor{
		{Entity: navigate.Entity{UID: "bob", Kind: "person", DisplayName: "Sam"}, Via: navigate.Relation{UID: "r1", SubjectUID: "alice", Predicate: "reports_to", ObjectUID: "bob"}, Direction: directionOutgoing, Hops: 1},
	}})
	body, err := n.Invoke(context.Background(), json.RawMessage(`{"seed":"entities/alice","depth":1}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out neighborsOutput
	_ = json.Unmarshal(body, &out)
	if len(out.Neighbors) != 1 {
		t.Fatalf("neighbors = %s, want 1", body)
	}
	nb := out.Neighbors[0]
	if nb.Entity.Name != "entities/bob" || nb.Direction != directionOutgoing || nb.Hops != 1 || nb.Via.Name != "relations/r1" {
		t.Fatalf("neighbor = %+v, want hydrated bob via relations/r1 outgoing hop 1", nb)
	}
}

func TestNeighbors_EmptySeed_Validation(t *testing.T) {
	t.Parallel()
	n := NewNeighbors(&fakeNavigator{})
	_, err := n.Invoke(context.Background(), json.RawMessage(`{"seed":""}`))
	wantCode(t, err, mcp.CodeValidation)
}

// ---- entities ----

func TestEntities_Recent_HappyPath(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{entities: []navigate.Entity{
		{UID: "alice", Kind: "person", DisplayName: "Merry", Confidence: 1.0, ResolutionStatus: "confirmed"},
	}}, nil)
	body, err := e.Invoke(context.Background(), json.RawMessage(`{"kind":"person"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out entitiesOutput
	_ = json.Unmarshal(body, &out)
	if len(out.Entities) != 1 || out.Entities[0].Name != "entities/alice" {
		t.Fatalf("entities = %s, want one alice", body)
	}
	if out.Entities[0].RecallCount != nil {
		t.Fatalf("recent sort must not attach recallCount: %s", body)
	}
}

// TestEntities_NameExact_CaseInsensitiveAndExact pins that nameExact matches a
// display name case-insensitively (COLLATE NOCASE parity with the canonical
// NameExact resolver) AND exactly — a lowercase "mithril" resolves "Mithril" but does
// NOT also return the substring-matching "Mithril Corp" the way nameLike would.
func TestEntities_NameExact_CaseInsensitiveAndExact(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{entities: []navigate.Entity{
		{UID: "mithril", Kind: "organization", DisplayName: "Mithril", Confidence: 1.0, ResolutionStatus: "confirmed"},
		{UID: "mithrilcorp", Kind: "organization", DisplayName: "Mithril Corp", Confidence: 1.0, ResolutionStatus: "confirmed"},
	}}, nil)
	body, err := e.Invoke(context.Background(), json.RawMessage(`{"nameExact":"mithril"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out entitiesOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if len(out.Entities) != 1 || out.Entities[0].Name != "entities/mithril" {
		t.Fatalf("nameExact \"mithril\" = %s, want exactly entities/mithril (case-insensitive, not substring)", body)
	}
}

func TestEntities_RecallCountWithoutStats_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{}, nil) // no stats store wired
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recallCount"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestEntities_FilterWithRecallCount_Validation pins that the structured filter
// is rejected under sortBy=recallCount rather than silently ignored: that sort
// hydrates from the usage-stats store, which the filter cannot post-filter
// coherently.
func TestEntities_FilterWithRecallCount_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{}, fakeTopStore{})
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recallCount","filter":"status = \"pending\""}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestEntities_NameExactWithRecallCount_Validation pins that nameExact is
// rejected under sortBy=recallCount rather than silently ignored: the
// recall-count path hydrates ranked rows and applies only kind/confidence, so
// an exact-name filter would otherwise return unrelated most-recalled entities.
func TestEntities_NameExactWithRecallCount_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{}, fakeTopStore{})
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recallCount","nameExact":"mithril"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestEntities_NameLikeWithRecallCount_Validation pins the same rejection for
// the nameLike substring filter, which the recall-count path also cannot apply.
func TestEntities_NameLikeWithRecallCount_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{}, fakeTopStore{})
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recallCount","nameLike":"ac"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestEntities_BadFilter_Validation maps a malformed filter (ErrInvalidFilter
// from the compiler) to E_VALIDATION through the shared navQueryError helper.
func TestEntities_BadFilter_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{err: fmt.Errorf("%w: at <input>:1:6", navigate.ErrInvalidFilter)}, nil)
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"filter":"kind ="}`))
	wantCode(t, err, mcp.CodeValidation)
}

// fakeTopStore is a minimal statsTopStore for the recallCount path.
type fakeTopStore struct{ items []stats.TopItem }

func (f fakeTopStore) Top(_ context.Context, n int, _ stats.Window) ([]stats.TopItem, error) {
	// items are pre-sorted by recall desc; honor the top-n cut so tests can
	// reproduce the "filtered match falls past the requested page" case.
	if n > 0 && n < len(f.items) {
		return f.items[:n], nil
	}
	return f.items, nil
}

func TestEntities_RecallCount_HydratesAndCounts(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entity: &navigate.Entity{UID: "alice", Kind: "person", DisplayName: "Merry", Confidence: 1.0, ResolutionStatus: "confirmed"}}
	e := NewEntities(nav, fakeTopStore{items: []stats.TopItem{{UID: "alice", Name: "Merry", RecallCount: 7}}})
	body, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recallCount","window":"lifetime"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out entitiesOutput
	_ = json.Unmarshal(body, &out)
	if len(out.Entities) != 1 || out.Entities[0].RecallCount == nil || *out.Entities[0].RecallCount != 7 {
		t.Fatalf("recallCount entities = %s, want alice with recallCount 7", body)
	}
}

// ---- introspect ----

// fakeOntology is a minimal ontologyLister.
type fakeOntology struct{ schemas []kernel.Schema }

func (f fakeOntology) List(_ context.Context) ([]kernel.Schema, error) { return f.schemas, nil }

func TestIntrospect_HappyPath(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{
		preds: []navigate.Predicate{{Predicate: "works_at", Kind: "functional", Description: "Subject is employed by object."}},
		kinds: []navigate.KindCount{{Kind: "person", Count: 3}},
	}
	ont := fakeOntology{schemas: []kernel.Schema{{Kind: "relation", Version: "v1.0.0", Provenance: "core"}}}
	i := NewIntrospect(nav, ont)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out introspectOutput
	_ = json.Unmarshal(body, &out)
	if len(out.Predicates) != 1 || out.Predicates[0].Predicate != "works_at" {
		t.Fatalf("predicates = %s", body)
	}
	if len(out.EntityKinds) != 1 || out.EntityKinds[0].Count != 3 {
		t.Fatalf("entityKinds = %s", body)
	}
	if len(out.EventTypes) != 1 || out.EventTypes[0].Kind != "relation" {
		t.Fatalf("eventTypes = %s", body)
	}
	// Namespaces must be a non-nil slice even when no registry rows exist.
	if out.Namespaces == nil {
		t.Fatalf("namespaces must be non-nil even when registry is empty: %s", body)
	}
}

func TestIntrospect_NilOntology_EmptyEventTypes(t *testing.T) {
	t.Parallel()
	i := NewIntrospect(&fakeNavigator{}, nil)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out introspectOutput
	_ = json.Unmarshal(body, &out)
	if out.EventTypes == nil || len(out.EventTypes) != 0 {
		t.Fatalf("eventTypes = %s, want [] (non-nil empty) when no ontology wired", body)
	}
}

// TestIntrospect_EnrichedEntityKinds: when RegisteredEntityKinds returns rows
// the entityKinds items carry taxonomy metadata joined to live counts.
func TestIntrospect_EnrichedEntityKinds(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{
		kinds: []navigate.KindCount{{Kind: "person", Count: 5}},
		registeredKinds: []navigate.RegisteredEntityKind{
			{Name: "person", DisplayName: "Person", Description: "A human individual.", Provenance: nsCore, AddedBy: "seed"},
		},
	}
	i := NewIntrospect(nav, nil)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out introspectOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v (%s)", uerr, body)
	}
	if len(out.EntityKinds) != 1 {
		t.Fatalf("entityKinds len = %d, want 1: %s", len(out.EntityKinds), body)
	}
	ek := out.EntityKinds[0]
	if ek.Kind != "person" || ek.Count != 5 {
		t.Errorf("kind/count = %s/%d, want person/5", ek.Kind, ek.Count)
	}
	if ek.DisplayName != "Person" {
		t.Errorf("displayName = %q, want Person", ek.DisplayName)
	}
	if ek.Provenance != nsCore {
		t.Errorf("provenance = %q, want %s", ek.Provenance, nsCore)
	}
	if len(out.Namespaces) != 1 || out.Namespaces[0].Namespace != nsCore || out.Namespaces[0].KindCount != 1 {
		t.Errorf("namespaces = %v, want [{%s 1}]", out.Namespaces, nsCore)
	}
}

// TestIntrospect_RegisteredZeroCountKind: a kind registered in the taxonomy but
// absent from the entities table appears in entityKinds with count 0.
func TestIntrospect_RegisteredZeroCountKind(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{
		kinds: []navigate.KindCount{}, // nothing ingested
		registeredKinds: []navigate.RegisteredEntityKind{
			{Name: "organisation", DisplayName: "Organisation", Description: "A legal entity.", Provenance: nsCore, AddedBy: "seed"},
		},
	}
	i := NewIntrospect(nav, nil)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out introspectOutput
	_ = json.Unmarshal(body, &out)
	if len(out.EntityKinds) != 1 || out.EntityKinds[0].Kind != "organisation" || out.EntityKinds[0].Count != 0 {
		t.Fatalf("registered zero-count kind missing from entityKinds: %s", body)
	}
}

// TestIntrospect_DataOnlyKindNoEnrichment: a kind present in entity data but
// absent from the registry appears with count only and no taxonomy fields.
func TestIntrospect_DataOnlyKindNoEnrichment(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{
		kinds:           []navigate.KindCount{{Kind: "unknown_kind", Count: 2}},
		registeredKinds: []navigate.RegisteredEntityKind{}, // empty registry
	}
	i := NewIntrospect(nav, nil)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out introspectOutput
	_ = json.Unmarshal(body, &out)
	if len(out.EntityKinds) != 1 {
		t.Fatalf("entityKinds len = %d, want 1: %s", len(out.EntityKinds), body)
	}
	ek := out.EntityKinds[0]
	if ek.Kind != "unknown_kind" || ek.Count != 2 {
		t.Errorf("kind/count = %s/%d, want unknown_kind/2", ek.Kind, ek.Count)
	}
	if ek.DisplayName != "" || ek.Provenance != "" {
		t.Errorf("data-only kind must have no enrichment fields, got displayName=%q provenance=%q", ek.DisplayName, ek.Provenance)
	}
}

// TestIntrospect_RegistryFailureDegrades: when RegisteredEntityKinds returns an
// error, Invoke still succeeds returning the count-only shape (no error surfaced).
func TestIntrospect_RegistryFailureDegrades(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{
		kinds: []navigate.KindCount{{Kind: "person", Count: 4}},
	}
	// Wire a registeredKinds error by using a custom fakeNavigator that injects it.
	errNav := &fakeRegistryErrNavigator{fakeNavigator: nav, registryErr: errors.New("entity_kinds table not found")}
	i := NewIntrospect(errNav, nil)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke must succeed when registry fails (degrade-independently contract): %v", err)
	}
	var out introspectOutput
	_ = json.Unmarshal(body, &out)
	// Degraded shape: count-only, no enrichment fields.
	if len(out.EntityKinds) != 1 || out.EntityKinds[0].Kind != "person" || out.EntityKinds[0].Count != 4 {
		t.Fatalf("degraded entityKinds = %s, want [{person 4}]", body)
	}
	if out.EntityKinds[0].DisplayName != "" {
		t.Errorf("degraded kind must not carry displayName, got %q", out.EntityKinds[0].DisplayName)
	}
}

// TestIntrospect_NamespaceOrdering: namespaces are returned in the canonical
// order — core first, user second, extension:* alphabetically.
func TestIntrospect_NamespaceOrdering(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{
		registeredKinds: []navigate.RegisteredEntityKind{
			{Name: "vehicle", Provenance: "extension:transport"},
			{Name: "person", Provenance: nsCore},
			{Name: "org", Provenance: nsUser},
			{Name: "route", Provenance: "extension:maps"},
			{Name: "location", Provenance: nsCore},
		},
	}
	i := NewIntrospect(nav, nil)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out introspectOutput
	_ = json.Unmarshal(body, &out)
	if len(out.Namespaces) != 4 {
		t.Fatalf("namespaces len = %d, want 4: %s", len(out.Namespaces), body)
	}
	wantOrder := []string{nsCore, nsUser, "extension:maps", "extension:transport"}
	for idx, ns := range out.Namespaces {
		if ns.Namespace != wantOrder[idx] {
			t.Errorf("namespaces[%d] = %q, want %q (full: %s)", idx, ns.Namespace, wantOrder[idx], body)
		}
	}
	// core has 2 kinds, user 1, extension:maps 1, extension:transport 1.
	if out.Namespaces[0].KindCount != 2 {
		t.Errorf("%s kindCount = %d, want 2", nsCore, out.Namespaces[0].KindCount)
	}
}

// fakeRegistryErrNavigator wraps a fakeNavigator but returns a fixed error from
// RegisteredEntityKinds so the degrade-independently path can be exercised.
type fakeRegistryErrNavigator struct {
	*fakeNavigator
	registryErr error
}

func (f *fakeRegistryErrNavigator) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	return nil, f.registryErr
}

// TestIntrospect_PropertySchema verifies that entityKind entries in the
// introspect output carry propertySchema when the registered kind has one, and
// omit the field when it is nil.
func TestIntrospect_PropertySchema(t *testing.T) {
	t.Parallel()
	schema := json.RawMessage(`{"type":"object","properties":{"vin":{"type":"string"}}}`)
	nav := &fakeNavigator{
		kinds: []navigate.KindCount{
			{Kind: "user:vehicle", Count: 2},
			{Kind: "person", Count: 10},
		},
		registeredKinds: []navigate.RegisteredEntityKind{
			{
				Name: "user:vehicle", DisplayName: "Vehicle", Description: "A road vehicle.",
				Provenance: nsUser, AddedBy: "user",
				PropertySchema: schema,
			},
			{
				Name: "person", DisplayName: "Person", Description: "A human individual.",
				Provenance: nsCore, AddedBy: "seed",
				// PropertySchema intentionally nil.
			},
		},
	}
	i := NewIntrospect(nav, nil)
	body, err := i.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out introspectOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v (%s)", uerr, body)
	}

	byKind := make(map[string]entityKindJSON, len(out.EntityKinds))
	for _, ek := range out.EntityKinds {
		byKind[ek.Kind] = ek
	}

	t.Run("kind with schema surfaces propertySchema", func(t *testing.T) {
		t.Parallel()
		v := byKind["user:vehicle"]
		if v.PropertySchema == nil {
			t.Fatal("user:vehicle propertySchema = nil, want JSON object")
		}
		var ps map[string]any
		if err := json.Unmarshal(v.PropertySchema, &ps); err != nil {
			t.Fatalf("propertySchema not valid JSON: %v", err)
		}
		props, ok := ps["properties"].(map[string]any)
		if !ok {
			t.Fatalf("propertySchema.properties missing; got %v", ps)
		}
		if _, ok := props["vin"]; !ok {
			t.Errorf("propertySchema.properties missing vin; got %v", props)
		}
	})

	t.Run("kind without schema omits propertySchema", func(t *testing.T) {
		t.Parallel()
		// Re-marshal the person entry to check for key presence.
		personRaw, _ := json.Marshal(byKind["person"])
		var personMap map[string]json.RawMessage
		_ = json.Unmarshal(personRaw, &personMap)
		if _, present := personMap["propertySchema"]; present {
			t.Errorf("person propertySchema present, want omitted; got %s", personMap["propertySchema"])
		}
	})
}

// ---- relate ----

// captureAppender records the events it is handed and returns one receipt.
type captureAppender struct{ events []kernel.Event }

func (c *captureAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	c.events = append(c.events, events...)
	return []kernel.Receipt{{UID: "evt1"}}, nil
}

// relateNav returns an entity resolver whose GetEntity succeeds for any uid, so
// relate's endpoint-existence check passes. Tests that exercise rejection wire a
// bare &fakeNavigator{} (GetEntity → ErrNotFound) instead.
func relateNav() *fakeNavigator { return &fakeNavigator{entity: &navigate.Entity{UID: "endpoint"}} }

func TestRelate_HappyPath_AssertsRelation(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewRelate(app, relateNav())
	body, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV","predicate":"reports_to","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out relateOutput
	_ = json.Unmarshal(body, &out)
	if out.Status != "asserted" || out.Name == "" {
		t.Fatalf("relate output = %s, want status=asserted with a name", body)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	// The returned name must address the relation UID written to the relations
	// table (rel.UID), so a follow-up get(name) resolves.
	var rel struct {
		Name       string `json:"name"`
		UID        string `json:"uid"`
		SubjectUID string `json:"subjectUid"`
		Predicate  string `json:"predicate"`
	}
	_ = json.Unmarshal(app.events[0].Payload, &rel)
	if out.Name != rel.Name || out.Name != "relations/"+rel.UID {
		t.Fatalf("returned name %q must equal payload relation name %q (uid %q)", out.Name, rel.Name, rel.UID)
	}
	if rel.SubjectUID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" || rel.Predicate != "reports_to" {
		t.Fatalf("payload subject/predicate = %s/%s, want stripped uid + reports_to", rel.SubjectUID, rel.Predicate)
	}
}

func TestRelate_SelfSubject_Allowed(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewRelate(app, relateNav())
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lives_in","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("self-subject relate: %v", err)
	}
	var rel struct {
		SubjectUID string `json:"subjectUid"`
	}
	_ = json.Unmarshal(app.events[0].Payload, &rel)
	if rel.SubjectUID != "self" {
		t.Fatalf("subjectUid = %q, want self", rel.SubjectUID)
	}
}

func TestRelate_BadValidFrom_Validation(t *testing.T) {
	t.Parallel()
	r := NewRelate(&captureAppender{}, relateNav())
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lives_in","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0","validFrom":"nope"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestRelate_NonULIDEndpoint_Validation guards the endpoint check: a non-ULID,
// non-self reference is rejected as E_VALIDATION up front, rather than being
// appended and surfacing later as an opaque E_INTERNAL when the ontology schema
// rejects it downstream. No event must be appended.
func TestRelate_NonULIDEndpoint_Validation(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewRelate(app, relateNav())
	// Object "alice" is neither a ULID nor the "self" sentinel.
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lives_in","object":"alice"}`))
	wantCode(t, err, mcp.CodeValidation)
	// A subject that is neither "self" nor a ULID is likewise rejected.
	_, err = r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"bob","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	wantCode(t, err, mcp.CodeValidation)
	if len(app.events) != 0 {
		t.Fatalf("rejected relations must not be appended, got %d events", len(app.events))
	}
}

// ---- entity-arg resolution (uid / entities-ref / display name) ----

// TestResolveEntityArg_ResourceName: an entities/{uid} ref yields the uid; a
// non-entity collection is rejected up front.
func TestResolveEntityArg_ResourceName(t *testing.T) {
	t.Parallel()
	uid, err := resolveEntityArg(context.Background(), &fakeNavigator{}, nil, "entities/alice")
	if err != nil || uid != "alice" {
		t.Fatalf("resolveEntityArg(entities/alice) = %q, %v; want alice, nil", uid, err)
	}
	if _, err := resolveEntityArg(context.Background(), &fakeNavigator{}, nil, "episodes/ep1"); err == nil {
		t.Fatal("resolveEntityArg(episodes/ep1) must reject a non-entity collection")
	}
}

// TestResolveEntityArg_ExistingUID: a bare token that IS a known uid resolves to
// itself (GetEntity finds it) without a name scan.
func TestResolveEntityArg_ExistingUID(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entity: &navigate.Entity{UID: "alice", DisplayName: "Merry Brandybuck"}}
	uid, err := resolveEntityArg(context.Background(), nav, nil, "alice")
	if err != nil || uid != "alice" {
		t.Fatalf("resolveEntityArg(alice) = %q, %v; want alice, nil", uid, err)
	}
}

// TestResolveEntityArg_DisplayNameResolves is the bug fix: a bare token that is
// NOT a uid (GetEntity → ErrNotFound) resolves case-insensitively by display
// name. Before the fix the token was handed to EntityRelations verbatim as a
// "uid" and produced a silent empty result.
func TestResolveEntityArg_DisplayNameResolves(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{ // entity nil → GetEntity returns ErrNotFound for any uid
		entities: []navigate.Entity{{UID: "u_alice", Kind: "person", DisplayName: "Merry Brandybuck"}},
	}
	uid, err := resolveEntityArg(context.Background(), nav, nil, "merry brandybuck") // lower-case on purpose
	if err != nil {
		t.Fatalf("resolveEntityArg(\"merry brandybuck\") errored: %v", err)
	}
	if uid != "u_alice" {
		t.Fatalf("resolveEntityArg(\"merry brandybuck\") = %q, want u_alice (resolved by display name)", uid)
	}
}

// TestResolveEntityArg_AmbiguousName: a token with no exact match but several
// substring candidates returns E_VALIDATION naming the candidate uids (the two-
// Erik case) — never a silent empty or an arbitrary pick.
func TestResolveEntityArg_AmbiguousName(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: "u_solberg", Kind: "person", DisplayName: "Erik Solberg"},
		{UID: "u_lindqvist", Kind: "person", DisplayName: "Erik Lindqvist"},
	}}
	_, err := resolveEntityArg(context.Background(), nav, nil, "Erik")
	wantCode(t, err, mcp.CodeValidation)
	if !strings.Contains(err.Error(), "u_solberg") || !strings.Contains(err.Error(), "u_lindqvist") {
		t.Fatalf("ambiguous error must list candidate uids so the agent can disambiguate, got: %v", err)
	}
}

// TestResolveEntityArg_TwoExactMatches: two entities sharing a display name →
// E_VALIDATION (disambiguate by uid), not an arbitrary pick.
func TestResolveEntityArg_TwoExactMatches(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: "u1", Kind: "person", DisplayName: "Sam Okoro"},
		{UID: "u2", Kind: "person", DisplayName: "Sam Okoro"},
	}}
	_, err := resolveEntityArg(context.Background(), nav, nil, "Sam Okoro")
	wantCode(t, err, mcp.CodeValidation)
}

// TestResolveEntityArg_UnknownName: no candidate at all → E_NOT_FOUND.
func TestResolveEntityArg_UnknownName(t *testing.T) {
	t.Parallel()
	_, err := resolveEntityArg(context.Background(), &fakeNavigator{}, nil, "Nobody")
	wantCode(t, err, mcp.CodeNotFound)
}

// TestRelations_UnknownName_NotFound is the end-to-end guard: a display name
// that resolves to nothing is an explicit E_NOT_FOUND through the full Invoke
// path, not the silent empty list the bare-name-as-uid passthrough returned.
func TestRelations_UnknownName_NotFound(t *testing.T) {
	t.Parallel()
	r := NewRelations(&fakeNavigator{})
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"entity":"Merry Brandybuck"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestNeighbors_UnknownName_NotFound: same contract for the neighbors seed.
func TestNeighbors_UnknownName_NotFound(t *testing.T) {
	t.Parallel()
	n := NewNeighbors(&fakeNavigator{})
	_, err := n.Invoke(context.Background(), json.RawMessage(`{"seed":"Merry Brandybuck"}`))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestResolveEntityArg_ExactMatchBeyondSuggestCap guards the display-name
// truncation fix: 30 entities share the "al" substring and the exact match is
// last, well past the nameSuggestLimit page. A single capped NameLike scan would
// miss it; the dedicated unbounded NameExact query must still resolve it.
func TestResolveEntityArg_ExactMatchBeyondSuggestCap(t *testing.T) {
	t.Parallel()
	ents := make([]navigate.Entity, 0, 30)
	for i := range 29 {
		ents = append(ents, navigate.Entity{UID: fmt.Sprintf("u%02d", i), Kind: "person", DisplayName: fmt.Sprintf("Alpha %02d", i)})
	}
	ents = append(ents, navigate.Entity{UID: "u_exact", Kind: "person", DisplayName: "Al"})
	uid, err := resolveEntityArg(context.Background(), &fakeNavigator{entities: ents}, nil, "Al")
	if err != nil {
		t.Fatalf("resolveEntityArg(\"Al\") errored: %v", err)
	}
	if uid != "u_exact" {
		t.Fatalf("resolveEntityArg(\"Al\") = %q, want u_exact (exact match beyond the suggestion cap)", uid)
	}
}

// fixedSelfBinder is an in-package selfBinder stub for the read-side rebind
// tests: bound returns (principalUID, true); unbound returns ("", false).
type fixedSelfBinder struct {
	principalUID string
	bound        bool
}

func (b fixedSelfBinder) SelfPrincipal(context.Context) (string, bool) {
	return b.principalUID, b.bound
}

const navTestPrincipalUID = "01PRINCIPAL000000000000001"

// TestResolveEntityArg_SelfRebindsToPrincipalWhenBound is the F5 read-side
// guard for the node-walking tools: a "self" arg (bare or entities/self)
// resolves to the bound principal, so neighbors/relations follow first-person
// data after genesis moves it off the seeded sentinel.
func TestResolveEntityArg_SelfRebindsToPrincipalWhenBound(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: navTestPrincipalUID, Kind: "person", DisplayName: "Me"},
	}}
	binder := fixedSelfBinder{principalUID: navTestPrincipalUID, bound: true}
	for _, arg := range []string{"self", "entities/self"} {
		uid, err := resolveEntityArg(context.Background(), nav, binder, arg)
		if err != nil {
			t.Fatalf("resolveEntityArg(%q) when bound: %v", arg, err)
		}
		if uid != navTestPrincipalUID {
			t.Errorf("resolveEntityArg(%q) = %q, want rebound principal %q", arg, uid, navTestPrincipalUID)
		}
	}
}

// TestResolveEntityArg_SelfStaysLiteralWhenUnbound confirms the pre-identity
// path: an unbound device leaves "self" verbatim (here resolvable as a uid).
func TestResolveEntityArg_SelfStaysLiteralWhenUnbound(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{{UID: "self", Kind: "person", DisplayName: "Me"}}}
	uid, err := resolveEntityArg(context.Background(), nav, fixedSelfBinder{bound: false}, "entities/self")
	if err != nil {
		t.Fatalf("resolveEntityArg(entities/self) unbound: %v", err)
	}
	if uid != "self" {
		t.Errorf("resolveEntityArg(entities/self) unbound = %q, want literal self", uid)
	}
}

// TestToFilter_ClampsLimitToMax guards the limit-cap bypass fix: a CLI/in-process
// caller skips the MCP schema's 200 ceiling, so the shared conversion must clamp.
func TestToFilter_ClampsLimitToMax(t *testing.T) {
	t.Parallel()
	huge := 10_000_000
	f, err := relationFilterInput{Limit: &huge}.toFilter()
	if err != nil {
		t.Fatalf("toFilter: %v", err)
	}
	if f.Limit != maxNavigateLimit {
		t.Fatalf("toFilter limit = %d, want clamp to %d", f.Limit, maxNavigateLimit)
	}
}

// TestToFilter_NegativeLimitNotUnbounded guards the lower-bound half of the same
// bypass: the navigator reads a negative limit as the "no cap" sentinel, so a
// CLI/in-process caller passing --limit -1 must not reach it as an unbounded
// scan. It normalizes to 0 (the navigator's default cap), never a negative.
func TestToFilter_NegativeLimitNotUnbounded(t *testing.T) {
	t.Parallel()
	neg := -1
	f, err := relationFilterInput{Limit: &neg}.toFilter()
	if err != nil {
		t.Fatalf("toFilter: %v", err)
	}
	if f.Limit < 0 {
		t.Fatalf("toFilter limit = %d, must not pass a negative (no-cap) sentinel to the navigator", f.Limit)
	}
}

// TestEntities_RecallCount_FilterUnderfill guards the filter-after-top-N fix: the
// 10 most-recalled are organizations and the only person is rank 11, so a naive
// Top(limit) + post-filter returns nothing. The over-fetch must still surface it.
func TestEntities_RecallCount_FilterUnderfill(t *testing.T) {
	t.Parallel()
	ents := make([]navigate.Entity, 0, 11)
	items := make([]stats.TopItem, 0, 11)
	for i := range 10 {
		uid := fmt.Sprintf("org%02d", i)
		ents = append(ents, navigate.Entity{UID: uid, Kind: "organization", DisplayName: uid, Confidence: 1, ResolutionStatus: "confirmed"})
		items = append(items, stats.TopItem{UID: uid, RecallCount: int64(100 - i)})
	}
	ents = append(ents, navigate.Entity{UID: "person1", Kind: "person", DisplayName: "Dana", Confidence: 1, ResolutionStatus: "confirmed"})
	items = append(items, stats.TopItem{UID: "person1", RecallCount: 1})
	e := NewEntities(&fakeNavigator{entities: ents}, fakeTopStore{items: items})
	body, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recallCount","kind":"person","limit":10,"window":"lifetime"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out entitiesOutput
	_ = json.Unmarshal(body, &out)
	if len(out.Entities) != 1 || out.Entities[0].Name != "entities/person1" {
		t.Fatalf("filtered recallCount must over-fetch to the rank-11 person, got %s", body)
	}
}

// TestEntities_BadWindow_Validation guards the silent-window-fallback fix: an
// unknown window is rejected rather than silently scoped to "today".
func TestEntities_BadWindow_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{entity: &navigate.Entity{UID: "x"}}, fakeTopStore{})
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recallCount","window":"lastSevenDays"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestRelate_NonexistentEndpoint_NotFound guards the dangling-edge fix: a valid-
// shaped but unknown object uid is rejected as E_NOT_FOUND, and nothing is
// appended, rather than writing an edge that hydrates a blank node later.
func TestRelate_NonexistentEndpoint_NotFound(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewRelate(app, &fakeNavigator{}) // GetEntity → ErrNotFound for any uid
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lives_in","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	wantCode(t, err, mcp.CodeNotFound)
	if len(app.events) != 0 {
		t.Fatalf("a relation to a nonexistent entity must not be appended, got %d events", len(app.events))
	}
}

// TestRelate_InvertedValidity_Validation guards the empty-window fix: validUntil
// must be strictly after validFrom, else the write succeeds but navigation never
// returns the edge.
func TestRelate_InvertedValidity_Validation(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewRelate(app, relateNav())
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lived_in","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0","validFrom":"2025-01-01T00:00:00Z","validUntil":"2024-01-01T00:00:00Z"}`))
	wantCode(t, err, mcp.CodeValidation)
	if len(app.events) != 0 {
		t.Fatalf("an inverted validity window must not be appended, got %d events", len(app.events))
	}
}

// TestEntities_BadSortBy_Validation guards the silent-sort-fallback fix: an
// unknown sortBy (CLI/in-process dispatch bypasses the schema enum) is rejected
// rather than silently falling through to the recent listing.
func TestEntities_BadSortBy_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{entity: &navigate.Entity{UID: "x"}}, fakeTopStore{})
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"sortBy":"recall-count"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestEntities_OutOfRangeMinConfidence_Validation guards the minConfidence-bounds
// fix: a value outside [0,1] (CLI/in-process bypasses the schema) is rejected
// rather than silently emptying (>1) or disabling (<0) the filter.
func TestEntities_OutOfRangeMinConfidence_Validation(t *testing.T) {
	t.Parallel()
	e := NewEntities(&fakeNavigator{entity: &navigate.Entity{UID: "x"}}, fakeTopStore{})
	_, err := e.Invoke(context.Background(), json.RawMessage(`{"minConfidence":2}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestRelate_OutOfRangeConfidence_Validation guards the confidence-bounds fix: a
// confidence outside [0,1] is rejected rather than persisted out of range where
// it would skew later confidence filtering.
func TestRelate_OutOfRangeConfidence_Validation(t *testing.T) {
	t.Parallel()
	app := &captureAppender{}
	r := NewRelate(app, relateNav())
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0","confidence":2}`))
	wantCode(t, err, mcp.CodeValidation)
	if len(app.events) != 0 {
		t.Fatalf("an out-of-range confidence must not be appended, got %d events", len(app.events))
	}
}

// ---- kind-partitioned display-name resolution (fakeNavigator fallback path) ----

// TestResolveEntityArg_PrefersNonDocument: a product and a document both named
// "Mithrilmail" — the fakeNavigator fallback path (no displayNameResolver) still
// returns the first entity. The important correctness test (preferring
// non-document) lives in navigate/resolve_test.go using the real Navigator;
// this test just ensures the fakeNavigator path does not crash and the caller
// correctly wires the kind hint through.
func TestResolveEntityArg_PrefersNonDocument(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: "product-uid", Kind: "product", DisplayName: "Mithrilmail"},
		{UID: "doc-uid", Kind: "document", DisplayName: "Mithrilmail"},
	}}
	// With the fakeNavigator (fallback path), two exact matches → E_VALIDATION.
	// The real kind-partition fix is exercised via navigate.ResolveDisplayName
	// in TestResolveDisplayName_PrefersNonDocument in navigate/resolve_test.go.
	_, err := resolveEntityArg(context.Background(), nav, nil, "Mithrilmail")
	wantCode(t, err, mcp.CodeValidation)
}

// TestGet_AmbiguousNonDocument_Validation: two persons sharing a name → E_VALIDATION.
// This exercises the end-to-end get tool path through the fakeNavigator.
func TestGet_AmbiguousNonDocument_Validation(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: "u1", Kind: "person", DisplayName: "Sam Gamgee"},
		{UID: "u2", Kind: "person", DisplayName: "Sam Gamgee"},
	}}
	g := NewGet(nav)
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":"Sam Gamgee"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestResolveEntityArg_AmbiguityDetails: an ambiguous exact match returns
// E_VALIDATION with a non-nil Details payload that unmarshals to the
// AIP-193 errorInfo shape with reason=AMBIGUOUS_NAME and a candidates array.
func TestResolveEntityArg_AmbiguityDetails(t *testing.T) {
	t.Parallel()
	nav := &fakeNavigator{entities: []navigate.Entity{
		{UID: "u1", Kind: "person", DisplayName: "Sam Gamgee"},
		{UID: "u2", Kind: "person", DisplayName: "Sam Gamgee"},
	}}
	_, err := resolveEntityArg(context.Background(), nav, nil, "Sam Gamgee")
	if err == nil {
		t.Fatal("expected E_VALIDATION, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error is not *mcp.ToolError: %v", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Fatalf("code = %q, want E_VALIDATION", te.Code)
	}
	if te.Details == nil {
		t.Fatal("Details is nil, want AIP-193 errorInfo payload")
	}
	var details struct {
		Reason     string `json:"reason"`
		Candidates []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Kind        string `json:"kind"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(te.Details, &details); err != nil {
		t.Fatalf("Details is not valid JSON: %v (raw: %s)", err, te.Details)
	}
	if details.Reason != "AMBIGUOUS_NAME" {
		t.Errorf("reason = %q, want AMBIGUOUS_NAME", details.Reason)
	}
	if len(details.Candidates) < 2 {
		t.Errorf("candidates len = %d, want >= 2", len(details.Candidates))
	}
	for _, c := range details.Candidates {
		if c.Name == "" || c.DisplayName == "" || c.Kind == "" {
			t.Errorf("candidate missing fields: %+v", c)
		}
	}
}
