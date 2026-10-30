package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Reader is the read-model the orchestrator consults for dedup + resume state.
// It is satisfied by *SQLReader over the SQLite projection; tests pass a fake so
// the orchestrator can be exercised without the CGO sqlite driver.
type Reader interface {
	// SourceRefHash returns the stored content hash for a dedup key and whether a
	// SourceRef row already exists (the idempotency probe).
	SourceRefHash(ctx context.Context, dedupKey string) (string, bool, error)
	// Watermark returns the persisted resume position for one (connector, stream).
	// A missing row is the zero watermark (first connect / full backfill).
	Watermark(ctx context.Context, connector, stream string) (Watermark, error)
}

// SQLReader is the production Reader over the SQLite read pool.
type SQLReader struct{ db *sql.DB }

// NewSQLReader wraps a SQLite reader pool as a Reader.
func NewSQLReader(db *sql.DB) *SQLReader { return &SQLReader{db: db} }

// SourceRefHash reads source_refs.content_hash by the dedup-key-derived uid
// (ontology.SourceRefUID). A missing row reports found=false.
func (r *SQLReader) SourceRefHash(ctx context.Context, dedupKey string) (string, bool, error) {
	uid := string(ontology.SourceRefUID(dedupKey))
	var hash sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT content_hash FROM source_refs WHERE uid = ?`, uid).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("source ref dedup query: %w", err)
	}
	return hash.String, true, nil
}

// WatermarkStatus is one connector_watermarks row, flattened for listSources.
type WatermarkStatus struct {
	Connector   string `json:"connector"`
	Stream      string `json:"stream"`
	Sequence    uint64 `json:"sequence"`
	AdvanceTime string `json:"advanceTime,omitempty"`
}

// Watermark health tokens. The string values are a stable wire + CLI contract
// (listSources JSON, `maz source list`, `maz doctor`).
const (
	WatermarkLive  = "live"  // advanced within the last day
	WatermarkStale = "stale" // not advanced for more than a day
	WatermarkDown  = "down"  // not advanced for more than a week, or never advanced
)

// Watermark health thresholds. These match the `maz watch` [5] sources pane
// glyph thresholds (● live / ▲ stale / ✕ down), so the dashboard and the
// scriptable surfaces agree on what counts as stale or down.
const (
	watermarkLiveWithin = 24 * time.Hour
	watermarkDownAfter  = 7 * 24 * time.Hour
)

// ClassifyWatermark derives a health token and the age in seconds for a
// watermark's advance time relative to now. An empty or unparseable advance
// time — a source that has a row but never advanced — is WatermarkDown with
// age -1 (unknown). This is the single source of truth for source freshness,
// shared by listSources, `maz source list`, and `maz doctor`.
func ClassifyWatermark(advanceTime string, now time.Time) (status string, ageSeconds int64) {
	if advanceTime == "" {
		return WatermarkDown, -1
	}
	t, err := time.Parse(time.RFC3339Nano, advanceTime)
	if err != nil {
		return WatermarkDown, -1
	}
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	secs := int64(age.Seconds())
	switch {
	case age <= watermarkLiveWithin:
		return WatermarkLive, secs
	case age <= watermarkDownAfter:
		return WatermarkStale, secs
	default:
		return WatermarkDown, secs
	}
}

// ListWatermarks returns every persisted watermark, ordered (connector, stream),
// for the listSources read surface.
func (r *SQLReader) ListWatermarks(ctx context.Context) ([]WatermarkStatus, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT connector, stream, sequence, advance_time
		   FROM connector_watermarks ORDER BY connector, stream`)
	if err != nil {
		return nil, fmt.Errorf("list watermarks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WatermarkStatus
	for rows.Next() {
		var w WatermarkStatus
		if err := rows.Scan(&w.Connector, &w.Stream, &w.Sequence, &w.AdvanceTime); err != nil {
			return nil, fmt.Errorf("scan watermark: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watermarks: %w", err)
	}
	return out, nil
}

// Watermark reads connector_watermarks for one (connector, stream). A missing
// row is the zero watermark.
func (r *SQLReader) Watermark(ctx context.Context, connector, stream string) (Watermark, error) {
	wm := Watermark{Connector: connector, Stream: stream}
	var cursor string
	var seq uint64
	err := r.db.QueryRowContext(ctx,
		`SELECT cursor, sequence FROM connector_watermarks WHERE connector = ? AND stream = ?`,
		connector, stream).Scan(&cursor, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return wm, nil
	}
	if err != nil {
		return wm, fmt.Errorf("load watermark: %w", err)
	}
	wm.Cursor = json.RawMessage(cursor)
	wm.Sequence = seq
	return wm, nil
}
