package git

// commit_relations_test.go pins the relationRetracted classification in the
// structured commit audit trail. A retraction payload carries only the closed
// relation UID and its replacement (no subject/predicate/object), so the
// generic relationChangeFromPayload would record an empty "~  --[]--> " line
// with zero confidence. relationRetractionFromPayload must instead surface the
// two real IDs so `git log` identifies which arm was closed and what replaced
// it.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestAppendEventsToDetails_ClassifiesRelationRetraction(t *testing.T) {
	const (
		relUID     = "01HRELRETRACTEDARM000000XX"
		superseder = "01HRELREPLACEMENT000000YYY"
	)
	raw, _ := json.Marshal(ontology.RelationRetractedEvent{
		RelationUID:     ontology.ULID(relUID),
		SupersededByUID: ontology.ULID(superseder),
		RetractedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	ev := kernel.Event{
		UID:        "01HRETRACTIONEVENTUID00000",
		Kind:       kernel.EventKind(ontology.KindRelationRetracted),
		Confidence: 1.0,
		Payload:    raw,
	}

	d := &commitDetails{}
	appendEventsToDetails(d, []kernel.Event{ev})

	if len(d.Relations) != 1 {
		t.Fatalf("Relations = %d, want 1", len(d.Relations))
	}
	got := d.Relations[0]
	if got.Action != "~" {
		t.Errorf("Action = %q, want ~", got.Action)
	}
	// The regression: the closed arm and its replacement must be surfaced from
	// relationUid/supersededByUid, not left empty by decoding the wrong shape.
	if got.SubjectUID != relUID {
		t.Errorf("SubjectUID = %q, want the retracted relation UID %q", got.SubjectUID, relUID)
	}
	if got.ObjectUID != superseder {
		t.Errorf("ObjectUID = %q, want the superseder UID %q", got.ObjectUID, superseder)
	}
	if got.Predicate != "superseded_by" {
		t.Errorf("Predicate = %q, want superseded_by", got.Predicate)
	}
	if got.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (from the event envelope)", got.Confidence)
	}

	// The rendered audit line must name both relations, not collapse to empties.
	var b strings.Builder
	writeRelations(&b, d.Relations)
	line := b.String()
	if !strings.Contains(line, relUID) || !strings.Contains(line, superseder) {
		t.Errorf("rendered relations missing retraction IDs:\n%s", line)
	}
}
