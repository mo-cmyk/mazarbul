package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// errEmptySourceRefDedupKey rejects a sourceRefRecorded event whose DedupKey is
// blank. DedupKey is the UNIQUE(dedup_key) upsert target; allowing an empty key
// would let two distinct SourceRefs collide on "" and silently overwrite one
// another, so an empty key is a hard decode-time error rather than a row.
var errEmptySourceRefDedupKey = errors.New("sourceRefRecorded: dedupKey is required")

// applySourceRefRecorded runs the single-layer (SQLite) write for one
// sourceRefRecorded event. Like a Fact, a SourceRef is a scalar provenance
// record rather than an edge, so there is no LadybugDB / vector / FTS fan-out —
// only the relational source_refs table.
//
// The write is idempotent under Rebuild: writeSourceRefRow upserts the row on
// the UNIQUE(dedup_key) constraint without touching uid, so replaying the event
// log in order always converges to the same state regardless of how many times
// any single event is applied.
func (d *Dispatcher) applySourceRefRecorded(ctx context.Context, ev kernel.Event) (retErr error) {
	var sr ontology.SourceRefRecordedEvent
	if err := json.Unmarshal(ev.Payload, &sr); err != nil {
		return fmt.Errorf("decode sourceRefRecorded payload: %w", err)
	}
	if sr.DedupKey == "" {
		return errEmptySourceRefDedupKey
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if err := writeSourceRefRow(ctx, tx, ev, sr); err != nil {
		return fmt.Errorf("write source ref row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// writeSourceRefRow upserts the SourceRef row inside tx. The conflict target is
// dedup_key (the idempotency key), not uid: a Rebuild replay of the same event
// re-applies identical values, and a genuinely-duplicate external item (a
// second event sharing the dedup_key) collapses onto the first row.
//
// The row uid is DERIVED from dedup_key (ontology.SourceRefUID), not taken from
// the payload: dedup_key is the SourceRef's identity, so uid is a pure function
// of it and the projection cannot be poisoned by a hand-appended event whose
// payload.UID disagrees. This is what guarantees recordSourceRef's returned
// sourceRefs/{uid} name always resolves. Because the incoming uid therefore
// always equals the existing row's uid, the UPDATE branch leaving uid untouched
// is a provable no-op on identity. Source_event_uid IS re-stamped on conflict,
// so the back-link tracks the most recently applied carrying event — that link,
// unlike uid, is timing-dependent across independently-recorded duplicates.
func writeSourceRefRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, sr ontology.SourceRefRecordedEvent) error {
	createTime := ev.CreateTime.UTC().Format(time.RFC3339Nano)
	fetchTime := sr.FetchTime.UTC().Format(time.RFC3339Nano)
	uid := string(ontology.SourceRefUID(sr.DedupKey))

	// Route the provenance subject FK to the surviving entity if the original
	// subject was merged away.
	subjectUID := string(sr.SubjectUID)
	if subjectUID != "" {
		resolved, rerr := resolveRedirect(ctx, tx, subjectUID)
		if rerr != nil {
			return fmt.Errorf("resolve source ref subject: %w", rerr)
		}
		subjectUID = resolved
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO source_refs
			(uid, subject_uid, source_type, connector, stream, external_id,
			 external_url, content_hash, fetch_time, dedup_key, source_event_uid,
			 create_time, scope, freshness_token, tombstone)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(dedup_key) DO UPDATE SET
			subject_uid      = excluded.subject_uid,
			source_type      = excluded.source_type,
			connector        = excluded.connector,
			stream           = excluded.stream,
			external_id      = excluded.external_id,
			external_url     = excluded.external_url,
			content_hash     = excluded.content_hash,
			fetch_time       = excluded.fetch_time,
			source_event_uid = excluded.source_event_uid,
			create_time      = excluded.create_time,
			scope            = excluded.scope,
			freshness_token  = excluded.freshness_token,
			tombstone        = excluded.tombstone`,
		uid, subjectUID, sr.SourceType, sr.Connector, sr.Stream,
		sr.ExternalID, sr.ExternalURL, sr.ContentHash, fetchTime, sr.DedupKey, ev.UID,
		createTime, scopeValue(ev), nullableText(sr.FreshnessToken), nullableText(sr.Tombstone),
	); err != nil {
		return fmt.Errorf("exec insert source ref: %w", err)
	}
	return nil
}
