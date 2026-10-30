package dublincore_test

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/dublincore"
)

// TestManifest validates the embedded manifest.json parses and carries
// provenance metadata.
func TestManifest(t *testing.T) {
	t.Parallel()
	m, err := dublincore.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Vocabulary == "" || m.SnapshotVersion == "" || m.SnapshotDate == "" {
		t.Errorf("manifest missing provenance fields: %+v", m)
	}
	if m.Vocabulary != "dublincore" {
		t.Errorf("manifest vocabulary = %q, want dublincore", m.Vocabulary)
	}
}

// TestEntries verifies the embedded types.json parses and every entry carries
// a well-formed IRI (either dcterms or dcmitype namespace) and a label.
func TestEntries(t *testing.T) {
	t.Parallel()
	entries, err := dublincore.Entries()
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
		inDcterms := strings.HasPrefix(e.ID, "http://purl.org/dc/terms/")
		inDcmitype := strings.HasPrefix(e.ID, "http://purl.org/dc/dcmitype/")
		if !inDcterms && !inDcmitype {
			t.Errorf("entry %q IRI %q not in dcterms or dcmitype namespace", e.Label, e.ID)
		}
	}
}

// TestNewImporter_Import wires the embedded snapshot through the importer and
// asserts it produces a well-formed event per entry.
func TestNewImporter_Import(t *testing.T) {
	t.Parallel()
	imp, err := dublincore.NewImporter()
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
		if !strings.HasPrefix(ev.Name, "extension:dublincore:") {
			t.Errorf("kind name %q not namespaced under extension:dublincore:", ev.Name)
		}
		if ev.DisplayName == "" {
			t.Errorf("event %q has empty displayName", ev.Name)
		}
	}
}

// TestNewImporter_BibliographicResourceGraftsOntoDocument verifies the
// crosswalk maps dcmitype:BibliographicResource onto the core "document" kind.
func TestNewImporter_BibliographicResourceGraftsOntoDocument(t *testing.T) {
	t.Parallel()
	imp, err := dublincore.NewImporter()
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	res, err := imp.Import(vocab.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, ev := range res.Events {
		if ev.DisplayName == "BibliographicResource" {
			if ev.Extends != "document" {
				t.Errorf("dcmitype:BibliographicResource extends = %q, want document", ev.Extends)
			}
			return
		}
	}
	t.Fatal("BibliographicResource not found in import results")
}
