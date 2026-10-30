package resolver

// supersede_test.go covers the fact-level dedup query introduced in Chunk 5
// and the open-row contract that supersedeCheck depends on. Pure SQL-level
// tests so the Resolver struct's many dependencies do not have to be
// constructed; openRelationExists is a thin wrapper around one query that
// the rest of the resolver also relies on, so pinning it here pins the
// implicit contract for everything that reads from the relations table.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func TestOpenRelationExists_DetectsOpenRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelation(t, ctx, store, "01HREL000000000000000001", "subjA", "works_at", "objX", nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, dup, err := r.openRelationExists(ctx, "subjA", "works_at", "objX", ontology.RelationStatusAsserted)
	if err != nil {
		t.Fatalf("openRelationExists: %v", err)
	}
	if !dup || uid != "01HREL000000000000000001" {
		t.Errorf("dup=%v uid=%q, want dup=true uid=01HREL000000000000000001", dup, uid)
	}
}

func TestOpenRelationExists_IgnoresClosedRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	closedAt := "2026-01-01T00:00:00Z"
	insertRelation(t, ctx, store, "01HREL000000000000000010", "subjA", "works_at", "objX", &closedAt)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, dup, err := r.openRelationExists(ctx, "subjA", "works_at", "objX", ontology.RelationStatusAsserted)
	if err != nil {
		t.Fatalf("openRelationExists: %v", err)
	}
	if dup {
		t.Errorf("closed row collapsed as dup (uid=%q); expected dup=false", uid)
	}
}

func TestOpenRelationExists_NoMatchReturnsFalse(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	if _, dup, err := r.openRelationExists(ctx, "nope", "works_at", "objX", ontology.RelationStatusAsserted); err != nil {
		t.Fatalf("openRelationExists: %v", err)
	} else if dup {
		t.Error("empty table returned dup=true")
	}
}

func TestOpenRelationExists_DifferentObjectStaysDistinct(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	// Open (subjA, works_at, objX) exists; dedup must NOT match objY.
	insertRelation(t, ctx, store, "01HREL000000000000000020", "subjA", "works_at", "objX", nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	if _, dup, err := r.openRelationExists(ctx, "subjA", "works_at", "objY", ontology.RelationStatusAsserted); err != nil {
		t.Fatalf("openRelationExists: %v", err)
	} else if dup {
		t.Error("different object_uid collapsed as dup; supersede path is the right handler, not dedup")
	}
}

// TestFunctionalSupersede_FindsOpenWorksAtRelation is the failing-test-
// first for the case-04 regression. Reproducer: insert an open
// works_at(Eowyn, Erebor) row, then call functionalSupersede for the same
// (subject, predicate); the function must return the existing UID so
// processCandidate can emit a RelationRetracted event. Today the path
// is exercised end-to-end by the E2E validation suite's case 04, where
// `superseded_relations=0` proves the closing UPDATE never runs.
//
// This pure SQL-level test isolates whether the failure is in the
// SELECT itself or further up the chain. If this passes, the bug is in
// processCandidate's orchestration; if it fails, the bug is in the
// SELECT predicate.
func TestFunctionalSupersede_FindsOpenWorksAtRelation(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelation(t, ctx, store, "01HREL00000000000000EREBOR", "eowyn-uid", "works_at", "erebor-uid", nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.functionalSupersede(ctx, "eowyn-uid", "works_at")
	if err != nil {
		t.Fatalf("functionalSupersede: %v", err)
	}
	if uid != "01HREL00000000000000EREBOR" {
		t.Errorf("functionalSupersede returned uid=%q, want %q (open works_at must be found)",
			uid, "01HREL00000000000000EREBOR")
	}
}

// TestFunctionalSupersede_IgnoresClosedRelation pins the "valid_until
// IS NULL" filter. A previously-closed row must not be returned again
// or the supersede loop would close the same fact repeatedly on every
// new contradictory remember.
func TestFunctionalSupersede_IgnoresClosedRelation(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	closedAt := "2025-03-15T00:00:00Z"
	insertRelation(t, ctx, store, "01HREL00000000000000EREBOR", "eowyn-uid", "works_at", "erebor-uid", &closedAt)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.functionalSupersede(ctx, "eowyn-uid", "works_at")
	if err != nil {
		t.Fatalf("functionalSupersede: %v", err)
	}
	if uid != "" {
		t.Errorf("functionalSupersede returned uid=%q on a closed row, want empty", uid)
	}
}

// TestCrossSubjectRoleSupersede_FindsOpenCEOAcrossSubjects is the failing-
// test-first for the role-uniqueness gap. When (Rosie, works_at, Erebor,
// role=CEO) is open and a new (Frodo, works_at, Erebor, role=CEO) lands,
// the slot (object=Erebor, predicate=works_at, role=CEO) is functional and
// Rosie's row must be superseded even though the subjects differ. The
// existing functionalSupersede only matches on (subject, predicate) and
// silently skips this case; see eval/LOGIC-REPORT.md.
func TestCrossSubjectRoleSupersede_FindsOpenCEOAcrossSubjects(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelationWithAttrs(t, ctx, store, "01HREL00CROSSCEOOLDSARAH00", "rosie-uid", "works_at", "erebor-uid", `{"role":"CEO"}`, nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	got, err := r.crossSubjectRoleSupersede(ctx, "frodo-uid", "works_at", "erebor-uid", "CEO")
	if err != nil {
		t.Fatalf("crossSubjectRoleSupersede: %v", err)
	}
	if len(got) != 1 || got[0] != "01HREL00CROSSCEOOLDSARAH00" {
		t.Errorf("got %v, want [01HREL00CROSSCEOOLDSARAH00] (Rosie's open CEO row must be flagged)", got)
	}
}

// TestCrossSubjectRoleSupersede_LowercaseAliasMatchesCEO pins the lexical
// normalisation contract: "ceo" matches "CEO" matches "Chief Executive
// Officer". A new "Chief Executive Officer" should close an existing "CEO"
// row; without normalisation the two would coexist forever.
func TestCrossSubjectRoleSupersede_LowercaseAliasMatchesCEO(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelationWithAttrs(t, ctx, store, "01HREL00CROSSCEOLOWER00000", "rosie-uid", "works_at", "erebor-uid", `{"role":"ceo"}`, nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	got, err := r.crossSubjectRoleSupersede(ctx, "frodo-uid", "works_at", "erebor-uid", "Chief Executive Officer")
	if err != nil {
		t.Fatalf("crossSubjectRoleSupersede: %v", err)
	}
	if len(got) != 1 || got[0] != "01HREL00CROSSCEOLOWER00000" {
		t.Errorf("got %v, want [01HREL00CROSSCEOLOWER00000] (Chief Executive Officer must alias to ceo)", got)
	}
}

// TestCrossSubjectRoleSupersede_SkipsNonSingularRoles pins the allowlist:
// "engineer" or "member" or "lead" are filled by many people at once and
// must NOT be auto-superseded across subjects. Only singular executive
// roles trigger the cross-subject close.
func TestCrossSubjectRoleSupersede_SkipsNonSingularRoles(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelationWithAttrs(t, ctx, store, "01HREL00CROSSENG00000RAREL", "frodo-uid", "works_at", "erebor-uid", `{"role":"engineer"}`, nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	got, err := r.crossSubjectRoleSupersede(ctx, "eowyn-uid", "works_at", "erebor-uid", "engineer")
	if err != nil {
		t.Fatalf("crossSubjectRoleSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want [] (engineer is multi-incumbent, never closes a peer)", got)
	}
}

// TestCrossSubjectRoleSupersede_DifferentObjectStaysDistinct: Rosie CEO of
// Erebor and Frodo CEO of Rivendell must coexist. The slot is bound to
// the OBJECT, not the role string in isolation.
func TestCrossSubjectRoleSupersede_DifferentObjectStaysDistinct(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelationWithAttrs(t, ctx, store, "01HREL00CROSSCEOOTHEROG000", "rosie-uid", "works_at", "erebor-uid", `{"role":"CEO"}`, nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	got, err := r.crossSubjectRoleSupersede(ctx, "frodo-uid", "works_at", "rivendell-uid", "CEO")
	if err != nil {
		t.Fatalf("crossSubjectRoleSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want [] (different object_uid must NOT trigger supersede)", got)
	}
}

// TestCrossSubjectRoleSupersede_IgnoresClosedRows pins the valid_until
// filter: a previously-closed CEO row must not be reopened or flagged by
// the cross-subject check.
func TestCrossSubjectRoleSupersede_IgnoresClosedRows(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	closedAt := "2026-01-15T00:00:00Z"
	insertRelationWithAttrs(t, ctx, store, "01HREL00CROSSCEOCLOSED0000", "rosie-uid", "works_at", "erebor-uid", `{"role":"CEO"}`, &closedAt)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	got, err := r.crossSubjectRoleSupersede(ctx, "frodo-uid", "works_at", "erebor-uid", "CEO")
	if err != nil {
		t.Fatalf("crossSubjectRoleSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want [] (closed row must not re-trigger supersede)", got)
	}
}

// TestCrossSubjectRoleSupersede_SameSubjectStaysQuiet: when the new
// relation has the same subject as the existing one (Rosie promoted from
// CTO to CEO at Erebor), the regular functionalSupersede path closes the
// old row via (subject, predicate); cross-subject must NOT fire and double-
// close the same row.
func TestCrossSubjectRoleSupersede_SameSubjectStaysQuiet(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelationWithAttrs(t, ctx, store, "01HREL00CROSSCEOSAMESUBJ00", "rosie-uid", "works_at", "erebor-uid", `{"role":"CEO"}`, nil)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	got, err := r.crossSubjectRoleSupersede(ctx, "rosie-uid", "works_at", "erebor-uid", "CEO")
	if err != nil {
		t.Fatalf("crossSubjectRoleSupersede: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want [] (same subject is the functionalSupersede path's job)", got)
	}
}

// TestNormalizeRole_LexicalAndAliases pins the normalisation behaviour:
// case folding, whitespace + article stripping, and the seeded executive-
// role alias map all collapse to one canonical token.
func TestNormalizeRole_LexicalAndAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"CEO", "ceo"},
		{"ceo", "ceo"},
		{"  CEO  ", "ceo"},
		{"the CEO", "ceo"},
		{"CEO.,", "ceo"},     // multi-char trailing punct stripped in one pass
		{"the CEO!?", "ceo"}, // article + multi-char trailing punct
		{"Chief Executive Officer", "ceo"},
		{"chief executive officer", "ceo"},
		{"Chief Executive", "ceo"},
		{"CTO", "cto"},
		{"chief technology officer", "cto"},
		{"engineer", "engineer"}, // not aliased; passes through
		{"backend engineer", "backend engineer"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeRole(tc.in); got != tc.want {
			t.Errorf("normalizeRole(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIsSingularRole pins the allowlist + alias-routing pre-singular-check.
// Slice-based table so failure output is deterministic.
func TestIsSingularRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"CEO", true},
		{"ceo", true},
		{"Chief Executive Officer", true},
		{"CTO", true},
		{"CFO", true},
		{"COO", true},
		{"President", true},
		{"chair", true},
		{"Chairman of the Board", true},
		{"founder", true},
		{"engineer", false},
		{"backend engineer", false},
		{"member", false},
		{"lead", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isSingularRole(tc.in); got != tc.want {
			t.Errorf("isSingularRole(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func openResolverStore(t *testing.T, ctx context.Context) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func insertRelation(t *testing.T, ctx context.Context, store *sqlite.Store, uid, subjUID, predicate, objUID string, validUntil *string) {
	t.Helper()
	insertRelationWithAttrs(t, ctx, store, uid, subjUID, predicate, objUID, "{}", validUntil)
}

// insertRelationWithAttrs inserts a relation row with an explicit
// attributes JSON literal. Used by the cross-subject role supersede
// tests which need the role qualifier on the existing row.
func insertRelationWithAttrs(t *testing.T, ctx context.Context, store *sqlite.Store, uid, subjUID, predicate, objUID, attrs string, validUntil *string) {
	t.Helper()
	var vu any
	if validUntil != nil {
		vu = *validUntil
	}
	const stmt = "INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, validity_score, superseded_by_uid, provenance, attributes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	if _, err := store.Writer().ExecContext(ctx, stmt,
		uid, subjUID, predicate, objUID,
		1.0, "src-ep",
		time.Now().UTC().Format(time.RFC3339Nano),
		nil, vu, nil, nil, "test", attrs,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}
}
