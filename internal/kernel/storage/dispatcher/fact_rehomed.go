package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// applyFactRehomed moves a Fact from its current subject entity to the new
// subject entity recorded in the event payload. Only the facts projection
// (SQLite) is mutated; the event log is append-only.
//
// Idempotency: if the fact row already has subject_uid == ToSubjectUID the
// function returns nil immediately. This makes replaying the same rehome event
// twice (or applying it across two devices) a safe no-op that converges to the
// same state regardless of how many times it runs.
//
// After moving the row the function re-derives the supersede chains for both
// the old slot (fromSubjectUID, stateKey) and the new slot (toSubjectUID,
// stateKey) via reconcileFactSlot, so the validity windows stay consistent.
// The content_hash is recomputed for the new (subjectUID, stateKey, value)
// triple because observationContentHash includes the subject so that the same
// observation text homed to two different subjects yields two distinct,
// non-colliding content keys.
func (d *Dispatcher) applyFactRehomed(ctx context.Context, ev kernel.Event) (retErr error) {
	var rehome ontology.FactRehomed
	if err := json.Unmarshal(ev.Payload, &rehome); err != nil {
		return fmt.Errorf("decode factRehomed payload: %w", err)
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

	// Read the fact row: current subject_uid, state_key, value, scope.
	// If the row does not exist yet (a rehome replayed before its fact event in
	// a cross-device reorder), record nothing and return cleanly; the rehome
	// will be re-applied when the fact event lands and the row appears.
	var (
		currentSubject string
		stateKey       string
		value          string
		scope          string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT subject_uid, state_key, value, scope FROM facts WHERE uid = ?`,
		rehome.FactUID,
	).Scan(&currentSubject, &stateKey, &value, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		// Fact not yet projected; record the rehome as a no-op for now. The
		// rebuild path replays in log order so the fact event will land first
		// in normal operation; cross-device reorders are the edge case.
		if err2 := tx.Commit(); err2 != nil {
			return fmt.Errorf("commit (fact absent): %w", err2)
		}
		committed = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read fact row: %w", err)
	}

	// Idempotent fast-path: fact is already on the target subject.
	if currentSubject == rehome.ToSubjectUID {
		if err2 := tx.Commit(); err2 != nil {
			return fmt.Errorf("commit (already homed): %w", err2)
		}
		committed = true
		return nil
	}

	// Recompute content_hash for the new (subjectUID, stateKey, value) triple
	// using the SHARED observation hash (ontology.ObservationContentHash) so a
	// re-homed fact and a freshly re-imported one land on byte-identical hashes
	// and dedup against the same open slot. Using factContentHash (MD5) here
	// would diverge from the importer's SHA-256 hash and leave duplicate open
	// facts on the live rebuild path.
	newHash := ontology.ObservationContentHash(rehome.ToSubjectUID, stateKey, value)

	if _, err := tx.ExecContext(ctx,
		`UPDATE facts SET subject_uid = ?, content_hash = ? WHERE uid = ?`,
		rehome.ToSubjectUID, newHash, rehome.FactUID,
	); err != nil {
		return fmt.Errorf("update fact subject: %w", err)
	}

	// Reconcile the old slot so any remaining facts in (fromSubjectUID,
	// stateKey, scope) get a consistent supersede chain.
	if err := reconcileFactSlot(ctx, tx, rehome.FromSubjectUID, stateKey, scope); err != nil {
		return fmt.Errorf("reconcile old slot: %w", err)
	}

	// Reconcile the new slot so the moved fact is correctly ordered relative to
	// any pre-existing facts on (toSubjectUID, stateKey, scope).
	if err := reconcileFactSlot(ctx, tx, rehome.ToSubjectUID, stateKey, scope); err != nil {
		return fmt.Errorf("reconcile new slot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
