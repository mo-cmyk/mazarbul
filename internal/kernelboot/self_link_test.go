package kernelboot_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestEnsureSelfPrincipalLink_LinksSelfToPrincipal is the headline acceptance:
// after genesis binds a principal (display name = github login, so the principal
// UID is a real ULID distinct from the "self" sentinel), EnsureSelfPrincipalLink
// appends a same_as relation from the seeded self anchor to that principal, and a
// from-log Rebuild projects a live same_as relation row (subject_uid='self',
// object_uid=<principalUID>) plus the registered same_as predicate.
func TestEnsureSelfPrincipalLink_LinksSelfToPrincipal(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	idStore := identity.New(providerWithWatchword(t))

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	// Genesis with a nil anchorer registers a principal under a minted ULID (the
	// principal UID is always a ULID, never "self", regardless of the anchor).
	if serr := kernelboot.SeedIdentity(ctx, log, db, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity: %v", serr)
	}
	principalUID, perr := config.SelfPrincipalUID(dataDir)
	if perr != nil || principalUID == "" {
		t.Fatalf("self binding after genesis = (%q, %v)", principalUID, perr)
	}
	// The principal entity must be projected before the link can reference it.
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post-genesis): %v", rerr)
	}

	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, db, dataDir); lerr != nil {
		t.Fatalf("EnsureSelfPrincipalLink: %v", lerr)
	}
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post-link): %v", rerr)
	}

	if n := liveSelfLinkCount(t, ctx, db, principalUID); n != 1 {
		t.Fatalf("live same_as self->principal relation count = %d, want 1", n)
	}
	if !sameAsPredicateRegistered(t, ctx, db) {
		t.Fatalf("same_as predicate not registered after link")
	}
}

// TestEnsureSelfPrincipalLink_Idempotent confirms a second reconciler run does
// not append a duplicate same_as edge: the existence check short-circuits it, so
// exactly one live self->principal relation remains after two calls + a rebuild.
func TestEnsureSelfPrincipalLink_Idempotent(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	idStore := identity.New(providerWithWatchword(t))

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	if serr := kernelboot.SeedIdentity(ctx, log, db, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity: %v", serr)
	}
	principalUID, perr := config.SelfPrincipalUID(dataDir)
	if perr != nil || principalUID == "" {
		t.Fatalf("self binding after genesis = (%q, %v)", principalUID, perr)
	}
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post-genesis): %v", rerr)
	}

	// First run appends the link; rebuild so the existence check sees the row.
	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, db, dataDir); lerr != nil {
		t.Fatalf("EnsureSelfPrincipalLink (first run): %v", lerr)
	}
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post first link): %v", rerr)
	}

	// Second run must short-circuit on the existence check: no new append.
	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, db, dataDir); lerr != nil {
		t.Fatalf("EnsureSelfPrincipalLink (second run): %v", lerr)
	}
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post second link): %v", rerr)
	}

	if n := liveSelfLinkCount(t, ctx, db, principalUID); n != 1 {
		t.Fatalf("live same_as self->principal relation count after two runs = %d, want 1", n)
	}
	if n := countEventKind(t, dataDir, "relation"); n != 1 {
		t.Fatalf("relation event count after two runs = %d, want 1 (no duplicate append)", n)
	}
}

// TestEnsureSelfPrincipalLink_NoOpWhenUnbound confirms the reconciler is a clean
// no-op against a fresh data dir where genesis has not run (no bound principal):
// nothing is appended.
func TestEnsureSelfPrincipalLink_NoOpWhenUnbound(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	if uid, _ := config.SelfPrincipalUID(dataDir); uid != "" {
		t.Fatalf("precondition: self bound to %q on a fresh dir; want unbound", uid)
	}
	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, db, dataDir); lerr != nil {
		t.Fatalf("EnsureSelfPrincipalLink(unbound) = %v, want nil no-op", lerr)
	}
	if n := relationEventCount(t, dataDir); n != 0 {
		t.Fatalf("relation event count = %d, want 0 (unbound is a no-op)", n)
	}
}

// TestEnsureSelfPrincipalLink_NoOpWhenPrincipalIsSelf confirms the reconciler is
// a no-op when the bound principal IS the seeded self anchor (principalUID ==
// "self"): there is no separate node to link, and a self->self edge would be a
// degenerate self-loop. No relation event is appended.
func TestEnsureSelfPrincipalLink_NoOpWhenPrincipalIsSelf(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	// Bind the principal to the seeded "self" sentinel directly to exercise the
	// guard (the genesis path never does this, but a degenerate binding must not
	// produce a self-loop).
	if berr := config.BindSelfPrincipal(dataDir, "self", "device-test", "sigkey-test"); berr != nil {
		t.Fatalf("BindSelfPrincipal(self): %v", berr)
	}

	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, db, dataDir); lerr != nil {
		t.Fatalf("EnsureSelfPrincipalLink(principal=self) = %v, want nil no-op", lerr)
	}
	if n := relationEventCount(t, dataDir); n != 0 {
		t.Fatalf("relation event count = %d, want 0 (principal=self is a no-op)", n)
	}
}

// relationEventCount counts relation events in the log, tolerating a missing
// events/ directory (the log dir is created lazily on first append, so a clean
// no-op leaves it absent). A present directory delegates to countEventKind.
func relationEventCount(t *testing.T, dataDir string) int {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dataDir, "events")); errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	return countEventKind(t, dataDir, "relation")
}

// liveSelfLinkCount returns the number of live (not retracted/superseded) same_as
// relation rows from the seeded self anchor to principalUID. Mirrors the
// canonical live-relation predicate used by the navigate layer.
func liveSelfLinkCount(t *testing.T, ctx context.Context, db *sql.DB, principalUID string) int {
	t.Helper()
	var n int
	const q = `
		SELECT COUNT(*) FROM relations
		WHERE subject_uid = 'self'
		  AND predicate = 'same_as'
		  AND object_uid = ?
		  AND (superseded_by_uid IS NULL OR superseded_by_uid = '')`
	if err := db.QueryRowContext(ctx, q, principalUID).Scan(&n); err != nil {
		t.Fatalf("query live same_as link: %v", err)
	}
	return n
}

// sameAsPredicateRegistered reports whether the same_as predicate row exists in
// the predicates vocabulary projection.
func sameAsPredicateRegistered(t *testing.T, ctx context.Context, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM predicates WHERE predicate = 'same_as'`).Scan(&n); err != nil {
		t.Fatalf("query same_as predicate: %v", err)
	}
	return n > 0
}

// TestEnsureSelfPrincipalLink_SurvivesFromLogRebuild is the integration-level
// guard for the realistic event ordering [pre-genesis self relation ->
// principalRegistered -> same_as link]. A relation is written under the local
// "self" anchor BEFORE genesis binds a principal; then SeedIdentity +
// EnsureSelfPrincipalLink run, the derived stores are dropped, and the entire
// JSONL log is replayed into a fresh dispatcher. The cold from-log Rebuild must:
//
//	(a) repoint the pre-genesis self relation onto the principal — the
//	    first-creation repoint in applyPrincipalRegistered fires during replay
//	    because the principal entity does not yet exist at that log position; and
//	(b) preserve the same_as link's literal "self" subject — the link replays
//	    AFTER principalRegistered, so the repoint never reaches it, leaving
//	    exactly one live self->principal edge.
//
// Incremental Apply tests cover (a) and (b) in isolation; this proves the
// guarantee holds under a real cold replay in the order events actually land.
func TestEnsureSelfPrincipalLink_SurvivesFromLogRebuild(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	idStore := identity.New(providerWithWatchword(t))

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	// (1) A relation under the local "self" anchor, BEFORE genesis. Its object is
	// an unresolved employer UID; relations may reference unmaterialized objects.
	preGenesisRelUID := testULID("RE01")
	if _, aerr := log.Append(ctx, []kernel.Event{selfRelationEvent(t, preGenesisRelUID, testULID("RG01"))}); aerr != nil {
		t.Fatalf("append pre-genesis self relation: %v", aerr)
	}

	// (2) Genesis binds the principal under a minted ULID (never "self").
	if serr := kernelboot.SeedIdentity(ctx, log, db, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity: %v", serr)
	}
	principalUID, perr := config.SelfPrincipalUID(dataDir)
	if perr != nil || principalUID == "" {
		t.Fatalf("self binding after genesis = (%q, %v)", principalUID, perr)
	}
	// Project so EnsureSelfPrincipalLink sees the principal and can emit the link.
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post-genesis): %v", rerr)
	}
	// (3) Link the self anchor to the principal.
	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, db, dataDir); lerr != nil {
		t.Fatalf("EnsureSelfPrincipalLink: %v", lerr)
	}

	// Cold rebuild: drop every derived store, reopen a fresh dispatcher at the
	// same dataDir, and replay the whole JSONL log into it — the exact event
	// ordering a from-scratch boot projects.
	if cerr := disp.Close(); cerr != nil {
		t.Fatalf("dispatcher.Close: %v", cerr)
	}
	for _, p := range []string{"index.db", "index.db-wal", "index.db-shm", "graph.ladybug"} {
		_ = os.RemoveAll(filepath.Join(dataDir, p))
	}
	fresh, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open (fresh): %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	freshDB, err := fresh.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite (fresh): %v", err)
	}
	if _, rerr := log.Rebuild(ctx, fresh); rerr != nil {
		t.Fatalf("Rebuild (cold): %v", rerr)
	}

	// (a) The pre-genesis self relation was repointed onto the principal.
	if subj := relationSubject(t, ctx, freshDB, preGenesisRelUID); subj != principalUID {
		t.Fatalf("pre-genesis relation subject after cold rebuild = %q, want %q (principal)", subj, principalUID)
	}
	// (b) Exactly one live same_as link, subject still the literal "self".
	if n := liveSelfLinkCount(t, ctx, freshDB, principalUID); n != 1 {
		t.Fatalf("live same_as self->principal links after cold rebuild = %d, want 1", n)
	}
}

// selfRelationEvent builds a KindRelation event whose subject is the local
// "self" anchor, suitable for log.Append (validates against the built-in
// Relation schema; "works_at" is a seeded predicate). The relation UID is also
// its SourceEventUID, mirroring the genesis assertion path.
func selfRelationEvent(t *testing.T, relUID, objectUID string) kernel.Event {
	t.Helper()
	rel := ontology.Relation{
		Name:           "relations/" + relUID,
		UID:            relUID,
		SubjectUID:     "self",
		Predicate:      "works_at",
		ObjectUID:      objectUID,
		Confidence:     1.0,
		SourceEventUID: relUID,
		CreateTime:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Status:         ontology.RelationStatusAsserted,
	}
	payload, err := json.Marshal(&rel)
	if err != nil {
		t.Fatalf("marshal self relation: %v", err)
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "test:pre-genesis",
		Confidence: 1.0,
		Payload:    payload,
		CreateTime: rel.CreateTime,
	}
}

// relationSubject returns the projected subject_uid of the relation row with the
// given UID.
func relationSubject(t *testing.T, ctx context.Context, db *sql.DB, relUID string) string {
	t.Helper()
	var subj string
	if err := db.QueryRowContext(ctx,
		`SELECT subject_uid FROM relations WHERE uid = ?`, relUID).Scan(&subj); err != nil {
		t.Fatalf("query relation %s subject: %v", relUID, err)
	}
	return subj
}

// testULID left-pads a short suffix into a valid 26-char Crockford-base32 ULID
// for deterministic fixtures. The suffix must use only characters the Relation
// schema's UID/object pattern accepts (no I, L, O, U).
func testULID(suffix string) string {
	const width = 26
	return strings.Repeat("0", width-len(suffix)) + suffix
}
