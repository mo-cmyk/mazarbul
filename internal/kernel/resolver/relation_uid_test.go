package resolver

// relation_uid_test.go pins the contract that appendRelation returns the
// RELATION's own ULID (the payload UID the dispatcher keys the relations row
// on), NOT the event-envelope ULID the log mints on append. processCandidate
// feeds this return value back into appendRelationRetracted as the
// supersededByUid; if it were the event UID, the materialized
// superseded_by_uid would dangle (match no relations row), silently hollowing
// out the #205 convergent-retraction guarantee. Surfaced by live CLI
// inspection: a real FileEventLog assigns the event a fresh ULID distinct from
// the relation payload UID, so the bug only manifests outside fakes that echo
// the payload UID back as the receipt.

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// distinctReceiptLog records appended events and returns a receipt whose UID is
// a fixed value DISTINCT from any payload UID — mirroring the real
// FileEventLog, which mints its own event-envelope ULID on append.
type distinctReceiptLog struct {
	events   []kernel.Event
	eventUID string
}

func (l *distinctReceiptLog) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	l.events = append(l.events, events...)
	receipts := make([]kernel.Receipt, len(events))
	for i := range receipts {
		receipts[i] = kernel.Receipt{UID: l.eventUID}
	}
	return receipts, nil
}

func (l *distinctReceiptLog) Read(context.Context, kernel.ReadOpts) iter.Seq2[kernel.Event, error] {
	return func(func(kernel.Event, error) bool) {}
}

func (l *distinctReceiptLog) At(context.Context, string) (kernel.Event, error) {
	return kernel.Event{}, kernel.ErrNotFound
}

func (l *distinctReceiptLog) Rebuild(context.Context, kernel.Storage, ...kernel.RebuildOption) ([]kernel.ApplyError, error) {
	return nil, nil
}

func (l *distinctReceiptLog) Provenance(context.Context, string) ([]kernel.ProvenanceRecord, error) {
	return nil, nil
}

func TestAppendRelation_ReturnsPayloadUIDNotEventUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const eventUID = "01HEVENTENVELOPE00000000XX"
	log := &distinctReceiptLog{eventUID: eventUID}
	r := &Resolver{eventLog: log, logger: zap.NewNop()}

	got, err := r.appendRelation(ctx, "01HEPISODE0000000000000001", kernel.ScopePersonal,
		"01HSUBJECT0000000000000001", "01HOBJECT00000000000000001",
		ontology.RelationCandidate{Predicate: "works_at"}, 1.0, ontology.RelationStatusAsserted)
	if err != nil {
		t.Fatalf("appendRelation: %v", err)
	}

	// Recover the relation payload UID the dispatcher will key on.
	if len(log.events) != 1 {
		t.Fatalf("expected 1 appended event, got %d", len(log.events))
	}
	var rel ontology.Relation
	if err := json.Unmarshal(log.events[0].Payload, &rel); err != nil {
		t.Fatalf("unmarshal relation payload: %v", err)
	}

	if got == eventUID {
		t.Fatalf("appendRelation returned the event-envelope UID %q; "+
			"it must return the relation payload UID so supersededByUid resolves", got)
	}
	if got != rel.UID {
		t.Errorf("appendRelation returned %q, want relation payload UID %q", got, rel.UID)
	}
	if rel.Name != "relations/"+rel.UID {
		t.Errorf("relation name %q out of sync with payload UID %q", rel.Name, rel.UID)
	}
}
