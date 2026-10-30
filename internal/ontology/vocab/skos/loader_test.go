package skos_test

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/skos"
)

// TestManifest validates the embedded manifest.json parses and carries
// provenance metadata.
func TestManifest(t *testing.T) {
	t.Parallel()
	m, err := skos.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Vocabulary == "" || m.SnapshotVersion == "" || m.SnapshotDate == "" {
		t.Errorf("manifest missing provenance fields: %+v", m)
	}
	if m.Vocabulary != "skos" {
		t.Errorf("manifest vocabulary = %q, want skos", m.Vocabulary)
	}
}

// TestEntries verifies the embedded types.json parses and every entry carries
// a well-formed IRI in the SKOS namespace and a label.
func TestEntries(t *testing.T) {
	t.Parallel()
	entries, err := skos.Entries()
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
		if !strings.HasPrefix(e.ID, "http://www.w3.org/2004/02/skos/core#") {
			t.Errorf("entry %q IRI %q not in SKOS namespace", e.Label, e.ID)
		}
	}
}

// TestEntries_SubClassOfResolvesWithin verifies that every non-empty
// subClassOf reference points to an IRI present in the snapshot.
func TestEntries_SubClassOfResolvesWithin(t *testing.T) {
	t.Parallel()
	entries, err := skos.Entries()
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
	imp, err := skos.NewImporter()
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
		if !strings.HasPrefix(ev.Name, "extension:skos:") {
			t.Errorf("kind name %q not namespaced under extension:skos:", ev.Name)
		}
		if ev.DisplayName == "" {
			t.Errorf("event %q has empty displayName", ev.Name)
		}
		if len(ev.SameAs) != 1 || !strings.HasPrefix(ev.SameAs[0], "http://www.w3.org/2004/02/skos/core#") {
			t.Errorf("event %q sameAs = %v, want one SKOS IRI", ev.Name, ev.SameAs)
		}
	}
}
