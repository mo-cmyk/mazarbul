package kernel_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// rebuildFakeStorage is a kernel.Storage that records Apply calls and
// optionally returns errors keyed by event UID. It tracks "applied"
// state in a map so HasEvent semantics match a real partial-rebuild
// scenario: events already in the store skip a second Apply.
type rebuildFakeStorage struct {
	kernel.NoopStorage

	mu        sync.Mutex
	applied   map[string]bool        // uid -> applied
	preExist  map[string]bool        // uid -> pretend it was already applied (HasEvent returns true)
	errOnUID  map[string]error       // uid -> error to return from Apply
	hasErrFn  func(uid string) error // optional HasEvent error injection
	applyHook func(events []kernel.Event)
	calls     []kernel.Event
}

func newRebuildFakeStorage() *rebuildFakeStorage {
	return &rebuildFakeStorage{
		applied:  make(map[string]bool),
		preExist: make(map[string]bool),
		errOnUID: make(map[string]error),
	}
}

func (s *rebuildFakeStorage) Apply(_ context.Context, events []kernel.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, events...)
	for _, ev := range events {
		if err, ok := s.errOnUID[ev.UID]; ok {
			return err
		}
		s.applied[ev.UID] = true
	}
	if s.applyHook != nil {
		s.applyHook(events)
	}
	return nil
}

func (s *rebuildFakeStorage) HasEvent(_ context.Context, uid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasErrFn != nil {
		if err := s.hasErrFn(uid); err != nil {
			return false, err
		}
	}
	return s.applied[uid] || s.preExist[uid], nil
}

func (s *rebuildFakeStorage) callsSnapshot() []kernel.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]kernel.Event, len(s.calls))
	copy(out, s.calls)
	return out
}

// appendN appends n sample events through w and returns the receipts in
// append order. Tests use this to seed the log before invoking Rebuild.
func appendN(t *testing.T, w *kernel.FileEventLog, n int) []kernel.Receipt {
	t.Helper()
	ctx := testutil.TestContext(t)
	events := make([]kernel.Event, n)
	for i := range events {
		events[i] = sampleEvent()
	}
	receipts, err := w.Append(ctx, events)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != n {
		t.Fatalf("Append receipts: got %d, want %d", len(receipts), n)
	}
	return receipts
}

func TestFileEventLog_Rebuild_EmptyLog(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	store := newRebuildFakeStorage()

	errs, err := w.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("ApplyError slice: got %d, want 0", len(errs))
	}
	if got := store.callsSnapshot(); len(got) != 0 {
		t.Errorf("Apply called %d times on empty log; expected 0", len(got))
	}
}

func TestFileEventLog_Rebuild_AllSucceed(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 7
	receipts := appendN(t, w, n)

	store := newRebuildFakeStorage()
	errs, err := w.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("ApplyError slice: got %d, want 0", len(errs))
	}

	calls := store.callsSnapshot()
	if len(calls) != n {
		t.Fatalf("Apply call count: got %d, want %d", len(calls), n)
	}
	// Apply order must match the append order: receipts are returned
	// in chronological order and Rebuild iterates Read which yields the
	// same order. UID match nails this without inspecting CreateTime.
	for i, got := range calls {
		if got.UID != receipts[i].UID {
			t.Errorf("apply order[%d]: got UID %q, want %q", i, got.UID, receipts[i].UID)
		}
	}
}

func TestFileEventLog_Rebuild_PartialSkipsAlreadyApplied(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 6
	receipts := appendN(t, w, n)

	store := newRebuildFakeStorage()
	// Pretend the first half is already applied (simulates a crash mid-rebuild).
	for i := range n / 2 {
		store.preExist[receipts[i].UID] = true
	}

	errs, err := w.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("ApplyError slice: got %d, want 0", len(errs))
	}

	calls := store.callsSnapshot()
	if len(calls) != n/2 {
		t.Fatalf("Apply call count: got %d, want %d (only the unapplied half)", len(calls), n/2)
	}
	// The applied events must be the second half, in order.
	for i, got := range calls {
		want := receipts[n/2+i].UID
		if got.UID != want {
			t.Errorf("apply order[%d]: got UID %q, want %q", i, got.UID, want)
		}
	}
}

func TestFileEventLog_Rebuild_NonStrictCollectsErrors(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 5
	receipts := appendN(t, w, n)

	sentinel := errors.New("boom")
	store := newRebuildFakeStorage()
	// Fail the third event; the rebuild should record the error and continue.
	store.errOnUID[receipts[2].UID] = sentinel

	errs, err := w.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("Rebuild (non-strict): %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("ApplyError slice: got %d, want 1", len(errs))
	}
	if errs[0].UID != receipts[2].UID {
		t.Errorf("ApplyError UID: got %q, want %q", errs[0].UID, receipts[2].UID)
	}
	if !errors.Is(errs[0], sentinel) {
		t.Errorf("ApplyError must wrap sentinel; errors.Is(=%v, sentinel)=false", errs[0])
	}

	// Apply must have been invoked for all five (the failing one returned
	// before recording into applied, but the call was made).
	if got := len(store.callsSnapshot()); got != n {
		t.Errorf("Apply call count: got %d, want %d", got, n)
	}
}

func TestFileEventLog_Rebuild_StrictAbortsOnFirstError(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 5
	receipts := appendN(t, w, n)

	sentinel := errors.New("strict boom")
	store := newRebuildFakeStorage()
	store.errOnUID[receipts[2].UID] = sentinel

	errs, err := w.Rebuild(ctx, store, kernel.WithStrict())
	if err == nil {
		t.Fatal("Rebuild (strict) must return an error")
	}
	// The returned error must wrap an ApplyError so callers can errors.As.
	var ae kernel.ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("returned error must wrap ApplyError; got %T: %v", err, err)
	}
	if ae.UID != receipts[2].UID {
		t.Errorf("wrapped ApplyError UID: got %q, want %q", ae.UID, receipts[2].UID)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("returned error must unwrap to sentinel; errors.Is = false")
	}

	// The collected slice contains the same error.
	if len(errs) != 1 {
		t.Fatalf("ApplyError slice: got %d, want 1", len(errs))
	}

	// Apply must have stopped after the failure: events at index 3 and 4
	// never got dispatched. Calls = first 3 events.
	calls := store.callsSnapshot()
	if len(calls) != 3 {
		t.Errorf("Apply call count: got %d, want 3 (events 0, 1, 2; 2 failed and aborted)", len(calls))
	}
}

func TestFileEventLog_Rebuild_HasEventErrorAborts(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	appendN(t, w, 3)

	sentinel := errors.New("driver fault")
	store := newRebuildFakeStorage()
	calls := 0
	store.hasErrFn = func(_ string) error {
		calls++
		if calls == 2 {
			return sentinel
		}
		return nil
	}

	_, err := w.Rebuild(ctx, store)
	if err == nil {
		t.Fatal("Rebuild must return error when HasEvent fails")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("returned error must wrap HasEvent failure; errors.Is = false: %v", err)
	}
}

func TestFileEventLog_Rebuild_ContextCancellation(t *testing.T) {
	t.Parallel()
	parent := testutil.TestContext(t)
	w := newTestWriter(t)
	appendN(t, w, 10)

	ctx, cancel := context.WithCancel(parent)
	store := newRebuildFakeStorage()
	// Cancel as soon as the second Apply lands.
	store.applyHook = func(_ []kernel.Event) {
		if len(store.calls) == 2 {
			cancel()
		}
	}

	_, err := w.Rebuild(ctx, store)
	if err == nil {
		t.Fatal("Rebuild must return error when ctx is canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("returned error must wrap context.Canceled; got %v", err)
	}
}

func TestFileEventLog_Rebuild_ProgressWriter(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 25
	appendN(t, w, n)

	var buf bytes.Buffer
	store := newRebuildFakeStorage()
	// Cadence of 10 yields lines at events 10, 20 plus a terminal summary.
	_, err := w.Rebuild(ctx, store, kernel.WithProgress(&buf), kernel.WithProgressEvery(10))
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "rebuilt") {
		t.Fatalf("progress output missing keyword: %q", out)
	}
	// The terminal summary must report the full count and include the
	// failed column so operators can spot non-strict drift at a glance.
	terminal := fmt.Sprintf("rebuilt %d events", n)
	if !strings.Contains(out, terminal) {
		t.Errorf("progress output missing terminal summary %q; got: %q", terminal, out)
	}
	if !strings.Contains(out, "failed=0") {
		t.Errorf("progress output missing failed=0 column; got: %q", out)
	}
	// Exactly three lines: cadence ticks at events 10 and 20, plus the
	// terminal flush after the loop completes at 25. The cadence boundary
	// at 20 (20 % 10 == 0) emits a tick; the run ends at 25 with no
	// further tick, so the terminal flush is the only line for the
	// remaining 5 events.
	lines := strings.Count(out, "\n")
	if lines != 3 {
		t.Errorf("progress lines: got %d, want 3 (10-tick, 20-tick, terminal); output: %q", lines, out)
	}
}

func TestFileEventLog_Rebuild_NilStorageReturnsError(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	_, err := w.Rebuild(ctx, nil)
	if err == nil {
		t.Fatal("Rebuild(ctx, nil) must return an error")
	}
	if !strings.Contains(err.Error(), "storage is nil") {
		t.Errorf("error message should name the nil storage; got: %v", err)
	}
}

// TestFileEventLog_Rebuild_IdempotentSecondPass exercises the contract:
// running Rebuild a second time against the same Storage applies no
// new events because HasEvent reports every UID as already applied.
func TestFileEventLog_Rebuild_IdempotentSecondPass(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 4
	appendN(t, w, n)

	store := newRebuildFakeStorage()
	if _, err := w.Rebuild(ctx, store); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	firstCount := len(store.callsSnapshot())
	if firstCount != n {
		t.Fatalf("first Rebuild applied %d events, want %d", firstCount, n)
	}

	if _, err := w.Rebuild(ctx, store); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	secondCount := len(store.callsSnapshot())
	if secondCount != firstCount {
		t.Errorf("second Rebuild Apply count: got %d, want %d (idempotent should skip all)", secondCount, firstCount)
	}
}

// TestFileEventLog_Rebuild_ProgressShortLogStillFlushes asserts the
// terminal summary fires even when applied < cadence, so an operator
// doing a 5-event rebuild with cadence=100 still sees the final count.
func TestFileEventLog_Rebuild_ProgressShortLogStillFlushes(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	appendN(t, w, 3)

	var buf bytes.Buffer
	store := newRebuildFakeStorage()
	if _, err := w.Rebuild(ctx, store, kernel.WithProgress(&buf)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !strings.Contains(buf.String(), "rebuilt 3 events") {
		t.Errorf("short rebuild must flush terminal summary; got: %q", buf.String())
	}
}

// TestFileEventLog_Rebuild_NonStrictFailedEventsAppearInProgress proves
// that a non-strict apply error increments the failed counter so the
// progress writer reports it. Before the fix, failed events were silently
// swallowed: the terminal line said `failed=0` even after dozens of
// per-event failures.
func TestFileEventLog_Rebuild_NonStrictFailedEventsAppearInProgress(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 4
	receipts := appendN(t, w, n)

	store := newRebuildFakeStorage()
	store.errOnUID[receipts[1].UID] = errors.New("boom1")
	store.errOnUID[receipts[2].UID] = errors.New("boom2")

	var buf bytes.Buffer
	errs, err := w.Rebuild(ctx, store, kernel.WithProgress(&buf))
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(errs) != 2 {
		t.Fatalf("ApplyError slice: got %d, want 2", len(errs))
	}
	out := buf.String()
	if !strings.Contains(out, "rebuilt 2 events") {
		t.Errorf("terminal applied count: got %q, want substring %q", out, "rebuilt 2 events")
	}
	if !strings.Contains(out, "failed=2") {
		t.Errorf("terminal failed count: got %q, want substring %q", out, "failed=2")
	}
}

// TestFileEventLog_Rebuild_StrictAbortFlushesProgress proves that an
// early return on strict abort still emits the terminal progress line.
// Before the fix, the defer-less code path bypassed flushProgress so
// operators staring at progress output saw the last cadence tick, not
// the actual abort count.
func TestFileEventLog_Rebuild_StrictAbortFlushesProgress(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)
	const n = 5
	receipts := appendN(t, w, n)

	store := newRebuildFakeStorage()
	store.errOnUID[receipts[2].UID] = errors.New("strict-boom")

	var buf bytes.Buffer
	_, err := w.Rebuild(ctx, store, kernel.WithProgress(&buf), kernel.WithStrict())
	if err == nil {
		t.Fatal("Rebuild (strict) must return an error")
	}
	out := buf.String()
	// Two events applied (indices 0 and 1) before the strict abort at
	// index 2. The terminal flush must reflect both applied and failed.
	if !strings.Contains(out, "rebuilt 2 events") {
		t.Errorf("terminal applied count after strict abort: got %q, want substring %q", out, "rebuilt 2 events")
	}
	if !strings.Contains(out, "failed=1") {
		t.Errorf("terminal failed count after strict abort: got %q, want substring %q", out, "failed=1")
	}
}

// TestFileEventLog_Rebuild_CtxCancelFlushesProgress proves that a
// caller-cancelled rebuild still emits the terminal progress line via
// the defer. Without the defer, the cancellation path returns from
// inside the loop and never reaches flushProgress.
func TestFileEventLog_Rebuild_CtxCancelFlushesProgress(t *testing.T) {
	t.Parallel()
	parent := testutil.TestContext(t)
	w := newTestWriter(t)
	appendN(t, w, 10)

	ctx, cancel := context.WithCancel(parent)
	store := newRebuildFakeStorage()
	store.applyHook = func(_ []kernel.Event) {
		if len(store.calls) == 3 {
			cancel()
		}
	}

	var buf bytes.Buffer
	_, err := w.Rebuild(ctx, store, kernel.WithProgress(&buf))
	if err == nil {
		t.Fatal("Rebuild must return error when ctx is canceled")
	}
	out := buf.String()
	// Three Apply calls landed before the cancellation; the terminal
	// flush should report that, not zero.
	if !strings.Contains(out, "rebuilt 3 events") {
		t.Errorf("terminal progress after ctx cancel: got %q, want substring %q", out, "rebuilt 3 events")
	}
}

// ctxObserverStorage cancels the caller ctx the moment Apply is invoked
// and records ctx.Err() on the Apply-scoped ctx at that exact instant.
// This pins the detached-context contract: Apply runs under
// context.WithoutCancel(caller) + WithTimeout, so a caller cancellation
// arriving during Apply must NOT propagate. Sampling ctx.Err() in-call
// avoids the "the rebuild loop's own cancel() fires after Apply returns"
// confound that would otherwise mask a real bug.
type ctxObserverStorage struct {
	kernel.NoopStorage

	cancelCaller func()
	observedErr  error
}

func (s *ctxObserverStorage) Apply(ctx context.Context, _ []kernel.Event) error {
	if s.cancelCaller != nil {
		s.cancelCaller()
	}
	// Sample applyCtx state at the moment Apply runs. If WithoutCancel
	// did its job, ctx.Err() is nil here even though the caller ctx is
	// canceled.
	s.observedErr = ctx.Err()
	return nil
}

func (s *ctxObserverStorage) HasEvent(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// TestFileEventLog_Rebuild_ApplyContextIsDetached proves that the ctx
// passed to Storage.Apply is detached from the caller's cancellation
// chain (via context.WithoutCancel + WithTimeout). The fake storage
// cancels the caller mid-call and then samples its own ctx.Err(); a
// non-nil sample means the cancellation propagated and the dispatcher
// would have lost a half-finished tx. That is the bug this fix prevents.
func TestFileEventLog_Rebuild_ApplyContextIsDetached(t *testing.T) {
	t.Parallel()
	parent := testutil.TestContext(t)
	w := newTestWriter(t)
	appendN(t, w, 1)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	store := &ctxObserverStorage{cancelCaller: cancel}

	// Rebuild returns ctx.Canceled on the post-Apply loop check (that's
	// the right behaviour — outer cancellation halts further events).
	// We only care that Apply's own ctx was alive WHILE it ran.
	_, _ = w.Rebuild(ctx, store)
	if store.observedErr != nil {
		t.Errorf("Apply ctx must be detached from caller cancellation; got Err=%v", store.observedErr)
	}
}

// TestFileEventLog_Rebuild_ApplyErrorImplementsErrorInterface pins the
// ApplyError.Error() / Unwrap contract that the kernel doc comment
// promises external callers.
func TestFileEventLog_Rebuild_ApplyErrorImplementsErrorInterface(t *testing.T) {
	t.Parallel()
	inner := errors.New("inner")
	ae := kernel.ApplyError{UID: "01HZZZZZZZZZZZZZZZZZZZZZ", Err: inner}

	if !strings.Contains(ae.Error(), "01HZZZZZZZZZZZZZZZZZZZZZ") {
		t.Errorf("ApplyError.Error() must include UID; got %q", ae.Error())
	}
	if !errors.Is(ae, inner) {
		t.Errorf("errors.Is(ApplyError, inner) must be true")
	}
}
