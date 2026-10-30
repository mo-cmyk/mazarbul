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

// errEmptyConnectorWatermark rejects a connectorWatermarkAdvanced event whose
// Connector is blank. Connector is half the (connector, stream) projection key;
// an empty connector would let unrelated sources collide on one row.
var errEmptyConnectorWatermark = errors.New("connectorWatermarkAdvanced: connector is required")

// applyConnectorWatermarkAdvanced runs the single-layer (SQLite) write for one
// connectorWatermarkAdvanced event. Like a SourceRef, a watermark is a scalar
// projection row rather than an edge, so there is no LadybugDB / vector / FTS
// fan-out — only the relational connector_watermarks table.
//
// The cursor is re-validated for SHAPE (size cap + well-formed JSON object) as
// defence-in-depth — a hand-corrupted log entry is refused rather than stored.
// Monotonicity is NOT re-checked here: a Rebuild legitimately replays an event
// whose sequence equals the persisted one, so the write uses a sequence-gated
// UPSERT (write only when excluded.sequence > the stored sequence). A replayed
// or regressing event is therefore a silent no-op, which keeps Rebuild
// idempotent (it always converges to the highest-sequence cursor) and prevents
// a forged regressing event from poisoning the projection.
func (d *Dispatcher) applyConnectorWatermarkAdvanced(ctx context.Context, ev kernel.Event) (retErr error) {
	var wm ontology.ConnectorWatermarkAdvancedEvent
	if err := json.Unmarshal(ev.Payload, &wm); err != nil {
		return fmt.Errorf("decode connectorWatermarkAdvanced payload: %w", err)
	}
	if wm.Connector == "" {
		return errEmptyConnectorWatermark
	}
	if err := ontology.ValidateCursorShape(wm.Cursor); err != nil {
		return fmt.Errorf("connectorWatermarkAdvanced: %w", err)
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

	if err := writeConnectorWatermarkRow(ctx, tx, ev, wm); err != nil {
		return fmt.Errorf("write connector watermark row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// writeConnectorWatermarkRow upserts the connector_watermarks row inside tx. The
// conflict target is (connector, stream, scope) (migration 0047); the DO UPDATE
// is gated on excluded.sequence > connector_watermarks.sequence so the
// projection is a monotone head-state: replays and regressions are no-ops,
// advances win. Scope is part of the key so a personal-scope and a team-scope
// advance of the same (connector, stream) keep independent head-state and never
// cross-contaminate each other's resume/freshness signal. The cursor is stored
// verbatim as TEXT (it is opaque to the projection).
func writeConnectorWatermarkRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, wm ontology.ConnectorWatermarkAdvancedEvent) error {
	advanceTime := wm.AdvanceTime.UTC().Format(time.RFC3339Nano)

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO connector_watermarks
			(connector, stream, cursor, sequence, advance_time, scope)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(connector, stream, scope) DO UPDATE SET
			cursor       = excluded.cursor,
			sequence     = excluded.sequence,
			advance_time = excluded.advance_time
		 WHERE excluded.sequence > connector_watermarks.sequence`,
		wm.Connector, wm.Stream, string(wm.Cursor), wm.Sequence, advanceTime, scopeValue(ev),
	); err != nil {
		return fmt.Errorf("exec insert connector watermark: %w", err)
	}
	return nil
}
