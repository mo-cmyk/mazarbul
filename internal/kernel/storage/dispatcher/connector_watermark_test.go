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

// cwAdvanceTime is a fixed instant so projected rows are byte-comparable across
// re-applies.
var cwAdvanceTime = time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

// makeWatermarkEvent builds a KindConnectorWatermarkAdvanced event.
func makeWatermarkEvent(t *testing.T, connector, stream string, seq uint64, cursor string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.ConnectorWatermarkAdvancedEvent{
		Connector:   connector,
		Stream:      stream,
		Cursor:      json.RawMessage(cursor),
		Sequence:    seq,
		AdvanceTime: cwAdvanceTime,
	})
	if err != nil {
		t.Fatalf("marshal watermark payload: %v", err)
	}
	return kernel.Event{
		UID:        newFactULID(t),
		Kind:       kernel.EventKind(ontology.KindConnectorWatermarkAdvanced),
		Source:     "test:watermark",
		Confidence: 1.0,
		CreateTime: cwAdvanceTime,
		Payload:    payload,
	}
}

type watermarkRow struct {
	connector   string
	stream      string
	cursor      string
	sequence    uint64
	advanceTime string
	scope       string
}

func readWatermark(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, connector, stream string) (watermarkRow, bool) {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var r watermarkRow
	qerr := reader.QueryRowContext(ctx,
		`SELECT connector, stream, cursor, sequence, advance_time, scope
		   FROM connector_watermarks WHERE connector = ? AND stream = ?`, connector, stream).
		Scan(&r.connector, &r.stream, &r.cursor, &r.sequence, &r.advanceTime, &r.scope)
	if qerr == sql.ErrNoRows {
		return watermarkRow{}, false
	}
	if qerr != nil {
		t.Fatalf("query connector_watermarks: %v", qerr)
	}
	return r, true
}

// TestApply_ConnectorWatermark_Projects asserts a connectorWatermarkAdvanced
// event lands in connector_watermarks with its cursor stored verbatim and scope
// defaulted to personal (the #206 fail-safe).
func TestApply_ConnectorWatermark_Projects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	ev := makeWatermarkEvent(t, "local-git", "commits", 1, `{"lastSHA":"abc"}`)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply watermark: %v", err)
	}

	row, ok := readWatermark(t, ctx, d, "local-git", "commits")
	if !ok {
		t.Fatal("connector_watermarks row not found")
	}
	if row.cursor != `{"lastSHA":"abc"}` {
		t.Errorf("cursor = %q, want verbatim json", row.cursor)
	}
	if row.sequence != 1 {
		t.Errorf("sequence = %d, want 1", row.sequence)
	}
	if row.scope != string(kernel.ScopePersonal) {
		t.Errorf("scope = %q, want personal", row.scope)
	}
}

// TestApply_ConnectorWatermark_MonotonicGate asserts the sequence-gated upsert:
// an advance wins, a replay at the same sequence is a no-op, and a regression to
// a lower sequence is a no-op (never resets the watermark).
func TestApply_ConnectorWatermark_MonotonicGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	// Advance to seq 2.
	if err := d.Apply(ctx, []kernel.Event{
		makeWatermarkEvent(t, "local-git", "commits", 1, `{"lastSHA":"one"}`),
		makeWatermarkEvent(t, "local-git", "commits", 2, `{"lastSHA":"two"}`),
	}); err != nil {
		t.Fatalf("apply advance: %v", err)
	}
	row, _ := readWatermark(t, ctx, d, "local-git", "commits")
	if row.sequence != 2 || row.cursor != `{"lastSHA":"two"}` {
		t.Fatalf("after advance: seq=%d cursor=%q, want 2 / two", row.sequence, row.cursor)
	}

	// Regression to seq 1 must be a no-op (cursor/seq stay at 2).
	if err := d.Apply(ctx, []kernel.Event{
		makeWatermarkEvent(t, "local-git", "commits", 1, `{"lastSHA":"REGRESS"}`),
	}); err != nil {
		t.Fatalf("apply regression: %v", err)
	}
	row, _ = readWatermark(t, ctx, d, "local-git", "commits")
	if row.sequence != 2 || row.cursor != `{"lastSHA":"two"}` {
		t.Errorf("after regression: seq=%d cursor=%q, want unchanged 2 / two", row.sequence, row.cursor)
	}

	// Replay at the same seq 2 is a no-op too.
	if err := d.Apply(ctx, []kernel.Event{
		makeWatermarkEvent(t, "local-git", "commits", 2, `{"lastSHA":"REPLAY"}`),
	}); err != nil {
		t.Fatalf("apply replay: %v", err)
	}
	row, _ = readWatermark(t, ctx, d, "local-git", "commits")
	if row.sequence != 2 || row.cursor != `{"lastSHA":"two"}` {
		t.Errorf("after replay: seq=%d cursor=%q, want unchanged 2 / two", row.sequence, row.cursor)
	}
}

// TestApply_ConnectorWatermark_StreamsIndependent asserts two streams of the
// same connector keep separate watermark rows.
func TestApply_ConnectorWatermark_StreamsIndependent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	if err := d.Apply(ctx, []kernel.Event{
		makeWatermarkEvent(t, "local-git", "commits", 5, `{"lastSHA":"c"}`),
		makeWatermarkEvent(t, "local-git", "pull-requests", 9, `{"lastPR":42}`),
	}); err != nil {
		t.Fatalf("apply two streams: %v", err)
	}
	c, ok := readWatermark(t, ctx, d, "local-git", "commits")
	if !ok || c.sequence != 5 {
		t.Errorf("commits stream: ok=%v seq=%d, want 5", ok, c.sequence)
	}
	pr, ok := readWatermark(t, ctx, d, "local-git", "pull-requests")
	if !ok || pr.sequence != 9 {
		t.Errorf("pull-requests stream: ok=%v seq=%d, want 9", ok, pr.sequence)
	}
}

// TestApply_ConnectorWatermark_EmptyConnectorRejected asserts a blank connector
// fails the apply rather than inserting a colliding row.
func TestApply_ConnectorWatermark_EmptyConnectorRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	if err := d.Apply(ctx, []kernel.Event{makeWatermarkEvent(t, "", "commits", 1, `{"k":"v"}`)}); err == nil {
		t.Fatal("expected apply to reject an empty connector, got nil")
	}
}

// TestApply_ConnectorWatermark_MalformedCursorRejected asserts a non-object
// cursor fails the apply (defence-in-depth shape guard).
func TestApply_ConnectorWatermark_MalformedCursorRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	if err := d.Apply(ctx, []kernel.Event{makeWatermarkEvent(t, "local-git", "commits", 1, `["array"]`)}); err == nil {
		t.Fatal("expected apply to reject a non-object cursor, got nil")
	}
}

// TestApply_ConnectorWatermark_RebuildIdempotent re-applies the same advancing
// batch (as Rebuild does) and asserts the projected row is byte-identical and
// still a single row.
func TestApply_ConnectorWatermark_RebuildIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	batch := []kernel.Event{
		makeWatermarkEvent(t, "local-git", "commits", 1, `{"lastSHA":"one"}`),
		makeWatermarkEvent(t, "local-git", "commits", 2, `{"lastSHA":"two"}`),
	}
	if err := d.Apply(ctx, batch); err != nil {
		t.Fatalf("apply batch (first): %v", err)
	}
	before, _ := readWatermark(t, ctx, d, "local-git", "commits")

	if err := d.Apply(ctx, batch); err != nil {
		t.Fatalf("apply batch (replay): %v", err)
	}
	after, _ := readWatermark(t, ctx, d, "local-git", "commits")

	if before != after {
		t.Errorf("row changed across re-apply:\n  before %+v\n  after  %+v", before, after)
	}
}
