package resolver

// concurrent_test.go covers DetectConcurrentSupersede: the resolver's
// post-pull replay check that flags functional slots whose
// (subject_uid, predicate) pair has more than one open relation. This
// situation only arises when two devices independently superseded the
// same prior relation along different paths and a sync pull merged
// both arms; the test seeds that state directly into SQLite to keep
// the detection layer testable without standing up the full event log.

import (
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// newFunctionalRegistry builds a Registry whose cache contains the
// listed predicates as Functional. Skips the sqlite-backed load() path
// so detection tests don't need to seed the predicates table.
func newFunctionalRegistry(functional ...string) *Registry {
	cache := make(map[string]PredicateKind, len(functional))
	for _, p := range functional {
		cache[p] = Functional
	}
	return &Registry{mu: sync.RWMutex{}, cache: cache}
}

func TestDetectConcurrentSupersede_NoConflictsOnEmptyTable(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	r := &Resolver{
		reader:     store.Reader(),
		logger:     zap.NewNop(),
		predicates: newFunctionalRegistry("works_at"),
	}
	got, err := r.DetectConcurrentSupersede(ctx)
	if err != nil {
		t.Fatalf("DetectConcurrentSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("conflicts on empty table: %d, want 0", len(got))
	}
}

func TestDetectConcurrentSupersede_NoConflictWithSingleOpenRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelation(t, ctx, store, "01HREL000000000000000001", "subjA", "works_at", "objX", nil)

	r := &Resolver{
		reader:     store.Reader(),
		logger:     zap.NewNop(),
		predicates: newFunctionalRegistry("works_at"),
	}
	got, err := r.DetectConcurrentSupersede(ctx)
	if err != nil {
		t.Fatalf("DetectConcurrentSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("one open row produced %d conflicts, want 0", len(got))
	}
}

func TestDetectConcurrentSupersede_FlagsTwoOpenForFunctionalPredicate(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	// Two open works_at rows for the same subject — the classic
	// cross-device concurrent-supersede shape. Primary = lowest ULID.
	insertRelation(t, ctx, store, "01HREL00000000000000AAAA", "subjA", "works_at", "objX", nil)
	insertRelation(t, ctx, store, "01HREL00000000000000BBBB", "subjA", "works_at", "objY", nil)

	r := &Resolver{
		reader:     store.Reader(),
		logger:     zap.NewNop(),
		predicates: newFunctionalRegistry("works_at"),
	}
	got, err := r.DetectConcurrentSupersede(ctx)
	if err != nil {
		t.Fatalf("DetectConcurrentSupersede: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(got))
	}
	c := got[0]
	if c.PrimaryRelationUID != "01HREL00000000000000AAAA" {
		t.Errorf("primary = %q, want lowest ULID 01HREL00000000000000AAAA", c.PrimaryRelationUID)
	}
	if c.ConflictingRelationUID != "01HREL00000000000000BBBB" {
		t.Errorf("conflicting = %q, want 01HREL00000000000000BBBB", c.ConflictingRelationUID)
	}
	if c.SubjectUID != "subjA" || c.Predicate != "works_at" {
		t.Errorf("slot mismatch: subj=%q pred=%q", c.SubjectUID, c.Predicate)
	}
	if c.Reason != concurrentSupersedeReason {
		t.Errorf("reason = %q, want %q", c.Reason, concurrentSupersedeReason)
	}
}

func TestDetectConcurrentSupersede_IgnoresNonFunctionalPredicate(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	// "supports_team" is non-functional in the predicate registry; a
	// person can support multiple teams without contradiction, so two
	// open rows are not a concurrent-supersede signal.
	insertRelation(t, ctx, store, "01HREL00000000000000AAAA", "subjA", "supports_team", "tA", nil)
	insertRelation(t, ctx, store, "01HREL00000000000000BBBB", "subjA", "supports_team", "tB", nil)

	r := &Resolver{
		reader:     store.Reader(),
		logger:     zap.NewNop(),
		predicates: newFunctionalRegistry(), // no functionals registered
	}
	got, err := r.DetectConcurrentSupersede(ctx)
	if err != nil {
		t.Fatalf("DetectConcurrentSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("non-functional slot flagged: %d conflicts, want 0", len(got))
	}
}

func TestDetectConcurrentSupersede_IgnoresClosedRows(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	closed := "2026-01-01T00:00:00Z"
	insertRelation(t, ctx, store, "01HREL00000000000000AAAA", "subjA", "works_at", "objX", nil)
	insertRelation(t, ctx, store, "01HREL00000000000000BBBB", "subjA", "works_at", "objY", &closed)

	r := &Resolver{
		reader:     store.Reader(),
		logger:     zap.NewNop(),
		predicates: newFunctionalRegistry("works_at"),
	}
	got, err := r.DetectConcurrentSupersede(ctx)
	if err != nil {
		t.Fatalf("DetectConcurrentSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("closed row flagged: %d conflicts, want 0", len(got))
	}
}

func TestDetectConcurrentSupersede_EmitsNMinus1Conflicts(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	// Three open functional rows → 2 conflicts, all against the
	// lowest-ULID primary.
	insertRelation(t, ctx, store, "01HREL00000000000000AAAA", "subjA", "works_at", "objX", nil)
	insertRelation(t, ctx, store, "01HREL00000000000000BBBB", "subjA", "works_at", "objY", nil)
	insertRelation(t, ctx, store, "01HREL00000000000000CCCC", "subjA", "works_at", "objZ", nil)

	r := &Resolver{
		reader:     store.Reader(),
		logger:     zap.NewNop(),
		predicates: newFunctionalRegistry("works_at"),
	}
	got, err := r.DetectConcurrentSupersede(ctx)
	if err != nil {
		t.Fatalf("DetectConcurrentSupersede: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("conflicts = %d, want 2 (N-1 against primary)", len(got))
	}
	for _, c := range got {
		if c.PrimaryRelationUID != "01HREL00000000000000AAAA" {
			t.Errorf("conflict primary = %q, want lowest ULID 01HREL00000000000000AAAA", c.PrimaryRelationUID)
		}
	}
}
