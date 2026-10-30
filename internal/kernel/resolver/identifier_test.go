package resolver

// identifier_test.go pins the W1.4 fix: the resolver's previously dead
// step-2 (entity_identifiers lookup) now merges alternate surface forms
// onto the same Entity instead of forking a fresh UID per casing or
// abbreviation.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func TestIdentifierMatch_HitsAliasRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT00000000000000ROSECT"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKindPerson, "Eowyn")
	insertIdentifier(t, ctx, store, entityUID, "Eowyn", ontology.IdentifierKindAlias)
	insertIdentifier(t, ctx, store, entityUID, "Lady Eowyn", ontology.IdentifierKindAlias)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "Eowyn",
		Kind:        ontology.EntityKindPerson,
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != entityUID {
		t.Errorf("uid = %q, want %q (alias should resolve to the original entity)", uid, entityUID)
	}
}

func TestIdentifierMatch_CaseInsensitive(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	const entityUID = "01HENT0000000000000LCSENS"
	insertEntity(t, ctx, store, entityUID, ontology.EntityKindPerson, "Eowyn")
	insertIdentifier(t, ctx, store, entityUID, "Eowyn", ontology.IdentifierKindAlias)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "eowyn",
		Kind:        ontology.EntityKindPerson,
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != entityUID {
		t.Errorf("uid = %q, want %q (lowercase alias should still match)", uid, entityUID)
	}
}

func TestIdentifierMatch_KindGate(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	// Place "Berlin" pre-existing; Person mention named "Berlin" must NOT
	// resolve to the place via the identifier path.
	const placeUID = "01HENT00000000000000BERLPL"
	insertEntity(t, ctx, store, placeUID, ontology.EntityKindPlace, "Berlin")
	insertIdentifier(t, ctx, store, placeUID, "Berlin", ontology.IdentifierKindAlias)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "Berlin",
		Kind:        ontology.EntityKindPerson,
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != "" {
		t.Errorf("uid = %q, want empty (cross-kind collision must not match)", uid)
	}
}

func TestIdentifierMatch_Miss(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.identifierMatch(ctx, ontology.Mention{
		DisplayName: "nobody",
		Kind:        ontology.EntityKindPerson,
	})
	if err != nil {
		t.Fatalf("identifierMatch: %v", err)
	}
	if uid != "" {
		t.Errorf("uid = %q, want empty on miss", uid)
	}
}

func insertEntity(t *testing.T, ctx context.Context, store *sqlite.Store, uid string, kind ontology.EntityKind, displayName string) {
	t.Helper()
	const stmt = `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time)
	              VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := store.Writer().ExecContext(ctx, stmt,
		uid, string(kind), displayName,
		1.0, string(ontology.ResolutionConfirmed),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert entity: %v", err)
	}
}

func insertIdentifier(t *testing.T, ctx context.Context, store *sqlite.Store, entityUID, identifier string, kind ontology.IdentifierKind) {
	t.Helper()
	const stmt = `INSERT INTO entity_identifiers (entity_uid, identifier, kind, source, added_at)
	              VALUES (?, ?, ?, ?, ?)`
	if _, err := store.Writer().ExecContext(ctx, stmt,
		entityUID, identifier, string(kind),
		string(ontology.IdentifierSourceExtracted),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert identifier: %v", err)
	}
}
