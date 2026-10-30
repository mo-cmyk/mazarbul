package entitymatch_test

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// openStore opens a real SQLite store, registers cleanup, and returns the Lookup.
// vec and embedder are intentionally nil so the name-embedding lane degrades gracefully.
func openStore(t *testing.T) (*sqlite.Store, *entitymatch.Lookup) {
	t.Helper()
	ctx := testutil.TestContext(t)
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lu := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)
	return store, lu
}

// execSeed is a helper that runs a single SQL statement and fails the test on error.
func execSeed(t *testing.T, store *sqlite.Store, q string, args ...any) {
	t.Helper()
	ctx := testutil.TestContext(t)
	if _, err := store.Writer().ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("seed %q: %v", q, err)
	}
}

// seedEntity inserts a row into the entities table.
func seedEntity(t *testing.T, store *sqlite.Store, uid, kind, displayName string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	execSeed(t, store,
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		uid, kind, displayName, 1.0, "confirmed", now,
	)
}

// seedIdentifier inserts a row into the entity_identifiers table.
func seedIdentifier(t *testing.T, store *sqlite.Store, entityUID, identifier, kind string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	execSeed(t, store,
		`INSERT INTO entity_identifiers (entity_uid, identifier, kind, source, added_at)
		 VALUES (?, ?, ?, ?, ?)`,
		entityUID, identifier, kind, "test", now,
	)
}

// seedRelation inserts a row into the relations table.
func seedRelation(t *testing.T, store *sqlite.Store, uid, subjectUID, predicate, objectUID string, supersededByUID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if supersededByUID == "" {
		execSeed(t, store,
			`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uid, subjectUID, predicate, objectUID, 1.0, "ep1", now, "{}",
		)
	} else {
		execSeed(t, store,
			`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes, superseded_by_uid)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uid, subjectUID, predicate, objectUID, 1.0, "ep1", now, "{}", supersededByUID,
		)
	}
}

const (
	uidPerson01 = "01HENT000000000000PERSON01"
	uidPerson02 = "01HENT000000000000PERSON02"
	uidOrg01    = "01HENT0000000000000000ORG1"
	uidOrg02    = "01HENT0000000000000000ORG2"
)

// ----------------------------------------------------------------------------
// 1. ExactNameMatch
// ----------------------------------------------------------------------------

func TestExactNameMatch(t *testing.T) {
	t.Parallel()

	t.Run("case-sensitive exact hit", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice Smith")

		got, err := lu.ExactNameMatch(ctx, "Alice Smith", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != uidPerson01 {
			t.Errorf("got %q, want %q", got, uidPerson01)
		}
	})

	t.Run("case-insensitive hit", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice Smith")

		got, err := lu.ExactNameMatch(ctx, "alice smith", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != uidPerson01 {
			t.Errorf("got %q, want %q", got, uidPerson01)
		}
	})

	t.Run("kind mismatch returns empty", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice Smith")

		got, err := lu.ExactNameMatch(ctx, "Alice Smith", "organization")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string (kind mismatch)", got)
		}
	})

	t.Run("no such name returns empty", func(t *testing.T) {
		t.Parallel()
		_, lu := openStore(t)
		ctx := testutil.TestContext(t)

		got, err := lu.ExactNameMatch(ctx, "Nobody Here", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})
}

// ----------------------------------------------------------------------------
// 2. IdentifierMatch
// ----------------------------------------------------------------------------

func TestIdentifierMatch(t *testing.T) {
	t.Parallel()

	t.Run("alias kind resolves when entity kind matches", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice Smith")
		seedIdentifier(t, store, uidPerson01, "Ali", "alias")

		got, _, err := lu.IdentifierMatch(ctx, "Ali", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != uidPerson01 {
			t.Errorf("got %q, want %q", got, uidPerson01)
		}
	})

	t.Run("empty kind disables kind gate", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidOrg01, "organization", "Acme Corp")
		seedIdentifier(t, store, uidOrg01, "Acme", "alias")

		// kind="" means no gate: should resolve regardless of entity kind.
		got, _, err := lu.IdentifierMatch(ctx, "Acme", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != uidOrg01 {
			t.Errorf("got %q, want %q", got, uidOrg01)
		}
	})

	t.Run("strong typed identifier resolves regardless of passed kind", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidOrg01, "organization", "Some Corp")
		// "hrb" is a strong typed identifier (not alias, not id), length >= 4.
		seedIdentifier(t, store, uidOrg01, "HRB12345", "hrb")

		// Passing a different kind should still resolve via the strong identifier lane.
		got, gotKind, err := lu.IdentifierMatch(ctx, "HRB12345", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != uidOrg01 {
			t.Errorf("got %q, want %q (strong identifier lane should ignore kind gate)", got, uidOrg01)
		}
		// The kind-blind strong lane must return the entity's STORED kind, not the
		// caller's declared kind, so a cross-kind reuse reports the truth.
		if gotKind != "organization" {
			t.Errorf("matched kind = %q, want %q (stored kind, not the passed kind)", gotKind, "organization")
		}
	})

	t.Run("short non-alias identifier does not anchor", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidOrg01, "organization", "Some Corp")
		// "ab" is shorter than StrongIdentifierMinLength (4) and kind is "hrb" (strong lane).
		seedIdentifier(t, store, uidOrg01, "ab", "hrb")

		got, _, err := lu.IdentifierMatch(ctx, "ab", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty (short identifier must not anchor)", got)
		}
	})

	t.Run("id kind is excluded from strong identifier lane", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidOrg01, "organization", "Some Corp")
		// "id" kind should be excluded from the strong-identifier lane.
		seedIdentifier(t, store, uidOrg01, "ref-99-xyz", "id")

		got, _, err := lu.IdentifierMatch(ctx, "ref-99-xyz", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty (id kind excluded from strong lane)", got)
		}
	})

	t.Run("alias kind is excluded from strong identifier lane", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidOrg01, "organization", "Some Corp")
		// "alias" kind is excluded from strong lane; and kind gate ("person") blocks alias lane.
		seedIdentifier(t, store, uidOrg01, "Acme Corp Alt", "alias")

		got, _, err := lu.IdentifierMatch(ctx, "Acme Corp Alt", "person")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty (alias excluded from strong lane and kind mismatch)", got)
		}
	})
}

// ----------------------------------------------------------------------------
// 3. CountActiveRelations
// ----------------------------------------------------------------------------

func TestCountActiveRelations(t *testing.T) {
	t.Parallel()

	t.Run("counts non-superseded relations as subject and object", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice")
		seedEntity(t, store, uidPerson02, "person", "Bob")
		seedEntity(t, store, uidOrg01, "organization", "Acme")

		// uidPerson01 is subject of r1, object of r2 => count = 2.
		seedRelation(t, store, "r1", uidPerson01, "works_at", uidOrg01, "")
		seedRelation(t, store, "r2", uidPerson02, "knows", uidPerson01, "")

		n, err := lu.CountActiveRelations(ctx, uidPerson01)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Errorf("got %d, want 2", n)
		}
	})

	t.Run("superseded relation is not counted", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice")
		seedEntity(t, store, uidOrg01, "organization", "Acme")
		seedEntity(t, store, uidOrg02, "organization", "BetaCorp")

		// r1 is superseded by r2; only r2 should be counted.
		seedRelation(t, store, "r1", uidPerson01, "works_at", uidOrg01, "r2")
		seedRelation(t, store, "r2", uidPerson01, "works_at", uidOrg02, "")

		n, err := lu.CountActiveRelations(ctx, uidPerson01)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 1 {
			t.Errorf("got %d, want 1 (superseded relation excluded)", n)
		}
	})

	t.Run("entity with no relations returns zero", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice")

		n, err := lu.CountActiveRelations(ctx, uidPerson01)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 0 {
			t.Errorf("got %d, want 0", n)
		}
	})
}

// ----------------------------------------------------------------------------
// 4. Describe
// ----------------------------------------------------------------------------

func TestDescribe(t *testing.T) {
	t.Parallel()

	t.Run("existing entity returns display name kind and relation count", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice Smith")
		seedEntity(t, store, uidOrg01, "organization", "Acme")
		seedRelation(t, store, "r1", uidPerson01, "works_at", uidOrg01, "")

		c, ok, err := lu.Describe(ctx, uidPerson01)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if c.DisplayName != "Alice Smith" {
			t.Errorf("DisplayName = %q, want %q", c.DisplayName, "Alice Smith")
		}
		if c.Kind != "person" {
			t.Errorf("Kind = %q, want %q", c.Kind, "person")
		}
		if c.RelationCount != 1 {
			t.Errorf("RelationCount = %d, want 1", c.RelationCount)
		}
		if c.UID != uidPerson01 {
			t.Errorf("UID = %q, want %q", c.UID, uidPerson01)
		}
	})

	t.Run("missing uid returns ok false", func(t *testing.T) {
		t.Parallel()
		_, lu := openStore(t)
		ctx := testutil.TestContext(t)

		_, ok, err := lu.Describe(ctx, "01HENT000DOESNOTEXIST0001")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Error("ok = true, want false for missing uid")
		}
	})
}

// ----------------------------------------------------------------------------
// 5. NameKNN and NameKNNByVector (nil-vec degrade path)
// ----------------------------------------------------------------------------

func TestNameKNN_NilVecDegrades(t *testing.T) {
	t.Parallel()

	t.Run("NameKNN returns nil with nil embedder and vec", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice Smith")

		candidates, err := lu.NameKNN(ctx, "Alice Smith", "person", 5)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if candidates != nil {
			t.Errorf("NameKNN with nil embedder = %v, want nil", candidates)
		}
	})

	t.Run("NameKNNByVector returns nil with nil vec", func(t *testing.T) {
		t.Parallel()
		store, lu := openStore(t)
		ctx := testutil.TestContext(t)
		seedEntity(t, store, uidPerson01, "person", "Alice Smith")

		vec := []float32{0.1, 0.2, 0.3}
		candidates, err := lu.NameKNNByVector(ctx, vec, "person", 5)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if candidates != nil {
			t.Errorf("NameKNNByVector with nil vec = %v, want nil", candidates)
		}
	})
}

// ----------------------------------------------------------------------------
// 6. L2DistanceToCosine
// ----------------------------------------------------------------------------

func TestL2DistanceToCosine(t *testing.T) {
	t.Parallel()

	t.Run("distance zero maps to cosine one", func(t *testing.T) {
		t.Parallel()
		got := entitymatch.L2DistanceToCosine(0)
		if got != 1.0 {
			t.Errorf("L2DistanceToCosine(0) = %f, want 1.0", got)
		}
	})

	t.Run("distance sqrt2 maps to approximately zero", func(t *testing.T) {
		t.Parallel()
		const epsilon = 1e-9
		got := entitymatch.L2DistanceToCosine(math.Sqrt2)
		if math.Abs(got) > epsilon {
			t.Errorf("L2DistanceToCosine(sqrt2) = %f, want ~0.0 (epsilon %e)", got, epsilon)
		}
	})

	t.Run("monotonically decreasing as distance increases", func(t *testing.T) {
		t.Parallel()
		distances := []float64{0.0, 0.5, 1.0, math.Sqrt2}
		prev := entitymatch.L2DistanceToCosine(distances[0])
		for i := 1; i < len(distances); i++ {
			curr := entitymatch.L2DistanceToCosine(distances[i])
			if curr >= prev {
				t.Errorf("not monotonically decreasing: L2DistanceToCosine(%f)=%f >= L2DistanceToCosine(%f)=%f",
					distances[i], curr, distances[i-1], prev)
			}
			prev = curr
		}
	})
}
