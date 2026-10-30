package kernel_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// recordingStorage captures every Apply call so a test can assert the
// writer fanned a batch out post-fsync. The applyErr field lets a test
// inject a failure to verify the warn-and-continue contract.
type recordingStorage struct {
	mu       sync.Mutex
	batches  [][]kernel.Event
	applyErr error
	lastCtx  context.Context
}

func (r *recordingStorage) record(events []kernel.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := make([]kernel.Event, len(events))
	copy(clone, events)
	r.batches = append(r.batches, clone)
}

func (r *recordingStorage) snapshot() [][]kernel.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]kernel.Event, len(r.batches))
	copy(out, r.batches)
	return out
}

// Apply records the batch + the ctx the writer passed in (so callers
// can prove the post-fsync fanout ran with a non-cancelable context
// per the Greptile #108 P1 finding) and returns applyErr, if any.
// The other kernel.Storage methods are inherited from the embedded
// NoopStorage on the adapter below.
func (r *recordingStorage) Apply(ctx context.Context, events []kernel.Event) error {
	r.mu.Lock()
	r.lastCtx = ctx
	r.mu.Unlock()
	r.record(events)
	return r.applyErr
}

// recordingStorageAdapter wraps recordingStorage with an embedded
// NoopStorage so the OpenSQLite / OpenGraph / OpenVector / OpenFTS /
// Close methods come for free — the writer only exercises Apply.
type recordingStorageAdapter struct {
	kernel.NoopStorage
	rec *recordingStorage
}

func (a *recordingStorageAdapter) Apply(ctx context.Context, events []kernel.Event) error {
	return a.rec.Apply(ctx, events)
}

func newRecordingStorage(applyErr error) (kernel.Storage, *recordingStorage) {
	rec := &recordingStorage{applyErr: applyErr}
	return &recordingStorageAdapter{rec: rec}, rec
}

// blockingStorage wraps a recordingStorage with a release gate. Apply
// blocks on release after capturing ctx, so a test can observe ctx
// behavior mid-call (e.g. caller cancellation propagating, or not).
// errOnSeen signals the captured ctx.Err() at the moment Apply was
// invoked so the test can verify a clean baseline before it starts
// poking at the parent.
type blockingStorage struct {
	kernel.NoopStorage
	rec       *recordingStorage
	release   <-chan struct{}
	errOnSeen chan<- error
}

func (b *blockingStorage) Apply(ctx context.Context, events []kernel.Event) error {
	b.rec.mu.Lock()
	b.rec.lastCtx = ctx
	b.rec.mu.Unlock()
	b.rec.record(events)
	b.errOnSeen <- ctx.Err()
	<-b.release
	return nil
}

func TestFileEventLog_ApplyFanout_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	storage, rec := newRecordingStorage(nil)

	w := newTestWriter(t, kernel.WithStorage(storage))

	receipts, err := w.Append(ctx, []kernel.Event{sampleEvent(), sampleEvent()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := len(receipts); got != 2 {
		t.Fatalf("receipts: got %d, want 2", got)
	}

	batches := rec.snapshot()
	if len(batches) != 1 {
		t.Fatalf("Apply call count: got %d, want 1", len(batches))
	}
	if got := len(batches[0]); got != 2 {
		t.Fatalf("Apply batch size: got %d, want 2", got)
	}
	// The prepared events handed to Apply must carry the writer-assigned
	// UID + CreateTime (not the caller-supplied zero values).
	for i, ev := range batches[0] {
		if ev.UID == "" {
			t.Errorf("batches[0][%d].UID is empty; writer should have assigned a ULID", i)
		}
		if ev.UID != receipts[i].UID {
			t.Errorf("batches[0][%d].UID = %q, receipts[%d].UID = %q", i, ev.UID, i, receipts[i].UID)
		}
		if ev.CreateTime.IsZero() {
			t.Errorf("batches[0][%d].CreateTime is zero", i)
		}
		if ev.Name == "" {
			t.Errorf("batches[0][%d].Name is empty; writer should have set it to events/{uid}", i)
		}
	}
}

func TestFileEventLog_ApplyFanout_FailureIsLoggedAndIgnored(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("dispatcher exploded")
	storage, rec := newRecordingStorage(sentinel)

	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	ctx := logging.WithLogger(testutil.TestContext(t), logger)

	w := newTestWriter(t, kernel.WithStorage(storage))

	receipts, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("Append must succeed even when Apply fails: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("receipts: got %d, want 1", len(receipts))
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("Apply must still have been invoked exactly once, got %d", len(rec.snapshot()))
	}

	entries := logs.FilterMessageSnippet("storage apply failed").All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one warn log about storage apply failure, got %d (all entries: %#v)",
			len(entries), logs.All())
	}
	if got := entries[0].Level; got != zap.WarnLevel {
		t.Errorf("warn-level expected, got %v", got)
	}
	// The error must round-trip into the log entry so an operator can see
	// the underlying cause.
	foundErr := false
	for _, f := range entries[0].Context {
		if f.Key == "error" {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("log entry missing the error field: %#v", entries[0].Context)
	}
}

// TestFileEventLog_ApplyFanout_DetachesCallerCancellation pins the
// Greptile-#108 P1 fix: applyToStorage must call Storage.Apply with
// a context detached from the caller's cancellation. We block Apply
// mid-call, cancel the caller's parent ctx, and assert the captured
// ctx is NOT canceled inside Apply — only the inner timeout (which
// fires at a much later wall-clock) can cancel it.
func TestFileEventLog_ApplyFanout_DetachesCallerCancellation(t *testing.T) {
	t.Parallel()

	rec := &recordingStorage{}
	gate := make(chan struct{})
	observed := make(chan error, 1)
	blocking := &blockingStorage{
		rec:       rec,
		release:   gate,
		errOnSeen: observed,
	}
	w := newTestWriter(t, kernel.WithStorage(blocking))

	ctx, cancel := context.WithCancel(testutil.TestContext(t))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := w.Append(ctx, []kernel.Event{sampleEvent()})
		done <- err
	}()

	// Wait until the storage observed its ctx (Apply was invoked).
	preCancelErr := <-observed
	if preCancelErr != nil {
		t.Fatalf("captured ctx unexpectedly canceled BEFORE parent cancel: %v", preCancelErr)
	}

	// Cancel the caller's parent. If applyToStorage forwarded ctx
	// directly, captured.Err() would now be non-nil. With
	// context.WithoutCancel in place, Apply's ctx must remain alive.
	cancel()
	// Give cancellation a beat to propagate (would be instant on a
	// child of ctx; should be never on a WithoutCancel detached).
	time.Sleep(50 * time.Millisecond)

	rec.mu.Lock()
	captured := rec.lastCtx
	rec.mu.Unlock()
	if captured == nil {
		t.Fatal("recording storage never saw a ctx")
	}
	if err := captured.Err(); err != nil {
		t.Errorf("Apply ctx canceled post parent-cancel (%v); WithoutCancel must detach it", err)
	}

	close(gate) // let Apply return
	if err := <-done; err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// TestFileEventLog_ApplyFanout_BoundedTimeout pins the Greptile-#108
// third-pass resolution: applyToStorage hands Apply a context that
// (a) detaches from the caller via context.WithoutCancel and (b)
// carries a finite deadline so a wedged dispatcher cannot pin the
// calling goroutine or Dispatcher.Close. The cache-singleflight race
// that would otherwise re-emerge under a cancelable ctx is owned at
// the dispatcher/cache boundary: Dispatcher.Close calls
// CachingEmbedder.Wait before tearing down SQLite pools.
func TestFileEventLog_ApplyFanout_BoundedTimeout(t *testing.T) {
	t.Parallel()
	storage, rec := newRecordingStorage(nil)
	w := newTestWriter(t, kernel.WithStorage(storage))

	if _, err := w.Append(testutil.TestContext(t), []kernel.Event{sampleEvent()}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rec.mu.Lock()
	captured := rec.lastCtx
	rec.mu.Unlock()

	if captured == nil {
		t.Fatal("recording storage never saw a ctx")
	}
	deadline, ok := captured.Deadline()
	if !ok {
		t.Fatal("Apply ctx has no Deadline; bounded timeout must be applied")
	}
	// The deadline must be in the future (we just set it) and within a
	// reasonable window of "now + defaultApplyTimeout". Allow generous
	// slack because tests may run on a slow CI box.
	dur := time.Until(deadline)
	if dur <= 0 {
		t.Errorf("Apply ctx deadline already passed: %v", deadline)
	}
	if dur > 90*time.Second {
		t.Errorf("Apply ctx deadline %v is too far in the future; expected ~30s budget", dur)
	}
}

func TestFileEventLog_ApplyFanout_DefaultsToNoop(t *testing.T) {
	t.Parallel()

	// Inject an observer so a stray warn-log surfaces here rather
	// than hiding behind logging.FromContext's NoOp fallback. The
	// whole point of the default-nil storage is "no side effects":
	// no Apply call, no warn entry, no surprise log volume once the
	// daemon (#42) wires a real logger.
	core, logs := observer.New(zap.WarnLevel)
	ctx := logging.WithLogger(testutil.TestContext(t), zap.New(core))

	w := newTestWriter(t)
	if _, err := w.Append(ctx, []kernel.Event{sampleEvent()}); err != nil {
		t.Fatalf("Append with default storage: %v", err)
	}

	if got := logs.FilterMessageSnippet("storage apply failed").All(); len(got) != 0 {
		t.Errorf("default-nil storage must not warn-log; got %d entries: %#v", len(got), got)
	}
}
