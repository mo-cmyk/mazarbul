package dispatcher_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// makeEntityKindEvent builds a kernel.Event whose payload is a valid
// EntityKindRegisteredEvent, ready for Apply.
func makeEntityKindEvent(t *testing.T, uid string, p ontology.EntityKindRegisteredEvent) kernel.Event {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal entity kind event: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
		Source:     "test",
		Confidence: 1.0,
		Payload:    raw,
	}
}

// makeSchemaEvent builds a kernel.Event whose payload is a valid
// SchemaRegisteredEvent.
func makeSchemaEvent(t *testing.T, uid string, p ontology.SchemaRegisteredEvent) kernel.Event {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal schema event: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindSchemaRegistered),
		Source:     "test",
		Confidence: 1.0,
		Payload:    raw,
	}
}

// TestApplyEntityKindRegistered verifies the dispatcher upserts into
// entity_kinds and is idempotent under replay.
func TestApplyEntityKindRegistered(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	ev := makeEntityKindEvent(t, "01HONTOLOGY0000000000KIND1", ontology.EntityKindRegisteredEvent{
		Name:        "vehicle",
		DisplayName: "Vehicle",
		Description: "A motorised conveyance.",
		SameAs:      []string{"https://schema.org/Vehicle"},
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByUser,
		AddedAt:     now,
	})

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var name, displayName, provenance, addedBy string
	if err := db.QueryRowContext(ctx,
		`SELECT name, display_name, provenance, added_by FROM entity_kinds WHERE name = 'vehicle'`,
	).Scan(&name, &displayName, &provenance, &addedBy); err != nil {
		t.Fatalf("query entity_kinds: %v", err)
	}
	if name != "vehicle" {
		t.Errorf("name = %q, want vehicle", name)
	}
	if displayName != "Vehicle" {
		t.Errorf("display_name = %q, want Vehicle", displayName)
	}
	if provenance != "user" {
		t.Errorf("provenance = %q, want user", provenance)
	}
	if addedBy != "user" {
		t.Errorf("added_by = %q, want user", addedBy)
	}

	// Idempotent replay: apply again, row must not duplicate.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply replay: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_kinds WHERE name = 'vehicle'`,
	).Scan(&n); err != nil {
		t.Fatalf("count replay: %v", err)
	}
	if n != 1 {
		t.Errorf("count after replay = %d, want 1", n)
	}
}

// TestApplyEntityKindRegistered_CoreGuard verifies that a user-provenance
// event cannot overwrite an existing core-provenance row.
func TestApplyEntityKindRegistered_CoreGuard(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)

	// First: seed a core row.
	coreEv := makeEntityKindEvent(t, "01HONTOLOGY0000000000CORE1", ontology.EntityKindRegisteredEvent{
		Name:        "person",
		DisplayName: "Person",
		Description: "A human individual.",
		Provenance:  "core",
		AddedBy:     ontology.EntityKindAddedBySeed,
		AddedAt:     now,
	})
	if err := d.Apply(ctx, []kernel.Event{coreEv}); err != nil {
		t.Fatalf("Apply core: %v", err)
	}

	// Then: attempt an overwrite from a user event.
	userEv := makeEntityKindEvent(t, "01HONTOLOGY0000000000USER1", ontology.EntityKindRegisteredEvent{
		Name:        "person",
		DisplayName: "Person (hacked)",
		Description: "Overwritten.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByUser,
		AddedAt:     now,
	})
	if err := d.Apply(ctx, []kernel.Event{userEv}); err != nil {
		t.Fatalf("Apply user overwrite: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var displayName string
	if err := db.QueryRowContext(ctx,
		`SELECT display_name FROM entity_kinds WHERE name = 'person'`,
	).Scan(&displayName); err != nil {
		t.Fatalf("query: %v", err)
	}
	if displayName != "Person" {
		t.Errorf("display_name = %q after user overwrite; core guard should have blocked it", displayName)
	}
}

// TestApplyEntityKindRegistered_BadPropertySchema verifies that an event with
// an invalid propertySchema is stored without the attr_schema column but still
// inserts the kind row.
func TestApplyEntityKindRegistered_BadPropertySchema(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	ev := makeEntityKindEvent(t, "01HONTOLOGY0000000000BAD01", ontology.EntityKindRegisteredEvent{
		Name:           "broken_kind",
		DisplayName:    "Broken Kind",
		Description:    "Has an invalid property schema.",
		PropertySchema: json.RawMessage(`{"type": 12}`), // type must be a string
		Provenance:     "user",
		AddedBy:        ontology.EntityKindAddedByUser,
		AddedAt:        now,
	})
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v (must not error on invalid propertySchema)", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var name string
	var attrSchema *string
	if err := db.QueryRowContext(ctx,
		`SELECT name, attr_schema FROM entity_kinds WHERE name = 'broken_kind'`,
	).Scan(&name, &attrSchema); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "broken_kind" {
		t.Errorf("kind not inserted at all; name = %q", name)
	}
	if attrSchema != nil {
		t.Errorf("attr_schema = %q, want NULL (bad schema should be dropped)", *attrSchema)
	}
}

// TestApplySchemaRegistered verifies the dispatcher upserts into
// ontology_schemas and is idempotent under replay.
func TestApplySchemaRegistered(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	ev := makeSchemaEvent(t, "01HONTOLOGY0000000000SCH01", ontology.SchemaRegisteredEvent{
		SchemaKind: "entityKind:vehicle",
		Version:    "v0.1.0",
		Definition: json.RawMessage(`{"type":"object","properties":{"vin":{"type":"string"}}}`),
		Provenance: "user",
		AddedBy:    ontology.EntityKindAddedByUser,
		AddedAt:    now,
	})
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var schemaKind, version string
	if err := db.QueryRowContext(ctx,
		`SELECT schema_kind, version FROM ontology_schemas WHERE schema_kind = 'entityKind:vehicle'`,
	).Scan(&schemaKind, &version); err != nil {
		t.Fatalf("query ontology_schemas: %v", err)
	}
	if schemaKind != "entityKind:vehicle" {
		t.Errorf("schema_kind = %q, want entityKind:vehicle", schemaKind)
	}
	if version != "v0.1.0" {
		t.Errorf("version = %q, want v0.1.0", version)
	}

	// Idempotent replay.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply replay: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ontology_schemas WHERE schema_kind = 'entityKind:vehicle'`,
	).Scan(&n); err != nil {
		t.Fatalf("count replay: %v", err)
	}
	if n != 1 {
		t.Errorf("count after replay = %d, want 1", n)
	}
}

// TestApplySchemaRegistered_InvalidDefinition verifies that an event with a
// definition that fails to compile is skipped (no row inserted) without
// aborting Rebuild.
func TestApplySchemaRegistered_InvalidDefinition(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	ev := makeSchemaEvent(t, "01HONTOLOGY0000000000BAD02", ontology.SchemaRegisteredEvent{
		SchemaKind: "entityKind:broken",
		Version:    "v0.1.0",
		Definition: json.RawMessage(`{"type": 12}`), // invalid JSON Schema
		Provenance: "user",
		AddedBy:    ontology.EntityKindAddedByUser,
		AddedAt:    now,
	})
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v (must not error on invalid definition)", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ontology_schemas WHERE schema_kind = 'entityKind:broken'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 (invalid definition must be skipped)", n)
	}
}

// TestApplySchemaRegistered_CoreGuard verifies that a non-core schema event
// cannot overwrite an existing core-provenance ontology_schemas row, mirroring
// the entity-kind core guard.
func TestApplySchemaRegistered_CoreGuard(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)

	coreEv := makeSchemaEvent(t, "01HONTOLOGY00000000SCHCORE1", ontology.SchemaRegisteredEvent{
		SchemaKind: "entityKind:person",
		Version:    "v0.1.0",
		Definition: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		Provenance: "core",
		AddedBy:    ontology.EntityKindAddedBySeed,
		AddedAt:    now,
	})
	if err := d.Apply(ctx, []kernel.Event{coreEv}); err != nil {
		t.Fatalf("Apply core: %v", err)
	}

	// A user event at a HIGHER version still must not overwrite a core row.
	userEv := makeSchemaEvent(t, "01HONTOLOGY00000000SCHUSER1", ontology.SchemaRegisteredEvent{
		SchemaKind: "entityKind:person",
		Version:    "v0.2.0",
		Definition: json.RawMessage(`{"type":"object","properties":{"hacked":{"type":"string"}}}`),
		Provenance: "user",
		AddedBy:    ontology.EntityKindAddedByUser,
		AddedAt:    now,
	})
	if err := d.Apply(ctx, []kernel.Event{userEv}); err != nil {
		t.Fatalf("Apply user overwrite: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var version, provenance, definition string
	if err := db.QueryRowContext(ctx,
		`SELECT version, provenance, definition FROM ontology_schemas WHERE schema_kind = 'entityKind:person'`,
	).Scan(&version, &provenance, &definition); err != nil {
		t.Fatalf("query: %v", err)
	}
	if version != "v0.1.0" || provenance != "core" {
		t.Errorf("row = (version=%q, provenance=%q) after user overwrite; core guard should have blocked it", version, provenance)
	}
	if definition != `{"type":"object","properties":{"name":{"type":"string"}}}` {
		t.Errorf("definition overwritten by non-core event: %q", definition)
	}
}

// TestApplySchemaRegistered_VersionMonotonic verifies the registry only moves
// forward: a strictly newer version replaces the row, a same-version event is
// immutable (no-op), and a lower version is rejected.
func TestApplySchemaRegistered_VersionMonotonic(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	const kind = "entityKind:vehicle"

	apply := func(uid, version, def string) {
		t.Helper()
		ev := makeSchemaEvent(t, uid, ontology.SchemaRegisteredEvent{
			SchemaKind: kind,
			Version:    version,
			Definition: json.RawMessage(def),
			Provenance: "user",
			AddedBy:    ontology.EntityKindAddedByUser,
			AddedAt:    now,
		})
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Apply %s: %v", version, err)
		}
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	read := func() (string, string) {
		t.Helper()
		var version, definition string
		if err := db.QueryRowContext(ctx,
			`SELECT version, definition FROM ontology_schemas WHERE schema_kind = ?`, kind,
		).Scan(&version, &definition); err != nil {
			t.Fatalf("query: %v", err)
		}
		return version, definition
	}

	apply("01HONTOLOGY0000000000VER10", "v0.1.0", `{"type":"object","title":"v1"}`)

	// Newer version replaces.
	apply("01HONTOLOGY0000000000VER20", "v0.2.0", `{"type":"object","title":"v2"}`)
	if v, def := read(); v != "v0.2.0" || def != `{"type":"object","title":"v2"}` {
		t.Errorf("after v0.2.0: (version=%q, def=%q), want v0.2.0 replacement", v, def)
	}

	// Same version with a DIFFERENT definition is immutable: no replace.
	apply("01HONTOLOGY0000000000VER21", "v0.2.0", `{"type":"object","title":"v2-tampered"}`)
	if v, def := read(); v != "v0.2.0" || def != `{"type":"object","title":"v2"}` {
		t.Errorf("after same-version tamper: (version=%q, def=%q), want untouched v2", v, def)
	}

	// Lower version does not move the registry backwards.
	apply("01HONTOLOGY0000000000VER11", "v0.1.0", `{"type":"object","title":"v1-again"}`)
	if v, def := read(); v != "v0.2.0" || def != `{"type":"object","title":"v2"}` {
		t.Errorf("after lower-version event: (version=%q, def=%q), want untouched v2", v, def)
	}
}

// TestApplySchemaRegistered_InvalidSemver verifies that a schemaRegistered
// event with a non-semver Version string is skipped.
func TestApplySchemaRegistered_InvalidSemver(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	ev := makeSchemaEvent(t, "01HONTOLOGY0000000000SEM01", ontology.SchemaRegisteredEvent{
		SchemaKind: "entityKind:badsemver",
		Version:    "v01.0.0", // invalid semver
		Definition: json.RawMessage(`{"type":"object"}`),
		Provenance: "user",
		AddedBy:    ontology.EntityKindAddedByUser,
		AddedAt:    now,
	})
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v (must not error on invalid semver)", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ontology_schemas WHERE schema_kind = 'entityKind:badsemver'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 (invalid semver must be skipped)", n)
	}
}

// TestApplyEntityKindRegistered_SameAsUnion verifies the alias merge semantics:
//
//   - A rich pack event sets two aliases; a sparse import adds a third; the
//     result is all three (set-union, no duplicates, no clobber).
//   - A later richer event can still update attr_schema (COALESCE prefers the
//     incoming non-NULL schema over the existing NULL schema in the sparse case,
//     and the incoming schema always wins when present so updates are possible).
func TestApplyEntityKindRegistered_SameAsUnion(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)

	// First: a rich pack event with propertySchema and two sameAs IRIs.
	richEv := makeEntityKindEvent(t, "01HONTOLOGY000000RICH0001", ontology.EntityKindRegisteredEvent{
		Name:           "extension:schema_org:person",
		DisplayName:    "Person",
		Description:    "A person.",
		SameAs:         []string{"https://schema.org/Person", "http://www.wikidata.org/entity/Q5"},
		PropertySchema: json.RawMessage(`{"type":"object","properties":{"birthDate":{"type":"string"}}}`),
		Provenance:     "extension:schema_org_core",
		AddedBy:        ontology.EntityKindAddedByExtension,
		AddedAt:        now,
	})
	if err := d.Apply(ctx, []kernel.Event{richEv}); err != nil {
		t.Fatalf("Apply rich: %v", err)
	}

	// Then: a sparse vocab-import event with ONE new alias (not in pack's list)
	// and no propertySchema.  The union must contain all three aliases.
	sparseEv := makeEntityKindEvent(t, "01HONTOLOGY000000SPARSE01", ontology.EntityKindRegisteredEvent{
		Name:        "extension:schema_org:person",
		DisplayName: "Person",
		Description: "A person.",
		SameAs:      []string{"https://dbpedia.org/resource/Person"},
		Provenance:  "extension:schema_org",
		AddedBy:     ontology.EntityKindAddedByExtension,
		AddedAt:     now,
	})
	if err := d.Apply(ctx, []kernel.Event{sparseEv}); err != nil {
		t.Fatalf("Apply sparse: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var attrSchema, sameAs *string
	if err := db.QueryRowContext(ctx,
		`SELECT attr_schema, same_as FROM entity_kinds WHERE name = 'extension:schema_org:person'`,
	).Scan(&attrSchema, &sameAs); err != nil {
		t.Fatalf("query: %v", err)
	}
	// attr_schema from the rich pack event must survive the sparse re-import.
	if attrSchema == nil {
		t.Error("attr_schema was cleared by sparse import; want preserved from rich pack event")
	}
	// same_as must contain all three IRIs (set-union of rich-pack + sparse-import).
	if sameAs == nil {
		t.Error("same_as was cleared; want union of all aliases")
	} else {
		for _, want := range []string{
			"https://schema.org/Person",
			"http://www.wikidata.org/entity/Q5",
			"https://dbpedia.org/resource/Person",
		} {
			if !strings.Contains(*sameAs, want) {
				t.Errorf("same_as = %q missing %q; want all three aliases after union", *sameAs, want)
			}
		}
	}

	// Verify that a later richer event CAN update attr_schema (COALESCE prefers
	// incoming non-NULL over existing — updates are not frozen).
	richerEv := makeEntityKindEvent(t, "01HONTOLOGY000000RICH0002", ontology.EntityKindRegisteredEvent{
		Name:           "extension:schema_org:person",
		DisplayName:    "Person",
		Description:    "A person.",
		SameAs:         []string{"https://schema.org/Person"},
		PropertySchema: json.RawMessage(`{"type":"object","properties":{"birthDate":{"type":"string"},"deathDate":{"type":"string"}}}`),
		Provenance:     "extension:schema_org_core",
		AddedBy:        ontology.EntityKindAddedByExtension,
		AddedAt:        now,
	})
	if err := d.Apply(ctx, []kernel.Event{richerEv}); err != nil {
		t.Fatalf("Apply richer: %v", err)
	}
	var updatedSchema *string
	if err := db.QueryRowContext(ctx,
		`SELECT attr_schema FROM entity_kinds WHERE name = 'extension:schema_org:person'`,
	).Scan(&updatedSchema); err != nil {
		t.Fatalf("query richer: %v", err)
	}
	if updatedSchema == nil || !strings.Contains(*updatedSchema, "deathDate") {
		t.Errorf("attr_schema = %v after richer event; want updated schema with deathDate", updatedSchema)
	}
}

// makePredicateEvent builds a kernel.Event whose payload is a valid
// PredicateRegisteredEvent, ready for Apply.
func makePredicateEvent(t *testing.T, uid string, p ontology.PredicateRegisteredEvent) kernel.Event {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal predicate event: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindPredicateRegistered),
		Source:     "test",
		Confidence: 1.0,
		Payload:    raw,
	}
}

// TestApplyPredicateRegistered_DomainRange verifies the dispatcher persists the
// P5 domain/range columns (subject_kinds, object_kinds, same_as, property_schema)
// from a predicateRegistered event into the predicates table, and that a replay
// is idempotent.
func TestApplyPredicateRegistered_DomainRange(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	ev := makePredicateEvent(t, "01HONTOLOGY000000000PRED01", ontology.PredicateRegisteredEvent{
		Predicate:      "employed_by",
		Kind:           ontology.PredicateKindNonFunctional,
		Description:    "Subject is employed by the object organization.",
		AddedBy:        ontology.PredicateAddedByUser,
		AddedAt:        now,
		SubjectKinds:   []string{"person"},
		ObjectKinds:    []string{"organization"},
		SameAs:         []string{"https://schema.org/worksFor"},
		PropertySchema: json.RawMessage(`{"type":"object","properties":{"role":{"type":"string"}}}`),
	})

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var subjectKinds, objectKinds, sameAs, propertySchema string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(subject_kinds,''), COALESCE(object_kinds,''),
		        COALESCE(same_as,''), COALESCE(property_schema,'')
		 FROM predicates WHERE predicate = 'employed_by'`,
	).Scan(&subjectKinds, &objectKinds, &sameAs, &propertySchema); err != nil {
		t.Fatalf("query predicates: %v", err)
	}
	if subjectKinds != `["person"]` {
		t.Errorf("subject_kinds = %q, want [\"person\"]", subjectKinds)
	}
	if objectKinds != `["organization"]` {
		t.Errorf("object_kinds = %q, want [\"organization\"]", objectKinds)
	}
	if sameAs != `["https://schema.org/worksFor"]` {
		t.Errorf("same_as = %q", sameAs)
	}
	if propertySchema == "" {
		t.Error("property_schema is empty, want the JSON Schema object")
	}

	// Idempotent replay.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply replay: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM predicates WHERE predicate = 'employed_by'`,
	).Scan(&count); err != nil {
		t.Fatalf("count replay: %v", err)
	}
	if count != 1 {
		t.Errorf("count after replay = %d, want 1", count)
	}
}

// TestApplyPredicateRegistered_PreservesTypedColumns verifies that replaying an
// older / untyped predicateRegistered event for an already-typed predicate does
// not clear the P5 columns: the UPSERT COALESCEs the incoming NULLs against the
// stored values, so domain/range and propertySchema survive a re-register that
// omits the additive fields.
func TestApplyPredicateRegistered_PreservesTypedColumns(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)

	// First: a fully-typed registration.
	typed := makePredicateEvent(t, "01HONTOLOGY000000000PRED10", ontology.PredicateRegisteredEvent{
		Predicate:      "employed_by",
		Kind:           ontology.PredicateKindNonFunctional,
		Description:    "Subject is employed by the object organization.",
		AddedBy:        ontology.PredicateAddedByUser,
		AddedAt:        now,
		SubjectKinds:   []string{"person"},
		ObjectKinds:    []string{"organization"},
		SameAs:         []string{"https://schema.org/worksFor"},
		PropertySchema: json.RawMessage(`{"type":"object","properties":{"role":{"type":"string"}}}`),
	})
	if err := d.Apply(ctx, []kernel.Event{typed}); err != nil {
		t.Fatalf("Apply typed: %v", err)
	}

	// Then: an untyped re-register of the same predicate (the omitted fields
	// marshal away under omitempty and reach the UPSERT as NULL).
	untyped := makePredicateEvent(t, "01HONTOLOGY000000000PRED11", ontology.PredicateRegisteredEvent{
		Predicate:   "employed_by",
		Kind:        ontology.PredicateKindNonFunctional,
		Description: "Subject is employed by the object organization.",
		AddedBy:     ontology.PredicateAddedByUser,
		AddedAt:     now,
	})
	if err := d.Apply(ctx, []kernel.Event{untyped}); err != nil {
		t.Fatalf("Apply untyped replay: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var subjectKinds, objectKinds, sameAs, propertySchema string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(subject_kinds,''), COALESCE(object_kinds,''),
		        COALESCE(same_as,''), COALESCE(property_schema,'')
		 FROM predicates WHERE predicate = 'employed_by'`,
	).Scan(&subjectKinds, &objectKinds, &sameAs, &propertySchema); err != nil {
		t.Fatalf("query predicates: %v", err)
	}
	if subjectKinds != `["person"]` {
		t.Errorf("subject_kinds = %q after untyped replay, want preserved [\"person\"]", subjectKinds)
	}
	if objectKinds != `["organization"]` {
		t.Errorf("object_kinds = %q after untyped replay, want preserved [\"organization\"]", objectKinds)
	}
	if sameAs != `["https://schema.org/worksFor"]` {
		t.Errorf("same_as = %q after untyped replay, want preserved", sameAs)
	}
	if propertySchema == "" {
		t.Error("property_schema cleared by untyped replay; want preserved")
	}
}

// TestApplyPredicateRegistered_ExtensionAddedBy verifies that a predicate event
// emitted by the schema-pack installer (AddedBy: extension) materialises with
// added_by='extension' in the predicates table after migration 0021 extended
// the CHECK constraint.  Before that migration this would fail with a SQLite
// constraint violation.
func TestApplyPredicateRegistered_ExtensionAddedBy(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	ev := makePredicateEvent(t, "01HONTOLOGY000000EXT00001", ontology.PredicateRegisteredEvent{
		Predicate:   "manages_company",
		Kind:        ontology.PredicateKindNonFunctional,
		Description: "Subject manages the object company.",
		AddedBy:     ontology.PredicateAddedByExtension,
		AddedAt:     now,
	})
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply extension predicate: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var addedBy string
	if err := db.QueryRowContext(ctx,
		`SELECT added_by FROM predicates WHERE predicate = 'manages_company'`,
	).Scan(&addedBy); err != nil {
		t.Fatalf("query: %v", err)
	}
	if addedBy != "extension" {
		t.Errorf("added_by = %q, want extension", addedBy)
	}
}

// ---------------------------------------------------------------------------
// C. Entity properties column persistence (Greptile fix regression tests)
// ---------------------------------------------------------------------------

// TestApplyEntity_PersistsProperties verifies the entities.properties column
// behaviour introduced in migration 0023:
//
//	(a) an entity event with Properties stores the compacted JSON in the column,
//	(b) a second event for the same UID WITHOUT Properties keeps the stored value
//	    (COALESCE semantics: excluded.properties IS NULL → keep existing),
//	(c) a third event WITH new Properties replaces the stored value.
func TestApplyEntity_PersistsProperties(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	const entityUID = "01HENTITY00000PROP00000001"

	entityCreateTime := time.Unix(1_700_000_000, 0).UTC()

	// (a) Apply with Properties {"vin": "V-1"} (whitespace variant).
	entity1 := ontology.Entity{
		UID:              entityUID,
		Kind:             ontology.EntityKindOrganization,
		DisplayName:      "Mithril Corp",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       entityCreateTime,
		Properties:       json.RawMessage(`{ "vin": "V-1" }`),
	}
	if err := d.Apply(ctx, []kernel.Event{makeEntityEvent(t, entityUID, entity1)}); err != nil {
		t.Fatalf("Apply entity with properties: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var props string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(properties,'') FROM entities WHERE uid = ?`, entityUID,
	).Scan(&props); err != nil {
		t.Fatalf("query properties (step a): %v", err)
	}
	if props != `{"vin":"V-1"}` {
		t.Errorf("step a: properties = %q, want %q", props, `{"vin":"V-1"}`)
	}

	// (b) Apply a second event for the same UID without Properties.
	entity2 := ontology.Entity{
		UID:              entityUID,
		Kind:             ontology.EntityKindOrganization,
		DisplayName:      "Mithril Corp (updated)",
		Confidence:       0.9,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       entityCreateTime,
		// No Properties.
	}
	if err := d.Apply(ctx, []kernel.Event{makeEntityEvent(t, "01HENTITY00000PROP00000002", entity2)}); err != nil {
		t.Fatalf("Apply entity without properties: %v", err)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(properties,'') FROM entities WHERE uid = ?`, entityUID,
	).Scan(&props); err != nil {
		t.Fatalf("query properties (step b): %v", err)
	}
	if props != `{"vin":"V-1"}` {
		t.Errorf("step b: properties wiped by no-properties update; got %q, want %q", props, `{"vin":"V-1"}`)
	}

	// (c) Apply a third event with new Properties {"vin": "V-2"}.
	entity3 := ontology.Entity{
		UID:              entityUID,
		Kind:             ontology.EntityKindOrganization,
		DisplayName:      "Mithril Corp (v3)",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       entityCreateTime,
		Properties:       json.RawMessage(`{"vin":"V-2"}`),
	}
	if err := d.Apply(ctx, []kernel.Event{makeEntityEvent(t, "01HENTITY00000PROP00000003", entity3)}); err != nil {
		t.Fatalf("Apply entity with new properties: %v", err)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(properties,'') FROM entities WHERE uid = ?`, entityUID,
	).Scan(&props); err != nil {
		t.Fatalf("query properties (step c): %v", err)
	}
	if props != `{"vin":"V-2"}` {
		t.Errorf("step c: properties not replaced; got %q, want %q", props, `{"vin":"V-2"}`)
	}
}
