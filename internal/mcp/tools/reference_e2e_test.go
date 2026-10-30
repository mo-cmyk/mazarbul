package tools_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestE2E_ExternalRefReadSurface proves the issue #208 acceptance criterion
// over the REAL stack: an external_ref entity (the canonical URI-keyed node)
// and a has_primary_source edge resolve through a real navigate.Navigator and
// the real MCP read tools (get / entities / relations / neighbors) with ZERO
// read-path change — the same verbs every other entity kind uses.
//
//	decision --has_primary_source--> external_ref(https://github.com/org/repo/issues/42)
func TestE2E_ExternalRefReadSurface(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const uri = "https://github.com/org/repo/issues/42"
	// The external_ref uid is the deterministic, scope-keyed node identity the
	// reference tool would mint — seeding it directly mirrors that projection.
	refUID := string(ontology.ExternalRefUID("project", uri))

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "decision1", "project", "Adopt Postgres", 1.0, "confirmed", now)
	exec(ent, refUID, "external_ref", uri, 1.0, "confirmed", now)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes) VALUES (?,?,?,?,?,?,?,?)`
	exec(rel, "r_grounds", "decision1", "has_primary_source", refUID, 1.0, "ep1", now, `{}`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	// get(entities/<refUID>) hydrates the external_ref like any entity.
	getOut := invokeOK(t, ctx, tools.NewGet(nav), `{"name":"entities/`+refUID+`"}`)
	if typ := jsonField(t, getOut, "type"); typ != "entity" {
		t.Fatalf("get type = %q, want entity (%s)", typ, getOut)
	}
	gotEnt, gerr := nav.GetEntity(ctx, refUID)
	if gerr != nil {
		t.Fatalf("GetEntity(%s): %v", refUID, gerr)
	}
	if gotEnt.Kind != "external_ref" || gotEnt.DisplayName != uri {
		t.Fatalf("external_ref = {kind:%q name:%q}, want {external_ref, %q}", gotEnt.Kind, gotEnt.DisplayName, uri)
	}

	// entities(kind=external_ref) discovers the reference by kind.
	entOut := invokeOK(t, ctx, tools.NewEntities(nav, nil), `{"kind":"external_ref"}`)
	var ents struct {
		Entities []struct {
			Name string `json:"name"`
		} `json:"entities"`
	}
	mustJSON(t, entOut, &ents)
	if len(ents.Entities) != 1 || ents.Entities[0].Name != "entities/"+refUID {
		t.Fatalf("entities(external_ref) = %s, want one entities/%s", entOut, refUID)
	}

	// relations(entities/decision1, has_primary_source) surfaces the edge to
	// the external_ref — the provenance grounding an agent follows.
	relOut := invokeOK(t, ctx, tools.NewRelations(nav),
		`{"entity":"entities/decision1","predicates":["has_primary_source"]}`)
	var rels struct {
		Relations []struct {
			Object    string `json:"object"`
			Predicate string `json:"predicate"`
		} `json:"relations"`
	}
	mustJSON(t, relOut, &rels)
	if len(rels.Relations) != 1 || rels.Relations[0].Object != "entities/"+refUID || rels.Relations[0].Predicate != "has_primary_source" {
		t.Fatalf("relations(decision1, has_primary_source) = %s, want one edge -> entities/%s", relOut, refUID)
	}

	// neighbors(entities/decision1) reaches the external_ref at hop 1.
	nbOut := invokeOK(t, ctx, tools.NewNeighbors(nav), `{"seed":"entities/decision1","depth":1}`)
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
	if reached["entities/"+refUID] != 1 {
		t.Fatalf("neighbors(decision1) = %s, want external_ref at hop 1", nbOut)
	}
}
