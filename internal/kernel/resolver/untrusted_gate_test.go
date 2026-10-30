package resolver

// untrusted_gate_test.go pins the SQL-level structural guarantees of the #211
// untrusted-input trust gate: the episode trust read, the status-scoped dedup,
// and the supersede SELECTs that exclude proposed (connector-ingested) rows.
// Pure SQL-level tests (no LLM) — they exercise the exact queries the live
// resolver runs for a connector-sourced episode.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// insertRelationWithStatus inserts an open relation carrying an explicit trust
// status so the proposed/asserted supersede behaviour can be exercised.
func insertRelationWithStatus(t *testing.T, ctx context.Context, store *sqlite.Store, uid, subjUID, predicate, objUID, status string) {
	t.Helper()
	const stmt = `INSERT INTO relations
		(uid, subject_uid, predicate, object_uid, confidence, source_event_uid,
		 create_time, valid_from, valid_until, validity_score, superseded_by_uid,
		 provenance, attributes, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := store.Writer().ExecContext(ctx, stmt,
		uid, subjUID, predicate, objUID, 1.0, "src-ep",
		time.Now().UTC().Format(time.RFC3339Nano),
		nil, nil, nil, nil, "test", "{}", status,
	); err != nil {
		t.Fatalf("insert relation with status: %v", err)
	}
}

// insertEpisode inserts an episodes row with the given source so episodeUntrusted
// can read its trust posture.
func insertEpisode(t *testing.T, ctx context.Context, store *sqlite.Store, uid, source string) {
	t.Helper()
	const stmt = `INSERT INTO episodes (uid, create_time, kind, payload_json, text, source, class, scope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := store.Writer().ExecContext(ctx, stmt,
		uid, time.Now().UTC().Format(time.RFC3339Nano), "episode", "{}", "body text", source, "episodic", "personal",
	); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
}

func TestEpisodeUntrusted_ConnectorSourceIsUntrusted(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertEpisode(t, ctx, store, "01HEPCONNECTOR0000000000001", "connector:local-git")
	insertEpisode(t, ctx, store, "01HEPTRUSTED00000000000001", "cli:remember")

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	if !r.episodeUntrusted(ctx, "01HEPCONNECTOR0000000000001") {
		t.Error("connector-sourced episode should be untrusted")
	}
	if r.episodeUntrusted(ctx, "01HEPTRUSTED00000000000001") {
		t.Error("cli-sourced episode should be trusted")
	}
	// Fail-safe: a missing episode row reads as TRUSTED (not a connector path).
	if r.episodeUntrusted(ctx, "01HEPMISSING000000000000001") {
		t.Error("missing episode should default to trusted")
	}
}

func TestFunctionalSupersede_IgnoresProposedRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	// A proposed (connector-ingested) open relation must NOT be a supersede
	// target: a later user fact for the same (subject, predicate) coexists.
	insertRelationWithStatus(t, ctx, store, "01HRELPROPOSED000000000001", "subjA", "works_at", "objX", ontology.RelationStatusProposed)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.functionalSupersede(ctx, "subjA", "works_at")
	if err != nil {
		t.Fatalf("functionalSupersede: %v", err)
	}
	if uid != "" {
		t.Errorf("functionalSupersede returned %q for a proposed row; proposed edges must never be superseded", uid)
	}
}

func TestFunctionalSupersede_StillFindsAssertedRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	// Regression guard: the status filter must not break the normal asserted path.
	insertRelationWithStatus(t, ctx, store, "01HRELASSERTED000000000001", "subjA", "works_at", "objX", ontology.RelationStatusAsserted)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	uid, err := r.functionalSupersede(ctx, "subjA", "works_at")
	if err != nil {
		t.Fatalf("functionalSupersede: %v", err)
	}
	if uid != "01HRELASSERTED000000000001" {
		t.Errorf("functionalSupersede returned %q, want the asserted row", uid)
	}
}

func TestOpenRelationExists_StatusScoped(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openResolverStore(t, ctx)
	insertRelationWithStatus(t, ctx, store, "01HRELASSERTED000000000010", "subjA", "works_at", "objX", ontology.RelationStatusAsserted)

	r := &Resolver{reader: store.Reader(), logger: zap.NewNop()}

	// A proposed candidate must NOT dedup against the existing asserted row, so
	// a user-asserted fact is never silently suppressed by a connector re-ingest.
	if _, dup, err := r.openRelationExists(ctx, "subjA", "works_at", "objX", ontology.RelationStatusProposed); err != nil {
		t.Fatalf("openRelationExists(proposed): %v", err)
	} else if dup {
		t.Error("proposed candidate deduped against an asserted row; status tiers must be isolated")
	}

	// An asserted candidate dedups normally against the asserted row.
	if _, dup, err := r.openRelationExists(ctx, "subjA", "works_at", "objX", ontology.RelationStatusAsserted); err != nil {
		t.Fatalf("openRelationExists(asserted): %v", err)
	} else if !dup {
		t.Error("asserted candidate did not dedup against the asserted row")
	}
}
