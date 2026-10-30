package dispatcher

import (
	"context"
	"crypto/md5" //nolint:gosec // non-cryptographic dedup key; see factContentHash
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// applyFact runs the single-layer (SQLite) write for one Fact event. Unlike a
// Relation, a Fact is a scalar slot rather than an edge, so there is no
// LadybugDB / vector / FTS fan-out — only the relational facts table.
//
// The write is idempotent under Rebuild: writeFactRow upserts the row by uid
// and then reconcileFactSlot re-derives the validity window + supersede chain
// for the whole (subject_uid, state_key) slot as a pure function of the rows
// present, so replaying the event log in order always converges to the same
// state regardless of how many times any single event is applied.
func (d *Dispatcher) applyFact(ctx context.Context, ev kernel.Event) (retErr error) {
	var fact ontology.Fact
	if err := json.Unmarshal(ev.Payload, &fact); err != nil {
		return fmt.Errorf("decode fact payload: %w", err)
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

	if err := writeFactRow(ctx, tx, ev, fact); err != nil {
		return fmt.Errorf("write fact row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// writeFactRow upserts the Fact row inside tx and reconciles its slot. Each
// fact event inserts its own row, keyed by uid (idempotent on Rebuild via
// ON CONFLICT). The slot's current value is the HLC-latest live row, derived
// by reconcileFactSlot as a pure fold over all rows in the slot. Every event
// therefore participates in the causal ordering regardless of arrival order,
// making the slot converge on any device that has applied the same set of
// events.
func writeFactRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, fact ontology.Fact) error {
	uid := fact.UID
	if uid == "" {
		uid = ev.UID
	}
	hash := fact.ContentHash
	if hash == "" {
		hash = factContentHash(fact.SubjectUID, fact.StateKey, fact.Value)
	}
	sourceEventUID := fact.SourceEventUID
	if sourceEventUID == "" {
		sourceEventUID = uid
	}
	createTime := ev.CreateTime.UTC().Format(time.RFC3339Nano)
	var eventStart any
	if fact.EventStart != nil {
		eventStart = fact.EventStart.UTC().Format(time.RFC3339Nano)
	}
	// SourcePriority is clamped to the column default (1.0) when unset/zero; the
	// ontology documents zero as "treat as default 1.0".
	priority := fact.SourcePriority
	if priority <= 0 {
		priority = 1.0
	}

	// Route any late-arriving fact for a merged-away subject to the surviving
	// entity, so FKs always land on an entity the redirect table recognises.
	subjectUID, err := resolveRedirect(ctx, tx, fact.SubjectUID)
	if err != nil {
		return fmt.Errorf("resolve fact subject redirect: %w", err)
	}

	// Insert (or update in place on exact-event replay). event_end /
	// superseded_by_uid are left NULL here and (re)derived by reconcileFactSlot.
	// source + confidence are denormalised from the event so that
	// get(facts/{uid}) can surface provenance without joining the
	// episodes-only provenance table. hlc carries the event's HLC stamp so
	// readFactSlot can order by causal time rather than wall-clock create_time.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO facts
			(uid, subject_uid, state_key, value, event_start, event_end,
			 create_time, source_event_uid, content_hash, superseded_by_uid,
			 source_priority, source, confidence, scope, hlc)
		 VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, NULL, ?, ?, ?, ?, ?)
		 ON CONFLICT(uid) DO UPDATE SET
			subject_uid      = excluded.subject_uid,
			state_key        = excluded.state_key,
			value            = excluded.value,
			event_start      = excluded.event_start,
			create_time      = excluded.create_time,
			source_event_uid = excluded.source_event_uid,
			content_hash     = excluded.content_hash,
			source_priority  = excluded.source_priority,
			source           = excluded.source,
			confidence       = excluded.confidence,
			scope            = excluded.scope,
			hlc              = excluded.hlc`,
		uid, subjectUID, fact.StateKey, fact.Value, eventStart,
		createTime, sourceEventUID, hash, priority, ev.Source, ev.Confidence, scopeValue(ev),
		ev.EffectiveHLC(),
	); err != nil {
		return fmt.Errorf("exec insert fact: %w", err)
	}

	return reconcileFactSlot(ctx, tx, subjectUID, fact.StateKey, scopeValue(ev))
}

// factSlotRow is one Fact's uid + effective start within a slot, plus its
// retraction tombstone (retractedAt empty ⇒ live).
type factSlotRow struct{ uid, start, retractedAt string }

// reconcileFactSlot re-derives the validity window and supersede pointers for
// every Fact occupying (subject_uid, state_key, scope). Rows are ordered by
// their effective start (event_start, falling back to create_time) then uid.
//
// The slot's current value is the CHRONOLOGICALLY-LAST row — UNLESS that row
// carries a retraction tombstone (#0046), in which case the slot has NO current
// value (it reads as absent). Retraction is "this value stopped being true",
// not "undo": a retracted current value empties the slot until a newer write
// reopens it; the slot does NOT silently fall back to an earlier value. This
// also keeps the temporal history gap-free — the interval between a retraction
// and the next write is genuinely empty, so no live row's validity window has
// to span it (the alternative, reverting to a prior value, would need that row
// to hold two disjoint intervals, which the single event_end column cannot
// represent).
//
// Every non-current live row is closed at the next row's effective start and
// points superseded_by_uid at it; a retracted row is force-closed at its own
// retracted_at with no superseder. The slot is scope-partitioned (#206): a fact
// never supersedes one in another scope, so each scope keeps its own current
// value. Being a pure function of the row set + retracted_at, it is idempotent
// under replay.
func reconcileFactSlot(ctx context.Context, tx *sql.Tx, subjectUID, stateKey, scope string) error {
	slot, err := readFactSlot(ctx, tx, subjectUID, stateKey, scope)
	if err != nil {
		return err
	}
	// The open value is the chronologically-last row, unless it is retracted —
	// then the slot is empty (no fallback to an earlier value).
	openIdx := -1
	if n := len(slot); n > 0 && slot[n-1].retractedAt == "" {
		openIdx = n - 1
	}
	for i, r := range slot {
		switch {
		case r.retractedAt != "":
			// Retracted: closed at the retraction instant, no superseder.
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE facts SET event_end = ?, superseded_by_uid = NULL WHERE uid = ?`,
				r.retractedAt, r.uid); uerr != nil {
				return fmt.Errorf("close retracted fact: %w", uerr)
			}
		case i == openIdx:
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE facts SET event_end = NULL, superseded_by_uid = NULL WHERE uid = ?`,
				r.uid); uerr != nil {
				return fmt.Errorf("open current fact: %w", uerr)
			}
		default:
			// A superseded historical value: closed at the next row's effective
			// start, pointing at the row that superseded it. That successor may
			// itself be retracted — the pointer is a faithful history link (this
			// value WAS superseded then), and the retracted row's own tombstone
			// keeps it out of current reads.
			next := slot[i+1]
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE facts SET event_end = ?, superseded_by_uid = ? WHERE uid = ?`,
				next.start, next.uid, r.uid); uerr != nil {
				return fmt.Errorf("close superseded fact: %w", uerr)
			}
		}
	}
	return nil
}

// readFactSlot returns every Fact in (subject_uid, state_key, scope) as uid +
// effective start, ordered by HLC then uid. Ordering by HLC makes the fold
// commutative: any arrival order of the same event set produces the same slot
// state, because the HLC stamp is fixed at write time and is the same on every
// device that applies the same event. The effective start (event_start falling
// back to create_time) is still returned so reconcileFactSlot can use it as
// the event_end boundary when closing superseded rows.
//
// The slot is scope-partitioned (#206) so each scope's supersede chain is
// derived only from its own rows. The cursor is fully drained and closed
// (via defer) before the caller issues any write, so SQLite's single writer
// connection never interleaves a read cursor with UPDATEs in the tx.
func readFactSlot(ctx context.Context, tx *sql.Tx, subjectUID, stateKey, scope string) (_ []factSlotRow, retErr error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT uid, COALESCE(event_start, create_time) AS eff_start, COALESCE(retracted_at, '')
		 FROM facts
		 WHERE subject_uid = ? AND state_key = ? AND scope = ?
		 ORDER BY COALESCE(hlc, printf('%012x%04x',
		              CAST(ROUND((julianday(create_time) - 2440587.5) * 86400000.0) AS INTEGER), 0)) ASC,
		          uid ASC`,
		subjectUID, stateKey, scope)
	if err != nil {
		return nil, fmt.Errorf("query fact slot: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close fact slot rows: %w", cerr)
		}
	}()

	var slot []factSlotRow
	for rows.Next() {
		var r factSlotRow
		if serr := rows.Scan(&r.uid, &r.start, &r.retractedAt); serr != nil {
			return nil, fmt.Errorf("scan fact slot row: %w", serr)
		}
		slot = append(slot, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate fact slot: %w", rerr)
	}
	return slot, nil
}

// factContentHash is the stable dedup key for a Fact: hex-encoded MD5 over
// subject_uid|state_key|value, matching the contract documented on the facts
// table (migration 0014). MD5 is used as a non-cryptographic content key only —
// collisions here merely risk suppressing a distinct value in the same slot,
// which is acceptable for a local dedup hint.
func factContentHash(subjectUID, stateKey, value string) string {
	sum := md5.Sum([]byte(subjectUID + "|" + stateKey + "|" + value)) //nolint:gosec // non-cryptographic dedup key
	return hex.EncodeToString(sum[:])
}

// factSlotAddress is the (subject_uid, state_key, scope) a fact row occupies —
// the key reconcileFactSlot needs after the row is mutated or removed.
type factSlotAddress struct{ subjectUID, stateKey, scope string }

// readFactSlotAddress returns the slot a fact uid occupies. A missing row
// yields ok=false so the retract/forget handlers no-op idempotently (the fact
// may already be forgotten, or the tombstone is replaying ahead of its target).
func readFactSlotAddress(ctx context.Context, tx *sql.Tx, uid string) (addr factSlotAddress, ok bool, err error) {
	row := tx.QueryRowContext(ctx,
		`SELECT subject_uid, state_key, scope FROM facts WHERE uid = ?`, uid)
	switch serr := row.Scan(&addr.subjectUID, &addr.stateKey, &addr.scope); {
	case errors.Is(serr, sql.ErrNoRows):
		return factSlotAddress{}, false, nil
	case serr != nil:
		return factSlotAddress{}, false, fmt.Errorf("read fact slot address: %w", serr)
	}
	return addr, true, nil
}

// applyFactRetracted stamps the targeted fact's retracted_at tombstone and
// re-derives its slot. ReconcileFactSlot then closes the retracted row at
// retracted_at and, when it was the slot's current value, leaves the slot EMPTY
// (no fallback to an earlier value) until a newer write reopens it. Idempotent:
// replaying the same event re-stamps the same instant and re-folds to the same
// state; a target row that no longer exists is a no-op.
func (d *Dispatcher) applyFactRetracted(ctx context.Context, ev kernel.Event) (retErr error) {
	var ret ontology.FactRetractedEvent
	if err := json.Unmarshal(ev.Payload, &ret); err != nil {
		return fmt.Errorf("decode factRetracted payload: %w", err)
	}
	if ret.FactUID == "" {
		return errors.New("factRetracted: missing factUid")
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

	addr, ok, err := readFactSlotAddress(ctx, tx, ret.FactUID)
	if err != nil {
		return err
	}
	if !ok {
		committed = true // nothing to retract; commit the empty tx to release it
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit empty retract: %w", err)
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE facts SET retracted_at = ? WHERE uid = ?`,
		ret.RetractedAt.UTC().Format(time.RFC3339Nano), ret.FactUID,
	); err != nil {
		return fmt.Errorf("stamp fact retraction: %w", err)
	}
	if err := reconcileFactSlot(ctx, tx, addr.subjectUID, addr.stateKey, addr.scope); err != nil {
		return fmt.Errorf("reconcile retracted fact slot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyFactForgotten hard-deletes the targeted fact row and re-derives the slot
// it occupied so the supersede chain converges without it. Unlike
// applyFactRetracted the row does not survive. Idempotent under replay: DELETE
// on a missing fact is a no-op.
func (d *Dispatcher) applyFactForgotten(ctx context.Context, ev kernel.Event) (retErr error) {
	var payload ontology.FactForgottenEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("decode factForgotten payload: %w", err)
	}
	if payload.FactUID == "" {
		return errors.New("factForgotten: missing factUid")
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

	addr, ok, err := readFactSlotAddress(ctx, tx, payload.FactUID)
	if err != nil {
		return err
	}
	if !ok {
		committed = true // already gone; nothing to delete
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit empty forget: %w", err)
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM facts WHERE uid = ?`, payload.FactUID); err != nil {
		return fmt.Errorf("delete fact: %w", err)
	}
	if err := reconcileFactSlot(ctx, tx, addr.subjectUID, addr.stateKey, addr.scope); err != nil {
		return fmt.Errorf("reconcile forgotten fact slot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
