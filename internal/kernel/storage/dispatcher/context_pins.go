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

// pinTypePin and pinTypeCorrection are the context_pins.pin_type discriminator
// values. The CHECK constraint in migration 0042 rejects any other value, so a
// projection write that drifted from these constants would fail the insert.
const (
	pinTypePin        = "pin"
	pinTypeCorrection = "correction"
)

// errEmptyContextPinUID rejects a contextPinned event whose PinUID is blank.
// PinUID is the context_pins primary key and the handle contextUnpinned
// references, so an empty uid would collide every free-floating pin onto one
// row — a hard decode-time error rather than a row.
var errEmptyContextPinUID = errors.New("contextPinned: pinUid is required")

// errEmptyContextPinText rejects a contextPinned event with empty text — a pin
// with nothing to surface is meaningless.
var errEmptyContextPinText = errors.New("contextPinned: text is required")

// errEmptyContextUnpinPinUID rejects a contextUnpinned tombstone with no target
// pin uid (the grouping key the active fold reads).
var errEmptyContextUnpinPinUID = errors.New("contextUnpinned: pinUid is required")

// errEmptyCorrectionUID rejects a contextCorrected event whose CorrectionUID is
// blank (the context_pins primary key for the correction row).
var errEmptyCorrectionUID = errors.New("contextCorrected: correctionUid is required")

// errEmptyCorrectionTargetID rejects a contextCorrected event with no targetId
// — a correction must name the surfaced item it overrides.
var errEmptyCorrectionTargetID = errors.New("contextCorrected: targetId is required")

// errEmptyCorrectionText rejects a contextCorrected event with empty corrected
// text.
var errEmptyCorrectionText = errors.New("contextCorrected: text is required")

// applyContextPinned projects one contextPinned event into the context_pins
// table as a pin_type='pin' row, then recomputes the row's active flag from its
// tombstone set. The insert is INSERT OR IGNORE keyed on uid, so a Rebuild
// replay is a no-op; the active recompute is a pure fold, so a contextUnpinned
// that already landed (cross-device reorder) correctly retires the pin the
// moment its row appears.
func (d *Dispatcher) applyContextPinned(ctx context.Context, ev kernel.Event) (retErr error) {
	var pin ontology.ContextPinnedEvent
	if err := json.Unmarshal(ev.Payload, &pin); err != nil {
		return fmt.Errorf("decode contextPinned payload: %w", err)
	}
	if pin.PinUID == "" {
		return errEmptyContextPinUID
	}
	if pin.Text == "" {
		return errEmptyContextPinText
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

	if err := insertContextPinRow(ctx, tx, ev, string(pin.PinUID), pinTypePin, pin.Text, pin.TargetRef); err != nil {
		return fmt.Errorf("insert context pin: %w", err)
	}
	if err := recomputeContextPinActive(ctx, tx, string(pin.PinUID)); err != nil {
		return fmt.Errorf("recompute context pin active: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyContextCorrected projects one contextCorrected event into the
// context_pins table as a pin_type='correction' row (text = corrected text,
// target_ref = the surfaced item's TargetId), then recomputes its active flag.
// Corrections are append-only: a fresh CorrectionUID per event, with the
// surfacing layer resolving the latest overlay per TargetId at read time.
func (d *Dispatcher) applyContextCorrected(ctx context.Context, ev kernel.Event) (retErr error) {
	var corr ontology.ContextCorrectedEvent
	if err := json.Unmarshal(ev.Payload, &corr); err != nil {
		return fmt.Errorf("decode contextCorrected payload: %w", err)
	}
	if corr.CorrectionUID == "" {
		return errEmptyCorrectionUID
	}
	if corr.TargetID == "" {
		return errEmptyCorrectionTargetID
	}
	if corr.Text == "" {
		return errEmptyCorrectionText
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

	if err := insertContextPinRow(ctx, tx, ev, string(corr.CorrectionUID), pinTypeCorrection, corr.Text, corr.TargetID); err != nil {
		return fmt.Errorf("insert context correction: %w", err)
	}
	if err := recomputeContextPinActive(ctx, tx, string(corr.CorrectionUID)); err != nil {
		return fmt.Errorf("recompute context correction active: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyContextUnpinned records the contextUnpinned tombstone (keyed by the event
// UID for OR-set idempotency) and recomputes the targeted pin's active flag. If
// the pin row has not arrived yet (cross-device reorder), the recompute no-ops
// on the absent row; when the pin later lands, ITS recompute sees this tombstone
// and retires it — so the fold is order-independent.
func (d *Dispatcher) applyContextUnpinned(ctx context.Context, ev kernel.Event) (retErr error) {
	var unpin ontology.ContextUnpinnedEvent
	if err := json.Unmarshal(ev.Payload, &unpin); err != nil {
		return fmt.Errorf("decode contextUnpinned payload: %w", err)
	}
	if unpin.PinUID == "" {
		return errEmptyContextUnpinPinUID
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

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO context_pin_tombstones
			(tombstone_event_uid, pin_uid, unpinned_at, scope)
		 VALUES (?, ?, ?, ?)`,
		ev.UID, string(unpin.PinUID),
		unpin.UnpinnedAt.UTC().Format(time.RFC3339Nano), scopeValue(ev),
	); err != nil {
		return fmt.Errorf("insert context pin tombstone: %w", err)
	}
	if err := recomputeContextPinActive(ctx, tx, string(unpin.PinUID)); err != nil {
		return fmt.Errorf("recompute context pin active: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// insertContextPinRow upserts one context_pins row inside tx. INSERT OR IGNORE
// keyed on uid makes a Rebuild replay (and a redelivered event) a no-op without
// clobbering the materialized active flag. An empty targetRef binds SQL NULL so
// the nullable column round-trips cleanly through the COALESCE read.
func insertContextPinRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, uid, pinType, text, targetRef string) error {
	var ref any
	if targetRef != "" {
		ref = targetRef
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO context_pins
			(uid, pin_type, text, target_ref, active, source_event_uid, create_time, scope)
		 VALUES (?, ?, ?, ?, 1, ?, ?, ?)`,
		uid, pinType, text, ref, ev.UID,
		ev.CreateTime.UTC().Format(time.RFC3339Nano), scopeValue(ev),
	); err != nil {
		return fmt.Errorf("exec insert context pin row: %w", err)
	}
	return nil
}

// recomputeContextPinActive re-derives the materialized active flag for pinUID
// from its FULL tombstone set: active=0 when any context_pin_tombstones row
// targets it, else 1. It is the SOLE authority for the active column, which is
// what makes retirement order-independent (the pin event always inserts active=1
// then this fold corrects it) and idempotent (re-running yields the same value).
// A no-op on an absent pin row is intentional: an unpin that races ahead of its
// pin recomputes nothing now and is folded in when the pin row lands.
func recomputeContextPinActive(ctx context.Context, tx *sql.Tx, pinUID string) error {
	// The tombstone must match the pin's OWN scope (#206 read isolation): a
	// retirement event recorded in one scope must not deactivate a pin in
	// another, even though pin_uid is globally unique. The correlated
	// context_pins.scope keeps the fold convergent — once the pin row lands, it
	// folds only same-scope tombstones — without leaking a cross-scope effect.
	if _, err := tx.ExecContext(ctx,
		`UPDATE context_pins
		    SET active = CASE
		        WHEN EXISTS (
		            SELECT 1 FROM context_pin_tombstones t
		            WHERE t.pin_uid = ? AND t.scope = context_pins.scope
		        ) THEN 0
		        ELSE 1
		    END
		  WHERE uid = ?`,
		pinUID, pinUID,
	); err != nil {
		return fmt.Errorf("exec recompute active: %w", err)
	}
	return nil
}
