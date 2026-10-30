package schemaorg_test

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/schemaorg"
)

// TestManifest validates the embedded manifest.json parses and records
// provenance metadata.
func TestManifest(t *testing.T) {
	t.Parallel()
	m, err := schemaorg.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Vocabulary == "" || m.SnapshotVersion == "" || m.SnapshotDate == "" {
		t.Errorf("manifest missing provenance fields: %+v", m)
	}
}

// TestEntries is the golden test over the embedded schema.org snapshot:
// types.json must parse and every entry must carry an IRI and a label.
func TestEntries(t *testing.T) {
	t.Parallel()
	entries, err := schemaorg.Entries()
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
	}
}

// TestNewImporter_Import wires the embedded snapshot through the importer and
// asserts it produces a well-formed event per entry.
func TestNewImporter_Import(t *testing.T) {
	t.Parallel()
	imp, err := schemaorg.NewImporter()
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
		if !strings.HasPrefix(ev.Name, "extension:schema_org:") {
			t.Errorf("kind name %q not namespaced under extension:schema_org:", ev.Name)
		}
		if ev.DisplayName == "" {
			t.Errorf("event %q has empty displayName", ev.Name)
		}
		if len(ev.SameAs) != 1 || !strings.HasPrefix(ev.SameAs[0], "https://schema.org/") {
			t.Errorf("event %q sameAs = %v, want one schema.org IRI", ev.Name, ev.SameAs)
		}
	}
}
