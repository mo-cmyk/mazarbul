package doctor

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// seedEntities opens (and migrates) the index.db under dir and inserts the
// given entity rows. Each row is (uid, kind, displayName).
func seedEntities(t *testing.T, dir string, rows []struct{ uid, kind, displayName string }) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqliteIndexPath(dir))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	for _, r := range rows {
		if _, err := store.Writer().ExecContext(ctx,
			`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status)
			 VALUES (?, ?, ?, ?, ?)`,
			r.uid, r.kind, r.displayName, 1.0, "resolved",
		); err != nil {
			t.Fatalf("insert entity %q: %v", r.uid, err)
		}
	}
}

// seedRelations opens (and migrates) the index.db under dir and inserts the
// given relation rows. supersededByUID may be empty string (NULL).
func seedRelations(t *testing.T, dir string, rows []struct {
	uid, subjectUID, predicate, objectUID, status, supersededByUID string
},
) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqliteIndexPath(dir))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	for _, r := range rows {
		var supersededByUID any
		if r.supersededByUID != "" {
			supersededByUID = r.supersededByUID
		}
		if _, err := store.Writer().ExecContext(ctx,
			`INSERT INTO relations
			   (uid, subject_uid, predicate, object_uid, confidence, source_event_uid,
			    create_time, superseded_by_uid, provenance, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.uid, r.subjectUID, r.predicate, r.objectUID,
			1.0, "EVTSOURCE0000000000000000",
			"2026-06-26T00:00:00Z",
			supersededByUID,
			"",
			r.status,
		); err != nil {
			t.Fatalf("insert relation %q: %v", r.uid, err)
		}
	}
}

// TestDuplicateEntities_WARN seeds two entities that share the same kind and a
// case-insensitive display name and asserts StatusWarn.
func TestDuplicateEntities_WARN(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEntities(t, dir, []struct{ uid, kind, displayName string }{
		{"01ENTAAAAAAAAAAAAAAAAAAAAA", "organisation", "Acme"},
		{"01ENTBBBBBBBBBBBBBBBBBBBBB", "organisation", "acme"},
	})
	status, detail, err := newDuplicateEntitiesCheck(newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v (%s), want WARN", status, detail)
	}
}

// TestDuplicateEntities_OK seeds two entities with distinct names and asserts
// StatusOK.
func TestDuplicateEntities_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEntities(t, dir, []struct{ uid, kind, displayName string }{
		{"01ENTAAAAAAAAAAAAAAAAAAAAA", "organisation", "Acme Corp"},
		{"01ENTBBBBBBBBBBBBBBBBBBBBB", "organisation", "Beta Inc"},
	})
	status, _, err := newDuplicateEntitiesCheck(newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK", status)
	}
}

// TestDuplicateActiveEdges_WARN seeds two relations with an identical
// (subject_uid, predicate, object_uid, status) triple and superseded_by_uid
// NULL and asserts StatusWarn.
func TestDuplicateActiveEdges_WARN(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedRelations(t, dir, []struct {
		uid, subjectUID, predicate, objectUID, status, supersededByUID string
	}{
		{
			uid: "01RELAAAAAAAAAAAAAAAAAAAAA", subjectUID: "01ENTAAAAAAAAAAAAAAAAAAAAA",
			predicate: "works_at", objectUID: "01ENTBBBBBBBBBBBBBBBBBBBBB",
			status: "asserted", supersededByUID: "",
		},
		{
			uid: "01RELCCCCCCCCCCCCCCCCCCCCC", subjectUID: "01ENTAAAAAAAAAAAAAAAAAAAAA",
			predicate: "works_at", objectUID: "01ENTBBBBBBBBBBBBBBBBBBBBB",
			status: "asserted", supersededByUID: "",
		},
	})
	status, detail, err := newDuplicateActiveEdgesCheck(newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v (%s), want WARN", status, detail)
	}
}

// TestDuplicateActiveEdges_OK_WhenSuperseded seeds two identical triples but
// one has superseded_by_uid set. Only one active row remains, so StatusOK.
func TestDuplicateActiveEdges_OK_WhenSuperseded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedRelations(t, dir, []struct {
		uid, subjectUID, predicate, objectUID, status, supersededByUID string
	}{
		{
			uid: "01RELAAAAAAAAAAAAAAAAAAAAA", subjectUID: "01ENTAAAAAAAAAAAAAAAAAAAAA",
			predicate: "works_at", objectUID: "01ENTBBBBBBBBBBBBBBBBBBBBB",
			status: "asserted", supersededByUID: "01RELCCCCCCCCCCCCCCCCCCCCC",
		},
		{
			uid: "01RELCCCCCCCCCCCCCCCCCCCCC", subjectUID: "01ENTAAAAAAAAAAAAAAAAAAAAA",
			predicate: "works_at", objectUID: "01ENTBBBBBBBBBBBBBBBBBBBBB",
			status: "asserted", supersededByUID: "",
		},
	})
	status, _, err := newDuplicateActiveEdgesCheck(newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (superseded row must not count as active duplicate)", status)
	}
}

// TestDuplicateActiveEdges_OK_WhenStatusDiffers seeds two rows with the same
// triple but different statuses (asserted vs proposed). Status is part of the
// dedup key, so they are not duplicates → StatusOK.
func TestDuplicateActiveEdges_OK_WhenStatusDiffers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedRelations(t, dir, []struct {
		uid, subjectUID, predicate, objectUID, status, supersededByUID string
	}{
		{
			uid: "01RELAAAAAAAAAAAAAAAAAAAAA", subjectUID: "01ENTAAAAAAAAAAAAAAAAAAAAA",
			predicate: "works_at", objectUID: "01ENTBBBBBBBBBBBBBBBBBBBBB",
			status: "asserted", supersededByUID: "",
		},
		{
			uid: "01RELCCCCCCCCCCCCCCCCCCCCC", subjectUID: "01ENTAAAAAAAAAAAAAAAAAAAAA",
			predicate: "works_at", objectUID: "01ENTBBBBBBBBBBBBBBBBBBBBB",
			status: "proposed", supersededByUID: "",
		},
	})
	status, _, err := newDuplicateActiveEdgesCheck(newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (differing status must not be a duplicate)", status)
	}
}

// TestDataQuality_CleanGraph_BothOK verifies that a store with no entities or
// relations reports StatusOK for both checks.
func TestDataQuality_CleanGraph_BothOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Open and migrate the database without seeding any data.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqliteIndexPath(dir))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if cerr := store.Close(); cerr != nil {
		t.Fatalf("close sqlite: %v", cerr)
	}

	shared := newSharedFor(dir)

	entStatus, _, entErr := newDuplicateEntitiesCheck(shared).Fn(ctx)
	if entErr != nil {
		t.Fatalf("duplicate entities err: %v", entErr)
	}
	if entStatus != StatusOK {
		t.Errorf("duplicate entities status = %v, want OK on clean graph", entStatus)
	}

	edgeStatus, _, edgeErr := newDuplicateActiveEdgesCheck(shared).Fn(ctx)
	if edgeErr != nil {
		t.Fatalf("duplicate active edges err: %v", edgeErr)
	}
	if edgeStatus != StatusOK {
		t.Errorf("duplicate active edges status = %v, want OK on clean graph", edgeStatus)
	}
}
