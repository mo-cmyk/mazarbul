package navigate_test

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// seedProposedQueue seeds a projection with a mix of proposed and asserted
// edges across scopes, each with a connector source recorded in provenance, and
// returns a Navigator over its reader. The graph:
//
//	(personal) Frodo --met--> Gandalf      proposed, connector:granola
//	(personal) Frodo --gave--> Sam         proposed, connector:claude-chats
//	(personal) Frodo --owns--> Sting       asserted (must never surface)
//	(team)     Aragorn --rules--> Gondor   proposed, connector:granola (team-only)
func seedProposedQueue(t *testing.T) *sqlite.Store {
	t.Helper()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, "frodo", "person", "Frodo", 1.0, "confirmed", now, "personal")
	exec(ent, "gandalf", "person", "Gandalf", 1.0, "confirmed", now, "personal")
	exec(ent, "sam", "person", "Sam", 1.0, "confirmed", now, "personal")
	exec(ent, "sting", "object", "Sting", 1.0, "confirmed", now, "personal")
	exec(ent, "aragorn", "person", "Aragorn", 1.0, "confirmed", now, "team")
	exec(ent, "gondor", "place", "Gondor", 1.0, "confirmed", now, "team")

	// Provenance FK references episodes(uid), so seed the origin events first.
	ep := `INSERT INTO episodes (uid, create_time, kind, payload_json, scope) VALUES (?,?,?,?,?)`
	exec(ep, "ev_granola_1", now, "episode", `{"text":"transcript","source":"connector:granola","confidence":0.7}`, "personal")
	exec(ep, "ev_claude_1", now, "episode", `{"text":"export","source":"connector:claude-chats","confidence":0.6}`, "personal")
	exec(ep, "ev_user_1", now, "episode", `{"text":"note","source":"cli:remember","confidence":1.0}`, "personal")
	exec(ep, "ev_granola_team", now, "episode", `{"text":"transcript","source":"connector:granola","confidence":0.7}`, "team")

	prov := `INSERT INTO provenance (event_uid, source, confidence, decided_at) VALUES (?,?,?,?)`
	exec(prov, "ev_granola_1", "connector:granola", 0.7, now)
	exec(prov, "ev_claude_1", "connector:claude-chats", 0.6, now)
	exec(prov, "ev_user_1", "cli:remember", 1.0, now)
	exec(prov, "ev_granola_team", "connector:granola", 0.7, now)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes, scope, status)
	        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
	exec(rel, "r_met", "frodo", "met", "gandalf", 0.7, "ev_granola_1", now, nil, nil, nil, `{}`, "personal", "proposed")
	exec(rel, "r_gave", "frodo", "gave", "sam", 0.6, "ev_claude_1", now, nil, nil, nil, `{}`, "personal", "proposed")
	exec(rel, "r_owns", "frodo", "owns", "sting", 1.0, "ev_user_1", now, nil, nil, nil, `{}`, "personal", "asserted")
	exec(rel, "r_rules", "aragorn", "rules", "gondor", 0.7, "ev_granola_team", now, nil, nil, nil, `{}`, "team", "proposed")

	return store
}

func navOver(t *testing.T, store *sqlite.Store, scopes kernel.ScopeSet) *navigate.Navigator {
	t.Helper()
	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav.WithScopes(scopes)
}

// TestListRelationsByStatusFiltersByStatus pins that only proposed edges in the
// requested scope surface, with the connector source and edge scope hydrated.
func TestListRelationsByStatusFiltersByStatus(t *testing.T) {
	t.Parallel()
	store := seedProposedQueue(t)
	nav := navOver(t, store, kernel.DefaultReadSet())

	rels, err := nav.ListRelationsByStatus(context.Background(), "proposed", "", 0)
	if err != nil {
		t.Fatalf("ListRelationsByStatus: %v", err)
	}
	// Two personal proposed edges; the asserted edge and the team edge are out.
	if len(rels) != 2 {
		t.Fatalf("got %d proposed edges, want 2: %+v", len(rels), rels)
	}
	byUID := map[string]navigate.ProposedRelation{}
	for _, r := range rels {
		byUID[r.UID] = r
		if r.Status != "proposed" {
			t.Errorf("edge %s status = %q, want proposed", r.UID, r.Status)
		}
	}
	if _, ok := byUID["r_owns"]; ok {
		t.Error("asserted edge r_owns leaked into the proposed queue")
	}
	if _, ok := byUID["r_rules"]; ok {
		t.Error("team-scope edge r_rules leaked into a {project,personal} read")
	}
	met, ok := byUID["r_met"]
	if !ok {
		t.Fatal("expected r_met in the queue")
	}
	if met.Source != "connector:granola" {
		t.Errorf("r_met source = %q, want connector:granola", met.Source)
	}
	if met.Scope != "personal" {
		t.Errorf("r_met scope = %q, want personal", met.Scope)
	}
	if met.SubjectName != "Frodo" || met.ObjectName != "Gandalf" {
		t.Errorf("r_met names = %q -> %q, want Frodo -> Gandalf", met.SubjectName, met.ObjectName)
	}
}

// TestListRelationsByStatusScopeIsolation pins #206: a team-scope proposal is
// visible ONLY under a team read, never under the personal default. This is the
// guard against leaking a team proposal into a personal review.
func TestListRelationsByStatusScopeIsolation(t *testing.T) {
	t.Parallel()
	store := seedProposedQueue(t)

	// Personal read: the team edge is invisible.
	personal := navOver(t, store, kernel.NewScopeSet(kernel.ScopePersonal))
	prels, err := personal.ListRelationsByStatus(context.Background(), "proposed", "", 0)
	if err != nil {
		t.Fatalf("personal ListRelationsByStatus: %v", err)
	}
	for _, r := range prels {
		if r.Scope == "team" {
			t.Fatalf("team-scope edge %s leaked into a personal read", r.UID)
		}
	}

	// Team read: the team edge surfaces.
	team := navOver(t, store, kernel.NewScopeSet(kernel.ScopeTeam))
	trels, err := team.ListRelationsByStatus(context.Background(), "proposed", "", 0)
	if err != nil {
		t.Fatalf("team ListRelationsByStatus: %v", err)
	}
	if len(trels) != 1 || trels[0].UID != "r_rules" {
		t.Fatalf("team read = %+v, want exactly r_rules", trels)
	}
	if trels[0].Scope != "team" {
		t.Errorf("r_rules scope = %q, want team", trels[0].Scope)
	}
}

// TestListRelationsByStatusEmptyScopeFailsClosed pins the fail-closed guarantee:
// an empty scope set returns zero rows, never the whole projection.
func TestListRelationsByStatusEmptyScopeFailsClosed(t *testing.T) {
	t.Parallel()
	store := seedProposedQueue(t)
	nav := navOver(t, store, kernel.ScopeSet{})

	rels, err := nav.ListRelationsByStatus(context.Background(), "proposed", "", 0)
	if err != nil {
		t.Fatalf("ListRelationsByStatus: %v", err)
	}
	if len(rels) != 0 {
		t.Fatalf("empty scope returned %d rows, want 0 (fail closed)", len(rels))
	}
}

// TestListRelationsByStatusConnectorFilter pins the --connector narrowing: only
// edges whose origin event source equals the filter come back.
func TestListRelationsByStatusConnectorFilter(t *testing.T) {
	t.Parallel()
	store := seedProposedQueue(t)
	nav := navOver(t, store, kernel.DefaultReadSet())

	rels, err := nav.ListRelationsByStatus(context.Background(), "proposed", "connector:granola", 0)
	if err != nil {
		t.Fatalf("ListRelationsByStatus: %v", err)
	}
	if len(rels) != 1 || rels[0].UID != "r_met" {
		t.Fatalf("connector filter = %+v, want exactly r_met (connector:granola, personal)", rels)
	}

	none, err := nav.ListRelationsByStatus(context.Background(), "proposed", "connector:nonexistent", 0)
	if err != nil {
		t.Fatalf("ListRelationsByStatus(nonexistent): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown connector returned %d rows, want 0", len(none))
	}
}
