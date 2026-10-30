package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// TestE2E_SeedThenTraverse exercises the headline flow over the real stack:
// a fully-migrated SQLite projection → a real navigate.Navigator → the real
// MCP navigation tools. It seeds a small graph, then drives the seed-then-
// traverse sequence an agent would run (get → relations → neighbors →
// entities → introspect) entirely through tool.Invoke, asserting the projected
// JSON at each hop.
//
// No chat provider exists in this test, so "the read path makes zero LLM
// calls" holds by construction: every verb here resolves against SQLite only.
//
//	Merry --reports_to--> Sam --works_at--> Mithril
//	Merry --lives_in----> Zurich
func TestE2E_SeedThenTraverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "alice", "person", "Merry", 1.0, "confirmed", now)
	exec(ent, "bob", "person", "Sam", 1.0, "confirmed", now)
	exec(ent, "mithril", "organization", "Mithril", 1.0, "confirmed", now)
	exec(ent, "zurich", "place", "Zurich", 1.0, "confirmed", now)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes) VALUES (?,?,?,?,?,?,?,?)`
	exec(rel, "r_reports", "alice", "reports_to", "bob", 1.0, "ep1", now, `{"since":"2024"}`)
	exec(rel, "r_works", "bob", "works_at", "mithril", 1.0, "ep1", now, `{}`)
	exec(rel, "r_lives", "alice", "lives_in", "zurich", 1.0, "ep1", now, `{}`)

	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)`,
		"ep1", now, "episode", `{"text":"Merry reports to Sam and lives in Zurich.","source":"cli:test","confidence":1.0}`)
	exec(`INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid) VALUES (?,?,?,?,?,?)`,
		"m1", "person", "Merry", "ep1", now, "alice")
	exec(`INSERT INTO provenance (event_uid, source, confidence, decided_at) VALUES (?,?,?,?)`,
		"ep1", "cli:test", 1.0, now)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	// Step 1: get(entities/alice) hydrates the anchor and walks its provenance
	// back to the citing episode — the structured lineage an agent needs to
	// judge a fact without composing prose.
	getOut := invokeOK(t, ctx, tools.NewGet(nav), `{"name":"entities/alice"}`)
	if typ := jsonField(t, getOut, "type"); typ != "entity" {
		t.Fatalf("get type = %q, want entity (%s)", typ, getOut)
	}
	var got struct {
		Provenance []struct {
			EventUID string `json:"eventUid"`
			Source   string `json:"source"`
		} `json:"provenance"`
	}
	mustJSON(t, getOut, &got)
	if len(got.Provenance) != 1 || got.Provenance[0].EventUID != "ep1" || got.Provenance[0].Source != "cli:test" {
		t.Fatalf("get(entities/alice) provenance = %s, want [ep1/cli:test] via the citing episode", getOut)
	}

	// Step 2: relations(entities/alice, reports_to) returns the Sam edge with
	// hydrated endpoint names + the object resource name to pivot on.
	relOut := invokeOK(t, ctx, tools.NewRelations(nav), `{"entity":"entities/alice","predicates":["reports_to"]}`)
	var rels struct {
		Relations []struct {
			Object      string `json:"object"`
			ObjectName  string `json:"objectName"`
			Predicate   string `json:"predicate"`
			SubjectName string `json:"subjectName"`
		} `json:"relations"`
	}
	mustJSON(t, relOut, &rels)
	if len(rels.Relations) != 1 || rels.Relations[0].Object != "entities/bob" || rels.Relations[0].Predicate != "reports_to" {
		t.Fatalf("relations(alice, reports_to) = %s, want one reports_to -> entities/bob", relOut)
	}

	// Step 3: neighbors(entities/alice, depth=2) reaches Mithril via Sam.
	nbOut := invokeOK(t, ctx, tools.NewNeighbors(nav), `{"seed":"entities/alice","depth":2}`)
	var nbs struct {
		Neighbors []struct {
			Entity struct {
				Name string `json:"name"`
			} `json:"entity"`
			Hops int `json:"hops"`
		} `json:"neighbors"`
	}
	mustJSON(t, nbOut, &nbs)
	reached := map[string]int{}
	for _, n := range nbs.Neighbors {
		reached[n.Entity.Name] = n.Hops
	}
	if reached["entities/bob"] != 1 || reached["entities/zurich"] != 1 {
		t.Fatalf("depth-2 neighbors = %s, want bob+zurich at hop 1", nbOut)
	}
	if reached["entities/mithril"] != 2 {
		t.Fatalf("depth-2 neighbors = %s, want mithril at hop 2 (via Sam)", nbOut)
	}

	// Step 4: entities(kind=person) discovers anchors by kind. self (bootstrap)
	// + Merry + Sam are persons.
	entOut := invokeOK(t, ctx, tools.NewEntities(nav, nil), `{"kind":"person"}`)
	var ents struct {
		Entities []struct {
			Name string `json:"name"`
		} `json:"entities"`
	}
	mustJSON(t, entOut, &ents)
	gotEnt := map[string]bool{}
	for _, e := range ents.Entities {
		gotEnt[e.Name] = true
	}
	if !gotEnt["entities/alice"] || !gotEnt["entities/bob"] {
		t.Fatalf("entities(person) = %s, want alice+bob", entOut)
	}

	// Step 5: introspect surfaces the predicate vocabulary the walk used.
	insOut := invokeOK(t, ctx, tools.NewIntrospect(nav, nil), `{}`)
	var ins struct {
		Predicates []struct {
			Predicate string `json:"predicate"`
		} `json:"predicates"`
		EntityKinds []struct {
			Kind string `json:"kind"`
		} `json:"entityKinds"`
	}
	mustJSON(t, insOut, &ins)
	preds := map[string]bool{}
	for _, p := range ins.Predicates {
		preds[p.Predicate] = true
	}
	// introspect lists *registered* predicates (the migration-seeded vocabulary),
	// not every predicate observed on an edge — reports_to is a valid edge but
	// not a seeded predicate, so assert on the canonical ones the graph uses.
	if !preds["lives_in"] || !preds["works_at"] {
		t.Fatalf("introspect predicates missing lives_in/works_at: %s", insOut)
	}
}

// TestE2E_GetEntityBitemporalFacts pins feature #2: get threads --at / --as-of /
// --include-inactive into the entity's fact SUB-QUERY only. The identity row is
// unchanged across anchors; only facts[] time-travels. Drives the real get tool
// over a migrated SQLite projection so the anchor genuinely reaches the SQL.
func TestE2E_GetEntityBitemporalFacts(t *testing.T) {
	t.Parallel()
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
	now := time.Now().UTC()
	from72 := now.Add(-72 * time.Hour).Format(time.RFC3339Nano)
	until24 := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	recordedNow := now.Format(time.RFC3339Nano)
	exec(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`,
		"dave", "person", "Dave", 1.0, "confirmed", from72, "personal")
	fact := `INSERT INTO facts
		(uid, subject_uid, state_key, value, event_start, event_end, create_time,
		 source_event_uid, content_hash, superseded_by_uid, source_priority, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	// state.role: Engineer over a window that closed 24h ago (ended, not
	// superseded), then Manager open since — recorded just now.
	exec(fact, "f_eng", "dave", "state.role", "Engineer", from72, until24, from72, "f_eng", "h1", nil, 1.0, "personal")
	exec(fact, "f_mgr", "dave", "state.role", "Manager", until24, nil, recordedNow, "f_mgr", "h2", nil, 1.0, "personal")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	get := tools.NewGet(nav)

	// roles returns stateKey->value for the state.role slot(s) of a get response,
	// and the entity displayName so the identity row can be asserted unchanged.
	roles := func(raw json.RawMessage) (name string, vals map[string]string) {
		t.Helper()
		var got struct {
			Entity struct {
				DisplayName string `json:"displayName"`
			} `json:"entity"`
			Facts []struct {
				StateKey string `json:"stateKey"`
				Value    string `json:"value"`
			} `json:"facts"`
		}
		mustJSON(t, raw, &got)
		vals = map[string]string{}
		for _, f := range got.Facts {
			if f.StateKey == "state.role" {
				vals[f.Value] = f.Value
			}
		}
		return got.Entity.DisplayName, vals
	}

	// Default (now): only the open Manager value of the role slot.
	name, vals := roles(invokeOK(t, ctx, get, `{"name":"entities/dave"}`))
	if name != "Dave" {
		t.Fatalf("default get displayName = %q, want Dave", name)
	}
	if _, ok := vals["Manager"]; !ok || vals["Engineer"] != "" {
		t.Fatalf("default role facts = %v, want only Manager", vals)
	}

	// --at inside the Engineer window: the role reads Engineer, Manager is absent,
	// and the identity row is unchanged.
	atBefore := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	name, vals = roles(invokeOK(t, ctx, get, `{"name":"entities/dave","at":"`+atBefore+`"}`))
	if name != "Dave" {
		t.Fatalf("--at before-close displayName = %q, want Dave (identity row must not move)", name)
	}
	if _, ok := vals["Engineer"]; !ok || vals["Manager"] != "" {
		t.Fatalf("--at before-close role facts = %v, want only Engineer", vals)
	}

	// --at after the window closed: Engineer is gone, Manager is the open value.
	atAfter := now.Add(time.Hour).Format(time.RFC3339Nano)
	_, vals = roles(invokeOK(t, ctx, get, `{"name":"entities/dave","at":"`+atAfter+`"}`))
	if _, ok := vals["Manager"]; !ok || vals["Engineer"] != "" {
		t.Fatalf("--at after-close role facts = %v, want only Manager", vals)
	}

	// --as-of before the Manager fact was recorded: it is hidden.
	asOf := now.Add(-12 * time.Hour).Format(time.RFC3339Nano)
	_, vals = roles(invokeOK(t, ctx, get, `{"name":"entities/dave","asOf":"`+asOf+`"}`))
	if vals["Manager"] != "" {
		t.Fatalf("--as-of before-record role facts = %v, want Manager hidden (recorded later)", vals)
	}

	// --include-inactive: the full role history (Engineer + Manager).
	_, vals = roles(invokeOK(t, ctx, get, `{"name":"entities/dave","includeInactive":true}`))
	if _, ok := vals["Engineer"]; !ok {
		t.Fatalf("--include-inactive role facts = %v, want Engineer present (history)", vals)
	}
	if _, ok := vals["Manager"]; !ok {
		t.Fatalf("--include-inactive role facts = %v, want Manager present (history)", vals)
	}
}

// e2eSelfBinder is a bound self->principal binder for the e2e get tests. Its
// SelfPrincipal method is exported, so this external-package type satisfies the
// tool's unexported selfBinder interface passed to SetSelfBinder.
type e2eSelfBinder struct{ uid string }

func (b e2eSelfBinder) SelfPrincipal(context.Context) (string, bool) { return b.uid, true }

// TestE2E_SelfPrincipalTeamRead pins the self-identity read fix (#213): the
// reader's own principal node resolves under a team-only read even though the
// principal lives in personal scope (the device-local private scope, always the
// reader's own — never a cross-scope leak), while its FACTS stay filtered to the
// requested scope. So `fact get self --scope team` returns the reader's
// team-shared facts and nothing personal, and `--scope personal` returns the
// personal facts and nothing team. The widening is gated on the entity being the
// reader's own principal, so a non-self personal entity stays fail-closed under
// a team-only read.
func TestE2E_SelfPrincipalTeamRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	const principal = "me_principal"
	const other = "other_person"
	// Both principal + other live in personal scope. 'other' is a non-self
	// personal entity used to prove the node widening is strictly self-gated.
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, principal, "person", "Me", 1.0, "confirmed", now, "personal")
	exec(ent, other, "person", "Someone", 1.0, "confirmed", now, "personal")

	fact := `INSERT INTO facts
		(uid, subject_uid, state_key, value, event_start, event_end, create_time,
		 source_event_uid, content_hash, superseded_by_uid, source_priority, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	// One team-shared fact + one personal fact on the principal (both open).
	exec(fact, "f_team", principal, "state.team.note", "hello team", nil, nil, now, "f_team", "h_team", nil, 0.5, "team")
	exec(fact, "f_pers", principal, "preference.coffee", "oat", nil, nil, now, "f_pers", "h_pers", nil, 0.5, "personal")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	get := tools.NewGet(nav)
	get.SetSelfBinder(e2eSelfBinder{uid: principal})

	// 1) self read, team scope: node resolves (personal-scoped principal), only
	//    the team fact surfaces — facts stay filtered to the requested scope.
	out := invokeOK(t, ctx, get, `{"name":"entities/self","scopes":["team"]}`)
	assertSelfFacts(t, out, "Me", map[string]string{"state.team.note": "hello team"})

	// 2) self read, personal scope: node resolves, only the personal fact.
	out = invokeOK(t, ctx, get, `{"name":"entities/self","scopes":["personal"]}`)
	assertSelfFacts(t, out, "Me", map[string]string{"preference.coffee": "oat"})

	// 3) boundary: a non-self personal entity is NOT widened — a team-only read
	//    returns E_NOT_FOUND, so the fail-closed boundary holds for everyone else.
	if _, derr := get.Invoke(ctx, json.RawMessage(`{"name":"entities/`+other+`","scopes":["team"]}`)); derr == nil {
		t.Fatalf("get(non-self personal entity, --scope team) = ok, want E_NOT_FOUND (boundary must stay closed)")
	}

	// 4) relations: the self seed resolves under a team-only read too — the team
	//    edge surfaces while the principal node stays personal-scoped
	//    (resolveEntityArg's own-principal short-circuit). The seed is passed as
	//    the bare principal uid (the form `relations self` resolves to via the
	//    CLI), exercising the bare-token path the fix targets.
	exec(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes, scope) VALUES (?,?,?,?,?,?,?,?,?)`,
		"r_team", principal, "relates_to", other, 1.0, "ep_x", now, "{}", "team")
	rels := tools.NewRelations(nav)
	rels.SetSelfBinder(e2eSelfBinder{uid: principal})
	relOut := invokeOK(t, ctx, rels, `{"entity":"`+principal+`","scopes":["team"]}`)
	var rel struct {
		Relations []struct {
			Predicate string `json:"predicate"`
		} `json:"relations"`
	}
	mustJSON(t, relOut, &rel)
	if len(rel.Relations) != 1 || rel.Relations[0].Predicate != "relates_to" {
		t.Fatalf("relations(self, --scope team) = %s, want one relates_to edge", relOut)
	}

	// 5) relations boundary: a non-self personal entity stays fail-closed under a
	//    team-only read (no own-principal short-circuit for it).
	if _, derr := rels.Invoke(ctx, json.RawMessage(`{"entity":"`+other+`","scopes":["team"]}`)); derr == nil {
		t.Fatalf("relations(non-self personal entity, --scope team) = ok, want E_NOT_FOUND")
	}
}

// assertSelfFacts checks an entity get response carries the expected display
// name and exactly the expected (stateKey -> value) open facts.
func assertSelfFacts(t *testing.T, raw json.RawMessage, wantName string, wantFacts map[string]string) {
	t.Helper()
	var got struct {
		Entity struct {
			DisplayName string `json:"displayName"`
		} `json:"entity"`
		Facts []struct {
			StateKey string `json:"stateKey"`
			Value    string `json:"value"`
		} `json:"facts"`
	}
	mustJSON(t, raw, &got)
	if got.Entity.DisplayName != wantName {
		t.Fatalf("entity displayName = %q, want %q (%s)", got.Entity.DisplayName, wantName, raw)
	}
	if len(got.Facts) != len(wantFacts) {
		t.Fatalf("facts = %d, want %d (%s)", len(got.Facts), len(wantFacts), raw)
	}
	for _, f := range got.Facts {
		if wantFacts[f.StateKey] != f.Value {
			t.Fatalf("unexpected fact %q=%q (%s)", f.StateKey, f.Value, raw)
		}
	}
}

// invokeOK invokes tool with raw JSON args and fails on any error, returning
// the raw response body.
func invokeOK(t *testing.T, ctx context.Context, tool interface {
	Invoke(context.Context, json.RawMessage) (json.RawMessage, error)
}, args string,
) json.RawMessage {
	t.Helper()
	out, err := tool.Invoke(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("invoke %T(%s): %v", tool, args, err)
	}
	return out
}

func mustJSON(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func jsonField(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	var m map[string]json.RawMessage
	mustJSON(t, raw, &m)
	var s string
	if err := json.Unmarshal(m[key], &s); err != nil {
		t.Fatalf("field %q in %s: %v", key, raw, err)
	}
	return s
}
