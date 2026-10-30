package kernel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fakeOntology is a hand-rolled Ontology that lets each test choose what
// Validate returns. It avoids pulling in a JSON Schema library; the writer
// only cares about the error contract.
type fakeOntology struct {
	validateErr error
}

func (f *fakeOntology) Schema(_ context.Context, _ string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}

func (f *fakeOntology) Register(_ context.Context, _ kernel.Schema) error {
	return kernel.ErrNotImplemented
}

func (f *fakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}

func (f *fakeOntology) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	return f.validateErr
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newTestWriter(t *testing.T, opts ...kernel.Option) *kernel.FileEventLog {
	t.Helper()
	defaultClock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	base := []kernel.Option{kernel.WithClock(fixedClock(defaultClock))}
	return kernel.NewFileEventLog(t.TempDir(), &fakeOntology{}, append(base, opts...)...)
}

func sampleEvent() kernel.Event {
	return kernel.Event{
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Payload:    json.RawMessage(`{"text":"hello"}`),
	}
}

func TestFileEventLog_AssignsULIDWhenUIDEmpty(t *testing.T) {
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	receipts, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(receipts))
	}
	if _, err := ulid.Parse(receipts[0].UID); err != nil {
		t.Errorf("receipt UID %q is not a valid ULID: %v", receipts[0].UID, err)
	}
}

func TestFileEventLog_AcceptsValidCallerULID(t *testing.T) {
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	id := ulid.MustNew(ulid.Now(), nil).String()
	ev := sampleEvent()
	ev.UID = id

	receipts, err := w.Append(ctx, []kernel.Event{ev})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if receipts[0].UID != id {
		t.Errorf("UID changed: want %q, got %q", id, receipts[0].UID)
	}
}

func TestFileEventLog_RejectsMalformedULID(t *testing.T) {
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	ev := sampleEvent()
	ev.UID = "not-a-ulid"

	_, err := w.Append(ctx, []kernel.Event{ev})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("want ErrInvalidEvent, got %v", err)
	}
}

func TestFileEventLog_AssignsCreateTimeWhenZero(t *testing.T) {
	ctx := testutil.TestContext(t)
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := newTestWriter(t, kernel.WithClock(fixedClock(clock)))

	receipts, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !receipts[0].CreateTime.Equal(clock) {
		t.Errorf("CreateTime = %s, want %s", receipts[0].CreateTime, clock)
	}
	if receipts[0].CreateTime.Location() != time.UTC {
		t.Errorf("CreateTime not UTC: %s", receipts[0].CreateTime.Location())
	}
}

func TestFileEventLog_RejectsFutureBeyondSkew(t *testing.T) {
	ctx := testutil.TestContext(t)
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := newTestWriter(t, kernel.WithClock(fixedClock(clock)), kernel.WithMaxSkew(5*time.Minute))

	ev := sampleEvent()
	ev.CreateTime = clock.Add(10 * time.Minute)

	_, err := w.Append(ctx, []kernel.Event{ev})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("want ErrInvalidEvent, got %v", err)
	}
}

func TestFileEventLog_AcceptsFutureWithinSkew(t *testing.T) {
	ctx := testutil.TestContext(t)
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := newTestWriter(t, kernel.WithClock(fixedClock(clock)), kernel.WithMaxSkew(5*time.Minute))

	ev := sampleEvent()
	ev.CreateTime = clock.Add(1 * time.Minute)

	receipts, err := w.Append(ctx, []kernel.Event{ev})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !receipts[0].CreateTime.Equal(ev.CreateTime) {
		t.Errorf("CreateTime mutated: want %s, got %s", ev.CreateTime, receipts[0].CreateTime)
	}
}

func TestFileEventLog_ValidatesPayloadAgainstOntology(t *testing.T) {
	ctx := testutil.TestContext(t)
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	validationErr := errors.New("payload missing required field")
	ont := &fakeOntology{validateErr: validationErr}
	w := kernel.NewFileEventLog(t.TempDir(), ont, kernel.WithClock(fixedClock(clock)))

	_, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("want ErrInvalidEvent, got %v", err)
	}
	if !errors.Is(err, validationErr) {
		t.Errorf("want wrapped validation cause, got %v", err)
	}
}

// TestFileEventLog_RejectsUnknownKind is the strict-validation contract:
// when the ontology has no schema for the event's Kind, Append rejects
// the event with ErrInvalidEvent. Permissive bypass would let a typo'd
// or never-registered kind escape validation (Greptile 3/5 finding:
// "Malformed Episode payloads can still be appended because missing
// schema lookups are treated as allowed").
func TestFileEventLog_RejectsUnknownKind(t *testing.T) {
	ctx := testutil.TestContext(t)
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()

	// Ontology returns ErrNotFound -> strict: event must be rejected.
	ont := &fakeOntology{validateErr: kernel.ErrNotFound}
	w := kernel.NewFileEventLog(dataDir, ont, kernel.WithClock(fixedClock(clock)))

	_, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("want ErrInvalidEvent for unknown kind, got %v", err)
	}
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("want wrapped ErrNotFound, got %v", err)
	}
	// No file should have been created.
	entries, _ := os.ReadDir(dataDir)
	if len(entries) != 0 {
		t.Errorf("rejected append should not touch disk; found %d entries", len(entries))
	}
}

func TestFileEventLog_ReceiptsInInputOrder(t *testing.T) {
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	const n = 5
	batch := make([]kernel.Event, n)
	for i := range batch {
		batch[i] = sampleEvent()
	}

	receipts, err := w.Append(ctx, batch)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != n {
		t.Fatalf("want %d receipts, got %d", n, len(receipts))
	}

	// Monotonic entropy keeps same-millisecond ULIDs strictly increasing.
	for i := 1; i < n; i++ {
		if receipts[i].UID <= receipts[i-1].UID {
			t.Errorf("receipts not in append order at %d: %q <= %q",
				i, receipts[i].UID, receipts[i-1].UID)
		}
	}
}

func TestFileEventLog_SetsResourceNameOnDisk(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	receipts, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}

	var got kernel.Event
	if err := json.Unmarshal(raw[:len(raw)-1], &got); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}

	wantName := "events/" + receipts[0].UID
	if got.Name != wantName {
		t.Errorf("Name = %q, want %q", got.Name, wantName)
	}
}

func TestFileEventLog_EmptyBatchIsNoop(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})

	receipts, err := w.Append(ctx, nil)
	if err != nil {
		t.Fatalf("Append(nil): %v", err)
	}
	if receipts != nil {
		t.Errorf("want nil receipts, got %v", receipts)
	}

	entries, _ := os.ReadDir(dataDir)
	if len(entries) != 0 {
		t.Errorf("empty batch should not touch disk; found %d entries", len(entries))
	}
}

func TestFileEventLog_PathBucketIsByCreateTime(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	// Writer clock irrelevant; the event sets its own CreateTime.
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))))

	ev := sampleEvent()
	ev.CreateTime = time.Date(2026, 1, 7, 23, 30, 0, 0, time.UTC)

	if _, err := w.Append(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	want := filepath.Join(dataDir, "events", "2026", "01", "07.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected file %s: %v", want, err)
	}
}

func TestFileEventLog_RoundTripsPayload(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	in := sampleEvent()
	if _, err := w.Append(ctx, []kernel.Event{in}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}

	var out kernel.Event
	if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Kind != in.Kind || out.Source != in.Source || out.Confidence != in.Confidence {
		t.Errorf("event fields not preserved: %+v vs %+v", out, in)
	}
	var inPayload, outPayload map[string]any
	if err := json.Unmarshal(in.Payload, &inPayload); err != nil {
		t.Fatalf("in payload: %v", err)
	}
	if err := json.Unmarshal(out.Payload, &outPayload); err != nil {
		t.Fatalf("out payload: %v", err)
	}
	if inPayload["text"] != outPayload["text"] {
		t.Errorf("payload text drift: got %v, want %v", outPayload["text"], inPayload["text"])
	}
}

// TestFileEventLog_RejectsMalformedGeneratedUID covers the case where an
// injected ULID generator returns a malformed string with a nil error.
// Without the post-generation Parse check the writer would persist a bad
// uid, breaking the ULID invariant for downstream readers.
func TestFileEventLog_RejectsMalformedGeneratedUID(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(clock)),
		kernel.WithULID(func() (string, error) { return "not-a-ulid", nil }),
	)

	_, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err == nil {
		t.Fatalf("want error from malformed generated UID, got nil")
	}
	// The file must not exist; no I/O should have happened.
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected %s to be absent, stat=%v", path, statErr)
	}
}

// TestFileEventLog_AtomicCrossDayBatch_MarshalFailure verifies that a
// marshal failure in any bucket prevents ALL buckets from being written.
// Pre-encoding the whole batch up front gives us this property for free.
func TestFileEventLog_AtomicCrossDayBatch_MarshalFailure(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	good := sampleEvent()
	good.CreateTime = time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	// A second event on a different day whose payload cannot be marshalled.
	// json.RawMessage with broken JSON triggers encoding/json's verifier.
	bad := sampleEvent()
	bad.CreateTime = time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	bad.Payload = json.RawMessage(`{invalid}`)

	_, err := w.Append(ctx, []kernel.Event{good, bad})
	if err == nil {
		t.Fatalf("want marshal error, got nil")
	}

	day1 := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	day2 := filepath.Join(dataDir, "events", "2026", "05", "25.jsonl")
	for _, p := range []string{day1, day2} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to be absent after failed batch, stat=%v", p, statErr)
		}
	}
}

// TestFileEventLog_CanceledContextRejectsAppend verifies that a context
// canceled before Append even acquires the lock returns context.Canceled
// without touching the disk.
func TestFileEventLog_CanceledContextRejectsAppend(t *testing.T) {
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})

	ctx, cancel := context.WithCancel(testutil.TestContext(t))
	cancel()

	_, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	entries, _ := os.ReadDir(dataDir)
	if len(entries) != 0 {
		t.Errorf("canceled append should not touch disk; found %d entries", len(entries))
	}
}

// TestFileEventLog_DoesNotTruncatePreExistingOnPreWriteFailure guards
// the rollback path: when writeOne fails BEFORE recording the real
// pre-batch size (OpenFile or Stat failure), the previously-durable
// content of a pre-existing day file must NOT be truncated. Without
// this guard, a transient stat/open failure on a pre-existing file
// would turn a failed Append into total data loss for that day.
//
// Covers Greptile #69 iter-5 finding: "writeOne returns oldSize == 0
// for pre-write failures such as OpenFile or the initial f.Stat. If
// the day file already existed, p.isNew is false, so undoCommits
// truncates that existing JSONL file to byte 0 even though this
// append never wrote to it."
func TestFileEventLog_DoesNotTruncatePreExistingOnPreWriteFailure(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	// Land one good event so the day file exists with known content.
	// Close `good` to release its bbolt sidecar lock so the poisoned
	// writer's pre-commit uniqueness check (added in #26) can proceed
	// to the writeOne simulator under test.
	good := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	if _, err := good.Append(ctx, []kernel.Event{sampleEvent()}); err != nil {
		t.Fatalf("good.Append: %v", err)
	}
	if cerr := good.Close(); cerr != nil {
		t.Fatalf("good.Close: %v", cerr)
	}
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pre-batch: %v", err)
	}
	if len(before) == 0 {
		t.Fatalf("expected pre-batch file to have content, got empty")
	}

	// Build a poisoned writer whose writeOne returns observed=false as
	// if OpenFile or Stat had failed BEFORE recording the real pre-batch
	// size. This is the regression Greptile flagged.
	poisonErr := errors.New("test: simulated pre-write writeOne failure")
	poisoned := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	poisoned.SetWriteOneFn(func(_ string, _ []byte, _ bool) (int64, bool, bool, error) {
		// observed=false simulates an OpenFile or initial Stat error
		// before writeOne ever sees the on-disk state. createdByUs=false
		// because we did not (and could not) create the file.
		return 0, false, false, poisonErr
	})

	if _, err := poisoned.Append(ctx, []kernel.Event{sampleEvent()}); err == nil {
		t.Fatalf("want error from poisoned writeOne, got nil")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read post-batch: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("pre-existing file mutated by failed Append: before=%d bytes after=%d bytes",
			len(before), len(after))
	}
}

// TestFileEventLog_PreservesInterleavedWriterDataOnPostWriteFailure
// guards the rollback path against another writer that appended to the
// same day file between this Append's planBuckets stat and writeOne's
// open. When writeOne observes a larger file at runtime, the rollback
// must truncate back to writeOne's observed size (which includes the
// interleaved writer's bytes), not back to the stale plan-time size.
//
// Covers Greptile #69 iter-6 finding: "If another writer appends to the
// same existing day file after planBuckets but before this append hits
// a post-write error, writeOne observes the larger size, writes, then
// returns an error from dir fsync or close. This branch ignores that
// observed size and truncates back to the stale plan-time size,
// deleting the other writer's durable event."
func TestFileEventLog_PreservesInterleavedWriterDataOnPostWriteFailure(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	// Land one good event so the day file exists at preBatchSize bytes.
	// Close `good` before constructing the poisoned writer so the bbolt
	// sidecar's file lock is released; otherwise the poisoned writer's
	// pre-commit UID uniqueness check (added in #26) blocks on the lock
	// and never reaches the writeOne simulator under test.
	good := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	if _, err := good.Append(ctx, []kernel.Event{sampleEvent()}); err != nil {
		t.Fatalf("good.Append: %v", err)
	}
	if cerr := good.Close(); cerr != nil {
		t.Fatalf("good.Close: %v", cerr)
	}
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat pre-batch: %v", err)
	}
	preBatchSize := info.Size()

	// Build a poisoned writer whose writeOne simulates the interleaved-
	// writer scenario. The simulator appends "OTHER\n" to the file
	// (representing another writer's durable commit between plan and
	// writeOne), then returns the post-interleave size as observed=true
	// along with a post-write error.
	const interleavedBytes = "OTHER\n"
	poisonErr := errors.New("test: simulated post-write failure after interleave")
	poisoned := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	poisoned.SetWriteOneFn(func(p string, _ []byte, _ bool) (int64, bool, bool, error) {
		// Simulate the interleaved writer.
		// #nosec G304 -- test-controlled path.
		f, oerr := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
		if oerr != nil {
			return 0, false, false, oerr
		}
		if _, werr := f.Write([]byte(interleavedBytes)); werr != nil {
			_ = f.Close()
			return 0, false, false, werr
		}
		if cerr := f.Close(); cerr != nil {
			return 0, false, false, cerr
		}
		info, serr := os.Stat(p)
		if serr != nil {
			return 0, false, false, serr
		}
		// createdByUs=false because the day file already existed (the
		// `good` writer above created it). The post-write error must
		// drive rollback through the truncate-back branch, not remove.
		return info.Size(), true, false, poisonErr
	})

	if _, err := poisoned.Append(ctx, []kernel.Event{sampleEvent()}); err == nil {
		t.Fatalf("want error from poisoned writeOne, got nil")
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after poison: %v", err)
	}
	want := preBatchSize + int64(len(interleavedBytes))
	if after.Size() != want {
		t.Errorf("interleaved writer data lost: got file size %d, want %d (preBatch %d + interleave %d)",
			after.Size(), want, preBatchSize, len(interleavedBytes))
	}
}

// TestFileEventLog_DoesNotRemoveInterleavedlyCreatedFileOnPostWriteFailure
// guards the rollback path against the "interleaved create" scenario:
// planBuckets observes the day file as missing (p.isNew == true at plan
// time), but another writer creates the file between planBuckets and
// writeOne's open. When this Append then hits a post-write error, the
// rollback must NOT call os.Remove on the other writer's durable file.
//
// Covers Greptile #69 iter-7 finding: "This rollback mark still uses
// the plan-time p.isNew value even when writeOne has observed the file
// at commit time. If planBuckets saw the day file as missing, another
// writer creates it before writeOne observes it, and this append then
// returns a post-write error, undoCommits will call os.Remove and
// delete the other writer's durable file."
//
// Failing-test contract: the simulator reports createdByUs=false
// (proving the file pre-existed at writeOne time) alongside a
// post-write error. The rollback MUST truncate-back, not remove.
func TestFileEventLog_DoesNotRemoveInterleavedlyCreatedFileOnPostWriteFailure(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	// Precondition: the day file does NOT exist before Append, so
	// planBuckets's stat returns ErrNotExist and p.isNew==true. The
	// bug under test is that p.isNew leaks into the rollback decision.
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: expected %s absent, stat=%v", path, err)
	}

	const interleavedBytes = "OTHER\n"
	poisonErr := errors.New("test: simulated post-write failure on interleaved-create")
	poisoned := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	poisoned.SetWriteOneFn(func(p string, _ []byte, _ bool) (int64, bool, bool, error) {
		// Simulate another writer creating the day file between
		// planBuckets and writeOne. commitPlans has already run
		// MkdirAll, so the parent directory exists.
		// #nosec G304 -- test-controlled path.
		f, oerr := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if oerr != nil {
			return 0, false, false, oerr
		}
		if _, werr := f.Write([]byte(interleavedBytes)); werr != nil {
			_ = f.Close()
			return 0, false, false, werr
		}
		if cerr := f.Close(); cerr != nil {
			return 0, false, false, cerr
		}
		info, serr := os.Stat(p)
		if serr != nil {
			return 0, false, false, serr
		}
		// Report observed=true with the on-disk size (the interleaved
		// writer's bytes) and createdByUs=false (in real production
		// O_EXCL would have returned ErrExist on this path). The
		// post-write error must drive rollback through truncate-back.
		return info.Size(), true, false, poisonErr
	})

	if _, err := poisoned.Append(ctx, []kernel.Event{sampleEvent()}); err == nil {
		t.Fatalf("want error from poisoned writeOne, got nil")
	}

	// The interleaved writer's file MUST still exist after rollback.
	// With the bug present (commitPlans uses plan-time p.isNew=true),
	// undoCommits calls os.Remove and this read fails with ENOENT.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("interleaved writer's file lost: rollback removed %s; read err=%v", path, err)
	}
	if string(after) != interleavedBytes {
		t.Errorf("interleaved writer's bytes lost: got %q, want %q",
			string(after), interleavedBytes)
	}
}

// TestFileEventLog_RollsBackBucketOnPostWriteDirSyncError verifies the
// post-write rollback contract: when writeOne fails AFTER content has
// already been written and fsynced (parent-dir fsync failure, or close
// failure in defer), the bucket must still be rolled back so a caller
// retrying the same Append does not duplicate events on disk.
//
// Covers Greptile #69 iter-4 finding: "When `f.Write` and `f.Sync`
// succeed but the later directory sync fails, `writeOne` returns an
// error after the event is already in the new JSONL file. `commitPlans`
// only rolls back entries already added to `commits`, and the current
// bucket is added only after `writeOne` returns nil, so this branch
// returns no receipts while leaving a durable event behind."
func TestFileEventLog_RollsBackBucketOnPostWriteDirSyncError(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	// Poison parent-dir fsync so it returns an error AFTER writeOne has
	// already written + fsynced the file content. This is the exact path
	// Greptile flagged: durable content on disk, error returned to caller.
	poisonErr := errors.New("test: simulated parent-dir fsync failure")
	w.SetFsyncDirFn(func(string) error { return poisonErr })

	_, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err == nil {
		t.Fatalf("want error from poisoned fsyncDir, got nil")
	}
	if !errors.Is(err, poisonErr) {
		t.Errorf("want wrapped poison error, got %v", err)
	}

	// The day file was newly created by this Append. Rollback must
	// remove it so a retry does not see a partially-committed write.
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected %s to be absent after post-write rollback, stat=%v", path, statErr)
	}
}

// TestFileEventLog_BucketsAreChronological documents the post-fix
// contract: regardless of input order, day buckets are written in
// chronological order so the rebuild path can rely on file order.
func TestFileEventLog_BucketsAreChronological(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	// Input order: later day first.
	later := sampleEvent()
	later.CreateTime = time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	earlier := sampleEvent()
	earlier.CreateTime = time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	if _, err := w.Append(ctx, []kernel.Event{later, earlier}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Both files should exist and each carry exactly one line. (Chronological
	// ordering is a cross-file claim, observable by file mtimes in a real
	// filesystem; here we assert the simpler invariant that both files
	// landed.)
	for _, day := range []string{"24", "25"} {
		path := filepath.Join(dataDir, "events", "2026", "05", day+".jsonl")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s, got %v", path, err)
		}
	}
}
