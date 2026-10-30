package ontology_test

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestFactSchema_HasRequiredFields pins the Fact schema's required-field
// invariant. A drift here (e.g. an accidentally-omitted `required` tag
// on stateKey) would let callers persist Facts without a slot address,
// which collapses the supersede pipeline: (subject_uid, state_key) is
// the natural unique key and a NULL state_key produces ambiguous
// matches.
func TestFactSchema_HasRequiredFields(t *testing.T) {
	t.Parallel()
	s := ontology.FactSchema()

	if s.Kind != ontology.KindFact {
		t.Errorf("Kind = %q, want %q", s.Kind, ontology.KindFact)
	}
	if s.Version != ontology.VersionFact {
		t.Errorf("Version = %q, want %q", s.Version, ontology.VersionFact)
	}

	var doc map[string]any
	if err := json.Unmarshal(s.Definition, &doc); err != nil {
		t.Fatalf("unmarshal schema definition: %v", err)
	}

	required, ok := doc["required"].([]any)
	if !ok {
		t.Fatalf("schema.required missing or wrong type: %T", doc["required"])
	}
	got := make(map[string]bool, len(required))
	for _, r := range required {
		s, _ := r.(string)
		got[s] = true
	}
	for _, want := range []string{"name", "uid", "subjectUid", "stateKey", "value", "createTime", "sourceEventUid"} {
		if !got[want] {
			t.Errorf("required field %q missing from schema", want)
		}
	}
}

// TestStateKeyPattern_MatchesMem0Categories proves the state_key regex
// admits the seven mem0 v3 category prefixes verbatim and rejects the
// obvious malformed shapes. The regex is the contract between the
// extractor (Phase 3 prompt v7) and the SQLite CHECK constraint;
// drift on either side would let bad data through.
func TestStateKeyPattern_MatchesMem0Categories(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(ontology.StateKeyPattern())

	good := []string{
		"event.flight.booked",
		"state.location.current",
		"relationship.spouse.name",
		"preference.coffee.milk",
		"plan.travel.next_trip",
		"absence.dietary.gluten",
		"timeless.birthday",
		"event", // single-segment is legal
		"preference.coffee.milk.brand_name",
	}
	for _, s := range good {
		if !re.MatchString(s) {
			t.Errorf("state_key %q rejected, expected accept", s)
		}
	}

	bad := []string{
		"",
		"Event.flight.booked", // uppercase
		"event..flight",       // empty segment
		".event",              // leading dot
		"event.",              // trailing dot
		"event-flight",        // hyphen
		"event flight",        // space
		"event.flight$booked", // illegal char
	}
	for _, s := range bad {
		if re.MatchString(s) {
			t.Errorf("state_key %q accepted, expected reject", s)
		}
	}
}

// TestRegisterFact_InstallsSchema confirms the schema reaches the
// memory ontology registry with the right (kind, version). Without this
// the daemon would boot a Fact-aware dispatcher against an empty
// registry.
func TestRegisterFact_InstallsSchema(t *testing.T) {
	t.Parallel()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterFact(context.Background(), o); err != nil {
		t.Fatalf("RegisterFact: %v", err)
	}

	got, err := o.Schema(context.Background(), ontology.KindFact)
	if err != nil {
		t.Fatalf("Schema(%s): %v", ontology.KindFact, err)
	}
	if got.Kind != ontology.KindFact {
		t.Errorf("Kind = %q, want %q", got.Kind, ontology.KindFact)
	}
	if got.Version != ontology.VersionFact {
		t.Errorf("Version = %q, want %q", got.Version, ontology.VersionFact)
	}
	if got.Provenance != "core" {
		t.Errorf("Provenance = %q, want core", got.Provenance)
	}
}
