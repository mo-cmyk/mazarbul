package vocab_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
)

func sampleEntries() []vocab.TypeEntry {
	return []vocab.TypeEntry{
		{ID: "https://schema.org/Person", Label: "Person", Comment: "A person."},
		{ID: "https://schema.org/Corporation", Label: "Corporation", Comment: "A corporation.", SubClassOf: "https://schema.org/Organization"},
		{ID: "https://schema.org/Quokka", Label: "Quokka", Comment: "Not in the crosswalk.", SubClassOf: "https://schema.org/Animal"},
	}
}

func TestParseTypeEntries(t *testing.T) {
	t.Parallel()
	entries, err := vocab.ParseTypeEntries([]byte(
		`[{"id":"https://schema.org/Person","label":"Person","comment":"A person."}]`))
	if err != nil {
		t.Fatalf("ParseTypeEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Label != "Person" {
		t.Fatalf("entries = %+v, want 1 Person", entries)
	}
}

func TestParseTypeEntries_Invalid(t *testing.T) {
	t.Parallel()
	if _, err := vocab.ParseTypeEntries([]byte(`{not json`)); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestImport_CrosswalkAndNamespacing(t *testing.T) {
	t.Parallel()
	imp := vocab.NewSchemaOrgImporter(sampleEntries())
	res, err := imp.Import(vocab.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(res.Events))
	}
	byDisplay := map[string]*ontology.EntityKindRegisteredEvent{}
	for _, e := range res.Events {
		byDisplay[e.DisplayName] = e
	}

	person := byDisplay["Person"]
	if person == nil {
		t.Fatal("Person event missing")
	}
	if person.Name != "extension:schema_org:person" {
		t.Errorf("Person kind name = %q, want extension:schema_org:person (normalized lowercase)", person.Name)
	}
	if person.Extends != "person" {
		t.Errorf("Person extends = %q, want person (direct crosswalk)", person.Extends)
	}
	if len(person.SameAs) != 1 || person.SameAs[0] != "https://schema.org/Person" {
		t.Errorf("Person sameAs = %v, want [the IRI]", person.SameAs)
	}
	if person.AddedBy != ontology.EntityKindAddedByExtension {
		t.Errorf("Person addedBy = %q, want extension", person.AddedBy)
	}
	if person.Provenance != "extension:schema_org" {
		t.Errorf("Person provenance = %q, want extension:schema_org", person.Provenance)
	}

	// Corporation is not a direct crosswalk hit, but its subClassOf
	// (Organization) is — so it inherits "organization".
	corp := byDisplay["Corporation"]
	if corp == nil || corp.Extends != "organization" {
		t.Errorf("Corporation extends = %q, want organization (via subClassOf)", corpExtends(corp))
	}

	// Quokka has neither a direct nor a subClassOf crosswalk hit → root extension.
	quokka := byDisplay["Quokka"]
	if quokka == nil || quokka.Extends != "" {
		t.Errorf("Quokka extends = %q, want empty (no crosswalk)", corpExtends(quokka))
	}
}

func corpExtends(e *ontology.EntityKindRegisteredEvent) string {
	if e == nil {
		return "<nil event>"
	}
	return e.Extends
}

func TestImport_TypesFilter(t *testing.T) {
	t.Parallel()
	imp := vocab.NewSchemaOrgImporter(sampleEntries())
	res, err := imp.Import(vocab.ImportOptions{Types: []string{"Person"}})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].DisplayName != "Person" {
		t.Fatalf("filtered events = %+v, want only Person", res.Events)
	}
	// Corporation and Quokka should be reported as skipped.
	if len(res.Skipped) != 2 {
		t.Errorf("skipped = %v, want 2 (Corporation, Quokka)", res.Skipped)
	}
}

func TestImport_CustomProvenance(t *testing.T) {
	t.Parallel()
	imp := vocab.NewSchemaOrgImporter(sampleEntries()[:1])
	res, err := imp.Import(vocab.ImportOptions{Provenance: "extension:custom"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Events[0].Provenance != "extension:custom" {
		t.Errorf("provenance = %q, want extension:custom", res.Events[0].Provenance)
	}
}
