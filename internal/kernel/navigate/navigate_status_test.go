package navigate_test

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// TestGetRelationSurfacesTrustStatus pins the #212 read-side label: GetRelation
// returns a relation's trust status verbatim ("proposed" for a connector-
// ingested edge) and reads a legacy/empty stored status back as the
// authoritative "asserted" default.
func TestGetRelationSurfacesTrustStatus(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "subj", "person", "Dana", 1.0, "confirmed", now)
	exec(ent, "obj", "organization", "Zephyr", 1.0, "confirmed", now)

	relStatus := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, status)
	              VALUES (?,?,?,?,?,?,?,?)`
	exec(relStatus, "r_proposed", "subj", "works_at", "obj", 1.0, "ep1", now, "proposed")
	// Legacy row: status column omitted, exercising the empty→asserted default.
	relNoStatus := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time)
	                VALUES (?,?,?,?,?,?,?)`
	exec(relNoStatus, "r_legacy", "subj", "founded", "obj", 1.0, "ep1", now)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	proposed, err := nav.GetRelation(ctx, "r_proposed")
	if err != nil {
		t.Fatalf("GetRelation(proposed): %v", err)
	}
	if proposed.Status != "proposed" {
		t.Errorf("proposed.Status = %q, want proposed", proposed.Status)
	}

	legacy, err := nav.GetRelation(ctx, "r_legacy")
	if err != nil {
		t.Fatalf("GetRelation(legacy): %v", err)
	}
	if legacy.Status != "asserted" {
		t.Errorf("legacy.Status = %q, want asserted (empty stored status defaults to asserted)", legacy.Status)
	}

	// EntityRelations surfaces the same status on a listing row.
	rels, err := nav.EntityRelations(ctx, "subj", navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	got := map[string]string{}
	for i := range rels {
		got[rels[i].UID] = rels[i].Status
	}
	if got["r_proposed"] != "proposed" {
		t.Errorf("EntityRelations r_proposed status = %q, want proposed", got["r_proposed"])
	}
	if got["r_legacy"] != "asserted" {
		t.Errorf("EntityRelations r_legacy status = %q, want asserted", got["r_legacy"])
	}
}
