package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// openDedupDB opens an in-memory SQLite database with the minimal schema the
// sweep needs: entities, entity_identifiers, entity_dup_candidates.
func openDedupDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema := []string{
		`CREATE TABLE entities (
			uid          TEXT PRIMARY KEY,
			kind         TEXT NOT NULL,
			display_name TEXT NOT NULL,
			scope        TEXT
		)`,
		`CREATE TABLE entity_identifiers (
			entity_uid  TEXT NOT NULL,
			identifier  TEXT NOT NULL,
			kind        TEXT NOT NULL,
			added_at    TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE entity_dup_candidates (
			entity_uid_a TEXT NOT NULL,
			entity_uid_b TEXT NOT NULL,
			reason       TEXT NOT NULL,
			score        REAL NOT NULL,
			detected_at  TEXT NOT NULL,
			PRIMARY KEY (entity_uid_a, entity_uid_b),
			CHECK (entity_uid_a < entity_uid_b)
		)`,
	}
	ctx := context.Background()
	for _, s := range schema {
		if _, serr := db.ExecContext(ctx, s); serr != nil {
			t.Fatalf("schema: %v", serr)
		}
	}
	return db
}

// insertEntity adds a row to entities.
func insertEntity(t *testing.T, db *sql.DB, uid, kind, name, scope string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO entities (uid, kind, display_name, scope) VALUES (?, ?, ?, ?)`,
		uid, kind, name, scope,
	); err != nil {
		t.Fatalf("insert entity %s: %v", uid, err)
	}
}

// insertIdentifier adds a row to entity_identifiers.
func insertIdentifier(t *testing.T, db *sql.DB, entityUID, identifier, kind string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO entity_identifiers (entity_uid, identifier, kind) VALUES (?, ?, ?)`,
		entityUID, identifier, kind,
	); err != nil {
		t.Fatalf("insert identifier %s/%s: %v", entityUID, identifier, err)
	}
}

// countCandidates returns the number of rows in entity_dup_candidates.
func countCandidates(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM entity_dup_candidates`,
	).Scan(&n); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	return n
}

// fakeAppender records Append calls for assertion.
type fakeAppender struct {
	events []kernel.Event
}

func (f *fakeAppender) Append(_ context.Context, evs []kernel.Event) ([]kernel.Receipt, error) {
	f.events = append(f.events, evs...)
	out := make([]kernel.Receipt, len(evs))
	for i := range evs {
		out[i] = kernel.Receipt{UID: "FAKE", CreateTime: kernel.Receipt{}.CreateTime}
	}
	return out, nil
}

// newSweep constructs a DedupSweep using db as both reader and writer,
// with no embedder (name-KNN lane disabled).
func newSweep(t *testing.T, db *sql.DB, app *fakeAppender) *daemon.DedupSweep {
	t.Helper()
	lu := entitymatch.NewLookup(db, nil, nil, 0, 0)
	s := daemon.NewDedupSweep(db, db, app, lu, zap.NewNop())
	if s == nil {
		t.Fatal("NewDedupSweep returned nil")
	}
	return s
}

// TestDedupSweep_ExactNameSameKind confirms that two entities sharing the same
// case-insensitive name and kind produce one candidate row (lane a) and no
// entityMerged event.
func TestDedupSweep_ExactNameSameKind(t *testing.T) {
	t.Parallel()
	db := openDedupDB(t)
	app := &fakeAppender{}
	s := newSweep(t, db, app)

	// UID "01A" sorts before "01B" (lexicographic).
	insertEntity(t, db, "01A", "person", "Acme GmbH", "personal")
	insertEntity(t, db, "01B", "person", "acme gmbh", "personal")

	s.Run(context.Background(), []string{"01B"})

	if got := countCandidates(t, db); got != 1 {
		t.Fatalf("want 1 candidate row, got %d", got)
	}
	if len(app.events) != 0 {
		t.Fatalf("want 0 auto-merge events, got %d", len(app.events))
	}
	// Verify the stored pair satisfies CHECK (entity_uid_a < entity_uid_b).
	var a, b, reason string
	if err := db.QueryRowContext(context.Background(),
		`SELECT entity_uid_a, entity_uid_b, reason FROM entity_dup_candidates`,
	).Scan(&a, &b, &reason); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if a >= b {
		t.Errorf("pair constraint violated: a=%s >= b=%s", a, b)
	}
	if reason != "exact_name" {
		t.Errorf("reason: want exact_name, got %s", reason)
	}
}

// TestDedupSweep_ExactNameDifferentKind confirms that two entities with the
// same display name but different entity kinds produce no candidate and no merge.
func TestDedupSweep_ExactNameDifferentKind(t *testing.T) {
	t.Parallel()
	db := openDedupDB(t)
	app := &fakeAppender{}
	s := newSweep(t, db, app)

	insertEntity(t, db, "01A", "person", "Acme", "personal")
	insertEntity(t, db, "01B", "company", "Acme", "personal")

	s.Run(context.Background(), []string{"01B"})

	if got := countCandidates(t, db); got != 0 {
		t.Fatalf("want 0 candidates (different kinds), got %d", got)
	}
	if len(app.events) != 0 {
		t.Fatalf("want 0 events, got %d", len(app.events))
	}
}

// TestDedupSweep_StrongIdentifierAutoMerge confirms that two entities sharing
// an email identifier of matching kind produce an entityMerged event (lane b)
// with the lexicographically smaller UID as the survivor. No candidate row.
func TestDedupSweep_StrongIdentifierAutoMerge(t *testing.T) {
	t.Parallel()
	db := openDedupDB(t)
	app := &fakeAppender{}
	s := newSweep(t, db, app)

	// "01A" < "01B" in lexicographic order — 01A is the older survivor.
	insertEntity(t, db, "01A", "person", "Alice", "personal")
	insertEntity(t, db, "01B", "person", "Alice Duplicate", "personal")
	insertIdentifier(t, db, "01A", "alice@example.com", string(ontology.IdentifierKindEmail))
	insertIdentifier(t, db, "01B", "alice@example.com", string(ontology.IdentifierKindEmail))

	s.Run(context.Background(), []string{"01B"})

	if got := countCandidates(t, db); got != 0 {
		t.Fatalf("want 0 candidate rows (auto-merge, not advisory), got %d", got)
	}
	if len(app.events) != 1 {
		t.Fatalf("want 1 entityMerged event, got %d", len(app.events))
	}
	ev := app.events[0]
	if string(ev.Kind) != ontology.KindEntityMerged {
		t.Fatalf("event kind: want %s, got %s", ontology.KindEntityMerged, ev.Kind)
	}
	var merged ontology.EntityMergedEvent
	if err := json.Unmarshal(ev.Payload, &merged); err != nil {
		t.Fatalf("unmarshal entityMerged payload: %v", err)
	}
	// Survivor = MIN uid = "01A"; source = "01B".
	if merged.TargetEntityUID != "01A" {
		t.Errorf("survivor: want 01A, got %s", merged.TargetEntityUID)
	}
	if merged.SourceEntityUID != "01B" {
		t.Errorf("source: want 01B, got %s", merged.SourceEntityUID)
	}
}

// TestDedupSweep_StrongIdentifierDifferentKindNoMerge confirms that a shared
// strong identifier between entities of different kinds is NOT auto-merged —
// a username shared by a person and a project is plausible data.
func TestDedupSweep_StrongIdentifierDifferentKindNoMerge(t *testing.T) {
	t.Parallel()
	db := openDedupDB(t)
	app := &fakeAppender{}
	s := newSweep(t, db, app)

	insertEntity(t, db, "01A", "person", "Alice", "personal")
	insertEntity(t, db, "01B", "project", "Alice Project", "personal")
	insertIdentifier(t, db, "01A", "alice@example.com", string(ontology.IdentifierKindEmail))
	insertIdentifier(t, db, "01B", "alice@example.com", string(ontology.IdentifierKindEmail))

	s.Run(context.Background(), []string{"01B"})

	if got := countCandidates(t, db); got != 0 {
		t.Fatalf("want 0 candidates (different kinds), got %d", got)
	}
	if len(app.events) != 0 {
		t.Fatalf("want 0 events (different kinds), got %d", len(app.events))
	}
}

// TestDedupSweep_NilIsNoop confirms that a nil DedupSweep's Run method is a
// safe no-op (callers assign nil when the sweep is disabled).
func TestDedupSweep_NilIsNoop(t *testing.T) {
	t.Parallel()
	var s *daemon.DedupSweep
	// Must not panic.
	s.Run(context.Background(), []string{"01A"})
}

// TestDedupSweep_EmptyUIDs confirms that Run with an empty slice is a no-op.
func TestDedupSweep_EmptyUIDs(t *testing.T) {
	t.Parallel()
	db := openDedupDB(t)
	app := &fakeAppender{}
	s := newSweep(t, db, app)

	insertEntity(t, db, "01A", "person", "Alice", "personal")

	s.Run(context.Background(), []string{})

	if got := countCandidates(t, db); got != 0 {
		t.Fatalf("want 0 candidates on empty uid list, got %d", got)
	}
}

// TestDedupSweep_DuplicateUIDsDeduped confirms that a UID appearing multiple
// times in the input (e.g. from several KindEntityIdentifierAdded events for the
// same entity) is swept only once.
func TestDedupSweep_DuplicateUIDsDeduped(t *testing.T) {
	t.Parallel()
	db := openDedupDB(t)
	app := &fakeAppender{}
	s := newSweep(t, db, app)

	insertEntity(t, db, "01A", "person", "Acme", "personal")
	insertEntity(t, db, "01B", "person", "Acme", "personal")

	// "01B" repeated three times — should produce exactly one candidate row,
	// not three (the upsert handles it, but dedupStringSlice avoids triple work).
	s.Run(context.Background(), []string{"01B", "01B", "01B"})

	if got := countCandidates(t, db); got != 1 {
		t.Fatalf("want 1 candidate (dedup input), got %d", got)
	}
}
