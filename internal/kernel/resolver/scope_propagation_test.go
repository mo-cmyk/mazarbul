package resolver

// scope_propagation_test.go guards the draft-isolation invariant: entity events
// the resolver emits when UPDATING an existing entity (summary regen, alias
// merge, re-mention, a second resolution pass) must carry the entity's CURRENT
// scope, never default to personal. A regression here re-opens the omnigraph
// draft-leak bug, where a `remember --scope draft` note's extracted entities
// were re-emitted at personal scope and leaked into the default read set,
// surviving `scope draft discard`.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// insertEntityWithScope inserts an entity row under an explicit scope so the
// scope-propagation tests can assert that updates preserve it.
func insertEntityWithScope(t *testing.T, ctx context.Context, store *sqlite.Store, uid, displayName, scope string) {
	t.Helper()
	const stmt = `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope)
	              VALUES (?, ?, ?, ?, ?, ?, ?)`
	if _, err := store.Writer().ExecContext(ctx, stmt,
		uid, string(ontology.EntityKindPerson), displayName,
		1.0, string(ontology.ResolutionConfirmed),
		time.Now().UTC().Format(time.RFC3339Nano), scope,
	); err != nil {
		t.Fatalf("insert entity (scope=%s): %v", scope, err)
	}
}

func TestAppendEntityUpdated_PreservesEntityScope(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	log := &captureLog{}
	r := &Resolver{reader: store.Reader(), eventLog: log, logger: zap.NewNop()}

	const draftUID = "01HENTDRAFT0000000000000DR"
	const personalUID = "01HENTPERSONAL00000000PERS"
	insertEntityWithScope(t, ctx, store, draftUID, "Moonbase Alpha", string(kernel.ScopeDraft))
	insertEntityWithScope(t, ctx, store, personalUID, "Frodo", string(kernel.ScopePersonal))

	if err := r.appendEntityUpdated(ctx, ontology.Entity{UID: draftUID, DisplayName: "Moonbase Alpha"}); err != nil {
		t.Fatalf("appendEntityUpdated draft: %v", err)
	}
	if err := r.appendEntityUpdated(ctx, ontology.Entity{UID: personalUID, DisplayName: "Frodo"}); err != nil {
		t.Fatalf("appendEntityUpdated personal: %v", err)
	}

	if len(log.events) != 2 {
		t.Fatalf("want 2 EntityUpdated events, got %d", len(log.events))
	}
	if got := log.events[0].Scope; got != kernel.ScopeDraft {
		t.Errorf("draft entity update scope = %q, want %q (draft must NOT leak to personal)", got, kernel.ScopeDraft)
	}
	if got := log.events[1].Scope; got != kernel.ScopePersonal {
		t.Errorf("personal entity update scope = %q, want %q (must stay personal)", got, kernel.ScopePersonal)
	}
}

func TestAppendIdentifierAdded_StampsGivenScope(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	log := &captureLog{}
	r := &Resolver{eventLog: log, logger: zap.NewNop()}

	r.appendIdentifierAdded(ctx, kernel.ScopeDraft, "01HENTDRAFT0000000000000DR", "Moonbase Alpha")
	if len(log.events) != 1 {
		t.Fatalf("want 1 entityIdentifierAdded event, got %d", len(log.events))
	}
	if got := log.events[0].Scope; got != kernel.ScopeDraft {
		t.Errorf("identifier scope = %q, want %q (a draft entity's alias must not leak to personal)", got, kernel.ScopeDraft)
	}
}

func TestCurrentEntityScope_Defaults(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	// nil reader -> default scope, never a panic.
	rNil := &Resolver{logger: zap.NewNop()}
	if got := rNil.currentEntityScope(ctx, "01HENTX000000000000000000X"); got != kernel.DefaultScope {
		t.Errorf("nil-reader scope = %q, want default %q", got, kernel.DefaultScope)
	}

	// Missing row -> default scope (a brand-new entity is stamped by its creator).
	store := openResolverStore(t, ctx)
	rDB := &Resolver{reader: store.Reader(), logger: zap.NewNop()}
	if got := rDB.currentEntityScope(ctx, "01HENTMISSING000000000000M"); got != kernel.DefaultScope {
		t.Errorf("missing-row scope = %q, want default %q", got, kernel.DefaultScope)
	}
}
