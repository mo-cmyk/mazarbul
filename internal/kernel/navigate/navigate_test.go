package navigate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// openNavStore opens a fresh migrated SQLite projection and returns it plus a
// fatal-on-error exec helper for seeding rows.
func openNavStore(t *testing.T) (*sqlite.Store, func(q string, args ...any)) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	return store, exec
}

// newSeededNavigator opens a fresh SQLite projection, seeds a small graph, and
// returns a Navigator over its reader pool. The graph:
//
//	Merry --lives_in--> Zurich
//	Merry --reports_to--> Sam          (predicate filter target)
//	Sam   --works_at--> Mithril           (2-hop reach from Merry)
//	Merry --knew--> Pippin  (superseded: valid_until in the past + superseded_by)
//
// Episode ep1 mentions Merry (anchor / episodes-citing).
func newSeededNavigator(t *testing.T) *navigate.Navigator {
	t.Helper()
	store, exec := openNavStore(t)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec(ent, "alice", "person", "Merry", 1.0, "confirmed", now)
	exec(ent, "zurich", "place", "Zurich", 1.0, "confirmed", now)
	exec(ent, "bob", "person", "Sam", 0.9, "confirmed", now)
	exec(ent, "mithril", "organization", "Mithril", 1.0, "confirmed", now)
	exec(ent, "carol", "person", "Pippin", 1.0, "confirmed", now)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes)
	        VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	exec(rel, "r_lives", "alice", "lives_in", "zurich", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "r_reports", "alice", "reports_to", "bob", 1.0, "ep1", now, nil, nil, nil, `{"since":"2024"}`)
	exec(rel, "r_works", "bob", "works_at", "mithril", 1.0, "ep1", now, nil, nil, nil, `{}`)
	// Superseded/closed edge: valid_until in the past AND superseded pointer set.
	past := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	exec(rel, "r_knew", "alice", "knew", "carol", 0.8, "ep0", past, nil, past, "r_reports", `{}`)

	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)`,
		"ep1", now, "episode", `{"text":"Merry lives in Zurich and reports to Sam.","source":"cli:test","confidence":1.0}`)
	exec(`INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid) VALUES (?,?,?,?,?,?)`,
		"m1", "person", "Merry", "ep1", now, "alice")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

func TestGetEntity(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	got, err := nav.GetEntity(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.DisplayName != "Merry" || got.Kind != "person" {
		t.Fatalf("GetEntity = %+v, want Merry/person", got)
	}
	if _, err := nav.GetEntity(context.Background(), "nope"); !errors.Is(err, navigate.ErrNotFound) {
		t.Fatalf("GetEntity(nope) err = %v, want ErrNotFound", err)
	}
}

func TestGetEpisode(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	ep, err := nav.GetEpisode(context.Background(), "ep1")
	if err != nil {
		t.Fatalf("GetEpisode: %v", err)
	}
	if ep.Text == "" || ep.Source != "cli:test" {
		t.Fatalf("GetEpisode = %+v, want text+source from payload", ep)
	}
	if _, err := nav.GetEpisode(context.Background(), "nope"); !errors.Is(err, navigate.ErrNotFound) {
		t.Fatalf("GetEpisode(nope) err = %v, want ErrNotFound", err)
	}
}

func TestGetRelationHydratesNames(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	rel, err := nav.GetRelation(context.Background(), "r_reports")
	if err != nil {
		t.Fatalf("GetRelation: %v", err)
	}
	if rel.SubjectName != "Merry" || rel.ObjectName != "Sam" || rel.Predicate != "reports_to" {
		t.Fatalf("GetRelation = %+v, want Merry -reports_to-> Sam with names", rel)
	}
	if rel.Attributes["since"] != "2024" {
		t.Fatalf("GetRelation attributes = %v, want since=2024", rel.Attributes)
	}
	// source_event_uid hydrates so get(relation) can walk back to its origin
	// episode for provenance; the seed sets it to ep1.
	if rel.SourceEventUID != "ep1" {
		t.Fatalf("GetRelation SourceEventUID = %q, want ep1", rel.SourceEventUID)
	}
}

// seedCrossScopeRelation seeds a personal-scoped relation whose subject is
// personal-scoped (in a personal read span) but whose object is project-scoped
// (out of span), returning a navigator restricted to {personal}. It is the
// fixture for the endpoint-name scope-leak guards below.
func seedCrossScopeRelation(t *testing.T) *navigate.Navigator {
	t.Helper()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, "p_alice", "person", "Merry", 1.0, "confirmed", now, "personal")
	exec(ent, "x_secret", "project", "Secret Project", 1.0, "confirmed", now, "project")
	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes, scope)
	        VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	exec(rel, "r_leak", "p_alice", "works_on", "x_secret", 1.0, "ep1", now, nil, nil, nil, `{}`, "personal")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))
}

// TestEntityRelationsRedactsOutOfScopeEndpointName guards the scope boundary:
// an in-scope relation may surface, but the display name of an endpoint entity
// that lies outside the read span must not be disclosed (only its UID).
func TestEntityRelationsRedactsOutOfScopeEndpointName(t *testing.T) {
	t.Parallel()
	nav := seedCrossScopeRelation(t)

	rels, err := nav.EntityRelations(context.Background(), "p_alice", navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	var got *navigate.Relation
	for i := range rels {
		if rels[i].UID == "r_leak" {
			got = &rels[i]
		}
	}
	if got == nil {
		t.Fatalf("personal-scoped relation r_leak not returned under a personal read")
	}
	if got.SubjectName != "Merry" {
		t.Errorf("SubjectName = %q, want Merry (in-scope endpoint name must still hydrate)", got.SubjectName)
	}
	if got.ObjectName != "" {
		t.Errorf("ObjectName = %q, want empty (project-scope endpoint must not leak its name into a personal read)", got.ObjectName)
	}
	if got.ObjectUID != "x_secret" {
		t.Errorf("ObjectUID = %q, want x_secret (the UID still surfaces; only the name is gated)", got.ObjectUID)
	}
}

// TestGetRelationRedactsOutOfScopeEndpointName is the GetRelation analogue.
func TestGetRelationRedactsOutOfScopeEndpointName(t *testing.T) {
	t.Parallel()
	nav := seedCrossScopeRelation(t)

	rel, err := nav.GetRelation(context.Background(), "r_leak")
	if err != nil {
		t.Fatalf("GetRelation: %v", err)
	}
	if rel.SubjectName != "Merry" {
		t.Errorf("SubjectName = %q, want Merry (in-scope endpoint name must still hydrate)", rel.SubjectName)
	}
	if rel.ObjectName != "" {
		t.Errorf("ObjectName = %q, want empty (project-scope endpoint must not leak its name into a personal read)", rel.ObjectName)
	}
}

func TestEntityRelationsFiltersInactiveByDefault(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	ctx := context.Background()

	active, err := nav.EntityRelations(ctx, "alice", navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	for _, r := range active {
		if r.UID == "r_knew" {
			t.Fatalf("default EntityRelations returned the superseded/expired r_knew: %+v", active)
		}
	}
	// lives_in + reports_to are active; knew is not.
	if len(active) != 2 {
		t.Fatalf("active relations = %d, want 2: %+v", len(active), active)
	}

	all, err := nav.EntityRelations(ctx, "alice", navigate.RelationFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("EntityRelations(IncludeInactive): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all relations = %d, want 3 (incl. superseded): %+v", len(all), all)
	}
}

func TestEntityRelationsPredicateFilter(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	rels, err := nav.EntityRelations(context.Background(), "alice", navigate.RelationFilter{Predicates: []string{"reports_to"}})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	if len(rels) != 1 || rels[0].Predicate != "reports_to" || rels[0].ObjectUID != "bob" {
		t.Fatalf("predicate-filtered relations = %+v, want only reports_to->bob", rels)
	}
}

func TestNeighborsDepthAndDirection(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	ctx := context.Background()

	// Depth 1 from Merry: Zurich (lives_in) + Sam (reports_to). Pippin is
	// excluded (superseded). Mithril is 2 hops away.
	d1, err := nav.Neighbors(ctx, "alice", 1, navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("Neighbors d1: %v", err)
	}
	got1 := neighborUIDs(d1)
	if !got1["zurich"] || !got1["bob"] || got1["mithril"] || got1["carol"] {
		t.Fatalf("depth-1 neighbors = %v, want {zurich,bob}", got1)
	}
	for _, nb := range d1 {
		if nb.Direction != "outgoing" {
			t.Fatalf("neighbor %s direction = %q, want outgoing", nb.Entity.UID, nb.Direction)
		}
		if nb.Entity.DisplayName == "" {
			t.Fatalf("neighbor %s not hydrated (empty displayName)", nb.Entity.UID)
		}
	}

	// Depth 2 reaches Mithril via Sam.
	d2, err := nav.Neighbors(ctx, "alice", 2, navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("Neighbors d2: %v", err)
	}
	got2 := neighborUIDs(d2)
	if !got2["mithril"] {
		t.Fatalf("depth-2 neighbors = %v, want to include mithril", got2)
	}
	for _, nb := range d2 {
		if nb.Entity.UID == "mithril" && nb.Hops != 2 {
			t.Fatalf("mithril hops = %d, want 2", nb.Hops)
		}
	}
}

// TestNeighborsDepthBeyondTwo proves the walk expands past the old 1..2 clamp
// (now 1..MaxHops): a linear chain a->b->c->d reaches d only at depth 3, where
// the previous hard clamp to 2 would have stopped at c.
func TestNeighborsDepthBeyondTwo(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, u := range []string{"a", "b", "c", "d"} {
		exec(ent, u, "person", "node-"+u, 1.0, "confirmed", now)
	}
	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes)
	        VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	exec(rel, "ab", "a", "knows", "b", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "bc", "b", "knows", "c", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "cd", "c", "knows", "d", 1.0, "ep1", now, nil, nil, nil, `{}`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	d2, err := nav.Neighbors(ctx, "a", 2, navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("Neighbors d2: %v", err)
	}
	if got := neighborUIDs(d2); got["d"] {
		t.Fatalf("depth-2 reached d %v, want d unreachable at 2 hops", got)
	}

	d3, err := nav.Neighbors(ctx, "a", 3, navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("Neighbors d3: %v", err)
	}
	got3 := neighborUIDs(d3)
	if !got3["d"] {
		t.Fatalf("depth-3 neighbors = %v, want to include d", got3)
	}
	for _, nb := range d3 {
		if nb.Entity.UID == "d" && nb.Hops != 3 {
			t.Fatalf("d hops = %d, want 3", nb.Hops)
		}
	}
}

// newSubgraphNavigator seeds the fixture used by TestSubgraph:
//
//	a --knows--> b      a --knows--> c      b --knows--> c   (sibling cross-link)
//	a --knows--> a      (self-loop, must be dropped)
//	c --knows--> x      (x is 2 hops from a: out of a depth-1 node set)
func newSubgraphNavigator(t *testing.T) *navigate.Navigator {
	t.Helper()
	store, exec := openNavStore(t)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, u := range []string{"a", "b", "c", "x"} {
		exec(ent, u, "person", "node-"+u, 1.0, "confirmed", now)
	}
	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes)
	        VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	exec(rel, "ab", "a", "knows", "b", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "ac", "a", "knows", "c", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "bc", "b", "knows", "c", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "aa", "a", "knows", "a", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "cx", "c", "knows", "x", 1.0, "ep1", now, nil, nil, nil, `{}`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

func TestSubgraph(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("collects nodes edges and cross-links", func(t *testing.T) {
		t.Parallel()
		nav := newSubgraphNavigator(t)
		sg, err := nav.Subgraph(ctx, "a", 1, navigate.RelationFilter{})
		if err != nil {
			t.Fatalf("Subgraph: %v", err)
		}
		// Nodes: seed a (hop 0) + b, c (hop 1). x is 2 hops out -> excluded.
		gotNodes := map[string]int{}
		for _, n := range sg.Nodes {
			gotNodes[n.Entity.UID] = n.Hops
		}
		if len(sg.Nodes) != 3 || gotNodes["a"] != 0 || gotNodes["b"] != 1 || gotNodes["c"] != 1 {
			t.Fatalf("nodes = %+v, want a@0,b@1,c@1", gotNodes)
		}
		// Seed is first (sorted by hops then uid).
		if sg.Nodes[0].Entity.UID != "a" {
			t.Fatalf("nodes[0] = %q, want seed a first", sg.Nodes[0].Entity.UID)
		}
		// Edges: ab, ac, AND the sibling cross-link bc. Self-loop aa dropped;
		// cx dropped (x out of set).
		gotEdges := map[string]bool{}
		for _, e := range sg.Edges {
			gotEdges[e.UID] = true
		}
		if len(sg.Edges) != 3 || !gotEdges["ab"] || !gotEdges["ac"] || !gotEdges["bc"] {
			t.Fatalf("edges = %+v, want {ab,ac,bc}", gotEdges)
		}
		if gotEdges["aa"] || gotEdges["cx"] {
			t.Fatalf("edges leaked a self-loop or out-of-set edge: %+v", gotEdges)
		}
	})

	t.Run("depth reaches the full chain", func(t *testing.T) {
		t.Parallel()
		nav := newSubgraphNavigator(t)
		sg, err := nav.Subgraph(ctx, "a", 2, navigate.RelationFilter{})
		if err != nil {
			t.Fatalf("Subgraph: %v", err)
		}
		gotNodes := map[string]int{}
		for _, n := range sg.Nodes {
			gotNodes[n.Entity.UID] = n.Hops
		}
		// x is now in the set at hop 2, and edge cx is included.
		if gotNodes["x"] != 2 {
			t.Fatalf("nodes = %+v, want x at hop 2", gotNodes)
		}
		gotEdges := map[string]bool{}
		for _, e := range sg.Edges {
			gotEdges[e.UID] = true
		}
		if !gotEdges["cx"] {
			t.Fatalf("edges = %+v, want cx present once x is in the set", gotEdges)
		}
	})

	t.Run("is deterministic", func(t *testing.T) {
		t.Parallel()
		nav := newSubgraphNavigator(t)
		first, err := nav.Subgraph(ctx, "a", 2, navigate.RelationFilter{})
		if err != nil {
			t.Fatalf("Subgraph first: %v", err)
		}
		second, err := nav.Subgraph(ctx, "a", 2, navigate.RelationFilter{})
		if err != nil {
			t.Fatalf("Subgraph second: %v", err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("Subgraph not deterministic:\n first=%+v\nsecond=%+v", first, second)
		}
	})

	t.Run("orphan seed yields a single node", func(t *testing.T) {
		t.Parallel()
		// Pippin (carol) has only a superseded/closed edge, so a NOW-view walk
		// finds no active neighbors: the subgraph is the lone seed node.
		nav := newSeededNavigator(t)
		isolated, err := nav.Subgraph(ctx, "carol", 2, navigate.RelationFilter{})
		if err != nil {
			t.Fatalf("Subgraph carol: %v", err)
		}
		if len(isolated.Nodes) != 1 || isolated.Nodes[0].Entity.UID != "carol" || len(isolated.Edges) != 0 {
			t.Fatalf("isolated subgraph = %d nodes / %d edges, want 1 node (carol) / 0 edges", len(isolated.Nodes), len(isolated.Edges))
		}
	})

	t.Run("missing seed is not found", func(t *testing.T) {
		t.Parallel()
		nav := newSubgraphNavigator(t)
		if _, err := nav.Subgraph(ctx, "nope", 1, navigate.RelationFilter{}); !errors.Is(err, navigate.ErrNotFound) {
			t.Fatalf("Subgraph(nope) err = %v, want ErrNotFound", err)
		}
	})
}

func TestListEntitiesFilters(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	ctx := context.Background()

	// The store ships a bootstrap "self" person node (migration 0012,
	// confidence 1.0), so the person set is {self, Merry, Sam, Pippin}.
	people, err := nav.ListEntities(ctx, navigate.EntityFilter{Kind: "person"})
	if err != nil {
		t.Fatalf("ListEntities(person): %v", err)
	}
	gotPeople := entityUIDs(people)
	for _, want := range []string{"alice", "bob", "carol", "self"} {
		if !gotPeople[want] {
			t.Fatalf("person entities = %v, want to include %q", gotPeople, want)
		}
	}
	for _, u := range people {
		if u.Kind != "person" {
			t.Fatalf("kind filter leaked a %q entity: %+v", u.Kind, u)
		}
	}

	// Sam (0.9) drops below the floor; Merry, Pippin, and self (all 1.0) stay.
	byConf, err := nav.ListEntities(ctx, navigate.EntityFilter{Kind: "person", MinConfidence: 0.95})
	if err != nil {
		t.Fatalf("ListEntities(minConf): %v", err)
	}
	gotHi := entityUIDs(byConf)
	if gotHi["bob"] {
		t.Fatalf("minConfidence=0.95 kept Sam (0.9): %v", gotHi)
	}
	for _, want := range []string{"alice", "carol", "self"} {
		if !gotHi[want] {
			t.Fatalf("high-confidence people = %v, want to include %q", gotHi, want)
		}
	}

	named, err := nav.ListEntities(ctx, navigate.EntityFilter{NameLike: "mer"})
	if err != nil {
		t.Fatalf("ListEntities(nameLike): %v", err)
	}
	if len(named) != 1 || named[0].UID != "alice" {
		t.Fatalf("nameLike 'mer' = %+v, want only Merry", named)
	}
}

func TestEpisodesCitingEntity(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	eps, err := nav.EpisodesCitingEntity(context.Background(), "alice", 10)
	if err != nil {
		t.Fatalf("EpisodesCitingEntity: %v", err)
	}
	if len(eps) != 1 || eps[0].UID != "ep1" {
		t.Fatalf("episodes citing alice = %+v, want [ep1]", eps)
	}
}

func TestPredicates(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	preds, err := nav.Predicates(context.Background())
	if err != nil {
		t.Fatalf("Predicates: %v", err)
	}
	// Migrations 0006 (10 canonical) + 0012 (has_name) seed the table.
	byName := map[string]navigate.Predicate{}
	for _, p := range preds {
		byName[p.Predicate] = p
	}
	if len(byName) < 11 {
		t.Fatalf("Predicates returned %d, want at least 11 seeded", len(byName))
	}
	hn, ok := byName["has_name"]
	if !ok {
		t.Fatalf("Predicates missing has_name: %+v", preds)
	}
	if hn.Kind != "functional" {
		t.Errorf("has_name kind = %q, want functional", hn.Kind)
	}
	// Alphabetical order is part of the contract.
	for i := 1; i < len(preds); i++ {
		if preds[i-1].Predicate > preds[i].Predicate {
			t.Fatalf("Predicates not sorted: %q before %q", preds[i-1].Predicate, preds[i].Predicate)
		}
	}
}

// TestPredicates_DomainRange asserts the P5 domain/range + sameAs columns
// surface through Navigator.Predicates so introspect can advertise them.
func TestPredicates_DomainRange(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	exec(`INSERT INTO predicates
	        (predicate, kind, description, added_at, added_by, subject_kinds, object_kinds, same_as)
	      VALUES ('employed_by','non-functional','', ?, 'user',
	              '["person"]', '["organization"]', '["https://schema.org/worksFor"]')`,
		time.Now().UTC().Format(time.RFC3339Nano))

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	preds, err := nav.Predicates(context.Background())
	if err != nil {
		t.Fatalf("Predicates: %v", err)
	}
	var got *navigate.Predicate
	for i := range preds {
		if preds[i].Predicate == "employed_by" {
			got = &preds[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("employed_by not returned: %+v", preds)
	}
	if len(got.SubjectKinds) != 1 || got.SubjectKinds[0] != "person" {
		t.Errorf("SubjectKinds = %v, want [person]", got.SubjectKinds)
	}
	if len(got.ObjectKinds) != 1 || got.ObjectKinds[0] != "organization" {
		t.Errorf("ObjectKinds = %v, want [organization]", got.ObjectKinds)
	}
	if len(got.SameAs) != 1 || got.SameAs[0] != "https://schema.org/worksFor" {
		t.Errorf("SameAs = %v, want [https://schema.org/worksFor]", got.SameAs)
	}

	// A seeded predicate with no P5 columns must surface empty slices, not crash.
	var worksAt *navigate.Predicate
	for i := range preds {
		if preds[i].Predicate == "works_at" {
			worksAt = &preds[i]
			break
		}
	}
	if worksAt == nil {
		t.Fatalf("seeded works_at missing")
	}
	if len(worksAt.SubjectKinds) != 0 || len(worksAt.ObjectKinds) != 0 {
		t.Errorf("untyped predicate should have empty domain/range, got %v/%v",
			worksAt.SubjectKinds, worksAt.ObjectKinds)
	}
}

func TestEntityKinds(t *testing.T) {
	t.Parallel()
	nav := newSeededNavigator(t)
	kinds, err := nav.EntityKinds(context.Background())
	if err != nil {
		t.Fatalf("EntityKinds: %v", err)
	}
	got := map[string]int{}
	for _, kc := range kinds {
		got[kc.Kind] = kc.Count
	}
	// person: alice, bob, carol, self (bootstrap). place: zurich. organization: mithril.
	if got["person"] != 4 {
		t.Errorf("person count = %d, want 4", got["person"])
	}
	if got["place"] != 1 || got["organization"] != 1 {
		t.Errorf("place/org counts = %d/%d, want 1/1", got["place"], got["organization"])
	}
	// Most populous kind first.
	if len(kinds) > 0 && kinds[0].Kind != "person" {
		t.Errorf("first kind = %q, want person (most populous)", kinds[0].Kind)
	}
}

// TestNeighborsDoesNotTruncatePerHop is the regression guard for the per-hop
// no-cap sentinel: hopFrom must fetch every edge of a wide node so the walk's
// overall limit (not EntityRelations' defaultLimit of 50) governs truncation.
// A hub with 60 outgoing edges, walked at depth 1 with an overall limit of 100,
// must surface all 60 — before the fix the per-hop fetch capped at 50.
func TestNeighborsDoesNotTruncatePerHop(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	exec(ent, "hub", "person", "Hub", 1.0, "confirmed", now)
	const spokes = 60
	for i := range spokes {
		id := fmt.Sprintf("spoke%02d", i)
		exec(ent, id, "thing", id, 1.0, "confirmed", now)
		exec(rel, "r_"+id, "hub", "knows", id, 1.0, "ep1", now, nil, nil, nil, `{}`)
	}
	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	nbs, err := nav.Neighbors(context.Background(), "hub", 1, navigate.RelationFilter{Limit: 100})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(nbs) != spokes {
		t.Fatalf("depth-1 neighbors = %d, want %d (per-hop fetch must not cap at defaultLimit)", len(nbs), spokes)
	}
}

// TestNeighborsCloserHopWins verifies that a node reachable at both hop 1 and
// hop 2 is emitted once, attributed to the closer hop. Graph: A->B, A->C, B->C;
// C is a direct neighbor of A (hop 1) and also reachable via B (hop 2).
func TestNeighborsCloserHopWins(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	for _, id := range []string{"a", "b", "c"} {
		exec(ent, id, "person", id, 1.0, "confirmed", now)
	}
	exec(rel, "r_ab", "a", "knows", "b", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "r_ac", "a", "knows", "c", 1.0, "ep1", now, nil, nil, nil, `{}`)
	exec(rel, "r_bc", "b", "knows", "c", 1.0, "ep1", now, nil, nil, nil, `{}`)
	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	nbs, err := nav.Neighbors(context.Background(), "a", 2, navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	hopsByUID := map[string]int{}
	for _, nb := range nbs {
		if _, dup := hopsByUID[nb.Entity.UID]; dup {
			t.Fatalf("neighbor %s emitted twice: %+v", nb.Entity.UID, nbs)
		}
		hopsByUID[nb.Entity.UID] = nb.Hops
	}
	if hopsByUID["c"] != 1 {
		t.Fatalf("c hops = %d, want 1 (closer hop wins over the A->B->C path)", hopsByUID["c"])
	}
}

// TestEntityRelationsTimeTravel exercises the At valid-time anchor: an edge with
// a closed valid window is absent from "current truth" but reappears when At is
// moved inside the window.
func TestEntityRelationsTimeTravel(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC()
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	nowStr := now.Format(time.RFC3339Nano)
	exec(ent, "dave", "person", "Dave", 1.0, "confirmed", nowStr)
	exec(ent, "oldco", "organization", "OldCo", 1.0, "confirmed", nowStr)
	// Dave worked at OldCo for a window that closed yesterday. Not superseded —
	// just expired — so only the valid-time gate (not the superseded filter)
	// governs visibility.
	from := now.Add(-72 * time.Hour)
	until := now.Add(-24 * time.Hour)
	exec(rel, "r_old", "dave", "works_at", "oldco", 1.0, "ep1", nowStr,
		from.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano), nil, `{}`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	// Current truth: the window closed, so nothing.
	cur, err := nav.EntityRelations(ctx, "dave", navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("EntityRelations(now): %v", err)
	}
	if len(cur) != 0 {
		t.Fatalf("current relations = %+v, want none (window closed yesterday)", cur)
	}

	// Time-travel into the window: the edge was live.
	mid := now.Add(-48 * time.Hour)
	past, err := nav.EntityRelations(ctx, "dave", navigate.RelationFilter{At: &mid})
	if err != nil {
		t.Fatalf("EntityRelations(At): %v", err)
	}
	if len(past) != 1 || past[0].UID != "r_old" {
		t.Fatalf("At-windowed relations = %+v, want [r_old]", past)
	}
}

// TestProvenanceWalksBackToOriginEpisode covers both provenance methods:
// EventProvenance reads the provenance row keyed on an episode event directly,
// and EntityProvenance aggregates across the episodes that mention an entity
// (an entity has no provenance row of its own). An unknown uid yields an empty
// slice, not an error.
func TestProvenanceWalksBackToOriginEpisode(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`,
		"alice", "person", "Merry", 1.0, "confirmed", now)
	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)`,
		"ep1", now, "episode", `{"text":"Merry.","source":"cli:remember","confidence":0.9}`)
	exec(`INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid) VALUES (?,?,?,?,?,?)`,
		"m1", "person", "Merry", "ep1", now, "alice")
	exec(`INSERT INTO provenance (event_uid, source, confidence, decided_at) VALUES (?,?,?,?)`,
		"ep1", "cli:remember", 0.9, now)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	// Direct: the episode's own provenance row.
	ev, err := nav.EventProvenance(ctx, "ep1")
	if err != nil {
		t.Fatalf("EventProvenance: %v", err)
	}
	if len(ev) != 1 || ev[0].Source != "cli:remember" || ev[0].Confidence != 0.9 {
		t.Fatalf("EventProvenance(ep1) = %+v, want one cli:remember/0.9 record", ev)
	}

	// Aggregated: the entity inherits its origin from the citing episode.
	en, err := nav.EntityProvenance(ctx, "alice")
	if err != nil {
		t.Fatalf("EntityProvenance: %v", err)
	}
	if len(en) != 1 || en[0].EventUID != "ep1" || en[0].Source != "cli:remember" {
		t.Fatalf("EntityProvenance(alice) = %+v, want [ep1/cli:remember] via mention", en)
	}

	// Unknown uid: empty, not an error.
	none, err := nav.EventProvenance(ctx, "ghost")
	if err != nil {
		t.Fatalf("EventProvenance(ghost): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("EventProvenance(ghost) = %+v, want empty", none)
	}
}

func neighborUIDs(ns []navigate.Neighbor) map[string]bool {
	m := make(map[string]bool, len(ns))
	for _, n := range ns {
		m[n.Entity.UID] = true
	}
	return m
}

func entityUIDs(es []navigate.Entity) map[string]bool {
	m := make(map[string]bool, len(es))
	for _, e := range es {
		m[e.UID] = true
	}
	return m
}

// TestRegisteredEntityKinds verifies that RegisteredEntityKinds returns all
// rows from entity_kinds in alphabetical order and includes the core-guard
// provenance field.
func TestRegisteredEntityKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range []struct{ name, display, prov string }{
		{"organization", "Organization", "core"},
		{"person", "Person", "core"},
		{"vehicle", "Vehicle", "user"},
	} {
		exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at) VALUES (?,?,?,?,?,?)`,
			row.name, row.display, "Test.", row.prov, "seed", now)
	}

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	kinds, err := nav.RegisteredEntityKinds(ctx)
	if err != nil {
		t.Fatalf("RegisteredEntityKinds: %v", err)
	}
	if len(kinds) != 3 {
		t.Fatalf("len(kinds) = %d, want 3", len(kinds))
	}

	// Alphabetical order by name is the contract.
	wantNames := []string{"organization", "person", "vehicle"}
	for i, k := range kinds {
		if k.Name != wantNames[i] {
			t.Errorf("kinds[%d].Name = %q, want %q", i, k.Name, wantNames[i])
		}
	}

	// Provenance field must survive the round-trip.
	byName := make(map[string]navigate.RegisteredEntityKind, len(kinds))
	for _, k := range kinds {
		byName[k.Name] = k
	}
	if byName["person"].Provenance != "core" {
		t.Errorf("person provenance = %q, want core", byName["person"].Provenance)
	}
	if byName["vehicle"].Provenance != "user" {
		t.Errorf("vehicle provenance = %q, want user", byName["vehicle"].Provenance)
	}
}

// TestRegisteredEntityKinds_Empty verifies that an empty entity_kinds table
// returns an empty (non-nil) slice without error.
func TestRegisteredEntityKinds_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := openNavStore(t)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	kinds, err := nav.RegisteredEntityKinds(ctx)
	if err != nil {
		t.Fatalf("RegisteredEntityKinds: %v", err)
	}
	if kinds == nil {
		t.Error("RegisteredEntityKinds returned nil slice, want empty non-nil")
	}
	if len(kinds) != 0 {
		t.Errorf("len(kinds) = %d, want 0", len(kinds))
	}
}

// TestRegisteredEntityKinds_SameAs verifies that the same_as JSON array
// stored in entity_kinds is unmarshalled into RegisteredEntityKind.SameAs,
// and that a row without same_as returns nil (not an empty slice).
func TestRegisteredEntityKinds_SameAs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Row with same_as populated.
	exec(`INSERT INTO entity_kinds (name, display_name, description, same_as, provenance, added_by, added_at)
	      VALUES (?,?,?,?,?,?,?)`,
		"person", "Person", "A human individual.",
		`["https://schema.org/Person","https://www.wikidata.org/wiki/Q5"]`,
		"core", "seed", now,
	)
	// Row without same_as (NULL).
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at)
	      VALUES (?,?,?,?,?,?)`,
		"project", "Project", "A coordinated undertaking.", "core", "seed", now,
	)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	kinds, err := nav.RegisteredEntityKinds(ctx)
	if err != nil {
		t.Fatalf("RegisteredEntityKinds: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("len(kinds) = %d, want 2", len(kinds))
	}

	byName := make(map[string]navigate.RegisteredEntityKind, len(kinds))
	for _, k := range kinds {
		byName[k.Name] = k
	}

	p := byName["person"]
	if len(p.SameAs) != 2 {
		t.Fatalf("person.SameAs len = %d, want 2; got %v", len(p.SameAs), p.SameAs)
	}
	if p.SameAs[0] != "https://schema.org/Person" {
		t.Errorf("person.SameAs[0] = %q, want https://schema.org/Person", p.SameAs[0])
	}
	if p.SameAs[1] != "https://www.wikidata.org/wiki/Q5" {
		t.Errorf("person.SameAs[1] = %q, want https://www.wikidata.org/wiki/Q5", p.SameAs[1])
	}

	proj := byName["project"]
	if proj.SameAs != nil {
		t.Errorf("project.SameAs = %v, want nil (no same_as stored)", proj.SameAs)
	}
}

// TestRegisteredEntityKinds_FiltersInactive verifies that RegisteredEntityKinds
// excludes rows with status='proposed' and status='rejected', returning only
// rows whose status is 'active' (or has the DEFAULT 'active' via omitted column).
func TestRegisteredEntityKinds_FiltersInactive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Active row: status column omitted, relies on DEFAULT 'active'.
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at) VALUES (?,?,?,?,?,?)`,
		"vehicle", "Vehicle", "A motorised road vehicle.", "user", "seed", now)
	// Proposed row: should be excluded.
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status) VALUES (?,?,?,?,?,?,?)`,
		"spaceship", "Spaceship", "An interplanetary vehicle.", "user", "seed", now, "proposed")
	// Rejected row: should be excluded.
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status) VALUES (?,?,?,?,?,?,?)`,
		"hovercraft", "Hovercraft", "An air-cushion vehicle.", "user", "seed", now, "rejected")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	kinds, err := nav.RegisteredEntityKinds(ctx)
	if err != nil {
		t.Fatalf("RegisteredEntityKinds: %v", err)
	}
	if len(kinds) != 1 {
		names := make([]string, len(kinds))
		for i, k := range kinds {
			names[i] = k.Name
		}
		t.Fatalf("len(kinds) = %d, want 1 (only active); got %v", len(kinds), names)
	}
	if kinds[0].Name != "vehicle" {
		t.Errorf("kinds[0].Name = %q, want vehicle", kinds[0].Name)
	}
}

// TestRegisteredEntityKinds_PropertySchema verifies that a non-trivial
// attr_schema blob stored in entity_kinds is surfaced as PropertySchema in the
// read model, and that a kind without attr_schema returns nil (not an empty
// slice or empty object).
func TestRegisteredEntityKinds_PropertySchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	schema := `{"type":"object","properties":{"vin":{"type":"string"}}}`
	// Row with attr_schema populated.
	exec(`INSERT INTO entity_kinds (name, display_name, description, attr_schema, provenance, added_by, added_at)
	      VALUES (?,?,?,?,?,?,?)`,
		"user:vehicle", "Vehicle", "A road vehicle.", schema, "user", "user", now,
	)
	// Row without attr_schema (NULL) — PropertySchema must be nil.
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at)
	      VALUES (?,?,?,?,?,?)`,
		"person", "Person", "A human individual.", "core", "seed", now,
	)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	kinds, err := nav.RegisteredEntityKinds(ctx)
	if err != nil {
		t.Fatalf("RegisteredEntityKinds: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("len(kinds) = %d, want 2", len(kinds))
	}

	byName := make(map[string]navigate.RegisteredEntityKind, len(kinds))
	for _, k := range kinds {
		byName[k.Name] = k
	}

	v := byName["user:vehicle"]
	if v.PropertySchema == nil {
		t.Fatal("user:vehicle.PropertySchema = nil, want non-nil JSON schema")
	}
	// Round-trip: ensure the raw JSON is valid and contains the vin property.
	var parsed map[string]any
	if err := json.Unmarshal(v.PropertySchema, &parsed); err != nil {
		t.Fatalf("user:vehicle.PropertySchema unmarshal: %v", err)
	}
	props, ok := parsed["properties"].(map[string]any)
	if !ok {
		t.Fatalf("user:vehicle.PropertySchema has no 'properties' object; got %v", parsed)
	}
	if _, ok := props["vin"]; !ok {
		t.Errorf("user:vehicle.PropertySchema.properties missing 'vin'; got %v", props)
	}

	p := byName["person"]
	if p.PropertySchema != nil {
		t.Errorf("person.PropertySchema = %s, want nil (no attr_schema stored)", p.PropertySchema)
	}
}
