package resolver

// domain_identifier_test.go pins the behaviour of entity_identifiers rows whose
// kind is a domain-specific type (e.g. "hrb") rather than the alias kind.
//
// Behaviour (since the strong-identifier lane landed in identifierMatch):
//
//  1. identifierMatch consults two lanes. Lane 1: alias rows, gated on the
//     mention's entity kind (unchanged). Lane 2: typed non-alias rows (email,
//     username, phone, domain types like hrb/lei/vin) match the mention's
//     surface form verbatim (case-insensitive), NOT gated on entity kind,
//     guarded by strongIdentifierMinLength. The catch-all "id" kind is
//     excluded: createEntity preserves unclassified values as "id", so those
//     rows carry no external-ID semantics. Typed rows only enter the table
//     through explicit structured writes (createEntity, schema packs),
//     so a verbatim match is a deliberate external-ID anchor — the blocking
//     stage the dynamic-ontology design calls for.
//
//  2. The structured lookup path (dispatcher.LookupEntityByIdentifier) also
//     queries by arbitrary (identifier, kind) pair; that path is used by the
//     createEntity tool and the sync transport.
//
// The tests below pin both lanes plus the minimum-length guard so the
// anchoring semantics cannot drift silently.

import (
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestDomainIdentifier_SQLLookup_FindsEntity pins that a domain-kind identifier
// row (kind='hrb') written to entity_identifiers is queryable directly by
// (identifier, kind). This is the foundation of the structured lookup path;
// it does not involve the resolver's identifierMatch at all.
func TestDomainIdentifier_SQLLookup_FindsEntity(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT000000000000HRB00001"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKind("extension:business_de:company"), "Muster GmbH")
	insertIdentifier(t, ctx, store, entityUID, "HRB 12345 B", ontology.IdentifierKind("hrb"))

	// Direct SQL — the same query dispatcher.LookupEntityByIdentifier runs.
	var gotUID string
	err := store.Reader().QueryRowContext(ctx,
		`SELECT entity_uid FROM entity_identifiers WHERE identifier = ? AND kind = ? LIMIT 1`,
		"HRB 12345 B", "hrb",
	).Scan(&gotUID)
	if err != nil {
		t.Fatalf("SQL lookup (identifier='HRB 12345 B', kind='hrb'): %v", err)
	}
	if gotUID != entityUID {
		t.Errorf("SQL lookup returned uid=%q, want %q", gotUID, entityUID)
	}
}

// TestDomainIdentifier_IdentifierMatch_DomainRowAnchorsMention pins the
// strong-identifier lane: a mention whose DisplayName verbatim-matches a
// domain-kind identifier row (kind='hrb') resolves to the owning entity,
// even though the mention's entity kind (organization, as the extractor
// would type it) differs from the entity's pack kind.
func TestDomainIdentifier_IdentifierMatch_DomainRowAnchorsMention(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT000000000000HRB00002"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKind("extension:business_de:company"), "Muster GmbH")
	// Write an hrb row — NOT an alias row.
	insertIdentifier(t, ctx, store, entityUID, "HRB 12345 B", ontology.IdentifierKind("hrb"))

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	// The extractor would type a bare registry-number mention as a plain
	// organization; the strong-identifier lane must anchor it regardless.
	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "HRB 12345 B",
		Kind:        ontology.EntityKindOrganization,
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != entityUID {
		t.Errorf("strong-identifier lane: got uid=%q, want %q (hrb row should anchor the mention)", uid, entityUID)
	}
}

// TestDomainIdentifier_IdentifierMatch_AliasRowDoesAnchor confirms the alias
// lane is unchanged: an alias row resolves a mention whose display name and
// entity kind both match.
func TestDomainIdentifier_IdentifierMatch_AliasRowDoesAnchor(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT000000000000HRB00003"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKind("extension:business_de:company"), "Muster GmbH")
	// Write an alias row with the same value — identifierMatch must find this.
	insertIdentifier(t, ctx, store, entityUID, "Muster GmbH", ontology.IdentifierKindAlias)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "Muster GmbH",
		Kind:        ontology.EntityKind("extension:business_de:company"),
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != entityUID {
		t.Errorf("alias row should resolve: got uid=%q, want %q", uid, entityUID)
	}
}

// TestDomainIdentifier_MultipleKinds_BothLanesResolve seeds both an alias row
// and an hrb row for the same entity and confirms each lane resolves its own
// surface form to the same entity.
func TestDomainIdentifier_MultipleKinds_BothLanesResolve(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT000000000000HRB00004"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKind("extension:business_de:company"), "Muster GmbH")
	insertIdentifier(t, ctx, store, entityUID, "Muster GmbH", ontology.IdentifierKindAlias)
	insertIdentifier(t, ctx, store, entityUID, "HRB 12345 B", ontology.IdentifierKind("hrb"))

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	// Alias value → resolves via lane 1.
	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "Muster GmbH",
		Kind:        ontology.EntityKind("extension:business_de:company"),
	})
	if err != nil {
		t.Fatalf("identifierMatch (alias): %v", err)
	}
	if uid != entityUID {
		t.Errorf("alias match: got uid=%q, want %q", uid, entityUID)
	}

	// HRB value → resolves via lane 2 (strong identifier), kind-agnostic.
	uid2, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "HRB 12345 B",
		Kind:        ontology.EntityKindOrganization,
	})
	if err != nil {
		t.Fatalf("identifierMatch (hrb): %v", err)
	}
	if uid2 != entityUID {
		t.Errorf("hrb strong-identifier match: got uid=%q, want %q", uid2, entityUID)
	}
}

// TestDomainIdentifier_ShortIdentifier_DoesNotAnchor pins the minimum-length
// guard: a non-alias identifier shorter than strongIdentifierMinLength must
// never anchor a mention, so a degenerate value like "12" cannot cross-link
// every entity that happens to share it.
func TestDomainIdentifier_ShortIdentifier_DoesNotAnchor(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT000000000000HRB00006"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKind("extension:business_de:company"), "Muster GmbH")
	insertIdentifier(t, ctx, store, entityUID, "12", ontology.IdentifierKind("hrb"))

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "12",
		Kind:        ontology.EntityKindOrganization,
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != "" {
		t.Errorf("short identifier (len < %d) must not anchor: got uid=%q", entitymatch.StrongIdentifierMinLength, uid)
	}
}

// TestDomainIdentifier_GenericIDKind_DoesNotAnchor pins that the catch-all
// "id" identifier kind never anchors a mention through the strong-identifier
// lane, regardless of length: createEntity preserves unclassified identifier
// values as kind "id" (e.g. a bare year "2024"), and letting those anchor
// would cross-link unrelated entities that share a generic value.
func TestDomainIdentifier_GenericIDKind_DoesNotAnchor(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT000000000000HRB00007"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKind("extension:business_de:company"), "Muster GmbH")
	// An unclassified value preserved as kind "id" — passes the length guard
	// but must still be excluded from the strong-identifier lane.
	insertIdentifier(t, ctx, store, entityUID, "2024", ontology.IdentifierKindID)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "2024",
		Kind:        ontology.EntityKindOrganization,
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != "" {
		t.Errorf("generic id-kind row must not anchor: got uid=%q", uid)
	}
}

// TestDomainIdentifier_CaseInsensitiveSQL_DomainKindLookup verifies that the
// structured SQL lookup (not identifierMatch) is also case-insensitive when
// the caller normalises the query to LOWER — pinning the foundation for a
// future LookupEntityByIdentifier extension that accepts a case-insensitive
// flag.
func TestDomainIdentifier_CaseInsensitiveSQL_DomainKindLookup(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT000000000000HRB00005"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKind("extension:business_de:company"), "Muster GmbH")
	// Store canonical form.
	insertIdentifier(t, ctx, store, entityUID, "HRB 12345 B", ontology.IdentifierKind("hrb"))

	// Query with LOWER() on both sides to simulate a case-insensitive lookup.
	var gotUID string
	err := store.Reader().QueryRowContext(ctx,
		`SELECT entity_uid FROM entity_identifiers
		  WHERE LOWER(identifier) = LOWER(?) AND kind = ? LIMIT 1`,
		"hrb 12345 b", "hrb",
	).Scan(&gotUID)
	if err != nil {
		t.Fatalf("case-insensitive SQL lookup: %v", err)
	}
	if gotUID != entityUID {
		t.Errorf("case-insensitive lookup returned uid=%q, want %q", gotUID, entityUID)
	}
}
