package kernelboot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// genesisTime is the canonical addedAt timestamp for all seed events. Fixed so
// that replays and rebuilds produce identical rows regardless of wall-clock at
// boot time. Aligns with the project bootstrap date from migration 0006.
//
//nolint:gochecknoglobals // process-lifetime constant; time.Time cannot be declared const
var genesisTime = time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)

// genesisTimeLate is the addedAt timestamp for predicates added in migrations
// 0012 and 0013 (has_name, studies_at).
//
//nolint:gochecknoglobals // process-lifetime constant; time.Time cannot be declared const
var genesisTimeLate = time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)

// SeedOntology appends the seven core entityKindRegistered events and the
// fourteen canonical predicateRegistered events to log on first boot. It is
// idempotent: if every core kind row already exists in db the function returns
// immediately without appending any events (the dispatcher UPSERT ensures
// replay convergence even if this sentinel check races, but the check avoids
// spamming the log on every daemon restart).
//
// Db must be the SQLite writer pool opened against the same dataDir as log.
// Callers invoke this after the ontology and event log are constructed and all
// schemas are registered (so the writer can validate the genesis payloads).
func SeedOntology(ctx context.Context, log kernel.EventLog, db *sql.DB) error {
	if seeded, err := isAlreadySeeded(ctx, db); err != nil {
		return fmt.Errorf("kernelboot: genesis seed check: %w", err)
	} else if seeded {
		return nil
	}

	// 7 entity kinds + 14 seed predicates.
	events := make([]kernel.Event, 0, 21)

	// --- seven core entity kinds ---
	//
	// SameAs IRI selection rationale (P3 multi-vocab seeds):
	//
	//   person:       schema.org/Person     — canonical web-data vocabulary
	//                 Wikidata Q5           — "human being"; broadest Wikidata human entity
	//                 FOAF foaf:Person       — established RDF social-graph vocabulary
	//
	//   organization: schema.org/Organization
	//                 Wikidata Q43229       — "organization"
	//                 FOAF foaf:Organization — FOAF complement to foaf:Person
	//
	//   place:        schema.org/Place
	//                 Wikidata Q2221906     — "geographic location"
	//                 (no FOAF/DC Place equivalent)
	//
	//   project:      schema.org/Project
	//                 Wikidata Q170584      — "project"
	//                 (no FOAF/DC Project equivalent)
	//
	//   event:        schema.org/Event
	//                 Wikidata Q1656682     — "event (things that happen)"
	//                 (no FOAF/DC Event equivalent)
	//
	//   document:     schema.org/CreativeWork — broadest schema.org authored artefact
	//                 Wikidata Q49848        — "document"
	//                 DC Terms dcterms:BibliographicResource — closest DC class for
	//                              authored, citable documents; dcterms:Text is more
	//                              restrictive (written text only)
	//
	// SKOS is omitted: it defines concept-scheme vocabulary (Concept, Collection,
	// ConceptScheme) rather than entity types in the person/place/thing sense.
	coreKinds := []*ontology.EntityKindRegisteredEvent{
		{
			Name:        "person",
			DisplayName: "Person",
			Description: "A human individual.",
			SameAs: []string{
				"https://schema.org/Person",
				"http://www.wikidata.org/entity/Q5",
				"http://xmlns.com/foaf/0.1/Person",
			},
			Provenance: "core",
			AddedBy:    ontology.EntityKindAddedBySeed,
			AddedAt:    genesisTime,
		},
		{
			Name:        "organization",
			DisplayName: "Organization",
			Description: "A company, institution, government body, or other collective.",
			SameAs: []string{
				"https://schema.org/Organization",
				"http://www.wikidata.org/entity/Q43229",
				"http://xmlns.com/foaf/0.1/Organization",
			},
			Provenance: "core",
			AddedBy:    ontology.EntityKindAddedBySeed,
			AddedAt:    genesisTime,
		},
		{
			Name:        "place",
			DisplayName: "Place",
			Description: "A physical or geopolitical location.",
			SameAs: []string{
				"https://schema.org/Place",
				"http://www.wikidata.org/entity/Q2221906",
			},
			Provenance: "core",
			AddedBy:    ontology.EntityKindAddedBySeed,
			AddedAt:    genesisTime,
		},
		{
			Name:        "project",
			DisplayName: "Project",
			Description: "A coordinated undertaking with a goal and timeline.",
			SameAs: []string{
				"https://schema.org/Project",
				"http://www.wikidata.org/entity/Q170584",
			},
			Provenance: "core",
			AddedBy:    ontology.EntityKindAddedBySeed,
			AddedAt:    genesisTime,
		},
		{
			Name:        "event",
			DisplayName: "Event",
			Description: "A happening at a specific time and place.",
			SameAs: []string{
				"https://schema.org/Event",
				"http://www.wikidata.org/entity/Q1656682",
			},
			Provenance: "core",
			AddedBy:    ontology.EntityKindAddedBySeed,
			AddedAt:    genesisTime,
		},
		{
			Name:        "document",
			DisplayName: "Document",
			Description: "A written or recorded artefact that conveys information.",
			SameAs: []string{
				"https://schema.org/CreativeWork",
				"http://www.wikidata.org/entity/Q49848",
				"http://purl.org/dc/terms/BibliographicResource",
			},
			Provenance: "core",
			AddedBy:    ontology.EntityKindAddedBySeed,
			AddedAt:    genesisTime,
		},
		{
			// external_ref (#208): a first-class local pointer to information held
			// in another system, identified by a {type:"uri"} Identifier. PROV-O
			// prov:Entity is the closest upstream class (the generic thing a
			// derivation points back at); schema.org/URL captures the URI shape.
			Name:        "external_ref",
			DisplayName: "External reference",
			Description: "A pointer to information held in another system, identified by a URI.",
			SameAs: []string{
				"http://www.w3.org/ns/prov#Entity",
				"https://schema.org/URL",
			},
			Provenance: "core",
			AddedBy:    ontology.EntityKindAddedBySeed,
			AddedAt:    genesisTimeLate,
		},
	}
	for i := range coreKinds {
		ev, err := makeGenesisEvent(ontology.KindEntityKindRegistered, coreKinds[i])
		if err != nil {
			return fmt.Errorf("kernelboot: marshal entity kind %q: %w", coreKinds[i].Name, err)
		}
		events = append(events, ev)
	}

	// --- fourteen seed predicates (from migrations 0006, 0012, 0013; #208 has_primary_source; same_as identity link) ---
	seedPredicates := []*ontology.PredicateRegisteredEvent{
		// 0006 canonical ten
		{Predicate: "works_at", Kind: ontology.PredicateKindFunctional, Description: "Subject is employed at the object organisation.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "lives_in", Kind: ontology.PredicateKindFunctional, Description: "Subject resides in the object location.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "born_in", Kind: ontology.PredicateKindFunctional, Description: "Subject was born in the object location.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "child_of", Kind: ontology.PredicateKindFunctional, Description: "Subject is a child of the object person.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "parent_of", Kind: ontology.PredicateKindFunctional, Description: "Subject is a parent of the object person.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "married_to", Kind: ontology.PredicateKindFunctional, Description: "Subject is married to the object person.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "member_of", Kind: ontology.PredicateKindFunctional, Description: "Subject is a member of the object group.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "located_in", Kind: ontology.PredicateKindFunctional, Description: "Subject is physically located in the object.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "has_role", Kind: ontology.PredicateKindFunctional, Description: "Subject holds the object role or title.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		{Predicate: "owns", Kind: ontology.PredicateKindFunctional, Description: "Subject owns the object.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTime},
		// 0012 has_name
		{Predicate: "has_name", Kind: ontology.PredicateKindFunctional, Description: "Subject is currently known by the object name.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTimeLate},
		// 0013 studies_at
		{Predicate: "studies_at", Kind: ontology.PredicateKindFunctional, Description: "Subject is currently enrolled at the object organisation.", AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTimeLate},
		// has_primary_source (#208): grounds a local record in an external_ref (PROV hadPrimarySource).
		// Non-functional: PROV-O hadPrimarySource is non-functional, and a single record can be
		// grounded in several sources (e.g. a decision backed by both an issue and a design doc).
		// Functional would route this through functionalSupersede and silently close every prior
		// grounding when a second one is asserted; non-functional lets them coexist.
		{Predicate: "has_primary_source", Kind: ontology.PredicateKindNonFunctional, Description: "Subject is grounded in the object external reference (PROV hadPrimarySource). Non-functional: a record may have multiple primary sources.", ObjectKinds: []string{"external_ref"}, SameAs: []string{"http://www.w3.org/ns/prov#hadPrimarySource"}, AddedBy: ontology.PredicateAddedBySeed, AddedAt: genesisTimeLate},
		// same_as: identity link from the local self anchor to the bound principal.
		// Shared with the EnsureSelfPrincipalLink reconciler so fresh installs and
		// existing installs register identical vocabulary (existing installs get it
		// from the reconciler because SeedOntology short-circuits when already seeded).
		selfLinkPredicateEvent(),
	}
	for i := range seedPredicates {
		ev, err := makeGenesisEvent(ontology.KindPredicateRegistered, seedPredicates[i])
		if err != nil {
			return fmt.Errorf("kernelboot: marshal predicate %q: %w", seedPredicates[i].Predicate, err)
		}
		events = append(events, ev)
	}

	if _, err := log.Append(ctx, events); err != nil {
		return fmt.Errorf("kernelboot: append genesis events: %w", err)
	}
	return nil
}

// seedSentinelKinds is the full set of core entity-kind names that must exist
// for the seed to be considered complete. Checking the full set prevents a
// partial seed (e.g. a crash mid-append) from being treated as done on the
// next boot.
//
//nolint:gochecknoglobals // fixed set of core kind names, never mutated
var seedSentinelKinds = []string{
	"person", "organization", "place", "project", "event", "document", "external_ref",
}

// isAlreadySeeded queries the entity_kinds table to confirm ALL seven core
// kind rows are present. Returns true only when every sentinel row exists,
// false when any row is missing, and an error on unexpected failure. Using the
// full set prevents a partial seed (crash mid-append or manual deletion of a
// single row) from being treated as done, which would leave the taxonomy
// incomplete; it also re-seeds a pre-#208 store that lacks external_ref (the
// kind/predicate upserts are idempotent, so the re-seed is safe).
func isAlreadySeeded(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_kinds WHERE name IN ('person','organization','place','project','event','document','external_ref')`,
	).Scan(&n)
	if err != nil {
		// Table may not exist yet on the very first boot before migrations run.
		// Treat "no such table" as not-seeded; all other errors bubble up.
		if isNoSuchTableErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("query entity_kinds sentinel: %w", err)
	}
	return n == len(seedSentinelKinds), nil
}

// isNoSuchTableErr reports whether err is an SQLite "no such table" error.
func isNoSuchTableErr(err error) bool {
	if err == nil {
		return false
	}
	// sqlite3 error strings contain "no such table" when the table has not
	// been created yet. Using errors.Is does not help here because the
	// mattn/go-sqlite3 driver wraps the error; a string check is the
	// pragmatic approach used elsewhere in the codebase.
	return errors.Is(err, sql.ErrNoRows) || containsNoSuchTable(err.Error())
}

// containsNoSuchTable does a case-insensitive scan of the error string.
func containsNoSuchTable(s string) bool {
	const needle = "no such table"
	if len(s) < len(needle) {
		return false
	}
	for i := range s[:len(s)-len(needle)+1] {
		match := true
		for j, c := range needle {
			sc := s[i+j]
			if sc >= 'A' && sc <= 'Z' {
				sc += 'a' - 'A'
			}
			if byte(c) != sc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// makeGenesisEvent builds a kernel.Event with an unset CreateTime (the event
// log assigns the real ULID and timestamp on Append). Source is "genesis" to
// distinguish seed events from user-originated ones in the provenance chain.
func makeGenesisEvent(kind string, payload any) (kernel.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return kernel.Event{}, fmt.Errorf("marshal genesis payload: %w", err)
	}
	return kernel.Event{
		Kind:       kernel.EventKind(kind),
		Source:     "genesis",
		Confidence: 1.0,
		Payload:    raw,
	}, nil
}
