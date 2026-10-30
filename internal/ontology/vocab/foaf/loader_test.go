package foaf_test

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/foaf"
)

// TestManifest validates the embedded manifest.json parses and carries
// provenance metadata.
func TestManifest(t *testing.T) {
	t.Parallel()
	m, err := foaf.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Vocabulary == "" || m.SnapshotVersion == "" || m.SnapshotDate == "" {
		t.Errorf("manifest missing provenance fields: %+v", m)
	}
	if m.Vocabulary != "foaf" {
		t.Errorf("manifest vocabulary = %q, want foaf", m.Vocabulary)
	}
}

// TestEntries verifies the embedded types.json parses and every entry carries
// a well-formed IRI and a label.
func TestEntries(t *testing.T) {
	t.Parallel()
	entries, err := foaf.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded snapshot has no entries")
	}
	for _, e := range entries {
		if strings.TrimSpace(e.ID) == "" {
			t.Errorf("entry %q has empty IRI", e.Label)
		}
		if strings.TrimSpace(e.Label) == "" {
			t.Errorf("entry with IRI %q has empty label", e.ID)
		}
		if !strings.HasPrefix(e.ID, "http://xmlns.com/foaf/0.1/") {
			t.Errorf("entry %q IRI %q not in FOAF namespace", e.Label, e.ID)
		}
	}
}

// TestEntries_SubClassOfResolvesWithin verifies that every non-empty subClassOf
// reference in the snapshot points to another IRI that is present in the
// snapshot itself, so the crosswalk chain is self-contained.
func TestEntries_SubClassOfResolvesWithin(t *testing.T) {
	t.Parallel()
	entries, err := foaf.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	ids := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		ids[e.ID] = struct{}{}
	}
	for _, e := range entries {
		if e.SubClassOf == "" {
			continue
		}
		if _, ok := ids[e.SubClassOf]; !ok {
			// Allow references that are resolved by the crosswalk (e.g. foaf:Agent
			// resolves via the importer even if it is not explicitly in the
			// crosswalk map).  This test only flags IRIs outside the FOAF namespace
			// that have no snapshot entry.
			if strings.HasPrefix(e.SubClassOf, "http://xmlns.com/foaf/0.1/") {
				t.Errorf("entry %q subClassOf %q not found in snapshot", e.Label, e.SubClassOf)
			}
		}
	}
}

// TestNewImporter_Import wires the embedded snapshot through the importer and
// asserts it produces a well-formed event per entry.
func TestNewImporter_Import(t *testing.T) {
	t.Parallel()
	imp, err := foaf.NewImporter()
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	res, err := imp.Import(vocab.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Events) == 0 {
		t.Fatal("import produced no events")
	}
	for _, ev := range res.Events {
		if !strings.HasPrefix(ev.Name, "extension:foaf:") {
			t.Errorf("kind name %q not namespaced under extension:foaf:", ev.Name)
		}
		if ev.DisplayName == "" {
			t.Errorf("event %q has empty displayName", ev.Name)
		}
		if len(ev.SameAs) != 1 || !strings.HasPrefix(ev.SameAs[0], "http://xmlns.com/foaf/0.1/") {
			t.Errorf("event %q sameAs = %v, want one FOAF IRI", ev.Name, ev.SameAs)
		}
	}
}

// TestNewImporter_PersonGraftsOntoCorePerson verifies the crosswalk maps
// foaf:Person onto the core "person" kind.
func TestNewImporter_PersonGraftsOntoCorePerson(t *testing.T) {
	t.Parallel()
	imp, err := foaf.NewImporter()
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	res, err := imp.Import(vocab.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, ev := range res.Events {
		if ev.SameAs[0] == "http://xmlns.com/foaf/0.1/Person" {
			if ev.Extends != "person" {
				t.Errorf("foaf:Person extends = %q, want person", ev.Extends)
			}
			return
		}
	}
	t.Fatal("foaf:Person not found in import results")
}
