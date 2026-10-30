package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestTypedRelations_WritePath_StrictVsLax is the P5 capstone: a relation whose
// subject kind falls outside the predicate's declared domain is rejected on the
// write path when strict_domain_range = true, and warn-accepted (Append
// succeeds) when it is false (the default). A conforming relation is accepted in
// both modes. This exercises the full FileEventLog.prepareEvent →
// checkRelationDomainRange → SQLiteOntology.ValidateRelation path.
func TestTypedRelations_WritePath_StrictVsLax(t *testing.T) {
	t.Parallel()

	// Stable ULIDs for the three seeded entities.
	alice := ulid.Make().String()  // person
	erebor := ulid.Make().String() // organization
	berlin := ulid.Make().String() // place

	buildLog := func(t *testing.T, strict bool) kernel.EventLog {
		t.Helper()
		ctx := context.Background()
		store, err := sqlite.Open(ctx, t.TempDir()+"/index.db")
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })

		ont, err := kernel.NewSQLiteOntology(store.Reader())
		if err != nil {
			t.Fatalf("NewSQLiteOntology: %v", err)
		}
		if err := ontology.RegisterRelation(ctx, ont); err != nil {
			t.Fatalf("register relation: %v", err)
		}

		// Seed the typed predicate: employed_by accepts person→organization only.
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := store.Writer().ExecContext(ctx,
			`INSERT INTO predicates
			   (predicate, kind, description, added_at, added_by, subject_kinds, object_kinds)
			   VALUES ('employed_by','non-functional','', ?, 'user', '["person"]', '["organization"]')`, now,
		); err != nil {
			t.Fatalf("seed predicate: %v", err)
		}
		// Seed the three endpoint entities.
		for _, e := range []struct{ uid, kind string }{
			{alice, "person"}, {erebor, "organization"}, {berlin, "place"},
		} {
			if _, err := store.Writer().ExecContext(ctx,
				`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status)
				   VALUES (?, ?, ?, 1.0, 'confirmed')`, e.uid, e.kind, e.kind,
			); err != nil {
				t.Fatalf("seed entity %s: %v", e.kind, err)
			}
		}

		opts := []kernel.Option{kernel.WithStrictDomainRange(strict)}
		return kernel.NewFileEventLog(t.TempDir(), ont, opts...)
	}

	makeRelationEvent := func(subjectUID, objectUID string) kernel.Event {
		uid := ulid.Make().String()
		payload, _ := json.Marshal(map[string]any{
			"name":           "relations/" + uid,
			"uid":            uid,
			"subjectUid":     subjectUID,
			"predicate":      "employed_by",
			"objectUid":      objectUID,
			"confidence":     1.0,
			"sourceEventUid": ulid.Make().String(),
			"createTime":     time.Now().UTC().Format(time.RFC3339),
		})
		return kernel.Event{
			Kind:       ontology.KindRelation,
			Source:     "test",
			Confidence: 1.0,
			Payload:    payload,
		}
	}

	t.Run("out of domain rejected when strict", func(t *testing.T) {
		t.Parallel()
		log := buildLog(t, true)
		ev := makeRelationEvent(berlin, erebor) // place employed_by organization
		_, err := log.Append(context.Background(), []kernel.Event{ev})
		if !errors.Is(err, kernel.ErrInvalidEvent) {
			t.Errorf("expected ErrInvalidEvent, got: %v", err)
		}
	})

	t.Run("out of domain warn accepted when lax", func(t *testing.T) {
		t.Parallel()
		log := buildLog(t, false)
		ev := makeRelationEvent(berlin, erebor)
		if _, err := log.Append(context.Background(), []kernel.Event{ev}); err != nil {
			t.Errorf("expected nil (warn-accept), got: %v", err)
		}
	})

	t.Run("conforming relation accepted when strict", func(t *testing.T) {
		t.Parallel()
		log := buildLog(t, true)
		ev := makeRelationEvent(alice, erebor) // person employed_by organization
		if _, err := log.Append(context.Background(), []kernel.Event{ev}); err != nil {
			t.Errorf("expected nil for conforming, got: %v", err)
		}
	})
}
