package dispatcher_test

// dispatcher_fact_rehomed_test.go verifies the applyFactRehomed dispatcher path:
// the fact's subject_uid moves to ToSubjectUID, both slots are reconciled, and
// replaying the same event a second time is a safe no-op (idempotency).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// applyTestFact appends a single KindFact event with the given subject.
func applyTestFact(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, factUID, subjectUID, stateKey, value string) {
	t.Helper()
	payload, err := json.Marshal(ontology.Fact{
		Name:           "facts/" + factUID,
		UID:            factUID,
		SubjectUID:     subjectUID,
		StateKey:       stateKey,
		Value:          value,
		CreateTime:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceEventUID: ulid.Make().String(),
	})
	if err != nil {
		t.Fatalf("marshal fact: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindFact),
		Source:     "document:import",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("apply fact: %v", err)
	}
}

// queryFactSubject returns the current subject_uid of a fact from SQLite.
func queryFactSubject(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, factUID string) string {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var subjectUID string
	err = reader.QueryRowContext(ctx,
		`SELECT subject_uid FROM facts WHERE uid = ?`, factUID,
	).Scan(&subjectUID)
	if err != nil {
		t.Fatalf("query fact subject: %v", err)
	}
	return subjectUID
}

// queryOpenFactsForSubject returns the count of open facts for a subject.
func queryOpenFactsForSubject(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, subjectUID string) int {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var count int
	if err := reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM facts WHERE subject_uid = ? AND event_end IS NULL`,
		subjectUID,
	).Scan(&count); err != nil {
		t.Fatalf("count open facts: %v", err)
	}
	return count
}

// applyTestFactRehomed appends a factRehomed event moving factUID from
// fromSubjectUID to toSubjectUID.
func applyTestFactRehomed(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, factUID, fromSubjectUID, toSubjectUID string) {
	t.Helper()
	rehomeUID := ulid.Make().String()
	payload, err := json.Marshal(ontology.FactRehomed{
		FactUID:        factUID,
		FromSubjectUID: fromSubjectUID,
		ToSubjectUID:   toSubjectUID,
		CreateTime:     time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		SourceEventUID: ulid.Make().String(),
	})
	if err != nil {
		t.Fatalf("marshal factRehomed: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        rehomeUID,
		Kind:       kernel.EventKind(ontology.KindFactRehomed),
		Source:     "document:sweep",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("apply factRehomed: %v", err)
	}
}

// TestApplyFactRehomed_MovesSubject verifies that applying a factRehomed event
// updates the fact's subject_uid in the facts projection.
func TestApplyFactRehomed_MovesSubject(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := openTestDispatcher(t, ctx)

	factUID := ulid.Make().String()
	docUID := ulid.Make().String()
	subjectUID := ulid.Make().String()

	applyTestFact(t, ctx, d, factUID, docUID, "timeless.decision.ha1b2c3d", "Ship Friday")

	if got := queryFactSubject(t, ctx, d, factUID); got != docUID {
		t.Fatalf("subject before rehome = %q, want %q", got, docUID)
	}

	applyTestFactRehomed(t, ctx, d, factUID, docUID, subjectUID)

	if got := queryFactSubject(t, ctx, d, factUID); got != subjectUID {
		t.Fatalf("subject after rehome = %q, want %q", got, subjectUID)
	}
}

// queryFactContentHash returns the content_hash of a fact row.
func queryFactContentHash(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, factUID string) string {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var hash string
	if err := reader.QueryRowContext(ctx,
		`SELECT content_hash FROM facts WHERE uid = ?`, factUID,
	).Scan(&hash); err != nil {
		t.Fatalf("query fact content_hash: %v", err)
	}
	return hash
}

// TestApplyFactRehomed_UsesObservationHash guards the dedup correctness of the
// rehome path (audit blocker 3): the recomputed content_hash MUST use the shared
// ontology.ObservationContentHash (SHA-256), not factContentHash (MD5), so a
// re-homed fact and a freshly re-imported observation fact land on byte-identical
// hashes and dedup against the same open slot instead of duplicating it.
func TestApplyFactRehomed_UsesObservationHash(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := openTestDispatcher(t, ctx)

	factUID := ulid.Make().String()
	docUID := ulid.Make().String()
	subjectUID := ulid.Make().String()
	const stateKey = "timeless.decision.hc3d4e5f"
	const value = "Adopt the new plan"

	applyTestFact(t, ctx, d, factUID, docUID, stateKey, value)
	applyTestFactRehomed(t, ctx, d, factUID, docUID, subjectUID)

	got := queryFactContentHash(t, ctx, d, factUID)
	want := ontology.ObservationContentHash(subjectUID, stateKey, value)
	if got != want {
		t.Errorf("rehomed content_hash = %q, want ObservationContentHash %q (an MD5/SHA-256 mismatch breaks dedup on the rebuild path)", got, want)
	}
}

// TestApplyFactRehomed_IdempotentOnReplay verifies that applying the same
// factRehomed event twice (replay) is a safe no-op: no error is returned, the
// subject remains on ToSubjectUID, and no extra fact rows are created.
func TestApplyFactRehomed_IdempotentOnReplay(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := openTestDispatcher(t, ctx)

	factUID := ulid.Make().String()
	docUID := ulid.Make().String()
	subjectUID := ulid.Make().String()

	applyTestFact(t, ctx, d, factUID, docUID, "timeless.decision.hb2c3d4e", "Launch Monday")

	// First rehome.
	applyTestFactRehomed(t, ctx, d, factUID, docUID, subjectUID)
	if got := queryFactSubject(t, ctx, d, factUID); got != subjectUID {
		t.Fatalf("subject after first rehome = %q, want %q", got, subjectUID)
	}

	// Second rehome: must be idempotent.
	applyTestFactRehomed(t, ctx, d, factUID, docUID, subjectUID)
	if got := queryFactSubject(t, ctx, d, factUID); got != subjectUID {
		t.Fatalf("subject after second rehome = %q, want %q", got, subjectUID)
	}

	// Exactly one open fact on the new subject; none on the old.
	if got := queryOpenFactsForSubject(t, ctx, d, subjectUID); got != 1 {
		t.Errorf("open facts on subject after double-rehome = %d, want 1", got)
	}
	if got := queryOpenFactsForSubject(t, ctx, d, docUID); got != 0 {
		t.Errorf("open facts on doc after double-rehome = %d, want 0", got)
	}
}

// TestApplyFactRehomed_FactAbsent verifies that applying a factRehomed event
// when the target fact row does not yet exist (cross-device reorder) returns
// no error and is a no-op.
func TestApplyFactRehomed_FactAbsent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := openTestDispatcher(t, ctx)

	// No fact inserted; apply the rehome event against an absent row.
	applyTestFactRehomed(t, ctx, d, ulid.Make().String(), ulid.Make().String(), ulid.Make().String())
	// If we reach here without a fatal, the no-op path works correctly.
}

// openTestDispatcher opens a fresh Dispatcher for a single test, closing it
// via t.Cleanup. Delegates to the existing newDispatcher helper from
// dispatcher_test.go so the embedder wiring stays consistent.
func openTestDispatcher(t *testing.T, ctx context.Context) *dispatcher.Dispatcher {
	t.Helper()
	return newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
}
