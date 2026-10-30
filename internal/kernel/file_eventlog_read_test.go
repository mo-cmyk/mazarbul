package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// drain collects every (event, error) pair from a Read iterator. The
// first error short-circuits, mirroring how production callers consume
// the iterator.
func drain(t *testing.T, seq func(yield func(kernel.Event, error) bool)) []kernel.Event {
	t.Helper()
	got := make([]kernel.Event, 0, 16)
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("iterator yielded error: %v", err)
		}
		got = append(got, ev)
	}
	return got
}

// drainAllowErr collects events and the first error (if any), without
// failing the test. Used by tests that explicitly assert error paths.
func drainAllowErr(seq func(yield func(kernel.Event, error) bool)) ([]kernel.Event, error) {
	got := make([]kernel.Event, 0, 16)
	var firstErr error
	for ev, err := range seq {
		if err != nil {
			firstErr = err
			break
		}
		got = append(got, ev)
	}
	return got, firstErr
}

func writeOne(t *testing.T, w *kernel.FileEventLog, ts time.Time, kind kernel.EventKind) kernel.Receipt {
	t.Helper()
	ev := sampleEvent()
	ev.Kind = kind
	ev.CreateTime = ts
	receipts, err := w.Append(testutil.TestContext(t), []kernel.Event{ev})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return receipts[0]
}

func TestFileEventLog_Read_EmptyLogYieldsNothing(t *testing.T) {
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 0 {
		t.Errorf("want 0 events from empty log, got %d", len(got))
	}
}

func TestFileEventLog_Read_RoundTripsAcrossDays(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	// Three days, written in non-chronological input order.
	d1 := writeOne(t, w, time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC), "remember")
	d3 := writeOne(t, w, time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC), "recall_hit")
	d2 := writeOne(t, w, time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC), "remember")

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}

	// Day 24 events appear before day 25; within day 24 the writer's
	// monotonic ULID keeps file-order == append-order.
	wantUIDs := []string{d1.UID, d2.UID, d3.UID}
	for i, uid := range wantUIDs {
		if got[i].UID != uid {
			t.Errorf("position %d: want UID %q, got %q", i, uid, got[i].UID)
		}
	}
}

func TestFileEventLog_Read_SinceInclusiveUntilExclusive(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	t10 := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	t11 := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	t12 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	r10 := writeOne(t, w, t10, "remember")
	r11 := writeOne(t, w, t11, "remember")
	_ = writeOne(t, w, t12, "remember")

	got := drain(t, w.Read(ctx, kernel.ReadOpts{Since: t10, Until: t12}))
	if len(got) != 2 {
		t.Fatalf("want 2 events in [t10, t12), got %d", len(got))
	}
	if got[0].UID != r10.UID || got[1].UID != r11.UID {
		t.Errorf("boundary semantics broken: got %q,%q want %q,%q",
			got[0].UID, got[1].UID, r10.UID, r11.UID)
	}
}

func TestFileEventLog_Read_KindsAllowList(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	rRemember := writeOne(t, w, base, "remember")
	_ = writeOne(t, w, base.Add(time.Second), "recall_hit")
	rRemember2 := writeOne(t, w, base.Add(2*time.Second), "remember")

	got := drain(t, w.Read(ctx, kernel.ReadOpts{Kinds: []kernel.EventKind{"remember"}}))
	if len(got) != 2 {
		t.Fatalf("want 2 remember events, got %d", len(got))
	}
	if got[0].UID != rRemember.UID || got[1].UID != rRemember2.UID {
		t.Errorf("Kinds filter broken: got %q,%q want %q,%q",
			got[0].UID, got[1].UID, rRemember.UID, rRemember2.UID)
	}

	// Empty Kinds == all.
	all := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(all) != 3 {
		t.Errorf("empty Kinds should match all; got %d, want 3", len(all))
	}
}

func TestFileEventLog_Read_LimitStopsExact(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	for i := range 5 {
		writeOne(t, w, base.Add(time.Duration(i)*time.Second), "remember")
	}

	got := drain(t, w.Read(ctx, kernel.ReadOpts{Limit: 3}))
	if len(got) != 3 {
		t.Errorf("want exactly 3 events under Limit=3, got %d", len(got))
	}
}

func TestFileEventLog_Read_CallerBreakStopsIterator(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	for i := range 10 {
		writeOne(t, w, base.Add(time.Duration(i)*time.Second), "remember")
	}

	var seen int
	for _, err := range w.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Errorf("iterator did not honour break: seen=%d", seen)
	}
}

func TestFileEventLog_Read_ParseErrorSurfacesTyped(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	good := writeOne(t, w, time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC), "remember")

	// Corrupt the day file by appending a malformed line. The writer
	// closes the file between Appends, so this is safe.
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	bad := []byte("{not a valid json}\n")
	if err := appendBytes(path, bad); err != nil {
		t.Fatalf("corrupt file: %v", err)
	}

	got, gotErr := drainAllowErr(w.Read(ctx, kernel.ReadOpts{}))
	if gotErr == nil {
		t.Fatalf("want parse error, got nil")
	}
	if !strings.Contains(gotErr.Error(), "line 2") {
		t.Errorf("want error to name line 2, got %v", gotErr)
	}
	// The good event is yielded first (load-sort path); the deferred
	// parse error fires after all valid events when the caller drains
	// to natural EOF. Bounded consumers (Limit, caller break) would
	// never see this error.
	if len(got) != 1 || got[0].UID != good.UID {
		t.Errorf("want 1 good event before deferred parse error, got %d", len(got))
	}
}

// TestFileEventLog_Read_LimitMasksLaterParseError documents the deferred
// parse-error behavior (Greptile PR #91 finding 6): a Limit-bounded read
// over a file with a valid first line and a malformed second line must
// yield the requested prefix successfully, without surfacing the parse
// error. The error only fires for callers who drain to natural EOF.
func TestFileEventLog_Read_LimitMasksLaterParseError(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	good := writeOne(t, w, time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC), "remember")

	// Append a malformed second line.
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if err := appendBytes(path, []byte("{not a valid json}\n")); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	// Limit:1 — the caller should get the 1 good event with NO error.
	got, gotErr := drainAllowErr(w.Read(ctx, kernel.ReadOpts{Limit: 1}))
	if gotErr != nil {
		t.Errorf("Limit:1 prefix should mask later parse error, got %v", gotErr)
	}
	if len(got) != 1 || got[0].UID != good.UID {
		t.Errorf("Limit:1 yielded wrong events: got %d, want 1", len(got))
	}
}

// TestFileEventLog_Read_CallerBreakMasksLaterParseError documents that
// a caller who breaks out of the iterator after consuming a prefix is
// not subjected to a later file's parse error. Same rationale as the
// Limit case (Greptile PR #91 finding 6).
func TestFileEventLog_Read_CallerBreakMasksLaterParseError(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	good := writeOne(t, w, time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC), "remember")
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if err := appendBytes(path, []byte("{not a valid json}\n")); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	var seen int
	var sawErr error
	for ev, err := range w.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			sawErr = err
			break
		}
		if ev.UID != good.UID {
			t.Errorf("unexpected UID %q", ev.UID)
		}
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("want 1 event before break, got %d", seen)
	}
	if sawErr != nil {
		t.Errorf("break should mask deferred parse error, got %v", sawErr)
	}
}

func TestFileEventLog_Read_TornLastLineTolerated(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	good := writeOne(t, w, time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC), "remember")

	// Append a partial line (no trailing newline) directly to the file,
	// simulating crash recovery: a prior writer process crashed mid-write
	// without rolling back. Read must yield the committed events before
	// the torn tail and stop cleanly.
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if err := appendBytes(path, []byte(`{"uid":"01J0NX1234567890ABCDEFGHJK"`)); err != nil {
		t.Fatalf("write partial: %v", err)
	}

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 1 {
		t.Fatalf("want 1 fully-committed event, got %d", len(got))
	}
	if got[0].UID != good.UID {
		t.Errorf("UID mismatch: got %q want %q", got[0].UID, good.UID)
	}
}

// TestFileEventLog_Read_SameWallMsEventsOrderedByHLCLogical documents
// the within-millisecond ordering contract under the HLC wave.
//
// When two events in a single Append batch share the same CreateTime
// (same wall millisecond), prepare() assigns HLC logical counters in
// batch-input order: first event → logical=0, second → logical=1.
// sortDayEvents orders by (effectiveHLC, UID), so the event supplied
// first in the batch reads first — regardless of its UID value.
//
// Pre-HLC the tiebreaker was UID lexicographic (lower UID → reads
// first). The HLC tiebreaker is HLC logical counter (input order →
// lower logical → reads first).
func TestFileEventLog_Read_SameWallMsEventsOrderedByHLCLogical(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	ts := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	// Two valid ULIDs from the same millisecond. The HIGHER ULID is
	// supplied first — it gets HLC logical=0 and therefore reads first.
	higher := "01J0NX1234567890ABCDEFGHJZ"
	lower := "01J0NX1234567890ABCDEFGHJK"

	evHigh := sampleEvent()
	evHigh.UID = higher
	evHigh.CreateTime = ts
	evLow := sampleEvent()
	evLow.UID = lower
	evLow.CreateTime = ts

	if _, err := w.Append(ctx, []kernel.Event{evHigh, evLow}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	// evHigh (Z) is first in batch → HLC logical=0 → reads first.
	// evLow  (K) is second in batch → HLC logical=1 → reads second.
	if got[0].UID != higher || got[1].UID != lower {
		t.Errorf("HLC logical order broken: got [%q, %q], want [%q (logical=0), %q (logical=1)]",
			got[0].UID, got[1].UID, higher, lower)
	}
}

// TestFileEventLog_Read_BackfillAppearsAfterEarlierAppends documents
// the HLC-based Read ordering for cross-batch backfill events.
//
// A "backfill" scenario: the first Append writes an event at 11:00
// (gets HLC logical=0 from the current wall clock), then a second
// Append writes an event at 10:00 (gets HLC logical=1 — the wall is
// the same fixed-clock value, so the logical counter increments).
// Read sorts by effectiveHLC, yielding [11:00, 10:00].
//
// This is correct causal ordering: the agent knew about 11:00 before
// it discovered the 10:00 backfill, so 11:00 sorts earlier in the
// event log's causal timeline.
//
// (Pre-HLC, Read re-sorted by CreateTime and would have yielded
// [10:00, 11:00]. That contract changed with the HLC wave.)
func TestFileEventLog_Read_BackfillAppearsAfterEarlierAppends(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	t10 := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	t11 := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)

	// First Append lands an event at 11:00 → lower HLC (appended first).
	// Then a backfill Append lands an event at 10:00 → higher HLC.
	// On disk the file ends up [11:00, 10:00]; Read keeps that HLC order.
	rLater := writeOne(t, w, t11, "remember")
	rEarlier := writeOne(t, w, t10, "remember")

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	// HLC order: rLater was appended first → lower HLC → reads first.
	if got[0].UID != rLater.UID || got[1].UID != rEarlier.UID {
		t.Errorf("HLC causal order broken: got [%q, %q], want [%q (11:00, appended first), %q (10:00, backfill)]",
			got[0].UID, got[1].UID, rLater.UID, rEarlier.UID)
	}
}

// TestFileEventLog_Read_SortsSameBatchByHLCInputOrder documents
// the HLC Read ordering for a same-batch backfill.
//
// When a single Append batch submits events in non-chronological order
// [later(11:00), earlier(10:00)]:
//   - The WRITE path (bucketByDate) still sorts by CreateTime for a
//     deterministic disk layout: [10:00(HLC=wall:1), 11:00(HLC=wall:0)].
//   - prepare() stamps HLC in INPUT order: later → logical=0, earlier → logical=1.
//   - sortDayEvents re-sorts by EffectiveHLC: [later(logical=0), earlier(logical=1)].
//
// Read yields events in HLC = batch-input order: [11:00, 10:00].
// Receipts still reflect caller input order (the public Append contract).
func TestFileEventLog_Read_SortsSameBatchByHLCInputOrder(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	later := sampleEvent()
	later.CreateTime = time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	earlier := sampleEvent()
	earlier.CreateTime = time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	receipts, err := w.Append(ctx, []kernel.Event{later, earlier})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	// later was first in input → HLC logical=0 → reads first.
	// earlier was second in input → HLC logical=1 → reads second.
	if !got[0].CreateTime.Equal(later.CreateTime) || !got[1].CreateTime.Equal(earlier.CreateTime) {
		t.Errorf("HLC input-order sort broken: got [%s, %s], want [%s (later, logical=0), %s (earlier, logical=1)]",
			got[0].CreateTime.Format(time.RFC3339),
			got[1].CreateTime.Format(time.RFC3339),
			later.CreateTime.Format(time.RFC3339),
			earlier.CreateTime.Format(time.RFC3339))
	}
	// Receipts still reflect caller input order (the public Append contract).
	if !receipts[0].CreateTime.Equal(later.CreateTime) {
		t.Errorf("receipts[0] should mirror input[0] (later); got %s", receipts[0].CreateTime)
	}
}

func TestFileEventLog_Read_ContextCancelStopsIteration(t *testing.T) {
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	for i := range 5 {
		writeOne(t, w,
			time.Date(2026, 5, 24, 10, i, 0, 0, time.UTC),
			"remember")
	}

	ctx, cancel := context.WithCancel(testutil.TestContext(t))
	cancel()

	_, err := drainAllowErr(w.Read(ctx, kernel.ReadOpts{}))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestFileEventLog_Read_SkipsFilesOutsideDateWindow(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	writeOne(t, w, time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC), "remember")
	writeOne(t, w, time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC), "remember")
	keep := writeOne(t, w, time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC), "remember")

	// Window covers only the 28th.
	since := time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)

	got := drain(t, w.Read(ctx, kernel.ReadOpts{Since: since, Until: until}))
	if len(got) != 1 || got[0].UID != keep.UID {
		t.Errorf("date-window prune broken: got %d events, want 1 (%q)", len(got), keep.UID)
	}
}

func TestFileEventLog_Read_IgnoresNonConformingFiles(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	good := writeOne(t, w, time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC), "remember")

	// Drop a misnamed file alongside the day bucket. Read must skip it.
	junk := filepath.Join(dataDir, "events", "2026", "05", "notes.txt")
	if err := os.WriteFile(junk, []byte("scratch\n"), 0o600); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	// And a directory that looks like a day file but is not.
	junkDir := filepath.Join(dataDir, "events", "2026", "05", "99.jsonl")
	if err := os.Mkdir(junkDir, 0o750); err != nil {
		t.Fatalf("mkdir junk: %v", err)
	}

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 1 || got[0].UID != good.UID {
		t.Errorf("non-conforming entries leaked into Read: got %d events", len(got))
	}
}

func TestFileEventLog_Read_RoundTripPayloadFidelity(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	in := sampleEvent()
	in.Source = "providers/anthropic"
	in.Confidence = 0.7
	in.Payload = json.RawMessage(`{"text":"hello","nested":{"k":1}}`)
	if _, err := w.Append(ctx, []kernel.Event{in}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := drain(t, w.Read(ctx, kernel.ReadOpts{}))
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Kind != in.Kind || got[0].Source != in.Source || got[0].Confidence != in.Confidence {
		t.Errorf("scalar fields not preserved: got %+v want %+v", got[0], in)
	}
	if !got[0].CreateTime.Equal(clock) {
		t.Errorf("CreateTime drifted: got %s, want %s", got[0].CreateTime, clock)
	}

	var inPayload, outPayload map[string]any
	if err := json.Unmarshal(in.Payload, &inPayload); err != nil {
		t.Fatalf("in payload: %v", err)
	}
	if err := json.Unmarshal(got[0].Payload, &outPayload); err != nil {
		t.Fatalf("out payload: %v", err)
	}
	if inPayload["text"] != outPayload["text"] {
		t.Errorf("payload text drift: got %v want %v", outPayload["text"], inPayload["text"])
	}
}

// appendBytes appends to path without going through FileEventLog. Used
// by tests that need to simulate corruption or in-flight writes.
func appendBytes(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}
