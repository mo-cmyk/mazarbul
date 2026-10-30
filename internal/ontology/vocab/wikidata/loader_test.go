package wikidata_test

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/wikidata"
)

// TestManifest validates the embedded manifest.json parses and carries
// provenance metadata.
func TestManifest(t *testing.T) {
	t.Parallel()
	m, err := wikidata.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Vocabulary == "" || m.SnapshotVersion == "" || m.SnapshotDate == "" {
		t.Errorf("manifest missing provenance fields: %+v", m)
	}
	if m.Vocabulary != "wikidata" {
		t.Errorf("manifest vocabulary = %q, want wikidata", m.Vocabulary)
	}
}

// TestEntries verifies the embedded types.json parses and every entry carries
// a well-formed Wikidata Q-IRI and a label.
func TestEntries(t *testing.T) {
	t.Parallel()
	entries, err := wikidata.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) < 20 {
		t.Fatalf("expected at least 20 entries, got %d", len(entries))
	}
	for _, e := range entries {
		if strings.TrimSpace(e.ID) == "" {
			t.Errorf("entry %q has empty IRI", e.Label)
		}
		if strings.TrimSpace(e.Label) == "" {
			t.Errorf("entry with IRI %q has empty label", e.ID)
		}
		if !strings.HasPrefix(e.ID, "http://www.wikidata.org/entity/Q") {
			t.Errorf("entry %q IRI %q not a Wikidata Q-IRI", e.Label, e.ID)
		}
	}
}

// TestEntries_SubClassOfResolvesWithin verifies that every non-empty
// subClassOf reference in the snapshot points to another IRI present in the
// snapshot.
func TestEntries_SubClassOfResolvesWithin(t *testing.T) {
	t.Parallel()
	entries, err := wikidata.Entries()
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
			t.Errorf("entry %q subClassOf %q not found in snapshot", e.Label, e.SubClassOf)
		}
	}
}

// TestNewImporter_Import wires the embedded snapshot through the importer and
// asserts it produces a well-formed event per entry.
func TestNewImporter_Import(t *testing.T) {
	t.Parallel()
	imp, err := wikidata.NewImporter()
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
		if !strings.HasPrefix(ev.Name, "extension:wikidata:") {
			t.Errorf("kind name %q not namespaced under extension:wikidata:", ev.Name)
		}
		if ev.DisplayName == "" {
			t.Errorf("event %q has empty displayName", ev.Name)
		}
		if len(ev.SameAs) != 1 || !strings.HasPrefix(ev.SameAs[0], "http://www.wikidata.org/entity/Q") {
			t.Errorf("event %q sameAs = %v, want one Wikidata Q-IRI", ev.Name, ev.SameAs)
		}
	}
}

// TestNewImporter_HumanGraftsOntoCorePerson verifies the crosswalk maps
// wikidata:Q5 (human) onto the core "person" kind.
func TestNewImporter_HumanGraftsOntoCorePerson(t *testing.T) {
	t.Parallel()
	imp, err := wikidata.NewImporter()
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	res, err := imp.Import(vocab.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, ev := range res.Events {
		if ev.SameAs[0] == "http://www.wikidata.org/entity/Q5" {
			if ev.Extends != "person" {
				t.Errorf("wikidata:Q5 (human) extends = %q, want person", ev.Extends)
			}
			return
		}
	}
	t.Fatal("wikidata:Q5 (human) not found in import results")
}

// TestNewImporter_OrganizationGraftsOntoCoreOrganization verifies the
// crosswalk maps wikidata:Q43229 (organization) onto the core "organization" kind.
func TestNewImporter_OrganizationGraftsOntoCoreOrganization(t *testing.T) {
	t.Parallel()
	imp, err := wikidata.NewImporter()
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	res, err := imp.Import(vocab.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, ev := range res.Events {
		if ev.SameAs[0] == "http://www.wikidata.org/entity/Q43229" {
			if ev.Extends != "organization" {
				t.Errorf("wikidata:Q43229 (organization) extends = %q, want organization", ev.Extends)
			}
			return
		}
	}
	t.Fatal("wikidata:Q43229 (organization) not found in import results")
}
