// Package vocab provides vocabulary importers that map external type systems
// (schema.org, Wikidata, FOAF, Dublin Core, SKOS) to Mazarbul
// [ontology.EntityKindRegisteredEvent] payloads.
//
// # Architecture
//
// Each supported vocabulary lives in its own sub-package (schemaorg/,
// foaf/, dublincore/, skos/, wikidata/).  The sub-package embeds an offline
// snapshot JSON file (avoiding live network fetches) alongside a manifest.json
// recording the snapshot version and date for provenance.  The crosswalk in
// this file maps vocabulary type IRIs to the six core Mazarbul kinds so
// imported types graft onto the core tree rather than floating as disconnected
// roots.
//
// # Supported vocabularies
//
//   - schema.org  — curated 30-type snapshot; full import via --from-file
//   - foaf        — 10 FOAF classes (Agent, Person, Organization, etc.)
//   - dublincore  — 8 DCMI type classes (Agent, BibliographicResource, etc.)
//   - skos        — 4 SKOS core classes (Concept, ConceptScheme, etc.)
//   - wikidata    — 25 curated Q-ID entity classes; full export via --from-file
package vocab

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TypeEntry is one entry in a vocabulary snapshot's types array.
type TypeEntry struct {
	// ID is the canonical IRI of the type, e.g. "https://schema.org/Person".
	ID string `json:"id"`

	// Label is the human-readable type name, e.g. "Person".
	Label string `json:"label"`

	// Comment is a one-sentence description.
	Comment string `json:"comment"`

	// SubClassOf is the parent type IRI, e.g. "https://schema.org/Thing".
	// Empty for root types.
	SubClassOf string `json:"subClassOf,omitempty"`
}

// ImportOptions controls which types the importer processes.
type ImportOptions struct {
	// Types restricts the import to only the named types (Label values).
	// When empty all types in the snapshot are imported.
	Types []string

	// Provenance is the provenance string for the generated events.
	// Defaults to "extension:<vocab-name>" when empty.
	Provenance string

	// AddedAt is the timestamp to embed in the events.  Uses time.Now().UTC()
	// when zero.
	AddedAt time.Time
}

// ImportResult holds the list of events produced by an import.
type ImportResult struct {
	// Events is the generated entityKindRegistered events.
	Events []*ontology.EntityKindRegisteredEvent

	// Skipped lists type Labels that were in the snapshot but not imported
	// (because of Types filtering or because they are root/Thing entries).
	Skipped []string
}

// Crosswalk maps a vocabulary type IRI to the canonical Mazarbul core kind
// name it should extend.  When the IRI is not in the crosswalk the importer
// derives a parent from the vocabulary's own subClassOf chain.
type Crosswalk map[string]string

// VocabName identifies which vocabulary an importer handles.
//
//nolint:revive // stutter is intentional: vocab.VocabName reads clearly at call sites
type VocabName string

const (
	// VocabSchemaOrg is the schema.org vocabulary.
	VocabSchemaOrg VocabName = "schema.org"

	// VocabFoaf is the FOAF (Friend of a Friend) vocabulary.
	VocabFoaf VocabName = "foaf"

	// VocabDublinCore is the Dublin Core (DCMI) vocabulary.
	VocabDublinCore VocabName = "dublincore"

	// VocabSkos is the SKOS (Simple Knowledge Organization System) vocabulary.
	VocabSkos VocabName = "skos"

	// VocabWikidata is the Wikidata entity vocabulary.
	VocabWikidata VocabName = "wikidata"
)

// Importer maps a vocabulary snapshot to entityKindRegistered events.
type Importer struct {
	// Name is the vocabulary identifier used in provenance and kind names.
	Name VocabName

	// Entries is the curated type list from the embedded snapshot.
	Entries []TypeEntry

	// crosswalk maps vocabulary type IRIs → Mazarbul core kind names.
	crosswalk Crosswalk
}

// Import generates entityKindRegistered events for every entry in the
// snapshot that passes the opts filter.  The caller is responsible for
// appending the returned events to the event log.
func (imp *Importer) Import(opts ImportOptions) (*ImportResult, error) {
	if opts.AddedAt.IsZero() {
		opts.AddedAt = time.Now().UTC()
	}
	provenance := opts.Provenance
	if provenance == "" {
		provenance = "extension:" + string(imp.Name)
		// Replace dots with underscores for the provenance pattern (must match
		// ^(core|user|extension:[a-z][a-z0-9_-]*)$).
		provenance = strings.ReplaceAll(provenance, ".", "_")
	}

	typeFilter := make(map[string]struct{}, len(opts.Types))
	for _, t := range opts.Types {
		typeFilter[t] = struct{}{}
	}

	result := &ImportResult{}
	for i := range imp.Entries {
		entry := &imp.Entries[i]
		if len(typeFilter) > 0 {
			if _, ok := typeFilter[entry.Label]; !ok {
				result.Skipped = append(result.Skipped, entry.Label)
				continue
			}
		}

		kindName, err := imp.kindName(entry)
		if err != nil {
			return nil, fmt.Errorf("vocab %s: derive kind name for %q: %w", imp.Name, entry.Label, err)
		}

		extends := imp.resolveExtends(entry)

		ev := &ontology.EntityKindRegisteredEvent{
			Name:        kindName,
			DisplayName: entry.Label,
			Description: entry.Comment,
			Extends:     extends,
			SameAs:      []string{entry.ID},
			Provenance:  provenance,
			AddedBy:     ontology.EntityKindAddedByExtension,
			AddedAt:     opts.AddedAt,
		}
		result.Events = append(result.Events, ev)
	}
	return result, nil
}

// kindName derives the Mazarbul kind name for a type entry.  The pattern is
// "extension:<vocab_ns>:<normalized_label>" where the label is lowercased and
// any character outside [a-z0-9_] is replaced with "_".  This ensures every
// generated name matches the ontology event-schema kind-name pattern so
// dry-run and real import agree regardless of external label formatting.
func (imp *Importer) kindName(entry *TypeEntry) (string, error) {
	if strings.TrimSpace(entry.Label) == "" {
		return "", fmt.Errorf("empty label for IRI %q", entry.ID)
	}
	// vocab namespace: replace dots/hyphens with underscores, lowercase.
	ns := strings.ToLower(strings.NewReplacer(".", "_", "-", "_").Replace(string(imp.Name)))
	// normalize label: lowercase, replace any non-[a-z0-9_] with "_".
	norm := normalizeLabel(entry.Label)
	return "extension:" + ns + ":" + norm, nil
}

// normalizeLabel lowercases s and replaces any character that is not a
// lowercase letter, digit, or underscore with an underscore, producing a
// valid snake_case kind-name segment.
func normalizeLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// resolveExtends finds the best Mazarbul core-kind parent for entry.
// It checks the crosswalk first; if not found it checks whether the
// entry's direct subClassOf IRI is itself in the crosswalk; otherwise
// returns empty (no explicit parent — the kind will be a root extension).
func (imp *Importer) resolveExtends(entry *TypeEntry) string {
	// Direct crosswalk hit.
	if parent, ok := imp.crosswalk[entry.ID]; ok {
		return parent
	}
	// Inherit from subClassOf if that IRI is in the crosswalk.
	if entry.SubClassOf != "" {
		if parent, ok := imp.crosswalk[entry.SubClassOf]; ok {
			return parent
		}
	}
	return ""
}

// schemaOrgCrosswalk maps the most common schema.org type IRIs to their
// nearest Mazarbul core kind.  Types not in this map are imported without
// an explicit Extends so they appear as extension roots.
//
//nolint:gochecknoglobals // read-only crosswalk lookup, never mutated
var schemaOrgCrosswalk = Crosswalk{
	// Core 1:1 mappings.
	"https://schema.org/Person":       "person",
	"https://schema.org/Organization": "organization",
	"https://schema.org/Place":        "place",
	"https://schema.org/Event":        "event",
	"https://schema.org/CreativeWork": "document",
	"https://schema.org/Project":      "project",

	// Organization subtypes → organization.
	"https://schema.org/Corporation":             "organization",
	"https://schema.org/EducationalOrganization": "organization",
	"https://schema.org/GovernmentOrganization":  "organization",
	"https://schema.org/LocalBusiness":           "organization",
	"https://schema.org/NGO":                     "organization",
	"https://schema.org/SportsOrganization":      "organization",
	"https://schema.org/MedicalOrganization":     "organization",

	// Place subtypes → place.
	"https://schema.org/City":                           "place",
	"https://schema.org/Country":                        "place",
	"https://schema.org/LandmarksOrHistoricalBuildings": "place",

	// CreativeWork subtypes → document.
	"https://schema.org/Article":             "document",
	"https://schema.org/Book":                "document",
	"https://schema.org/Movie":               "document",
	"https://schema.org/ScholarlyArticle":    "document",
	"https://schema.org/SoftwareApplication": "document",
	"https://schema.org/Dataset":             "document",
	"https://schema.org/ItemList":            "document",

	// Event subtypes → event.
	"https://schema.org/BusinessEvent": "event",
	"https://schema.org/SportsEvent":   "event",
	"https://schema.org/SocialEvent":   "event",

	// Project subtypes → project.
	"https://schema.org/ResearchProject": "project",

	// LocalBusiness subtypes → organization.
	"https://schema.org/FinancialService": "organization",
	"https://schema.org/LegalService":     "organization",
}

// NewSchemaOrgImporter returns an Importer loaded from the embedded
// schema.org snapshot.
func NewSchemaOrgImporter(entries []TypeEntry) *Importer {
	return &Importer{
		Name:      VocabSchemaOrg,
		Entries:   entries,
		crosswalk: schemaOrgCrosswalk,
	}
}

// ParseTypeEntries unmarshals a JSON array of TypeEntry values.
func ParseTypeEntries(data []byte) ([]TypeEntry, error) {
	var entries []TypeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse type entries: %w", err)
	}
	return entries, nil
}

// foafCrosswalk maps FOAF class IRIs to their nearest Mazarbul core kind.
// Agent has no direct crosswalk equivalent (it is the root FOAF class);
// its subclasses are mapped individually.
//
//nolint:gochecknoglobals // read-only crosswalk lookup, never mutated
var foafCrosswalk = Crosswalk{
	// Core FOAF agent subclasses.
	"http://xmlns.com/foaf/0.1/Person":       "person",
	"http://xmlns.com/foaf/0.1/Organization": "organization",
	"http://xmlns.com/foaf/0.1/Group":        "organization",

	// Document types.
	"http://xmlns.com/foaf/0.1/Document":                "document",
	"http://xmlns.com/foaf/0.1/Image":                   "document",
	"http://xmlns.com/foaf/0.1/PersonalProfileDocument": "document",
}

// NewFoafImporter returns an Importer loaded from the embedded FOAF snapshot.
func NewFoafImporter(entries []TypeEntry) *Importer {
	return &Importer{
		Name:      VocabFoaf,
		Entries:   entries,
		crosswalk: foafCrosswalk,
	}
}

// dublinCoreCrosswalk maps DCMI type IRIs to their nearest Mazarbul core kind.
//
//nolint:gochecknoglobals // read-only crosswalk lookup, never mutated
var dublinCoreCrosswalk = Crosswalk{
	// dcterms namespace.
	"http://purl.org/dc/terms/Agent": "person",

	// dcmitype namespace.
	"http://purl.org/dc/dcmitype/BibliographicResource": "document",
	"http://purl.org/dc/dcmitype/Collection":            "document",
	"http://purl.org/dc/dcmitype/Dataset":               "document",
	"http://purl.org/dc/dcmitype/Event":                 "event",
	"http://purl.org/dc/dcmitype/Image":                 "document",
	"http://purl.org/dc/dcmitype/Location":              "place",
}

// NewDublinCoreImporter returns an Importer loaded from the embedded Dublin
// Core snapshot.
func NewDublinCoreImporter(entries []TypeEntry) *Importer {
	return &Importer{
		Name:      VocabDublinCore,
		Entries:   entries,
		crosswalk: dublinCoreCrosswalk,
	}
}

// skosCrosswalk maps SKOS class IRIs to their nearest Mazarbul core kind.
// SKOS classes represent knowledge organization constructs; they do not map
// directly onto person/organization/place/event so most are left as extension
// roots.  Only Concept is mapped to a broadly useful core kind.
//
//nolint:gochecknoglobals // read-only crosswalk lookup, never mutated
var skosCrosswalk = Crosswalk{
	// SKOS Collection/OrderedCollection are document-like aggregations.
	"http://www.w3.org/2004/02/skos/core#Collection":        "document",
	"http://www.w3.org/2004/02/skos/core#OrderedCollection": "document",
}

// NewSkosImporter returns an Importer loaded from the embedded SKOS snapshot.
func NewSkosImporter(entries []TypeEntry) *Importer {
	return &Importer{
		Name:      VocabSkos,
		Entries:   entries,
		crosswalk: skosCrosswalk,
	}
}

// wikidataCrosswalk maps Wikidata entity Q-IRI strings to Mazarbul core kinds.
//
//nolint:gochecknoglobals // read-only crosswalk lookup, never mutated
var wikidataCrosswalk = Crosswalk{
	// Human / person.
	"http://www.wikidata.org/entity/Q5": "person",

	// Organizations.
	"http://www.wikidata.org/entity/Q43229":    "organization",
	"http://www.wikidata.org/entity/Q4830453":  "organization",
	"http://www.wikidata.org/entity/Q4671277":  "organization",
	"http://www.wikidata.org/entity/Q327333":   "organization",
	"http://www.wikidata.org/entity/Q163740":   "organization",
	"http://www.wikidata.org/entity/Q12973014": "organization",
	"http://www.wikidata.org/entity/Q7278":     "organization",

	// Places.
	"http://www.wikidata.org/entity/Q515":  "place",
	"http://www.wikidata.org/entity/Q6256": "place",
	"http://www.wikidata.org/entity/Q4022": "place",
	"http://www.wikidata.org/entity/Q8502": "place",

	// Events.
	"http://www.wikidata.org/entity/Q1656682": "event",

	// Documents / creative works.
	"http://www.wikidata.org/entity/Q49848":   "document",
	"http://www.wikidata.org/entity/Q571":     "document",
	"http://www.wikidata.org/entity/Q11424":   "document",
	"http://www.wikidata.org/entity/Q2188189": "document",
}

// NewWikidataImporter returns an Importer loaded from the embedded Wikidata
// snapshot.
func NewWikidataImporter(entries []TypeEntry) *Importer {
	return &Importer{
		Name:      VocabWikidata,
		Entries:   entries,
		crosswalk: wikidataCrosswalk,
	}
}
