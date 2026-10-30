package dispatcher_test

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

// accessEvent builds a memoryAccessed event for target at the given instant.
func accessEvent(t *testing.T, target string, at time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.MemoryAccessedEvent{
		TargetUID:  ontology.ULID(target),
		AccessedAt: at,
	})
	if err != nil {
		t.Fatalf("marshal memoryAccessed: %v", err)
	}
	return kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindMemoryAccessed),
		Source:     "recall/v1",
		Confidence: 1.0,
		CreateTime: at,
		Payload:    payload,
	}
}

// insightEvent builds an insightSynthesized event with the given source uids.
func insightEvent(t *testing.T, insightUID, statement string, sources []string) kernel.Event {
	t.Helper()
	src := make([]ontology.ULID, len(sources))
	for i, s := range sources {
		src[i] = ontology.ULID(s)
	}
	payload, err := json.Marshal(ontology.InsightSynthesizedEvent{
		InsightUID:            ontology.ULID(insightUID),
		Statement:             statement,
		InsightType:           "pattern",
		SourceUids:            src,
		SeedEntityUID:         ontology.ULID(ulid.Make().String()),
		SynthesizedConfidence: 0.8,
		SynthesizedAt:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal insightSynthesized: %v", err)
	}
	return kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindInsightSynthesized),
		Source:     "reflection/v1",
		Confidence: 0.8,
		CreateTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
}

type strengthRow struct {
	count int
	first sql.NullString
	last  sql.NullString
}

func readStrength(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, target string) (strengthRow, bool) {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	var r strengthRow
	err = reader.QueryRowContext(ctx,
		`SELECT access_count, first_accessed_at, last_accessed_at FROM memory_strength WHERE target_uid = ?`,
		target,
	).Scan(&r.count, &r.first, &r.last)
	if errors.Is(err, sql.ErrNoRows) {
		return strengthRow{}, false
	}
	if err != nil {
		t.Fatalf("read memory_strength: %v", err)
	}
	return r, true
}

// TestApply_MemoryAccessed_FoldsStrength applies three accesses for one target
// and asserts the materialized memory_strength row folds to count=3 with the
// MIN/MAX instants.
func TestApply_MemoryAccessed_FoldsStrength(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	target := ulid.Make().String()

	at1 := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	at2 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at3 := time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC)
	for _, ev := range []kernel.Event{
		accessEvent(t, target, at1),
		accessEvent(t, target, at2),
		accessEvent(t, target, at3),
	} {
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("apply access: %v", err)
		}
	}

	row, ok := readStrength(t, ctx, d, target)
	if !ok {
		t.Fatal("no memory_strength row after 3 accesses")
	}
	if row.count != 3 {
		t.Errorf("access_count = %d, want 3", row.count)
	}
	if got := row.first.String; got != at2.Format(time.RFC3339Nano) {
		t.Errorf("first_accessed_at = %q, want %q (MIN)", got, at2.Format(time.RFC3339Nano))
	}
	if got := row.last.String; got != at3.Format(time.RFC3339Nano) {
		t.Errorf("last_accessed_at = %q, want %q (MAX)", got, at3.Format(time.RFC3339Nano))
	}
}

// TestApply_MemoryAccessed_OrderIndependent is the convergence AC at the
// dispatcher level: the SAME access set (same event UIDs + instants) applied in
// two different orders to two fresh brains yields a byte-identical
// memory_strength row.
func TestApply_MemoryAccessed_OrderIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: exhaustive permutation test (~37s)")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	target := ulid.Make().String()
	e1 := accessEvent(t, target, time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))
	e2 := accessEvent(t, target, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	e3 := accessEvent(t, target, time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC))

	dA := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	for _, ev := range []kernel.Event{e1, e2, e3} {
		if err := dA.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("apply A: %v", err)
		}
	}
	dB := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	for _, ev := range []kernel.Event{e3, e1, e2} {
		if err := dB.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("apply B: %v", err)
		}
	}

	rowA, _ := readStrength(t, ctx, dA, target)
	rowB, _ := readStrength(t, ctx, dB, target)
	if rowA.count != rowB.count || rowA.first.String != rowB.first.String || rowA.last.String != rowB.last.String {
		t.Errorf("order changed folded strength: A=%+v B=%+v", rowA, rowB)
	}
}

// TestApply_MemoryAccessed_Idempotent re-applies the same access event twice and
// asserts the count stays 1 (INSERT OR IGNORE on the access event UID makes the
// OR-set union idempotent — a Rebuild replay does not double-count).
func TestApply_MemoryAccessed_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	target := ulid.Make().String()
	ev := accessEvent(t, target, time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))

	for i := range 2 {
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	row, _ := readStrength(t, ctx, d, target)
	if row.count != 1 {
		t.Errorf("access_count = %d, want 1 (idempotent)", row.count)
	}
}

// TestApply_InsightSynthesized_MaterializesRow projects an insight and asserts
// the row lands hot with current_confidence seeded from synthesized_confidence
// and the source uids persisted as a JSON array.
func TestApply_InsightSynthesized_MaterializesRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	insightUID := ulid.Make().String()
	src := []string{ulid.Make().String(), ulid.Make().String()}

	if err := d.Apply(ctx, []kernel.Event{insightEvent(t, insightUID, "a durable insight", src)}); err != nil {
		t.Fatalf("apply insight: %v", err)
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	var (
		state      string
		synthConf  float64
		currConf   float64
		sourceJSON string
	)
	if err := reader.QueryRowContext(ctx,
		`SELECT state, synthesized_confidence, current_confidence, source_uids FROM insights WHERE uid = ?`,
		insightUID,
	).Scan(&state, &synthConf, &currConf, &sourceJSON); err != nil {
		t.Fatalf("read insight: %v", err)
	}
	if state != "hot" {
		t.Errorf("state = %q, want hot", state)
	}
	if currConf != synthConf {
		t.Errorf("current_confidence %v != synthesized_confidence %v at projection time", currConf, synthConf)
	}
	var gotSrc []string
	if err := json.Unmarshal([]byte(sourceJSON), &gotSrc); err != nil {
		t.Fatalf("source_uids not valid JSON: %v", err)
	}
	if len(gotSrc) != len(src) {
		t.Errorf("source_uids len = %d, want %d", len(gotSrc), len(src))
	}
}

// TestApply_DerivedEvents_EmptySourceUidsRejected pins the auditable-provenance
// AC at the dispatcher: an insightSynthesized / memoryConsolidated with an empty
// sourceUids array fails Apply with no row written.
func TestApply_DerivedEvents_EmptySourceUidsRejected(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	cases := []struct {
		name    string
		kind    string
		table   string
		uidCol  string
		uid     string
		payload map[string]any
	}{
		{
			name:   "insightSynthesized",
			kind:   ontology.KindInsightSynthesized,
			table:  "insights",
			uidCol: "uid",
			uid:    ulid.Make().String(),
		},
		{
			name:   "memoryConsolidated",
			kind:   ontology.KindMemoryConsolidated,
			table:  "consolidated_memories",
			uidCol: "uid",
			uid:    ulid.Make().String(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
			var payload []byte
			if tc.kind == ontology.KindInsightSynthesized {
				payload, _ = json.Marshal(map[string]any{
					"insightUid": tc.uid, "statement": "x", "insightType": "pattern",
					"sourceUids": []string{}, "seedEntityUid": ulid.Make().String(),
					"synthesizedConfidence": 0.5, "synthesizedAt": "2026-01-01T00:00:00Z",
				})
			} else {
				payload, _ = json.Marshal(map[string]any{
					"memoryUid": tc.uid, "summary": "x", "memoryType": "semantic",
					"sourceUids": []string{}, "citedEntityUid": ulid.Make().String(),
					"confidence": 0.5, "consolidatedAt": "2026-01-01T00:00:00Z",
				})
			}
			ev := kernel.Event{
				UID: ulid.Make().String(), Kind: kernel.EventKind(tc.kind),
				Source: "reflection/v1", Confidence: 0.5, Payload: payload,
			}
			if err := d.Apply(ctx, []kernel.Event{ev}); err == nil {
				t.Fatal("expected Apply to reject empty sourceUids, got nil")
			}
			reader, err := d.SQLiteReader()
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			var n int
			if err := reader.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM "+tc.table+" WHERE "+tc.uidCol+" = ?", tc.uid,
			).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("row written despite empty sourceUids: count=%d", n)
			}
		})
	}
}

// TestApply_MemoryAccessed_RepromotesInsight demotes a hot insight in place (as
// the decay sweep would) then applies a memoryAccessed targeting it, and asserts
// the access re-promotes it to hot (reinforce-on-access).
func TestApply_MemoryAccessed_RepromotesInsight(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	insightUID := ulid.Make().String()
	if err := d.Apply(ctx, []kernel.Event{insightEvent(t, insightUID, "insight", []string{ulid.Make().String()})}); err != nil {
		t.Fatalf("apply insight: %v", err)
	}

	writer, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	// Demote in place, mimicking the projection-side decay sweep.
	if _, err := writer.ExecContext(ctx, `UPDATE insights SET state = 'demoted' WHERE uid = ?`, insightUID); err != nil {
		t.Fatalf("demote: %v", err)
	}

	if err := d.Apply(ctx, []kernel.Event{accessEvent(t, insightUID, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))}); err != nil {
		t.Fatalf("apply access: %v", err)
	}

	var state string
	if err := writer.QueryRowContext(ctx, `SELECT state FROM insights WHERE uid = ?`, insightUID).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "hot" {
		t.Errorf("state = %q, want hot (reinforce-on-access must re-promote)", state)
	}
}
