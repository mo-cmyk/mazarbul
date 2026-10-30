package render_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// TestRender_RelationRetracted_ConvergesRegardlessOfOrder pins the git
// projection's half of the #205 guarantee: closing the same relation arm with
// two different retractions must render byte-identical state regardless of the
// order the relationRetracted events replay in, so two paired devices never
// produce a churny git diff. The render fold uses the same
// ontology.ReduceRetractions reduction as the kernel dispatcher.
func TestRender_RelationRetracted_ConvergesRegardlessOfOrder(t *testing.T) {
	t.Parallel()

	const (
		subjectUID = "01HRENDRET0000000000SUBJ01"
		objectUID  = "01HRENDRET00000000000OBJ01"
		relUID     = "01HRENDRET00000000000REL01"
	)
	low := ulid.Make().String()
	high := ulid.Make().String()
	if high < low {
		low, high = high, low
	}
	earliest := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Cross superseder ULID against retraction time so the two folded columns
	// must reduce independently (lowest ULID; earliest time).
	retLow := makeEvent("01HRENDRET00000000EVENTLO01", ontology.KindRelationRetracted,
		ontology.RelationRetractedEvent{
			RelationUID:     ontology.ULID(relUID),
			SupersededByUID: ontology.ULID(low),
			RetractedAt:     later,
		})
	retHigh := makeEvent("01HRENDRET00000000EVENTHI01", ontology.KindRelationRetracted,
		ontology.RelationRetractedEvent{
			RelationUID:     ontology.ULID(relUID),
			SupersededByUID: ontology.ULID(high),
			RetractedAt:     earliest,
		})

	base := []kernel.Event{
		makeEvent(subjectUID, ontology.KindEntity, entityPayload(subjectUID, "Subject", ontology.EntityKindPerson)),
		makeEvent(objectUID, ontology.KindEntity, entityPayload(objectUID, "Object", ontology.EntityKindOrganization)),
		makeEvent(relUID, ontology.KindRelation, relationPayload(relUID, subjectUID, "works_at", objectUID)),
	}

	renderSuperseded := func(order []kernel.Event) []byte {
		events := append(append([]kernel.Event(nil), base...), order...)
		view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		f := fileByPath(view.Files, "rendered/superseded-relations.jsonl")
		if f == nil {
			t.Fatal("rendered/superseded-relations.jsonl not found")
			return nil
		}
		return f.Content
	}

	a := renderSuperseded([]kernel.Event{retLow, retHigh})
	b := renderSuperseded([]kernel.Event{retHigh, retLow})

	if !bytes.Equal(a, b) {
		t.Fatalf("render diverged across replay order:\n  A: %s\n  B: %s", a, b)
	}
	// The converged row must carry the lowest-ULID superseder and the earliest
	// close, and must be excluded from the current relations file.
	if !bytes.Contains(a, []byte(low)) {
		t.Errorf("superseded-relations.jsonl missing lowest-ULID superseder %q; got:\n%s", low, a)
	}
}

// TestRender_RelationRetracted_BeforeRelation pins the render side of replay
// hazard #1: a relationRetracted that arrives BEFORE the relation event it
// closes must still produce a superseded row. The render fold buffers the
// tombstone in pendingRetractions until applyRelation lands the row, then
// drains + folds it — mirroring the kernel dispatcher's recompute-after-upsert.
// Without the pending buffer the early retraction would be silently dropped and
// the arm would render as still-current, churning the git diff once the paired
// device replays in the causal order.
func TestRender_RelationRetracted_BeforeRelation(t *testing.T) {
	t.Parallel()

	const (
		subjectUID = "01HRENDBEFORE000000000SUBJ"
		objectUID  = "01HRENDBEFORE0000000000OBJ"
		relUID     = "01HRENDBEFORE0000000000REL"
		superseder = "01HRENDBEFORE00000000SUPER"
	)
	retractedAt := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	// Order: entities, then the RETRACTION, then the relation it closes — the
	// retraction is processed while the row is absent (pending-buffer path).
	events := []kernel.Event{
		makeEvent(subjectUID, ontology.KindEntity, entityPayload(subjectUID, "Subject", ontology.EntityKindPerson)),
		makeEvent(objectUID, ontology.KindEntity, entityPayload(objectUID, "Object", ontology.EntityKindOrganization)),
		makeEvent("01HRENDBEFORE00000000EVENT", ontology.KindRelationRetracted,
			ontology.RelationRetractedEvent{
				RelationUID:     ontology.ULID(relUID),
				SupersededByUID: ontology.ULID(superseder),
				RetractedAt:     retractedAt,
			}),
		makeEvent(relUID, ontology.KindRelation, relationPayload(relUID, subjectUID, "works_at", objectUID)),
	}

	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	sup := fileByPath(view.Files, "rendered/superseded-relations.jsonl")
	if sup == nil {
		t.Fatal("rendered/superseded-relations.jsonl not found")
		return
	}
	if !bytes.Contains(sup.Content, []byte(superseder)) {
		t.Errorf("retraction-before-relation: superseded-relations.jsonl missing superseder %q; the pending buffer dropped the early tombstone. got:\n%s", superseder, sup.Content)
	}

	// The closed arm must NOT linger in the current relations file.
	cur := fileByPath(view.Files, "rendered/relations.jsonl")
	if cur != nil && bytes.Contains(cur.Content, []byte(relUID)) {
		t.Errorf("retraction-before-relation: closed relation %q still rendered as current:\n%s", relUID, cur.Content)
	}
}
