//go:build integration

package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// entity_dedup_e2e_test.go is the integration proof for the entity-dedup wave:
// the create-time dedup gate and the merge-time edge dedup work end-to-end over
// a real orchestrator (event log -> synchronous projection -> SQLite), not just
// against unit fakes. It drives the actual MCP tools (createEntity, relate,
// entityMerged) wired to the same store, so it also pins the read-after-write
// guarantee the dedup gate depends on: an entity minted by one createEntity call
// is visible to the matcher on the next call.

// newEntityE2EOrchestrator builds an orchestrator wired with the schemas the
// entity-lifecycle tools emit (entity, identifier, relation, merge, delete,
// relation-forgotten). Registers a cleanup to close it.
func newEntityE2EOrchestrator(t *testing.T, ctx context.Context, dataDir string, emb embedder.Embedder) *orchestrator.Orchestrator {
	t.Helper()
	ont := kernel.NewMemoryOntology()
	regs := []func(context.Context, kernel.Ontology) error{
		ontology.RegisterEntity,
		ontology.RegisterEntityIdentifierAdded,
		ontology.RegisterRelation,
		ontology.RegisterEntityMerged,
		ontology.RegisterEntityDeleted,
		ontology.RegisterRelationForgotten,
	}
	for _, reg := range regs {
		if err := reg(ctx, ont); err != nil {
			t.Fatalf("register schema: %v", err)
		}
	}
	orch, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  dataDir,
		Ontology: ont,
		Embedder: emb,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	t.Cleanup(func() {
		if cerr := orch.Close(); cerr != nil {
			t.Logf("orch.Close: %v", cerr)
		}
	})
	return orch
}

// e2eCreateOutput mirrors the createEntity tool's output shape.
type e2eCreateOutput struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Candidates []struct {
		UID        string  `json:"uid"`
		Similarity float64 `json:"similarity"`
		Reason     string  `json:"reason"`
	} `json:"candidates"`
}

// invokeCreate calls createEntity.Invoke with args and decodes the output. A
// tool error fails the test.
func invokeCreate(t *testing.T, ctx context.Context, tool *tools.CreateEntity, args map[string]any) e2eCreateOutput {
	t.Helper()
	req, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal create args: %v", err)
	}
	raw, err := tool.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("createEntity.Invoke(%v): %v", args, err)
	}
	var out e2eCreateOutput
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode create output: %v", uerr)
	}
	return out
}

// assertEntityCount asserts the number of entity rows with the given kind and
// display name (case-sensitive, as minted).
func assertEntityCount(t *testing.T, ctx context.Context, reader *sql.DB, kind, name string, want int) {
	t.Helper()
	var n int
	if err := reader.QueryRowContext(ctx,
		`SELECT count(*) FROM entities WHERE kind = ? AND display_name = ?`, kind, name,
	).Scan(&n); err != nil {
		t.Fatalf("count entities (%s %q): %v", kind, name, err)
	}
	if n != want {
		t.Fatalf("entity count for (%s %q) = %d, want %d", kind, name, n, want)
	}
}

// TestE2E_EntityDedup_CreateRefusesDuplicate proves the create-time gate over a
// real orchestrator: a second createEntity with the same kind+name is refused
// (status duplicate_candidates, no second row), while force=true mints a genuine
// homonym. The second call reading the first call's freshly-projected entity
// also pins read-after-write through the materialising event log.
func TestE2E_EntityDedup_CreateRefusesDuplicate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newEntityE2EOrchestrator(t, ctx, dataDir, hashEmbedder{})
	reader, err := orch.Storage.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	// nil vec/embedder: the exact-name lane (all this test needs) works without
	// the vector lane, which degrades to a no-op.
	matcher := entitymatch.NewLookup(reader, nil, nil, 0, 0)
	createTool := tools.NewCreateEntity(orch.Log, nil).WithDedup(matcher)

	const name = "Constantin von Zitzewitz"

	out1 := invokeCreate(t, ctx, createTool, map[string]any{"kind": "person", "displayName": name})
	if out1.Status != "created" {
		t.Fatalf("first create status = %q, want created", out1.Status)
	}
	assertEntityCount(t, ctx, reader, "person", name, 1)

	out2 := invokeCreate(t, ctx, createTool, map[string]any{"kind": "person", "displayName": name})
	if out2.Status != "duplicate_candidates" {
		t.Fatalf("second create status = %q, want duplicate_candidates", out2.Status)
	}
	if len(out2.Candidates) == 0 || out2.Candidates[0].UID != out1.UID {
		t.Fatalf("expected candidate uid %q, got %+v", out1.UID, out2.Candidates)
	}
	if out2.UID != "" {
		t.Fatalf("refused create must not return a new uid, got %q", out2.UID)
	}
	// Still exactly one entity — the refusal did not write.
	assertEntityCount(t, ctx, reader, "person", name, 1)

	// force mints a genuine homonym (a second real person of the same name).
	out3 := invokeCreate(t, ctx, createTool, map[string]any{"kind": "person", "displayName": name, "force": true})
	if out3.Status != "created" {
		t.Fatalf("forced create status = %q, want created", out3.Status)
	}
	assertEntityCount(t, ctx, reader, "person", name, 2)
}

// TestE2E_EntityDedup_MergeRemovesDuplicateEdges proves the merge-time edge
// dedup over the real tool path: two entities each holding the same edge to a
// third collapse to a single active edge after entityMerged, with no duplicate
// left behind.
func TestE2E_EntityDedup_MergeRemovesDuplicateEdges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newEntityE2EOrchestrator(t, ctx, dataDir, hashEmbedder{})
	reader, err := orch.Storage.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	createTool := tools.NewCreateEntity(orch.Log, nil)
	relateTool := tools.NewRelate(orch.Log, nav)
	mergeTool := tools.NewMergeEntity(orch.Log, nav)

	// Two duplicate orgs (Erebor / Erebor Trading Company) and a shared partner.
	aGlobal := invokeCreate(t, ctx, createTool, map[string]any{"kind": "organization", "displayName": "Erebor"})
	aTech := invokeCreate(t, ctx, createTool, map[string]any{"kind": "organization", "displayName": "Erebor Trading Company"})
	partner := invokeCreate(t, ctx, createTool, map[string]any{"kind": "organization", "displayName": "Rivendell"})

	relate := func(subj, obj string) {
		t.Helper()
		req, _ := json.Marshal(map[string]any{"subject": subj, "predicate": "knows", "object": obj})
		if _, rerr := relateTool.Invoke(ctx, req); rerr != nil {
			t.Fatalf("relate %s knows %s: %v", subj, obj, rerr)
		}
	}
	// Both duplicates carry the SAME edge to the partner.
	relate(aGlobal.UID, partner.UID)
	relate(aTech.UID, partner.UID)

	// Merge Erebor -> Erebor Trading Company.
	mergeReq, _ := json.Marshal(map[string]any{
		"sourceEntityUid": aGlobal.UID,
		"targetEntityUid": aTech.UID,
	})
	if _, merr := mergeTool.Invoke(ctx, mergeReq); merr != nil {
		t.Fatalf("entityMerged: %v", merr)
	}

	// Exactly one active (aTech, knows, partner) edge survives.
	var n int
	if err := reader.QueryRowContext(ctx,
		`SELECT count(*) FROM relations
		  WHERE subject_uid = ? AND predicate = 'knows' AND object_uid = ?
		    AND superseded_by_uid IS NULL`,
		aTech.UID, partner.UID,
	).Scan(&n); err != nil {
		t.Fatalf("count active edges: %v", err)
	}
	if n != 1 {
		t.Fatalf("active (Erebor Trading Company, knows, Rivendell) edges = %d, want 1 (merge must dedup)", n)
	}

	// And the absorbed source is gone.
	var srcRows int
	if err := reader.QueryRowContext(ctx,
		`SELECT count(*) FROM entities WHERE uid = ?`, aGlobal.UID,
	).Scan(&srcRows); err != nil {
		t.Fatalf("count source entity: %v", err)
	}
	if srcRows != 0 {
		t.Fatalf("merged source entity still present (%d rows)", srcRows)
	}
}
