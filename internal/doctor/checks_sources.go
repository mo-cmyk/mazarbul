package doctor

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ingest"
)

// newSourceFreshnessCheck reports connectors whose resume watermark has gone
// stale (no advance in over a day) or down (over a week, or never advanced),
// reading the connector_watermarks projection via the shared read-only handle.
//
// It is advisory (WARN, never FAIL): an external source falling behind is not a
// broken install, and `maz doctor` must stay exit-0 in CI regardless of how
// recently a connector last synced. The thresholds match `maz source list` and
// the maz watch [5] sources pane — all three derive freshness from
// ingest.ClassifyWatermark, so the dashboard, the scriptable list, and doctor
// agree on what counts as stale or down.
func newSourceFreshnessCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "sync.source_freshness",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			rows, err := sourceWatermarkRows(ctx, s)
			if err != nil {
				return StatusFail, "", err
			}
			if len(rows) == 0 {
				return StatusOK, "no sources connected", nil
			}
			now := time.Now()
			var stale, down []string
			for _, r := range rows {
				label := r.connector
				if r.stream != "" {
					label += "/" + r.stream
				}
				switch status, _ := ingest.ClassifyWatermark(r.advanceTime, now); status {
				case ingest.WatermarkStale:
					stale = append(stale, label)
				case ingest.WatermarkDown:
					down = append(down, label)
				}
			}
			sort.Strings(stale)
			sort.Strings(down)
			const hint = " — re-run `maz source ingest <connector>` (see `maz source list`)"
			switch {
			case len(down) > 0 && len(stale) > 0:
				return StatusWarn, fmt.Sprintf("down (>7d/never): %v; stale (>1d): %v%s", down, stale, hint), nil
			case len(down) > 0:
				return StatusWarn, fmt.Sprintf("down (>7d/never): %v%s", down, hint), nil
			case len(stale) > 0:
				return StatusWarn, fmt.Sprintf("stale (>1d): %v%s", stale, hint), nil
			default:
				return StatusOK, fmt.Sprintf("%d source(s) fresh", len(rows)), nil
			}
		},
	}
}

// sourceWatermarkRow is one connector_watermarks row for the freshness check.
type sourceWatermarkRow struct {
	connector, stream, advanceTime string
}

// sourceWatermarkRows reads the connector watermarks via the shared read-only
// handle. Sqlite.Open runs every migration, so connector_watermarks is always
// present (empty on a fresh store, not absent).
func sourceWatermarkRows(ctx context.Context, s *sharedSQLite) ([]sourceWatermarkRow, error) {
	store, err := s.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	rows, err := store.Reader().QueryContext(ctx,
		"SELECT connector, stream, COALESCE(advance_time, '') FROM connector_watermarks ORDER BY connector, stream")
	if err != nil {
		return nil, fmt.Errorf("query connector_watermarks: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "doctor: close connector_watermarks rows: %v\n", cerr)
		}
	}()
	var out []sourceWatermarkRow
	for rows.Next() {
		var r sourceWatermarkRow
		if err := rows.Scan(&r.connector, &r.stream, &r.advanceTime); err != nil {
			return nil, fmt.Errorf("scan watermark: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connector_watermarks: %w", err)
	}
	return out, nil
}
