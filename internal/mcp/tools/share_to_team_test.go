package tools_test

// share_to_team_test.go covers the shareToTeam MCP tool (B4): re-publishing
// a personal-scope record as an ordinary team-scoped event batch. Tests cover
// the happy paths (entity, fact, relation share; withFacts subtree), the
// dedup-reuse path (same-name entity already in team scope), and the reader-
// role permission guard (ErrPermission → E_PERMISSION via mapAppendError).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// shareNav is a minimal Navigator fake for share_to_team tests. It serves as
// both the personal-scope and team-scope Navigator: navWithScopes only narrows
// *navigate.Navigator (the real production type) and returns the original nav
// unchanged for test fakes that don't implement the scopable interface.
type shareNav struct {
	// entities is the uid → entity lookup (GetEntity).
	entities map[string]navigate.Entity
	// facts is the uid → fact lookup (GetFact).
	facts map[string]navigate.Fact
	// relations is the uid → relation lookup (GetRelation).
	relations map[string]navigate.Relation
	// identifiers maps entity uid → identifiers (EntityIdentifiers).
	identifiers map[string][]navigate.EntityIdentifier
	// byName maps exact display name → []entity (ListEntities NameExact).
	byName map[string][]navigate.Entity
	// factsBySub maps subject uid → []fact (FactsForSubject).
	factsBySub map[string][]navigate.Fact
	// relsByUID maps entity uid → []relation (EntityRelations).
	relsByUID map[string][]navigate.Relation
	// listErr, when non-nil, is returned by ListEntities to simulate a
	// transient dedup-lookup failure.
	listErr error
}

func (n *shareNav) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	if e, ok := n.entities[uid]; ok {
		return e, nil
	}
	return navigate.Entity{}, navigate.ErrNotFound
}

func (n *shareNav) GetFact(_ context.Context, uid string) (navigate.Fact, error) {
	if f, ok := n.facts[uid]; ok {
		return f, nil
	}
	return navigate.Fact{}, navigate.ErrNotFound
}

func (n *shareNav) GetRelation(_ context.Context, uid string) (navigate.Relation, error) {
	if r, ok := n.relations[uid]; ok {
		return r, nil
	}
	return navigate.Relation{}, navigate.ErrNotFound
}

func (n *shareNav) EntityIdentifiers(_ context.Context, uid string) ([]navigate.EntityIdentifier, error) {
	return n.identifiers[uid], nil
}

func (n *shareNav) ListEntities(_ context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	if n.listErr != nil {
		return nil, n.listErr
	}
	if opts.NameExact == "" {
		// Name-like resolution for display-name refs: linear scan.
		var out []navigate.Entity
		for _, e := range n.entities {
			if opts.NameLike != "" && !strings.Contains(
				strings.ToLower(e.DisplayName), strings.ToLower(opts.NameLike)) {
				continue
			}
			if opts.Kind != "" && e.Kind != opts.Kind {
				continue
			}
			out = append(out, e)
		}
		if opts.Limit > 0 && len(out) > opts.Limit {
			out = out[:opts.Limit]
		}
		return out, nil
	}
	es := n.byName[opts.NameExact]
	if opts.Kind != "" {
		var filtered []navigate.Entity
		for _, e := range es {
			if e.Kind == opts.Kind {
				filtered = append(filtered, e)
			}
		}
		es = filtered
	}
	if opts.Limit > 0 && len(es) > opts.Limit {
		es = es[:opts.Limit]
	}
	return es, nil
}

func (n *shareNav) FactsForSubject(_ context.Context, uid string, _ navigate.FactFilter) ([]navigate.Fact, error) {
	return n.factsBySub[uid], nil
}

func (n *shareNav) EntityRelations(_ context.Context, uid string, _ navigate.RelationFilter) ([]navigate.Relation, error) {
	return n.relsByUID[uid], nil
}

// Unused Navigator methods return empty values; share_to_team doesn't invoke them.

func (n *shareNav) GetEpisode(_ context.Context, _ string) (navigate.Episode, error) {
	return navigate.Episode{}, navigate.ErrNotFound
}

func (n *shareNav) Neighbors(_ context.Context, _ string, _ int, _ navigate.RelationFilter) ([]navigate.Neighbor, error) {
	return nil, nil
}

func (n *shareNav) CountEntities(_ context.Context, _ navigate.EntityFilter) (int64, error) {
	return 0, nil
}

func (n *shareNav) AggregateEntities(_ context.Context, _ navigate.AggregateSpec) ([]navigate.GroupCount, error) {
	return nil, nil
}

func (n *shareNav) EpisodesCitingEntity(_ context.Context, _ string, _ int) ([]navigate.Episode, error) {
	return nil, nil
}

func (n *shareNav) EventProvenance(_ context.Context, _ string) ([]navigate.Provenance, error) {
	return nil, nil
}

func (n *shareNav) EntityProvenance(_ context.Context, _ string) ([]navigate.Provenance, error) {
	return nil, nil
}

func (n *shareNav) Predicates(_ context.Context) ([]navigate.Predicate, error) {
	return nil, nil
}

func (n *shareNav) EntityKinds(_ context.Context) ([]navigate.KindCount, error) {
	return nil, nil
}

func (n *shareNav) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	return nil, nil
}

func (n *shareNav) GetSourceRef(_ context.Context, _ string) (navigate.SourceRef, error) {
	return navigate.SourceRef{}, navigate.ErrNotFound
}

func (n *shareNav) GetInsight(_ context.Context, _ string) (navigate.Insight, error) {
	return navigate.Insight{}, navigate.ErrNotFound
}

func (n *shareNav) GetConsolidatedMemory(_ context.Context, _ string) (navigate.ConsolidatedMemory, error) {
	return navigate.ConsolidatedMemory{}, navigate.ErrNotFound
}

func (n *shareNav) ListEpisodes(_ context.Context, _ navigate.EpisodeFilter) ([]navigate.Episode, error) {
	return nil, nil
}

// shareAppender captures events and returns configurable receipts/errors.
type shareAppender struct {
	gotEvents []kernel.Event
	receipts  []kernel.Receipt
	err       error
}

func (a *shareAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	if a.err != nil {
		return nil, a.err
	}
	a.gotEvents = append(a.gotEvents, events...)
	return a.receipts, nil
}

// okShareAppender returns a shareAppender that always succeeds.
func okShareAppender() *shareAppender {
	return &shareAppender{receipts: []kernel.Receipt{{}}}
}

const (
	sharePersonalUID  = "01HZZZZZZZZZZZZZZZZZZZSHA1"
	shareFPersonalUID = "01HZZZZZZZZZZZZZZZZZZZSHA2"
	shareRPersonalUID = "01HZZZZZZZZZZZZZZZZZZZSHA3"
)

// --- entity share ---

// TestShareToTeam_Entity_NewTeamScopeEvent verifies that sharing a personal
// entity appends exactly one KindEntity event with Scope=team and
// Source="shared:<uid>".
func TestShareToTeam_Entity_NewTeamScopeEvent(t *testing.T) {
	t.Parallel()
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	raw, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "entities/" + sharePersonalUID,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Verify output structure.
	var out struct {
		Shared []struct {
			Name     string `json:"name"`
			TeamName string `json:"teamName"`
			Type     string `json:"type"`
			Status   string `json:"status"`
		} `json:"shared"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "shared" {
		t.Errorf("status = %q, want shared", out.Status)
	}
	if len(out.Shared) != 1 {
		t.Fatalf("shared count = %d, want 1", len(out.Shared))
	}
	if out.Shared[0].Type != "entity" {
		t.Errorf("type = %q, want entity", out.Shared[0].Type)
	}
	if out.Shared[0].Status != "shared" {
		t.Errorf("item status = %q, want shared", out.Shared[0].Status)
	}
	if out.Shared[0].Name != "entities/"+sharePersonalUID {
		t.Errorf("name = %q, want entities/%s", out.Shared[0].Name, sharePersonalUID)
	}
	if !strings.HasPrefix(out.Shared[0].TeamName, "entities/") {
		t.Errorf("teamName = %q, want entities/<uid> prefix", out.Shared[0].TeamName)
	}

	// Verify event envelope.
	if len(app.gotEvents) == 0 {
		t.Fatalf("no events appended")
	}
	ev := app.gotEvents[0]
	if ev.Scope != kernel.ScopeTeam {
		t.Errorf("event scope = %q, want %q", ev.Scope, kernel.ScopeTeam)
	}
	if string(ev.Kind) != ontology.KindEntity {
		t.Errorf("event kind = %q, want %s", ev.Kind, ontology.KindEntity)
	}
	wantSource := "shared:" + sharePersonalUID
	if ev.Source != wantSource {
		t.Errorf("event source = %q, want %q", ev.Source, wantSource)
	}
}

// TestShareToTeam_Entity_WithIdentifiers_IdentifierEventsTeamScoped verifies
// that when the personal entity has identifiers, the minted team-scope copy also
// gets team-scoped entityIdentifierAdded events.
func TestShareToTeam_Entity_WithIdentifiers_IdentifierEventsTeamScoped(t *testing.T) {
	t.Parallel()
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		identifiers: map[string][]navigate.EntityIdentifier{
			sharePersonalUID: {
				{Type: "email", Value: "eowyn@rohan.example"},
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	if _, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "entities/" + sharePersonalUID,
	})); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Expect: 1 entity event + 1 identifier event.
	if len(app.gotEvents) != 2 {
		t.Fatalf("expected 2 events, got %d", len(app.gotEvents))
	}
	for i, ev := range app.gotEvents {
		if ev.Scope != kernel.ScopeTeam {
			t.Errorf("event[%d] scope = %q, want team", i, ev.Scope)
		}
	}
	if string(app.gotEvents[1].Kind) != ontology.KindEntityIdentifierAdded {
		t.Errorf("event[1] kind = %q, want %s", app.gotEvents[1].Kind, ontology.KindEntityIdentifierAdded)
	}
}

// TestShareToTeam_Entity_NameMatch_Reused verifies that when a team-scope entity
// with the same display name already exists (exact-name dedup lane), no new
// event is appended and the output item carries status=reused.
func TestShareToTeam_Entity_NameMatch_Reused(t *testing.T) {
	t.Parallel()
	const teamUID = "01HZZZZZZZZZZZZZZZZZZZTEAM"
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		// byName is used by teamNav.ListEntities(NameExact=...) → returns existing team entity.
		byName: map[string][]navigate.Entity{
			"Eowyn": {{UID: teamUID, Kind: "person", DisplayName: "Eowyn"}},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	raw, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "entities/" + sharePersonalUID,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Shared []struct {
			TeamName string `json:"teamName"`
			Status   string `json:"status"`
		} `json:"shared"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if len(out.Shared) != 1 {
		t.Fatalf("shared count = %d, want 1", len(out.Shared))
	}
	if out.Shared[0].Status != "reused" {
		t.Errorf("status = %q, want reused", out.Shared[0].Status)
	}
	if out.Shared[0].TeamName != "entities/"+teamUID {
		t.Errorf("teamName = %q, want entities/%s", out.Shared[0].TeamName, teamUID)
	}
	if len(app.gotEvents) != 0 {
		t.Errorf("want 0 events on reuse, got %d", len(app.gotEvents))
	}
}

// --- fact share ---

// TestShareToTeam_Fact_TeamScopeEventWithProvenance verifies that sharing a
// personal fact appends one KindFact event with Scope=team and a provenance
// source of "shared:<sourceEventUID>".
func TestShareToTeam_Fact_TeamScopeEventWithProvenance(t *testing.T) {
	t.Parallel()
	const (
		srcEventUID        = "01HZZZZZZZZZZZZZZZZZZZEVT1"
		factSubjectTeamUID = "01HZZZZZZZZZZZZZZZZZZZTMF1"
	)
	nav := &shareNav{
		// personal subject entity — needed by resolveTeamUID → GetEntity.
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		// team counterpart returned by teamNav.ListEntities(NameExact="Eowyn").
		byName: map[string][]navigate.Entity{
			"Eowyn": {{UID: factSubjectTeamUID, Kind: "person", DisplayName: "Eowyn"}},
		},
		facts: map[string]navigate.Fact{
			shareFPersonalUID: {
				UID:            shareFPersonalUID,
				SubjectUID:     sharePersonalUID,
				StateKey:       "state.location.current",
				Value:          "Helm's Deep",
				SourceEventUID: srcEventUID,
				Confidence:     0.9,
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	raw, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "facts/" + shareFPersonalUID,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Shared []struct {
			Name     string `json:"name"`
			TeamName string `json:"teamName"`
			Type     string `json:"type"`
			Status   string `json:"status"`
		} `json:"shared"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if len(out.Shared) != 1 || out.Shared[0].Type != "fact" {
		t.Fatalf("want 1 fact item, got %v", out.Shared)
	}
	if out.Shared[0].Status != "shared" {
		t.Errorf("status = %q, want shared", out.Shared[0].Status)
	}

	if len(app.gotEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(app.gotEvents))
	}
	ev := app.gotEvents[0]
	if ev.Scope != kernel.ScopeTeam {
		t.Errorf("event scope = %q, want team", ev.Scope)
	}
	if string(ev.Kind) != ontology.KindFact {
		t.Errorf("event kind = %q, want %s", ev.Kind, ontology.KindFact)
	}
	wantSource := "shared:" + srcEventUID
	if ev.Source != wantSource {
		t.Errorf("event source = %q, want %q", ev.Source, wantSource)
	}
}

// --- relation share ---

// TestShareToTeam_Relation_TeamScopeEvent verifies sharing a personal relation
// appends one KindRelation event scoped to team.
func TestShareToTeam_Relation_TeamScopeEvent(t *testing.T) {
	t.Parallel()
	const (
		srcEventUID       = "01HZZZZZZZZZZZZZZZZZZZEVT2"
		relObjUID         = "01HZZZZZZZZZZZZZZZZZZZOBJ1"
		relSubjectTeamUID = "01HZZZZZZZZZZZZZZZZZZZTMR1"
		relObjectTeamUID  = "01HZZZZZZZZZZZZZZZZZZZTMR2"
	)
	nav := &shareNav{
		// personal endpoint entities — needed by resolveTeamUID → GetEntity.
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
			relObjUID:        {UID: relObjUID, Kind: "place", DisplayName: "Minas Tirith"},
		},
		// team counterparts returned by teamNav.ListEntities(NameExact=...).
		byName: map[string][]navigate.Entity{
			"Eowyn":        {{UID: relSubjectTeamUID, Kind: "person", DisplayName: "Eowyn"}},
			"Minas Tirith": {{UID: relObjectTeamUID, Kind: "place", DisplayName: "Minas Tirith"}},
		},
		relations: map[string]navigate.Relation{
			shareRPersonalUID: {
				UID:            shareRPersonalUID,
				SubjectUID:     sharePersonalUID,
				Predicate:      "leads",
				ObjectUID:      relObjUID,
				Confidence:     1.0,
				SourceEventUID: srcEventUID,
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	if _, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "relations/" + shareRPersonalUID,
	})); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if len(app.gotEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(app.gotEvents))
	}
	ev := app.gotEvents[0]
	if ev.Scope != kernel.ScopeTeam {
		t.Errorf("event scope = %q, want team", ev.Scope)
	}
	if string(ev.Kind) != ontology.KindRelation {
		t.Errorf("event kind = %q, want %s", ev.Kind, ontology.KindRelation)
	}
	if ev.Source != "shared:"+srcEventUID {
		t.Errorf("event source = %q, want %q", ev.Source, "shared:"+srcEventUID)
	}
}

// --- subtree share ---

// TestShareToTeam_WithFacts_BatchLands verifies that withFacts=true shares the
// entity plus all of its personal-scope facts in a single operation: the entity
// event lands first, then one event per fact.
func TestShareToTeam_WithFacts_BatchLands(t *testing.T) {
	t.Parallel()
	const factUID1 = "01HZZZZZZZZZZZZZZZZZZZFCT1"
	const factUID2 = "01HZZZZZZZZZZZZZZZZZZZFCT2"
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		facts: map[string]navigate.Fact{
			factUID1: {UID: factUID1, SubjectUID: sharePersonalUID, StateKey: "state.role", Value: "shield-maiden"},
			factUID2: {UID: factUID2, SubjectUID: sharePersonalUID, StateKey: "state.location.current", Value: "Rohan"},
		},
		factsBySub: map[string][]navigate.Fact{
			sharePersonalUID: {
				{UID: factUID1, SubjectUID: sharePersonalUID, StateKey: "state.role", Value: "shield-maiden"},
				{UID: factUID2, SubjectUID: sharePersonalUID, StateKey: "state.location.current", Value: "Rohan"},
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	raw, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref":       "entities/" + sharePersonalUID,
		"withFacts": true,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Shared []struct {
			Type string `json:"type"`
		} `json:"shared"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	// 1 entity + 2 facts.
	if len(out.Shared) != 3 {
		t.Errorf("shared count = %d, want 3 (1 entity + 2 facts)", len(out.Shared))
	}

	// All appended events must carry team scope.
	for i, ev := range app.gotEvents {
		if ev.Scope != kernel.ScopeTeam {
			t.Errorf("event[%d] scope = %q, want team", i, ev.Scope)
		}
	}
}

// TestShareToTeam_WithRelations_BatchLands verifies that withRelations=true
// shares the entity plus all of its personal-scope relations.
func TestShareToTeam_WithRelations_BatchLands(t *testing.T) {
	t.Parallel()
	const (
		relUID      = "01HZZZZZZZZZZZZZZZZZZZREL1"
		obj2UID     = "01HZZZZZZZZZZZZZZZZZZZOBJ2"
		obj2TeamUID = "01HZZZZZZZZZZZZZZZZZZZTMB1"
	)
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
			// object entity — needed by resolveEndpoint → resolveTeamUID → GetEntity.
			obj2UID: {UID: obj2UID, Kind: "organization", DisplayName: "Rohan Guard"},
		},
		// team counterpart for the object so the relation is shared, not skipped.
		byName: map[string][]navigate.Entity{
			"Rohan Guard": {{UID: obj2TeamUID, Kind: "organization", DisplayName: "Rohan Guard"}},
		},
		relations: map[string]navigate.Relation{
			relUID: {UID: relUID, SubjectUID: sharePersonalUID, Predicate: "serves", ObjectUID: obj2UID, Confidence: 1.0},
		},
		relsByUID: map[string][]navigate.Relation{
			sharePersonalUID: {
				{UID: relUID, SubjectUID: sharePersonalUID, Predicate: "serves", ObjectUID: obj2UID, Confidence: 1.0},
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	raw, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref":           "entities/" + sharePersonalUID,
		"withRelations": true,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Shared []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"shared"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	// 1 entity + 1 relation, both status=shared.
	if len(out.Shared) != 2 {
		t.Errorf("shared count = %d, want 2 (1 entity + 1 relation)", len(out.Shared))
	}
	types := map[string]int{}
	for _, s := range out.Shared {
		types[s.Type]++
		if s.Status != "shared" && s.Status != "reused" {
			t.Errorf("item %s has unexpected status %q", s.Type, s.Status)
		}
	}
	if types["entity"] != 1 || types["relation"] != 1 {
		t.Errorf("type distribution = %v, want entity:1 relation:1", types)
	}
}

// --- permission guard ---

// TestShareToTeam_ReaderRole_Permission verifies that when the dispatcher
// rejects the team-scope Append with ErrPermission (reader-role caller),
// shareToTeam surfaces E_PERMISSION via mapAppendError — not E_INTERNAL.
func TestShareToTeam_ReaderRole_Permission(t *testing.T) {
	t.Parallel()
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
	}
	// denyingAppender simulates the dispatcher's AuthorizeTeamWrite gate.
	tool := tools.NewShareToTeam(denyingAppender{}, nav)

	_, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "entities/" + sharePersonalUID,
	}))
	wantCode(t, err, mcp.CodePermission)
}

// --- error paths ---

// TestShareToTeam_EntityNotFound returns E_NOT_FOUND for an unknown entity uid.
func TestShareToTeam_EntityNotFound(t *testing.T) {
	t.Parallel()
	nav := &shareNav{} // empty — nothing found
	tool := tools.NewShareToTeam(okShareAppender(), nav)

	_, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "entities/" + sharePersonalUID,
	}))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestShareToTeam_FactNotFound returns E_NOT_FOUND for an unknown fact uid.
func TestShareToTeam_FactNotFound(t *testing.T) {
	t.Parallel()
	nav := &shareNav{}
	tool := tools.NewShareToTeam(okShareAppender(), nav)

	_, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "facts/" + shareFPersonalUID,
	}))
	wantCode(t, err, mcp.CodeNotFound)
}

// TestShareToTeam_MissingRef validates that an empty ref is rejected.
func TestShareToTeam_MissingRef(t *testing.T) {
	t.Parallel()
	tool := tools.NewShareToTeam(okShareAppender(), &shareNav{})
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestShareToTeam_UnsupportedCollection rejects resource names whose collection
// is not entities/facts/relations.
func TestShareToTeam_UnsupportedCollection(t *testing.T) {
	t.Parallel()
	tool := tools.NewShareToTeam(okShareAppender(), &shareNav{})
	_, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "episodes/01HZZZZZZZZZZZZZZZZZZZEP01",
	}))
	wantCode(t, err, mcp.CodeValidation)
}

// TestShareToTeam_Name_Schema exercises the tool's Name() method, ensuring it
// is registered as "shareToTeam" per the JSON Schema tool name contract.
func TestShareToTeam_Name_Schema(t *testing.T) {
	t.Parallel()
	tool := tools.NewShareToTeam(okShareAppender(), &shareNav{})
	if tool.Name() != "shareToTeam" {
		t.Errorf("Name() = %q, want shareToTeam", tool.Name())
	}
	if tool.InputSchema() == nil {
		t.Error("InputSchema() is nil")
	}
	if tool.OutputSchema() == nil {
		t.Error("OutputSchema() is nil")
	}
}

// TestShareToTeam_AppendError_LockContention verifies that a transient lock
// error from the event log surfaces as E_UNAVAILABLE via mapAppendError.
func TestShareToTeam_AppendError_LockContention(t *testing.T) {
	t.Parallel()
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
	}
	app := &shareAppender{err: fmt.Errorf("eventlog: %w", lockedError{})}
	tool := tools.NewShareToTeam(app, nav)

	_, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "entities/" + sharePersonalUID,
	}))
	wantCode(t, err, mcp.CodeUnavailable)
}

// lockedError mimics a transient database-lock error so IsTransientLockErr fires.
type lockedError struct{}

func (lockedError) Error() string { return "database is locked" }

// --- remap / endpoint-resolution tests (lead spec: (a)–(d)) ---

// TestShareToTeam_Subtree_FactPayloadCarriesTeamSubjectUID verifies spec (a):
// when sharing an entity subtree with withFacts=true, the fact event's payload
// SubjectUID must reference the minted team-scope entity UID, not the personal UID.
func TestShareToTeam_Subtree_FactPayloadCarriesTeamSubjectUID(t *testing.T) {
	t.Parallel()
	const factUID = "01HZZZZZZZZZZZZZZZZZZZFP01"
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		// No byName → entity is minted fresh; we capture the minted TeamName from output.
		facts: map[string]navigate.Fact{
			factUID: {UID: factUID, SubjectUID: sharePersonalUID, StateKey: "state.role", Value: "shield-maiden", Confidence: 1.0},
		},
		factsBySub: map[string][]navigate.Fact{
			sharePersonalUID: {
				{UID: factUID, SubjectUID: sharePersonalUID, StateKey: "state.role", Value: "shield-maiden", Confidence: 1.0},
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	raw, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref":       "entities/" + sharePersonalUID,
		"withFacts": true,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Shared []struct {
			TeamName string `json:"teamName"`
			Type     string `json:"type"`
		} `json:"shared"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if len(out.Shared) != 2 {
		t.Fatalf("want 2 items, got %d", len(out.Shared))
	}

	// First item is the entity; extract the minted team UID.
	if out.Shared[0].Type != "entity" {
		t.Fatalf("out.Shared[0].Type = %q, want entity", out.Shared[0].Type)
	}
	teamEntityName := out.Shared[0].TeamName // "entities/<minted-uid>"
	if !strings.HasPrefix(teamEntityName, "entities/") {
		t.Fatalf("entity teamName = %q, missing entities/ prefix", teamEntityName)
	}
	mintedTeamUID := strings.TrimPrefix(teamEntityName, "entities/")

	// The fact event's payload must carry the minted team UID as subjectUid.
	// app.gotEvents[0] = entity event; app.gotEvents[1] = fact event.
	if len(app.gotEvents) < 2 {
		t.Fatalf("want >=2 events, got %d", len(app.gotEvents))
	}
	var factPayload struct {
		SubjectUID string `json:"subjectUid"`
	}
	if jerr := json.Unmarshal(app.gotEvents[1].Payload, &factPayload); jerr != nil {
		t.Fatalf("unmarshal fact payload: %v", jerr)
	}
	if factPayload.SubjectUID != mintedTeamUID {
		t.Errorf("fact payload subjectUid = %q, want minted team UID %q", factPayload.SubjectUID, mintedTeamUID)
	}
	// Must NOT be the personal UID.
	if factPayload.SubjectUID == sharePersonalUID {
		t.Errorf("fact payload subjectUid = personal UID %q; expected team UID", sharePersonalUID)
	}
}

// TestShareToTeam_Subtree_FactPayloadCarriesReusedTeamUID verifies spec (b):
// when the subject entity is already in team scope (reuse path), the fact event's
// payload SubjectUID must reference the REUSED team UID, not the personal UID.
func TestShareToTeam_Subtree_FactPayloadCarriesReusedTeamUID(t *testing.T) {
	t.Parallel()
	const (
		factUID       = "01HZZZZZZZZZZZZZZZZZZZFP02"
		reusedTeamUID = "01HZZZZZZZZZZZZZZZZZZZTMRE"
	)
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		// byName returns existing team entity → shareEntity returns status=reused.
		byName: map[string][]navigate.Entity{
			"Eowyn": {{UID: reusedTeamUID, Kind: "person", DisplayName: "Eowyn"}},
		},
		facts: map[string]navigate.Fact{
			factUID: {UID: factUID, SubjectUID: sharePersonalUID, StateKey: "state.role", Value: "shield-maiden", Confidence: 1.0},
		},
		factsBySub: map[string][]navigate.Fact{
			sharePersonalUID: {
				{UID: factUID, SubjectUID: sharePersonalUID, StateKey: "state.role", Value: "shield-maiden", Confidence: 1.0},
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	if _, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref":       "entities/" + sharePersonalUID,
		"withFacts": true,
	})); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Entity is reused (0 entity events), fact is appended (1 fact event).
	if len(app.gotEvents) != 1 {
		t.Fatalf("want 1 event (fact only), got %d", len(app.gotEvents))
	}
	var factPayload struct {
		SubjectUID string `json:"subjectUid"`
	}
	if jerr := json.Unmarshal(app.gotEvents[0].Payload, &factPayload); jerr != nil {
		t.Fatalf("unmarshal fact payload: %v", jerr)
	}
	if factPayload.SubjectUID != reusedTeamUID {
		t.Errorf("fact payload subjectUid = %q, want reused team UID %q", factPayload.SubjectUID, reusedTeamUID)
	}
	if factPayload.SubjectUID == sharePersonalUID {
		t.Errorf("fact payload subjectUid = personal UID %q; expected reused team UID", sharePersonalUID)
	}
}

// TestShareToTeam_Fact_NoTeamSubject_Validation verifies spec (c): a bare fact
// share whose subject entity has no team-scope counterpart returns E_VALIDATION
// and appends no events.
func TestShareToTeam_Fact_NoTeamSubject_Validation(t *testing.T) {
	t.Parallel()
	nav := &shareNav{
		// Subject entity exists in personal scope but has NO team counterpart
		// (entities map has it, byName is empty → findTeamEntity returns false).
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		facts: map[string]navigate.Fact{
			shareFPersonalUID: {
				UID:        shareFPersonalUID,
				SubjectUID: sharePersonalUID,
				StateKey:   "state.role",
				Value:      "shield-maiden",
				Confidence: 1.0,
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	_, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "facts/" + shareFPersonalUID,
	}))
	wantCode(t, err, mcp.CodeValidation)
	if len(app.gotEvents) != 0 {
		t.Errorf("want 0 events on validation error, got %d", len(app.gotEvents))
	}
}

// TestShareToTeam_Subtree_RelationUnsharedObject_Skipped verifies spec (d): in a
// subtree withRelations share, a relation whose object has no team-scope
// counterpart is returned with status=skipped and a non-empty reason instead of
// being appended with a dangling personal UID.
func TestShareToTeam_Subtree_RelationUnsharedObject_Skipped(t *testing.T) {
	t.Parallel()
	const (
		relUID = "01HZZZZZZZZZZZZZZZZZZZSK01"
		objUID = "01HZZZZZZZZZZZZZZZZZZZSK02"
	)
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
			// object entity exists in personal scope but has no team counterpart.
			objUID: {UID: objUID, Kind: "organization", DisplayName: "Orc Horde"},
		},
		// byName has NO entry for "Orc Horde" → findTeamEntity returns false → skipped.
		relations: map[string]navigate.Relation{
			relUID: {UID: relUID, SubjectUID: sharePersonalUID, Predicate: "fights", ObjectUID: objUID, Confidence: 1.0},
		},
		relsByUID: map[string][]navigate.Relation{
			sharePersonalUID: {
				{UID: relUID, SubjectUID: sharePersonalUID, Predicate: "fights", ObjectUID: objUID, Confidence: 1.0},
			},
		},
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	raw, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref":           "entities/" + sharePersonalUID,
		"withRelations": true,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Shared []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"shared"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if len(out.Shared) != 2 {
		t.Fatalf("want 2 items (1 entity + 1 skipped relation), got %d", len(out.Shared))
	}
	entity := out.Shared[0]
	if entity.Type != "entity" {
		t.Errorf("out.Shared[0].Type = %q, want entity", entity.Type)
	}
	rel := out.Shared[1]
	if rel.Type != "relation" {
		t.Errorf("out.Shared[1].Type = %q, want relation", rel.Type)
	}
	if rel.Status != "skipped" {
		t.Errorf("relation status = %q, want skipped", rel.Status)
	}
	if rel.Reason == "" {
		t.Error("relation reason is empty; want non-empty explanation")
	}
	// Only the entity event must land; no relation event for a skipped item.
	for i, ev := range app.gotEvents {
		if string(ev.Kind) == ontology.KindRelation {
			t.Errorf("event[%d] is a relation event; skipped items must not be appended", i)
		}
	}
}

// TestShareToTeam_DedupLookupError_NoDuplicateMint verifies that a transient
// dedup-lookup failure surfaces as an error and does NOT fall through to
// minting a fresh team entity (which would silently create a duplicate when a
// team counterpart may already exist).
func TestShareToTeam_DedupLookupError_NoDuplicateMint(t *testing.T) {
	t.Parallel()
	nav := &shareNav{
		entities: map[string]navigate.Entity{
			sharePersonalUID: {UID: sharePersonalUID, Kind: "person", DisplayName: "Eowyn", Confidence: 1.0},
		},
		listErr: errors.New("database is busy"),
	}
	app := okShareAppender()
	tool := tools.NewShareToTeam(app, nav)

	_, err := tool.Invoke(context.Background(), mustMarshalJSON(t, map[string]any{
		"ref": "entities/" + sharePersonalUID,
	}))
	if err == nil {
		t.Fatal("want an error when the dedup lookup fails, got nil")
	}
	if len(app.gotEvents) != 0 {
		t.Errorf("want 0 events on dedup-lookup failure, got %d — a duplicate was minted", len(app.gotEvents))
	}
}
