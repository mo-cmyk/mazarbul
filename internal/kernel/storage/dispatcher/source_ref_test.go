package dispatcher_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// srSubject is an arbitrary subject UID; source_refs has no FK to entities, so
// the writer tests need not seed one.
const srSubject = "01J000000000000000000SUBJ"

// srFetchTime is a fixed instant so projected rows are byte-comparable across
// re-applies.
var srFetchTime = time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

// makeSourceRefEvent builds a KindSourceRefRecorded event with the given
// identity, dedup key, and external fields.
func makeSourceRefEvent(t *testing.T, uid, dedupKey, externalID, externalURL string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.SourceRefRecordedEvent{
		UID:         ontology.ULID(uid),
		SubjectUID:  ontology.ULID(srSubject),
		SourceType:  "email",
		Connector:   "gmail",
		Stream:      "INBOX",
		ExternalID:  externalID,
		ExternalURL: externalURL,
		FetchTime:   srFetchTime,
		DedupKey:    dedupKey,
	})
	if err != nil {
		t.Fatalf("marshal source ref payload: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindSourceRefRecorded),
		Source:     "test:sourceRef",
		Confidence: 1.0,
		CreateTime: srFetchTime,
		Payload:    payload,
	}
}

// sourceRefRow holds every projected column so the rebuild-idempotency test
// byte-checks the whole row, not a subset — a future schema column that is not
// replay-stable then fails the gate instead of slipping through.
type sourceRefRow struct {
	uid            string
	subjectUID     string
	sourceType     string
	connector      string
	stream         string
	externalID     string
	externalURL    string
	contentHash    string
	fetchTime      string
	dedupKey       string
	sourceEventUID string
	createTime     string
	scope          string
}

// srUID is the canonical row uid for a dedupKey: the dispatcher derives it from
// dedup_key, so tests address rows by the derived uid, never by the (ignored)
// payload uid.
func srUID(dedupKey string) string { return string(ontology.SourceRefUID(dedupKey)) }

func readSourceRef(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string) (sourceRefRow, bool) {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var r sourceRefRow
	qerr := reader.QueryRowContext(ctx,
		`SELECT uid, subject_uid, source_type, connector, stream, external_id, external_url,
		        content_hash, fetch_time, dedup_key, source_event_uid, create_time, scope
		   FROM source_refs WHERE uid = ?`, uid).
		Scan(&r.uid, &r.subjectUID, &r.sourceType, &r.connector, &r.stream, &r.externalID, &r.externalURL,
			&r.contentHash, &r.fetchTime, &r.dedupKey, &r.sourceEventUID, &r.createTime, &r.scope)
	if qerr == sql.ErrNoRows {
		return sourceRefRow{}, false
	}
	if qerr != nil {
		t.Fatalf("query source_refs: %v", qerr)
	}
	return r, true
}

func countSourceRefsByDedup(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, dedupKey string) int {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var n int
	if qerr := reader.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM source_refs WHERE dedup_key = ?`, dedupKey).Scan(&n); qerr != nil {
		t.Fatalf("count source_refs: %v", qerr)
	}
	return n
}

// TestApply_SourceRef_Projects asserts a sourceRefRecorded event lands in
// source_refs with its fields stored verbatim and the scope defaulted to
// personal (the #206 fail-safe).
func TestApply_SourceRef_Projects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	uid := newFactULID(t)
	ev := makeSourceRefEvent(t, uid, "gmail:msg-1", "msg-1", "https://mail.example.com/msg-1")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply source ref: %v", err)
	}

	row, ok := readSourceRef(t, ctx, d, srUID("gmail:msg-1"))
	if !ok {
		t.Fatal("source_refs row not found")
	}
	if row.subjectUID != srSubject {
		t.Errorf("subject_uid = %q, want %q", row.subjectUID, srSubject)
	}
	if row.sourceType != "email" {
		t.Errorf("source_type = %q, want email", row.sourceType)
	}
	// The projection stores external_id / external_url verbatim; the omit/hash
	// transform is render-only.
	if row.externalID != "msg-1" {
		t.Errorf("external_id = %q, want msg-1", row.externalID)
	}
	if row.externalURL != "https://mail.example.com/msg-1" {
		t.Errorf("external_url = %q, want the verbatim url", row.externalURL)
	}
	if row.dedupKey != "gmail:msg-1" {
		t.Errorf("dedup_key = %q, want gmail:msg-1", row.dedupKey)
	}
	if row.scope != string(kernel.ScopePersonal) {
		t.Errorf("scope = %q, want personal", row.scope)
	}
	if row.fetchTime == "" {
		t.Error("fetch_time is empty")
	}
	// The row uid is the dedupKey-derived identity, NOT the random payload uid:
	// the projection ignores payload.UID so a stamped-wrong uid cannot poison it.
	if row.uid != srUID("gmail:msg-1") {
		t.Errorf("uid = %q, want the dedupKey-derived %q", row.uid, srUID("gmail:msg-1"))
	}
	if _, ok := readSourceRef(t, ctx, d, uid); ok {
		t.Errorf("a row exists at the payload uid %q; identity must be derived from dedup_key", uid)
	}
}

// TestApply_SourceRef_UIDDerivedNotTrusted asserts the projection derives the
// row uid from dedup_key and ignores a mismatched payload.UID. This pins the
// invariant that recordSourceRef's returned sourceRefs/{uid} (also derived)
// always resolves, even against a hand-appended event whose payload uid is
// arbitrary.
func TestApply_SourceRef_UIDDerivedNotTrusted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	bogusPayloadUID := newFactULID(t)
	ev := makeSourceRefEvent(t, bogusPayloadUID, "web:page-9", "page-9", "https://example.com/9")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply source ref: %v", err)
	}

	if _, ok := readSourceRef(t, ctx, d, bogusPayloadUID); ok {
		t.Errorf("row found at the bogus payload uid %q; uid must be derived from dedup_key", bogusPayloadUID)
	}
	row, ok := readSourceRef(t, ctx, d, srUID("web:page-9"))
	if !ok {
		t.Fatal("no row at the dedupKey-derived uid")
	}
	if row.uid != srUID("web:page-9") {
		t.Errorf("uid = %q, want %q", row.uid, srUID("web:page-9"))
	}
}

// TestApply_SourceRef_DedupKeyUpsert asserts that two distinct events sharing a
// dedup_key collapse to one row, the surviving uid is the first-applied one, and
// the payload columns are updated to the second event's values.
func TestApply_SourceRef_DedupKeyUpsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	uid1, uid2 := newFactULID(t), newFactULID(t)
	if err := d.Apply(ctx, []kernel.Event{
		makeSourceRefEvent(t, uid1, "dup:key", "idA", "https://example.com/a"),
		makeSourceRefEvent(t, uid2, "dup:key", "idB", "https://example.com/b"),
	}); err != nil {
		t.Fatalf("apply source refs: %v", err)
	}

	if n := countSourceRefsByDedup(t, ctx, d, "dup:key"); n != 1 {
		t.Fatalf("got %d rows for dedup_key, want 1", n)
	}
	// Both events share a dedup_key, so both derive the SAME row uid; the row
	// lives at that derived uid and the second event's payload columns overwrite
	// it. Neither random payload uid ever becomes a row identity.
	row, ok := readSourceRef(t, ctx, d, srUID("dup:key"))
	if !ok {
		t.Fatal("derived-uid row missing after dedup upsert")
	}
	if row.externalID != "idB" {
		t.Errorf("external_id = %q, want idB (second event's value)", row.externalID)
	}
	if _, ok := readSourceRef(t, ctx, d, uid1); ok {
		t.Errorf("payload uid1 %q should not be a row identity", uid1)
	}
	if _, ok := readSourceRef(t, ctx, d, uid2); ok {
		t.Errorf("payload uid2 %q should not be a row identity", uid2)
	}
}

// TestApply_SourceRef_EmptyDedupKeyRejected asserts an empty dedup_key fails the
// apply rather than inserting a row that could collide with other empty keys.
func TestApply_SourceRef_EmptyDedupKeyRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	ev := makeSourceRefEvent(t, newFactULID(t), "", "msg-x", "https://example.com/x")
	if err := d.Apply(ctx, []kernel.Event{ev}); err == nil {
		t.Fatal("expected apply to reject an empty dedupKey, got nil")
	}
}

// TestApply_SourceRef_RebuildIdempotent re-applies the same batch (as Rebuild
// does, since HasEvent does not track sourceRefRecorded) and asserts the
// projected rows are byte-identical.
func TestApply_SourceRef_RebuildIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	uid1, uid2 := newFactULID(t), newFactULID(t)
	batch := []kernel.Event{
		makeSourceRefEvent(t, uid1, "k1", "id1", "https://example.com/1"),
		makeSourceRefEvent(t, uid2, "k2", "id2", "https://example.com/2"),
	}
	if err := d.Apply(ctx, batch); err != nil {
		t.Fatalf("apply batch (first): %v", err)
	}
	before1, _ := readSourceRef(t, ctx, d, srUID("k1"))
	before2, _ := readSourceRef(t, ctx, d, srUID("k2"))

	// Replay the identical batch.
	if err := d.Apply(ctx, batch); err != nil {
		t.Fatalf("apply batch (replay): %v", err)
	}
	after1, _ := readSourceRef(t, ctx, d, srUID("k1"))
	after2, _ := readSourceRef(t, ctx, d, srUID("k2"))

	if before1 != after1 || before2 != after2 {
		t.Errorf("rows changed across re-apply:\n  uid1 %+v -> %+v\n  uid2 %+v -> %+v",
			before1, after1, before2, after2)
	}
	if n := countSourceRefsByDedup(t, ctx, d, "k1") + countSourceRefsByDedup(t, ctx, d, "k2"); n != 2 {
		t.Errorf("got %d rows after replay, want 2", n)
	}
}
