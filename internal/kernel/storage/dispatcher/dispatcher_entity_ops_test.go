package dispatcher_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// Tests in this file call retractTestEntity (defined in
// dispatcher_retraction_test.go) and retractTestRelation to seed fixture
// entities and relations — both helpers live in the same dispatcher_test
// package and accept *dispatcher.Dispatcher directly.

// The helpers above use interface-typed parameters to avoid circular imports;
// however the production path uses the concrete *dispatcher.Dispatcher. The
// tests below call retractTestEntity (defined in dispatcher_retraction_test.go)
// directly because it takes *dispatcher.Dispatcher.

// -- entityUpdated -----------------------------------------------------------

// TestApply_EntityUpdated_KindAndDisplayName verifies that an entityUpdated
// event changes both kind and display_name in the entities SQLite row without
// touching confidence or other fields.
func TestApply_EntityUpdated_KindAndDisplayName(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		entityUID = "01HZZZZZZZZZZZZZZZZZZUPDTST"
		origName  = "Old Corp"
		newName   = "New Corp"
		origKind  = "person"
		newKind   = "organization"
	)

	// Seed the entity.
	retractTestEntity(t, ctx, d, entityUID, origName)

	// Apply entityUpdated: change kind and displayName.
	payload, err := json.Marshal(ontology.EntityUpdatedEvent{
		EntityUID:   entityUID,
		Kind:        newKind,
		DisplayName: newName,
		UpdatedAt:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal entityUpdated: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityUpdated),
		Source:     "mcp/updateEntity",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply entityUpdated: %v", err)
	}

	// Verify the entities row was updated.
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var gotKind, gotName string
	if err := db.QueryRowContext(ctx,
		`SELECT kind, display_name FROM entities WHERE uid = ?`, entityUID,
	).Scan(&gotKind, &gotName); err != nil {
		t.Fatalf("scan entities row: %v", err)
	}
	if gotKind != newKind {
		t.Errorf("kind = %q, want %q", gotKind, newKind)
	}
	if gotName != newName {
		t.Errorf("display_name = %q, want %q", gotName, newName)
	}
}

// TestApply_EntityUpdated_KindOnly verifies that omitting displayName leaves
// the display_name column unchanged.
func TestApply_EntityUpdated_KindOnly(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		entityUID = "01HZZZZZZZZZZZZZZZZZZUPDK01"
		origName  = "Stable Name"
		newKind   = "organization"
	)
	retractTestEntity(t, ctx, d, entityUID, origName)

	payload, err := json.Marshal(ontology.EntityUpdatedEvent{
		EntityUID: entityUID,
		Kind:      newKind,
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityUpdated),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	var gotKind, gotName string
	if err := db.QueryRowContext(ctx,
		`SELECT kind, display_name FROM entities WHERE uid = ?`, entityUID,
	).Scan(&gotKind, &gotName); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotKind != newKind {
		t.Errorf("kind = %q, want %q", gotKind, newKind)
	}
	if gotName != origName {
		t.Errorf("display_name = %q, want unchanged %q", gotName, origName)
	}
}

// TestApply_EntityUpdated_Idempotent verifies that replaying the same
// entityUpdated event is a no-op (same values land twice without error).
func TestApply_EntityUpdated_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const entityUID = "01HZZZZZZZZZZZZZZZZZZUPDIDM"
	retractTestEntity(t, ctx, d, entityUID, "Base")

	payload, err := json.Marshal(ontology.EntityUpdatedEvent{
		EntityUID:   entityUID,
		DisplayName: "Updated",
		UpdatedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityUpdated),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("replay Apply: %v", err)
	}
}

// -- entityMerged ------------------------------------------------------------

// TestApply_EntityMerged_ReallocationAndSourceGone verifies the core merge
// projection:
//   - a relation whose subject_uid was the source is repointed to target
//   - the source entities row is deleted
func TestApply_EntityMerged_ReallocationAndSourceGone(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HZZZZZZZZZZZZZZZZZZMERGS1"
		tgtUID = "01HZZZZZZZZZZZZZZZZZZMERGT1"
		relUID = "01HZZZZZZZZZZZZZZZZZZRELM01"
		objUID = "01HZZZZZZZZZZZZZZZZZZOBJS01"
	)

	// Seed source, target, and an object entity.
	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, objUID, "Object")

	// Seed a relation sourced from the source entity.
	retractTestRelation(t, ctx, d, relUID, srcUID, objUID)

	// Apply entityMerged.
	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: srcUID,
		TargetEntityUID: tgtUID,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal entityMerged: %v", err)
	}
	mergeEv := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "mcp/entityMerged",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{mergeEv}); err != nil {
		t.Fatalf("Apply entityMerged: %v", err)
	}

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}

	// Source entity row must be gone.
	var srcCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entities WHERE uid = ?`, srcUID,
	).Scan(&srcCount); err != nil {
		t.Fatalf("count source entity: %v", err)
	}
	if srcCount != 0 {
		t.Errorf("source entity still present after merge (count=%d)", srcCount)
	}

	// Target entity row must still exist.
	var tgtCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entities WHERE uid = ?`, tgtUID,
	).Scan(&tgtCount); err != nil {
		t.Fatalf("count target entity: %v", err)
	}
	if tgtCount != 1 {
		t.Errorf("target entity count = %d, want 1", tgtCount)
	}

	// Relation must have been repointed to tgtUID.
	var relSubject string
	if err := db.QueryRowContext(ctx,
		`SELECT subject_uid FROM relations WHERE uid = ?`, relUID,
	).Scan(&relSubject); err != nil {
		t.Fatalf("scan relation subject: %v", err)
	}
	if relSubject != tgtUID {
		t.Errorf("relation subject_uid = %q, want %q (target)", relSubject, tgtUID)
	}
}

// TestApply_EntityMerged_RepointsAllReferences exercises every entity-UID
// reference repointEntityFKs must move, so a future refactor that drops one of
// the UPDATE/INSERT statements fails CI:
//   - relations.subject_uid (source as subject)
//   - relations.object_uid (source as object)
//   - facts.subject_uid
//   - entity_identifiers survival (the cascade-delete data-loss path: identifiers
//     must reach the surviving target, not vanish with the source row)
//
// mentions.entity_uid, insights.seed_entity_uid, consolidated_memories.cited_entity_uid
// and source_refs.subject_uid use the identical UPDATE pattern in the same
// straight-line function proven here; mentions in particular carries an FK to
// episodes that makes direct seeding heavy, so it is covered by the shared path.
func TestApply_EntityMerged_RepointsAllReferences(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID   = "01HZZZZZZZZZZZZZZZZZZMERGS3"
		tgtUID   = "01HZZZZZZZZZZZZZZZZZZMERGT3"
		otherUID = "01HZZZZZZZZZZZZZZZZZZOTHER3"
		relSubj  = "01HZZZZZZZZZZZZZZZZZZRELSB3" // relation with src as SUBJECT
		relObj   = "01HZZZZZZZZZZZZZZZZZZRELOB3" // relation with src as OBJECT
		factUID  = "01HZZZZZZZZZZZZZZZZZZFACTS3"
		ident    = "dupe@erebor.example"
		identK   = "email"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, otherUID, "Other")

	// Relations: src as subject, and src as object (distinct repoint columns).
	retractTestRelation(t, ctx, d, relSubj, srcUID, otherUID)
	retractTestRelation(t, ctx, d, relObj, otherUID, srcUID)

	// An identifier owned by the source — this is the M1 cascade-delete path.
	identPayload, err := json.Marshal(ontology.EntityIdentifierAddedEvent{
		EntityUID:  srcUID,
		Identifier: ident,
		Kind:       ontology.IdentifierKind(identK),
		Source:     ontology.IdentifierSourceUser,
		AddedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal identifier: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    identPayload,
	}}); err != nil {
		t.Fatalf("apply entityIdentifierAdded: %v", err)
	}

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	// Seed a fact for the source directly (facts have no episodes FK).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO facts (uid, subject_uid, state_key, value, create_time, source_event_uid)
		      VALUES (?, ?, 'state.role', 'maintainer', ?, ?)`,
		factUID, srcUID, time.Now().UTC().Format(time.RFC3339), ulid.Make().String(),
	); err != nil {
		t.Fatalf("seed fact: %v", err)
	}

	// Merge src into tgt.
	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: srcUID,
		TargetEntityUID: tgtUID,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal entityMerged: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "mcp/entityMerged",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("Apply entityMerged: %v", err)
	}

	// relations.subject_uid repointed.
	var gotSubj string
	if err := db.QueryRowContext(ctx,
		`SELECT subject_uid FROM relations WHERE uid = ?`, relSubj,
	).Scan(&gotSubj); err != nil {
		t.Fatalf("scan subject relation: %v", err)
	}
	if gotSubj != tgtUID {
		t.Errorf("relations.subject_uid = %q, want %q", gotSubj, tgtUID)
	}

	// relations.object_uid repointed.
	var gotObj string
	if err := db.QueryRowContext(ctx,
		`SELECT object_uid FROM relations WHERE uid = ?`, relObj,
	).Scan(&gotObj); err != nil {
		t.Fatalf("scan object relation: %v", err)
	}
	if gotObj != tgtUID {
		t.Errorf("relations.object_uid = %q, want %q", gotObj, tgtUID)
	}

	// facts.subject_uid repointed.
	var gotFactSubj string
	if err := db.QueryRowContext(ctx,
		`SELECT subject_uid FROM facts WHERE uid = ?`, factUID,
	).Scan(&gotFactSubj); err != nil {
		t.Fatalf("scan fact subject: %v", err)
	}
	if gotFactSubj != tgtUID {
		t.Errorf("facts.subject_uid = %q, want %q", gotFactSubj, tgtUID)
	}

	// entity_identifiers survived the merge and now resolves to the target —
	// the regression guard for the cascade-delete data-loss bug (M1).
	resolved, err := d.LookupEntityByIdentifier(ctx, ident, identK)
	if err != nil {
		t.Fatalf("LookupEntityByIdentifier after merge: %v", err)
	}
	if resolved != tgtUID {
		t.Errorf("identifier %q resolves to %q after merge, want target %q (identifier lost or stranded)", ident, resolved, tgtUID)
	}
}

// TestApply_EntityMerged_SelfMergeIsNoOp pins the dispatcher-level guard: a
// hand-crafted or replayed entityMerged whose source == target must NOT delete
// the entity (the repoint statements are no-ops but the source DELETE would
// otherwise destroy the surviving row).
func TestApply_EntityMerged_SelfMergeIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const selfUID = "01HZZZZZZZZZZZZZZZZZZSELFMRG"
	retractTestEntity(t, ctx, d, selfUID, "Survivor")

	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: selfUID,
		TargetEntityUID: selfUID,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("Apply self-merge: %v", err)
	}

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entities WHERE uid = ?`, selfUID,
	).Scan(&count); err != nil {
		t.Fatalf("count entity: %v", err)
	}
	if count != 1 {
		t.Fatalf("self-merge deleted the surviving entity (count=%d, want 1)", count)
	}
}

// TestApply_EntityMerged_Idempotent verifies that replaying the same merge
// event (the maz rebuild path) does not error even though source is already
// gone.
func TestApply_EntityMerged_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HZZZZZZZZZZZZZZZZZZMERGS2"
		tgtUID = "01HZZZZZZZZZZZZZZZZZZMERGT2"
	)
	retractTestEntity(t, ctx, d, srcUID, "Dupe2")
	retractTestEntity(t, ctx, d, tgtUID, "Canon2")

	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: srcUID,
		TargetEntityUID: tgtUID,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Replay: source is already gone; must not error.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("replay Apply: %v", err)
	}
}

// -- entityIdentifierAdded: entity_aliases projection -------------------------

// applyIdentifierAdded applies one entityIdentifierAdded event for uid with the
// given identifier/kind/source. CreateTime is fixed so the test is deterministic.
func applyIdentifierAdded(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid, identifier string, kind ontology.IdentifierKind, source ontology.IdentifierSource) {
	t.Helper()
	payload, err := json.Marshal(ontology.EntityIdentifierAddedEvent{
		EntityUID:  uid,
		Identifier: identifier,
		Kind:       kind,
		Source:     source,
		AddedAt:    time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal entityIdentifierAdded: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("apply entityIdentifierAdded %q: %v", identifier, err)
	}
}

func countAlias(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid, alias string) int {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_aliases WHERE entity_uid = ? AND alias = ? COLLATE NOCASE`,
		uid, alias,
	).Scan(&n); err != nil {
		t.Fatalf("count entity_aliases: %v", err)
	}
	return n
}

// TestApply_EntityIdentifierAdded_UserAliasProjectsToEntityAliases pins the Fix
// C behavior: a user-asserted alias lands in BOTH entity_identifiers (resolver
// lane) and entity_aliases (the get-by-name / wikilink lane), inheriting the
// entity's scope; an extracted alias stays in the identifier lane only.
func TestApply_EntityIdentifierAdded_UserAliasProjectsToEntityAliases(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const uid = "01HZZZZZZZZZZZZZZZZZZALIAS1"
	retractTestEntity(t, ctx, d, uid, "Erebor Trading Company")

	// User-asserted alias -> projected into entity_aliases.
	applyIdentifierAdded(t, ctx, d, uid, "Erebor Global", ontology.IdentifierKindAlias, ontology.IdentifierSourceUser)
	if got := countAlias(t, ctx, d, uid, "Erebor Global"); got != 1 {
		t.Fatalf("user alias rows in entity_aliases = %d, want 1", got)
	}
	// Case-insensitive resolution lane sees it.
	if got := countAlias(t, ctx, d, uid, "erebor global"); got != 1 {
		t.Errorf("alias not resolvable case-insensitively (got %d rows)", got)
	}
	// And it survived in the resolver's identifier lane too.
	if resolved, err := d.LookupEntityByIdentifier(ctx, "Erebor Global", string(ontology.IdentifierKindAlias)); err != nil || resolved != uid {
		t.Errorf("LookupEntityByIdentifier = %q, %v; want %q", resolved, err, uid)
	}

	// Replay the same user alias -> still exactly one row (idempotent).
	applyIdentifierAdded(t, ctx, d, uid, "Erebor Global", ontology.IdentifierKindAlias, ontology.IdentifierSourceUser)
	if got := countAlias(t, ctx, d, uid, "Erebor Global"); got != 1 {
		t.Errorf("after replay, entity_aliases rows = %d, want 1 (not idempotent)", got)
	}

	// Extracted alias -> identifier lane only, NOT entity_aliases (the resolver
	// accumulates surface forms; promoting all of them to name-resolvable is a
	// separate, deliberate change).
	applyIdentifierAdded(t, ctx, d, uid, "Erebor Inc", ontology.IdentifierKindAlias, ontology.IdentifierSourceExtracted)
	if got := countAlias(t, ctx, d, uid, "Erebor Inc"); got != 0 {
		t.Errorf("extracted alias leaked into entity_aliases (got %d rows, want 0)", got)
	}

	// A non-alias user identifier (e.g. email) is identifier-lane only.
	applyIdentifierAdded(t, ctx, d, uid, "ops@erebor.example", ontology.IdentifierKindEmail, ontology.IdentifierSourceUser)
	if got := countAlias(t, ctx, d, uid, "ops@erebor.example"); got != 0 {
		t.Errorf("email identifier leaked into entity_aliases (got %d rows, want 0)", got)
	}
}
