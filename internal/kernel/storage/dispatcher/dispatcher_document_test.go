package dispatcher_test

// dispatcher_document_test.go: unit/integration tests for applyDocumentRevision
// and applyDocumentDeleted. All tests run against a real in-process SQLite +
// FTS5 + sqlite-vec stack opened via dispatcher.Open; no mock stores.
//
// Coverage:
//   - Apply a documentRevision event → documents row exists + FTS searchable.
//   - Rebuild-twice idempotency: apply the same event twice, assert identical
//     row count + identical sidecar chunk count.
//   - Verbatim byte-stable round-trip: body stored == body returned.
//   - Soft-delete: applyDocumentDeleted sets deleted=1, FTS entry removed,
//     vec sidecar rows removed.
//   - HasEvent recognises an applied documentRevision by revision_uid.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// makeDocumentRevisionEvent builds a kernel.Event whose payload is a valid
// DocumentRevision. documentUID is the stable identity; revUID is the event UID
// (= revision UID). body is stored verbatim.
func makeDocumentRevisionEvent(t *testing.T, revUID, documentUID, path, body string) kernel.Event {
	t.Helper()
	contentHash := repeatHex('a', 64) // fake but pattern-valid SHA-256
	payload, err := json.Marshal(ontology.DocumentRevision{
		DocumentUID: documentUID,
		Path:        path,
		Title:       "Test Document",
		Body:        body,
		ContentHash: contentHash,
	})
	if err != nil {
		t.Fatalf("marshal DocumentRevision: %v", err)
	}
	return kernel.Event{
		UID:        revUID,
		Kind:       kernel.EventKind(ontology.KindDocumentRevision),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
}

// makeDocumentDeletedEvent builds a kernel.Event whose payload is a valid
// DocumentDeleted tombstone for the given documentUID.
func makeDocumentDeletedEvent(t *testing.T, evUID, documentUID string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.DocumentDeleted{
		DocumentUID: documentUID,
		Path:        "notes/deleted.md",
		Reason:      "test tombstone",
	})
	if err != nil {
		t.Fatalf("marshal DocumentDeleted: %v", err)
	}
	return kernel.Event{
		UID:        evUID,
		Kind:       kernel.EventKind(ontology.KindDocumentDeleted),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
}

// openSQLiteReader returns the dispatcher's writer pool (used to query derived
// state without going through the kernel surface).
func openSQLiteReader(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher) *sql.DB {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return db
}

// TestDocumentRevision_Apply_RowExists verifies that applying a single
// documentRevision event creates a documents row with the correct path, title,
// body, and revision_uid.
func TestDocumentRevision_Apply_RowExists(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil) // no embedder — vec writes are skipped

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	body := "# Hello World\n\nThis is the **verbatim** body.\nLine two.\n"

	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/hello.md", body)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	var (
		gotPath, gotTitle, gotBody, gotRevUID string
		gotDeleted                            int
	)
	err := db.QueryRowContext(ctx,
		`SELECT path, title, body, revision_uid, deleted FROM documents WHERE document_uid = ?`,
		docUID,
	).Scan(&gotPath, &gotTitle, &gotBody, &gotRevUID, &gotDeleted)
	if err != nil {
		t.Fatalf("query documents row: %v", err)
	}
	if gotPath != "notes/hello.md" {
		t.Errorf("path = %q, want %q", gotPath, "notes/hello.md")
	}
	if gotTitle != "Test Document" {
		t.Errorf("title = %q, want %q", gotTitle, "Test Document")
	}
	if gotBody != body {
		t.Errorf("body mismatch\n want: %q\n  got: %q", body, gotBody)
	}
	if gotRevUID != revUID {
		t.Errorf("revision_uid = %q, want %q", gotRevUID, revUID)
	}
	if gotDeleted != 0 {
		t.Errorf("deleted = %d, want 0", gotDeleted)
	}
}

// TestDocumentRevision_Apply_FTSSearchable verifies that the AFTER INSERT
// trigger from migration 0016 indexes the document body in documents_fts so
// that an FTS5 search on a unique token in the body returns the row.
func TestDocumentRevision_Apply_FTSSearchable(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	// unique token UNIQFTS9 will be searched
	body := "Project notes. Unique search token UNIQFTS9 appears here."

	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/fts.md", body)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	// FTS5 search: MATCH the unique token.
	rows, err := db.QueryContext(ctx,
		`SELECT rowid FROM documents_fts WHERE documents_fts MATCH 'UNIQFTS9'`)
	if err != nil {
		t.Fatalf("FTS search: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var count int
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("FTS rows.Err: %v", err)
	}
	if count == 0 {
		t.Error("FTS search for unique body token returned 0 rows; AFTER INSERT trigger may not have fired")
	}
}

// TestDocumentRevision_RebuildTwiceIdempotency verifies that applying the same
// documentRevision event twice (simulating a Rebuild) results in exactly one
// documents row and the same number of sidecar chunk rows as a single apply.
// This is the core Rebuild-idempotency acceptance criterion for P0.
func TestDocumentRevision_RebuildTwiceIdempotency(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	// Body large enough to produce multiple chunks (> 6000 bytes)
	body := repeatStr("Mazarbul knowledge vault entry. ", 200) // ~6400 bytes

	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/long.md", body)

	// First apply.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	var docCount, sidecarCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM documents WHERE document_uid = ?`, docUID).Scan(&docCount); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM documents_vec_ids WHERE document_uid = ?`, docUID).Scan(&sidecarCount); err != nil {
		t.Fatalf("count sidecar: %v", err)
	}
	if docCount != 1 {
		t.Errorf("documents count after first Apply = %d, want 1", docCount)
	}
	if sidecarCount == 0 {
		t.Error("documents_vec_ids count after first Apply = 0, want > 0")
	}

	// Second apply (Rebuild replay).
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	var docCount2, sidecarCount2 int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM documents WHERE document_uid = ?`, docUID).Scan(&docCount2); err != nil {
		t.Fatalf("count documents after second Apply: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM documents_vec_ids WHERE document_uid = ?`, docUID).Scan(&sidecarCount2); err != nil {
		t.Fatalf("count sidecar after second Apply: %v", err)
	}
	if docCount2 != docCount {
		t.Errorf("documents count after second Apply = %d, want %d (not idempotent)", docCount2, docCount)
	}
	if sidecarCount2 != sidecarCount {
		t.Errorf("sidecar count after second Apply = %d, want %d (not idempotent)", sidecarCount2, sidecarCount)
	}
}

// TestDocumentRevision_VerbatimBodyRoundTrip is the byte-stable round-trip
// acceptance criterion. The body stored in the documents row must equal the
// body that was in the payload — no normalisation, no line-ending changes.
func TestDocumentRevision_VerbatimBodyRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	// Body with intentional irregularities that a normaliser would change.
	body := "# Title\r\n\r\nTrailing spaces:   \nEmpty line:\n\nWindows CRLF:\r\nEnd\n"

	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/verbatim.md", body)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)
	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT body FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&stored); err != nil {
		t.Fatalf("query body: %v", err)
	}
	if stored != body {
		t.Errorf("body not verbatim\n want (%d bytes): %q\n  got (%d bytes): %q",
			len(body), body, len(stored), stored)
	}
}

// TestDocumentDeleted_SoftDelete verifies that applying a documentDeleted event
// after a documentRevision sets deleted=1 in the documents row and removes the
// documents_vec_ids sidecar rows.
func TestDocumentDeleted_SoftDelete(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	delUID := ulid.Make().String()
	body := "Some document content that will be soft-deleted."

	// Apply a revision first.
	revEv := makeDocumentRevisionEvent(t, revUID, docUID, "notes/todel.md", body)
	if err := d.Apply(ctx, []kernel.Event{revEv}); err != nil {
		t.Fatalf("Apply revision: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	// Confirm the row exists and is live.
	var deletedBefore int
	if err := db.QueryRowContext(ctx,
		`SELECT deleted FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&deletedBefore); err != nil {
		t.Fatalf("query deleted before: %v", err)
	}
	if deletedBefore != 0 {
		t.Fatalf("deleted before tombstone = %d, want 0", deletedBefore)
	}

	// Apply the tombstone.
	delEv := makeDocumentDeletedEvent(t, delUID, docUID)
	if err := d.Apply(ctx, []kernel.Event{delEv}); err != nil {
		t.Fatalf("Apply deleted: %v", err)
	}

	// documents row must still exist (tombstone preserved for Rebuild).
	var deletedAfter int
	err := db.QueryRowContext(ctx,
		`SELECT deleted FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&deletedAfter)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("documents row removed on soft-delete; must be preserved as tombstone")
	}
	if err != nil {
		t.Fatalf("query deleted after: %v", err)
	}
	if deletedAfter != 1 {
		t.Errorf("deleted after tombstone = %d, want 1", deletedAfter)
	}

	// Vec sidecar rows must be removed.
	var sidecarCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM documents_vec_ids WHERE document_uid = ?`, docUID,
	).Scan(&sidecarCount); err != nil {
		t.Fatalf("count sidecar after delete: %v", err)
	}
	if sidecarCount != 0 {
		t.Errorf("sidecar count after soft-delete = %d, want 0", sidecarCount)
	}
}

// TestDocumentRevision_HasEvent verifies that HasEvent returns true for an
// applied documentRevision event (keyed on revision_uid = ev.UID) and false
// for an unknown UID.
func TestDocumentRevision_HasEvent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/hasevent.md", "body text")

	// Before Apply: HasEvent must return false.
	has, err := d.HasEvent(ctx, revUID)
	if err != nil {
		t.Fatalf("HasEvent before Apply: %v", err)
	}
	if has {
		t.Fatal("HasEvent before Apply = true, want false")
	}

	// After Apply: HasEvent must return true.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	has, err = d.HasEvent(ctx, revUID)
	if err != nil {
		t.Fatalf("HasEvent after Apply: %v", err)
	}
	if !has {
		t.Fatal("HasEvent after Apply = false, want true")
	}
}

// TestDocumentRevision_HasEventFalseAfterSidecarLoss pins the Rebuild repair
// contract: HasEvent must report a documentRevision as NOT applied when the
// documents row survived but the chunk sidecar (documents_vec_ids) was lost.
// FileEventLog.Rebuild skips Apply for events HasEvent reports present — a
// row-presence-only check would short-circuit the same-revision repair path
// in applyDocumentRevision and leave the sidecar unrepairable from the log.
func TestDocumentRevision_HasEventFalseAfterSidecarLoss(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/rebuild-repair.md", "repairable body")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if has, err := d.HasEvent(ctx, revUID); err != nil || !has {
		t.Fatalf("HasEvent after Apply = (%v, %v), want (true, nil)", has, err)
	}

	// Simulate partial derived-store loss: row survives, sidecar vanishes.
	db := openSQLiteReader(t, ctx, d)
	if _, err := db.ExecContext(ctx,
		`DELETE FROM documents_vec_ids WHERE document_uid = ?`, docUID,
	); err != nil {
		t.Fatalf("simulate sidecar loss: %v", err)
	}

	// HasEvent must now report the revision as missing so Rebuild re-Applies.
	if has, err := d.HasEvent(ctx, revUID); err != nil || has {
		t.Fatalf("HasEvent after sidecar loss = (%v, %v), want (false, nil) so Rebuild repairs", has, err)
	}

	// The Rebuild flow: HasEvent said false → Apply runs → sidecar restored.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("repair Apply: %v", err)
	}
	if has, err := d.HasEvent(ctx, revUID); err != nil || !has {
		t.Fatalf("HasEvent after repair Apply = (%v, %v), want (true, nil)", has, err)
	}
	var sidecar int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM documents_vec_ids WHERE document_uid = ?`, docUID,
	).Scan(&sidecar); err != nil {
		t.Fatalf("count sidecar: %v", err)
	}
	if sidecar == 0 {
		t.Error("sidecar rows after repair Apply = 0, want > 0")
	}
}

// TestDocumentDeleted_HasEventTrueForTombstonedRevision verifies that a
// tombstoned document's trailing revision still reports as applied: the
// sidecar is gone BY DESIGN (applyDocumentDeleted removes it), so Rebuild must
// not see a missing-sidecar repair signal and re-Apply the revision (the
// replay would be skipped by the tombstone guard anyway — this just keeps
// Rebuild from churning on every deleted document).
func TestDocumentDeleted_HasEventTrueForTombstonedRevision(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	delUID := ulid.Make().String()

	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/tombstoned.md", "soon gone")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply revision: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{makeDocumentDeletedEvent(t, delUID, docUID)}); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}

	if has, err := d.HasEvent(ctx, revUID); err != nil || !has {
		t.Fatalf("HasEvent for tombstoned revision = (%v, %v), want (true, nil)", has, err)
	}
}

// TestDocumentRevision_HasEventFalseAfterVecLoss pins the embedder leg of the
// presence check: with an embedder configured, every sidecar chunk must have
// its vec0 row — recall joins documents_vec_ids to documents_vec by rowid, so
// a lost embedding makes the chunk invisible to the vector lane and Rebuild
// must re-Apply (which re-schedules the chunk embeddings).
func TestDocumentRevision_HasEventFalseAfterVecLoss(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/vec-loss.md", "embedded body")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := d.WaitForBackgroundVec(ctx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	if has, err := d.HasEvent(ctx, revUID); err != nil || !has {
		t.Fatalf("HasEvent after Apply+embed = (%v, %v), want (true, nil)", has, err)
	}

	// Simulate vec0 loss with the sidecar intact (vec0 deletes must go by
	// rowid; the virtual table does not accept subqueries in DELETE).
	db := openSQLiteReader(t, ctx, d)
	// Collect inside a closure so the rows handle is closed (releasing the
	// pool connection) before the DELETE loop below runs.
	rowids := func() []int64 {
		rows, err := db.QueryContext(ctx,
			`SELECT rowid FROM documents_vec_ids WHERE document_uid = ?`, docUID)
		if err != nil {
			t.Fatalf("query sidecar rowids: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan rowid: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate rowids: %v", err)
		}
		return ids
	}()
	if len(rowids) == 0 {
		t.Fatal("no sidecar rows after Apply, want > 0")
	}
	for _, id := range rowids {
		if _, err := db.ExecContext(ctx, `DELETE FROM documents_vec WHERE rowid = ?`, id); err != nil {
			t.Fatalf("simulate vec loss rowid=%d: %v", id, err)
		}
	}

	if has, err := d.HasEvent(ctx, revUID); err != nil || has {
		t.Fatalf("HasEvent after vec loss = (%v, %v), want (false, nil) so Rebuild re-embeds", has, err)
	}
}

// TestDocumentRevision_MultipleRevisions verifies that applying two revisions
// for the same document (sequential, monotonically increasing event UIDs)
// results in one documents row holding the LATEST revision's body, and HasEvent
// returns true for the latest revision UID.
//
// ulid.Make() is monotonic, so revUID2 > revUID1, and the monotonic-revision
// guard in applyDocumentRevision applies rev2 over rev1 (a strictly newer
// revision is never skipped). The reverse — replaying an OLDER revision after a
// newer one — is covered by TestDocumentRevision_OlderRevisionReplayIsNoOp.
func TestDocumentRevision_MultipleRevisions(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID1 := ulid.Make().String()
	revUID2 := ulid.Make().String()

	ev1 := makeDocumentRevisionEvent(t, revUID1, docUID, "notes/rev.md", "body v1")
	ev2 := makeDocumentRevisionEvent(t, revUID2, docUID, "notes/rev.md", "body v2")

	if err := d.Apply(ctx, []kernel.Event{ev1, ev2}); err != nil {
		t.Fatalf("Apply both revisions: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	// Only one row for this document.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM documents WHERE document_uid = ?`, docUID).Scan(&count); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if count != 1 {
		t.Errorf("documents count = %d, want 1", count)
	}

	// The row holds the second revision's body.
	var body string
	if err := db.QueryRowContext(ctx,
		`SELECT body FROM documents WHERE document_uid = ?`, docUID).Scan(&body); err != nil {
		t.Fatalf("query body: %v", err)
	}
	if body != "body v2" {
		t.Errorf("body = %q, want %q", body, "body v2")
	}

	// HasEvent for the latest revision UID must be true.
	has, err := d.HasEvent(ctx, revUID2)
	if err != nil {
		t.Fatalf("HasEvent rev2: %v", err)
	}
	if !has {
		t.Error("HasEvent(revUID2) = false, want true")
	}
}

// TestDocumentDeleted_NotSearchable verifies that a soft-deleted document is
// removed from the FTS index: after the tombstone, an FTS MATCH on a token that
// was in the body returns zero rows. This guards the migration-0016 trigger
// fix — the AFTER UPDATE trigger re-indexes only live rows (WHERE new.deleted=0),
// so a soft-delete drops the doc from full-text search even though the body is
// preserved in the documents row for Rebuild determinism.
func TestDocumentDeleted_NotSearchable(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	delUID := ulid.Make().String()
	body := "Deletable note. Unique token DELFTS7 to search for."

	revEv := makeDocumentRevisionEvent(t, revUID, docUID, "notes/delsearch.md", body)
	if err := d.Apply(ctx, []kernel.Event{revEv}); err != nil {
		t.Fatalf("Apply revision: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	// Sanity: the token is searchable while the doc is live.
	if got := ftsMatchCount(t, ctx, db, "DELFTS7"); got == 0 {
		t.Fatal("token not searchable before delete; FTS insert trigger may be broken")
	}

	// Soft-delete.
	delEv := makeDocumentDeletedEvent(t, delUID, docUID)
	if err := d.Apply(ctx, []kernel.Event{delEv}); err != nil {
		t.Fatalf("Apply deleted: %v", err)
	}

	// After the tombstone the token must no longer match.
	if got := ftsMatchCount(t, ctx, db, "DELFTS7"); got != 0 {
		t.Errorf("FTS match count after soft-delete = %d, want 0 (deleted doc still searchable)", got)
	}
}

// TestDocumentRevision_OlderRevisionReplayIsNoOp verifies the monotonic-revision
// guard: once a newer revision is applied, replaying an OLDER revision (smaller
// ULID) for the same document is a no-op — the row keeps the newer body and the
// newer revision_uid. This prevents a Rebuild (or out-of-order re-Append) from
// rolling the projection back to stale content.
func TestDocumentRevision_OlderRevisionReplayIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	olderUID := ulid.Make().String() // minted first → smaller ULID
	newerUID := ulid.Make().String() // minted second → larger ULID

	// Apply the NEWER revision first.
	newerEv := makeDocumentRevisionEvent(t, newerUID, docUID, "notes/mono.md", "newer body")
	if err := d.Apply(ctx, []kernel.Event{newerEv}); err != nil {
		t.Fatalf("Apply newer: %v", err)
	}

	// Then replay the OLDER revision — must be skipped.
	olderEv := makeDocumentRevisionEvent(t, olderUID, docUID, "notes/mono.md", "older body")
	if err := d.Apply(ctx, []kernel.Event{olderEv}); err != nil {
		t.Fatalf("Apply older: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)
	var gotBody, gotRev string
	if err := db.QueryRowContext(ctx,
		`SELECT body, revision_uid FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&gotBody, &gotRev); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if gotBody != "newer body" {
		t.Errorf("body = %q, want %q (older revision replay rolled the row back)", gotBody, "newer body")
	}
	if gotRev != newerUID {
		t.Errorf("revision_uid = %q, want %q (newer)", gotRev, newerUID)
	}
}

// TestDocumentRevision_SameRevisionReplayRestoresSidecar verifies that
// re-applying the CURRENT revision is a repair path, not a skip: if the
// documents row survived a partial derived-store loss but the vec sidecar rows
// did not, replaying the same event rebuilds documents_vec_ids. This pins the
// strictly-greater comparison in documentRevisionIsStale (an equal revision_uid
// must run the idempotent delete-and-insert sidecar path).
func TestDocumentRevision_SameRevisionReplayRestoresSidecar(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()

	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/repair.md", "repairable body")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	var before int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM documents_vec_ids WHERE document_uid = ?`, docUID,
	).Scan(&before); err != nil {
		t.Fatalf("count sidecar before: %v", err)
	}
	if before == 0 {
		t.Fatal("sidecar count after first Apply = 0, want > 0")
	}

	// Simulate a partial derived-store loss: row survives, sidecar vanishes.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM documents_vec_ids WHERE document_uid = ?`, docUID,
	); err != nil {
		t.Fatalf("simulate sidecar loss: %v", err)
	}

	// Replay the SAME event — must rebuild the sidecar, not skip.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("replay Apply: %v", err)
	}

	var after int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM documents_vec_ids WHERE document_uid = ?`, docUID,
	).Scan(&after); err != nil {
		t.Fatalf("count sidecar after replay: %v", err)
	}
	if after != before {
		t.Errorf("sidecar count after same-revision replay = %d, want %d (replay must repair a lost sidecar)", after, before)
	}
}

// TestDocumentRevision_ReplayAfterDeleteStaysDeleted verifies that the
// same-revision repair path cannot resurrect a tombstoned document: after
// revision R → documentDeleted, replaying R again must be skipped — the row
// stays deleted=1, no vec sidecar rows reappear, and the body stays out of
// FTS. Only a STRICTLY newer revision (an intentional recreate) may clear the
// tombstone.
func TestDocumentRevision_ReplayAfterDeleteStaysDeleted(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	delUID := ulid.Make().String() // minted after revUID → larger ULID

	ev := makeDocumentRevisionEvent(t, revUID, docUID, "notes/lazarus.md", "resurrectionword body")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply revision: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{makeDocumentDeletedEvent(t, delUID, docUID)}); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}
	// Re-apply the delete (idempotent replay): the documents_au trigger must
	// not issue an FTS 'delete' for the already-unindexed row (deleted 1 -> 1),
	// which would corrupt the external-content FTS index.
	if err := d.Apply(ctx, []kernel.Event{makeDocumentDeletedEvent(t, delUID, docUID)}); err != nil {
		t.Fatalf("re-Apply delete: %v", err)
	}

	// Replay the SAME revision after the tombstone — must be a no-op skip.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("replay Apply after delete: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	var gotDeleted int
	if err := db.QueryRowContext(ctx,
		`SELECT deleted FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&gotDeleted); err != nil {
		t.Fatalf("query documents row: %v", err)
	}
	if gotDeleted != 1 {
		t.Errorf("deleted = %d, want 1 (same-revision replay resurrected a tombstoned document)", gotDeleted)
	}

	var sidecar int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM documents_vec_ids WHERE document_uid = ?`, docUID,
	).Scan(&sidecar); err != nil {
		t.Fatalf("count sidecar rows: %v", err)
	}
	if sidecar != 0 {
		t.Errorf("sidecar rows after replay = %d, want 0 (replay re-added vec sidecar for a deleted document)", sidecar)
	}

	var ftsHits int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM documents_fts WHERE documents_fts MATCH 'resurrectionword'`,
	).Scan(&ftsHits); err != nil {
		t.Fatalf("FTS query: %v", err)
	}
	if ftsHits != 0 {
		t.Errorf("FTS hits after replay = %d, want 0 (deleted document re-indexed)", ftsHits)
	}

	// A STRICTLY newer revision (intentional recreate) must still clear the
	// tombstone — pin that the fix did not break the legitimate recreate path.
	recreateUID := ulid.Make().String()
	recreate := makeDocumentRevisionEvent(t, recreateUID, docUID, "notes/lazarus.md", "recreated body")
	if err := d.Apply(ctx, []kernel.Event{recreate}); err != nil {
		t.Fatalf("Apply recreate: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT deleted FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&gotDeleted); err != nil {
		t.Fatalf("query documents row after recreate: %v", err)
	}
	if gotDeleted != 0 {
		t.Errorf("deleted after recreate = %d, want 0 (strictly newer revision must clear the tombstone)", gotDeleted)
	}
}

// TestDocumentRevision_SlugFrozenAcrossRename verifies that the slug is frozen
// at first sight: a later revision that moves the document to a new path updates
// documents.path but leaves documents.slug at the value derived from the
// original filename, so rendered/documents/<slug>.md does not move in git.
func TestDocumentRevision_SlugFrozenAcrossRename(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	rev1 := ulid.Make().String()
	rev2 := ulid.Make().String()

	ev1 := makeDocumentRevisionEvent(t, rev1, docUID, "notes/original-name.md", "v1")
	ev2 := makeDocumentRevisionEvent(t, rev2, docUID, "notes/renamed-file.md", "v2")
	if err := d.Apply(ctx, []kernel.Event{ev1, ev2}); err != nil {
		t.Fatalf("Apply both: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)
	var gotPath, gotSlug string
	if err := db.QueryRowContext(ctx,
		`SELECT path, slug FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&gotPath, &gotSlug); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if gotPath != "notes/renamed-file.md" {
		t.Errorf("path = %q, want %q (rename must update path)", gotPath, "notes/renamed-file.md")
	}
	if gotSlug != "original-name" {
		t.Errorf("slug = %q, want %q (slug must be frozen at first revision)", gotSlug, "original-name")
	}
}

// TestDocumentRevision_FrontmatterNonObjectDefaults verifies that a revision
// whose frontmatter payload is not a JSON object (here a JSON array) is coerced
// to "{}" in frontmatter_json, so json_each(frontmatter_json) tag queries never
// fault on a non-object root.
func TestDocumentRevision_FrontmatterNonObjectDefaults(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()

	payload, err := json.Marshal(ontology.DocumentRevision{
		DocumentUID: docUID,
		Path:        "notes/badfm.md",
		Title:       "Bad Frontmatter",
		Body:        "body",
		Frontmatter: json.RawMessage(`[1, 2, 3]`), // a JSON array, not an object
		ContentHash: repeatHex('b', 64),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev := kernel.Event{
		UID:        revUID,
		Kind:       kernel.EventKind(ontology.KindDocumentRevision),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)
	var gotFM string
	if err := db.QueryRowContext(ctx,
		`SELECT frontmatter_json FROM documents WHERE document_uid = ?`, docUID,
	).Scan(&gotFM); err != nil {
		t.Fatalf("query frontmatter_json: %v", err)
	}
	if gotFM != "{}" {
		t.Errorf("frontmatter_json = %q, want %q (non-object frontmatter must default)", gotFM, "{}")
	}
}

// TestDocumentDeleted_CleansEntityNameVec verifies the two extra cleanup steps
// that applyDocumentDeleted must perform after removing the document vec sidecar:
//
//  1. The document entity's name-embedding rows are removed from entities_vec_ids
//     (and thus from entities_vec), so the nameEmb recall lane no longer surfaces
//     the deleted document.
//  2. The document entity's resolution_status in the entities table is updated to
//     "deleted", so callers that filter on resolution_status can exclude it.
//
// The test applies an entity event (simulating the companion entity written by
// the ingest pipeline alongside every documentRevision) then the revision and
// the tombstone.  After the tombstone both invariants must hold.
func TestDocumentDeleted_CleansEntityNameVec(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	delUID := ulid.Make().String()

	// Apply the companion entity event that the ingest pipeline emits.  The
	// entityUID is the same ULID as docUID — the document-entity identity is
	// kept stable so the ingest code can correlate them.
	entity := ontology.Entity{
		UID:              docUID,
		Kind:             ontology.EntityKindDocument,
		DisplayName:      "My Important Document",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
	}
	entPayload, err := json.Marshal(entity)
	if err != nil {
		t.Fatalf("marshal entity: %v", err)
	}
	entEv := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC),
		Payload:    entPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{entEv}); err != nil {
		t.Fatalf("Apply entity: %v", err)
	}
	// Drain background entity embeddings so the vec rows are present before delete.
	if err := d.WaitForBackgroundVec(ctx); err != nil {
		t.Fatalf("WaitForBackgroundVec after entity: %v", err)
	}

	db := openSQLiteReader(t, ctx, d)

	// Confirm the entity vec row was written.
	var vecRowsBefore int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entities_vec_ids WHERE id = ?`, docUID,
	).Scan(&vecRowsBefore); err != nil {
		t.Fatalf("count entities_vec_ids before delete: %v", err)
	}
	if vecRowsBefore == 0 {
		t.Fatal("entities_vec_ids has no rows for entity before delete; embedding may not have landed")
	}

	// Apply revision then tombstone.
	revEv := makeDocumentRevisionEvent(t, revUID, docUID, "notes/important.md", "body content")
	if err := d.Apply(ctx, []kernel.Event{revEv}); err != nil {
		t.Fatalf("Apply revision: %v", err)
	}
	delEv := makeDocumentDeletedEvent(t, delUID, docUID)
	if err := d.Apply(ctx, []kernel.Event{delEv}); err != nil {
		t.Fatalf("Apply deleted: %v", err)
	}

	// Invariant 1: entity name-vec rows must be gone from entities_vec_ids.
	var vecRowsAfter int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entities_vec_ids WHERE id = ?`, docUID,
	).Scan(&vecRowsAfter); err != nil {
		t.Fatalf("count entities_vec_ids after delete: %v", err)
	}
	if vecRowsAfter != 0 {
		t.Errorf("entities_vec_ids rows after delete = %d, want 0 (nameEmb lane must be cleared)", vecRowsAfter)
	}

	// Invariant 2: entity resolution_status must be "deleted", not "confirmed".
	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT resolution_status FROM entities WHERE uid = ?`, docUID,
	).Scan(&status); err != nil {
		t.Fatalf("query resolution_status: %v", err)
	}
	if status == string(ontology.ResolutionConfirmed) {
		t.Errorf("resolution_status = %q after delete; want %q so entity is excluded from confirmed-only queries",
			status, "deleted")
	}
	if status != "deleted" {
		t.Errorf("resolution_status = %q, want %q", status, "deleted")
	}
}

// TestDocumentRevision_Canonical_ProjectedFromFrontmatter verifies that the
// dispatcher projects the `canonical` boolean key from the documentRevision
// frontmatter payload into the documents.canonical column:
//
//   - frontmatter {"canonical":true}  → canonical = 1
//   - frontmatter {"canonical":false} → canonical = 0
//   - frontmatter absent (nil)        → canonical = 0
//
// Idempotency: re-applying a canonical:true revision keeps canonical = 1;
// re-applying a canonical:false revision sets it back to 0.
func TestDocumentRevision_Canonical_ProjectedFromFrontmatter(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)
	db := openSQLiteReader(t, ctx, d)

	readCanonical := func(t *testing.T, docUID string) int {
		t.Helper()
		var v int
		if err := db.QueryRowContext(ctx,
			`SELECT canonical FROM documents WHERE document_uid = ?`, docUID,
		).Scan(&v); err != nil {
			t.Fatalf("query canonical for %s: %v", docUID, err)
		}
		return v
	}

	t.Run("canonical true stores 1", func(t *testing.T) {
		docUID := ulid.Make().String()
		payload, err := json.Marshal(ontology.DocumentRevision{
			DocumentUID: docUID,
			Path:        "notes/canonical-true.md",
			Title:       "Canonical True",
			Body:        "body",
			Frontmatter: json.RawMessage(`{"canonical":true}`),
			ContentHash: repeatHex('c', 64),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		ev := kernel.Event{
			UID:        ulid.Make().String(),
			Kind:       kernel.EventKind(ontology.KindDocumentRevision),
			Source:     "test",
			Confidence: 1.0,
			CreateTime: time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC),
			Payload:    payload,
		}
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readCanonical(t, docUID); got != 1 {
			t.Errorf("canonical = %d, want 1 (frontmatter canonical:true)", got)
		}

		// Idempotency: re-apply the same event; canonical must stay 1.
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("second Apply: %v", err)
		}
		if got := readCanonical(t, docUID); got != 1 {
			t.Errorf("canonical after re-apply = %d, want 1 (idempotent)", got)
		}
	})

	t.Run("canonical false stores 0", func(t *testing.T) {
		docUID := ulid.Make().String()
		payload, err := json.Marshal(ontology.DocumentRevision{
			DocumentUID: docUID,
			Path:        "notes/canonical-false.md",
			Title:       "Canonical False",
			Body:        "body",
			Frontmatter: json.RawMessage(`{"canonical":false}`),
			ContentHash: repeatHex('d', 64),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		ev := kernel.Event{
			UID:        ulid.Make().String(),
			Kind:       kernel.EventKind(ontology.KindDocumentRevision),
			Source:     "test",
			Confidence: 1.0,
			CreateTime: time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC),
			Payload:    payload,
		}
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readCanonical(t, docUID); got != 0 {
			t.Errorf("canonical = %d, want 0 (frontmatter canonical:false)", got)
		}
	})

	t.Run("canonical absent stores 0", func(t *testing.T) {
		docUID := ulid.Make().String()
		payload, err := json.Marshal(ontology.DocumentRevision{
			DocumentUID: docUID,
			Path:        "notes/canonical-absent.md",
			Title:       "Canonical Absent",
			Body:        "body",
			ContentHash: repeatHex('e', 64),
			// Frontmatter omitted — canonical key is absent.
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		ev := kernel.Event{
			UID:        ulid.Make().String(),
			Kind:       kernel.EventKind(ontology.KindDocumentRevision),
			Source:     "test",
			Confidence: 1.0,
			CreateTime: time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC),
			Payload:    payload,
		}
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readCanonical(t, docUID); got != 0 {
			t.Errorf("canonical = %d, want 0 (frontmatter absent)", got)
		}
	})

	t.Run("true then false clears to 0", func(t *testing.T) {
		docUID := ulid.Make().String()
		rev1UID := ulid.Make().String()

		payloadTrue, err := json.Marshal(ontology.DocumentRevision{
			DocumentUID: docUID,
			Path:        "notes/canonical-flip.md",
			Title:       "Canonical Flip",
			Body:        "body v1",
			Frontmatter: json.RawMessage(`{"canonical":true}`),
			ContentHash: repeatHex('f', 64),
		})
		if err != nil {
			t.Fatalf("marshal true: %v", err)
		}
		ev1 := kernel.Event{
			UID:        rev1UID,
			Kind:       kernel.EventKind(ontology.KindDocumentRevision),
			Source:     "test",
			Confidence: 1.0,
			CreateTime: time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC),
			Payload:    payloadTrue,
		}
		if err := d.Apply(ctx, []kernel.Event{ev1}); err != nil {
			t.Fatalf("Apply true revision: %v", err)
		}
		if got := readCanonical(t, docUID); got != 1 {
			t.Errorf("canonical after true revision = %d, want 1", got)
		}

		payloadFalse, err := json.Marshal(ontology.DocumentRevision{
			DocumentUID: docUID,
			Path:        "notes/canonical-flip.md",
			Title:       "Canonical Flip",
			Body:        "body v2",
			Frontmatter: json.RawMessage(`{"canonical":false}`),
			ContentHash: repeatHex('1', 64),
		})
		if err != nil {
			t.Fatalf("marshal false: %v", err)
		}
		rev2UID := ulid.Make().String()
		ev2 := kernel.Event{
			UID:        rev2UID,
			Kind:       kernel.EventKind(ontology.KindDocumentRevision),
			Source:     "test",
			Confidence: 1.0,
			CreateTime: time.Date(2026, 6, 24, 11, 0, 0, 0, time.UTC),
			Payload:    payloadFalse,
		}
		if err := d.Apply(ctx, []kernel.Event{ev2}); err != nil {
			t.Fatalf("Apply false revision: %v", err)
		}
		if got := readCanonical(t, docUID); got != 0 {
			t.Errorf("canonical after false revision = %d, want 0", got)
		}
	})
}

// -- local helpers -----------------------------------------------------------

// ftsMatchCount returns the number of documents_fts rows matching the given FTS5
// query expression.
func ftsMatchCount(t *testing.T, ctx context.Context, db *sql.DB, match string) int {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT rowid FROM documents_fts WHERE documents_fts MATCH ?`, match)
	if err != nil {
		t.Fatalf("FTS search %q: %v", match, err)
	}
	defer func() { _ = rows.Close() }()
	var count int
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("FTS rows.Err: %v", err)
	}
	return count
}

// repeatHex returns a hex string of length n by repeating ch.
func repeatHex(ch byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ch
	}
	return string(b)
}

// repeatStr returns s repeated n times.
func repeatStr(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for range n {
		b = append(b, []byte(s)...)
	}
	return string(b)
}
