//go:build integration

package kernel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func TestIntegration_ConcurrentAppendsRaceClean(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})

	const goroutines = 10
	const eventsPerGoroutine = 100
	var wg sync.WaitGroup
	var failures atomic.Int64

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range eventsPerGoroutine {
				if _, err := w.Append(ctx, []kernel.Event{sampleEvent()}); err != nil {
					failures.Add(1)
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("%d concurrent appends failed", failures.Load())
	}

	got := countLines(t, dataDir)
	want := goroutines * eventsPerGoroutine
	if got != want {
		t.Errorf("line count: got %d, want %d", got, want)
	}
}

func TestIntegration_BatchSpansMidnightUTC(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	// Clock sits past the latest event so neither is "in the future".
	clock := time.Date(2026, 5, 25, 1, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	first := sampleEvent()
	first.CreateTime = time.Date(2026, 5, 24, 23, 59, 59, 0, time.UTC)
	second := sampleEvent()
	second.CreateTime = time.Date(2026, 5, 25, 0, 0, 1, 0, time.UTC)

	receipts, err := w.Append(ctx, []kernel.Event{first, second})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != 2 {
		t.Fatalf("want 2 receipts, got %d", len(receipts))
	}

	day1 := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	day2 := filepath.Join(dataDir, "events", "2026", "05", "25.jsonl")
	assertSingleLine(t, day1)
	assertSingleLine(t, day2)
}

func TestIntegration_DurabilityAcrossClose(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	w1 := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	first, err := w1.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("w1.Append: %v", err)
	}

	// Close w1 before instantiating w2 over the same dataDir. Mazarbul's
	// data dir is owned by a single daemon: bbolt's sidecar holds an
	// exclusive file lock and the second writer cannot proceed while
	// the first still has it open.
	if cerr := w1.Close(); cerr != nil {
		t.Fatalf("w1.Close: %v", cerr)
	}
	w2 := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	t.Cleanup(func() { _ = w2.Close() })
	second, err := w2.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("w2.Append: %v", err)
	}

	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}

	var ev0, ev1 kernel.Event
	if err := json.Unmarshal(lines[0], &ev0); err != nil {
		t.Fatalf("line 0: %v", err)
	}
	if err := json.Unmarshal(lines[1], &ev1); err != nil {
		t.Fatalf("line 1: %v", err)
	}
	if ev0.UID != first[0].UID || ev1.UID != second[0].UID {
		t.Errorf("UID order broken: file=[%q,%q] receipts=[%q,%q]",
			ev0.UID, ev1.UID, first[0].UID, second[0].UID)
	}
}

func TestIntegration_RollbackOnUIDFailure(t *testing.T) {
	t.Parallel()
	// Exercises the pre-write failure path: a poisoned ULID generator
	// triggers ErrInvalidEvent before any disk I/O. The on-disk file should
	// be untouched. This is the partial-batch durability guarantee callers
	// rely on when Append returns an error.
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	// First, write one good event with the default writer so the file
	// has a known size we can later compare.
	good := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))
	if _, err := good.Append(ctx, []kernel.Event{sampleEvent()}); err != nil {
		t.Fatalf("good.Append: %v", err)
	}
	path := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	knownSize := info.Size()

	// Now build a writer whose ULID generator fails on the second call.
	var calls atomic.Int64
	poisoned := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(clock)),
		kernel.WithULID(func() (string, error) {
			n := calls.Add(1)
			if n == 1 {
				return "01J0NX1234567890ABCDEFGHJK", nil
			}
			return "", errPoisoned
		}),
	)

	_, err = poisoned.Append(ctx, []kernel.Event{sampleEvent(), sampleEvent()})
	if err == nil {
		t.Fatalf("want failure, got nil")
	}

	// File must be byte-identical to before the failed append.
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after failure: %v", err)
	}
	if info2.Size() != knownSize {
		t.Errorf("file changed despite failed batch: was %d, now %d", knownSize, info2.Size())
	}
}

var errPoisoned = poisonedError("test: poisoned ULID generator")

type poisonedError string

func (e poisonedError) Error() string { return string(e) }

func countLines(t *testing.T, dataDir string) int {
	t.Helper()
	var total int
	err := filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		total += bytes.Count(raw, []byte("\n"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return total
}

func assertSingleLine(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := bytes.Count(raw, []byte("\n")); got != 1 {
		t.Errorf("%s: want 1 line, got %d", path, got)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Errorf("%s: missing trailing newline", path)
	}
}

// blockingOntology blocks the first Validate call until release is closed.
// Subsequent calls return immediately. Used to hold FileEventLog.mu for
// long enough to cancel a queued Append's context and assert the post-lock
// recheck rejects the canceled request without writing.
type blockingOntology struct {
	release chan struct{}
	called  atomic.Int64
}

func (b *blockingOntology) Schema(_ context.Context, _ string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}

func (b *blockingOntology) Register(_ context.Context, _ kernel.Schema) error {
	return kernel.ErrNotImplemented
}

func (b *blockingOntology) List(_ context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}

func (b *blockingOntology) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	if b.called.Add(1) == 1 {
		<-b.release
	}
	return nil
}

// TestIntegration_ConcurrentReadAndAppend exercises the reader's
// race-safety guarantee from issue #25: concurrent Read calls do not
// block each other, and concurrent with Append they only see fully-
// written events. We launch N writers and M readers against the same
// log; assert that every yielded event parses, every yielded UID is
// drawn from the set of appended UIDs, and at the end the reader sees
// the entire log.
func TestIntegration_ConcurrentReadAndAppend(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})

	const writers = 8
	const eventsPerWriter = 50
	const readers = 4

	appended := make(chan string, writers*eventsPerWriter)

	var wWG sync.WaitGroup
	for range writers {
		wWG.Add(1)
		go func() {
			defer wWG.Done()
			for range eventsPerWriter {
				receipts, err := w.Append(ctx, []kernel.Event{sampleEvent()})
				if err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				appended <- receipts[0].UID
			}
		}()
	}

	var rWG sync.WaitGroup
	var readerFailures atomic.Int64
	for range readers {
		rWG.Add(1)
		go func() {
			defer rWG.Done()
			// Each reader does several full drains while writers are
			// active. Every yielded UID must be a valid ULID and every
			// event must round-trip cleanly.
			for range 5 {
				for ev, err := range w.Read(ctx, kernel.ReadOpts{}) {
					if err != nil {
						readerFailures.Add(1)
						t.Errorf("Read yielded error: %v", err)
						return
					}
					if ev.UID == "" || ev.Name == "" {
						readerFailures.Add(1)
						t.Errorf("Read yielded incomplete event: %+v", ev)
						return
					}
				}
			}
		}()
	}

	wWG.Wait()
	close(appended)
	rWG.Wait()

	if readerFailures.Load() != 0 {
		t.Fatalf("%d reader failures observed", readerFailures.Load())
	}

	// Final drain after writers finish: must contain every UID exactly
	// once and never anything else.
	wantUIDs := make(map[string]struct{}, writers*eventsPerWriter)
	for uid := range appended {
		wantUIDs[uid] = struct{}{}
	}

	seen := make(map[string]struct{}, len(wantUIDs))
	for ev, err := range w.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			t.Fatalf("final Read: %v", err)
		}
		if _, ok := wantUIDs[ev.UID]; !ok {
			t.Errorf("final Read yielded unknown UID %q", ev.UID)
		}
		if _, dup := seen[ev.UID]; dup {
			t.Errorf("final Read yielded duplicate UID %q", ev.UID)
		}
		seen[ev.UID] = struct{}{}
	}
	if len(seen) != len(wantUIDs) {
		t.Errorf("final Read missed events: saw %d, want %d", len(seen), len(wantUIDs))
	}
}

// TestIntegration_ReadWaitsForInFlightAppend documents the fix for
// Greptile PR #91 finding 2 ("Avoid uncommitted reads"). When an
// Append is in flight (writer holds the exclusive Lock), any Read
// must wait for the Append to commit (or roll back) before it can
// observe the file's size. This guarantees Reads never see the
// page-cache window between f.Write and a successful f.Sync.
//
// Setup: pre-seed the log with one event so a day file exists. Then
// start writer A that blocks inside Validate (Lock held). Reader B
// attempts to read the existing file; its openCommitted's RLock
// cannot be acquired while A holds Lock. We assert Read returns
// AFTER A releases the Lock.
func TestIntegration_ReadWaitsForInFlightAppend(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	release := make(chan struct{})
	ont := &gatedOntology{release: release}
	w := kernel.NewFileEventLog(dataDir, ont)

	// First Append seeds the file with one event. We tell the ontology
	// gate to pass the first Validate immediately by leaving gateOn = 0.
	if _, err := w.Append(testutil.TestContext(t), []kernel.Event{sampleEvent()}); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	// Now arm the gate so the NEXT Validate blocks until release.
	ont.armBlock()

	// Writer A starts a second Append; blocks inside Validate, Lock held.
	appendDone := make(chan time.Time, 1)
	go func() {
		if _, err := w.Append(testutil.TestContext(t), []kernel.Event{sampleEvent()}); err != nil {
			t.Errorf("A.Append: %v", err)
		}
		appendDone <- time.Now()
	}()

	// Wait for A to enter Validate (proves Lock is held).
	deadline := time.Now().Add(2 * time.Second)
	for ont.blockedCount.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("A never entered the blocking Validate")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Reader B reads the existing file; openCommitted's RLock waits.
	readDone := make(chan time.Time, 1)
	go func() {
		var n int
		for range w.Read(testutil.TestContext(t), kernel.ReadOpts{}) {
			n++
		}
		readDone <- time.Now()
		if n < 1 {
			t.Errorf("Read drained %d events, want >=1", n)
		}
	}()

	// Confirm the reader is held off.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-readDone:
		t.Fatal("Read returned while Append held the Lock")
	default:
	}

	// Release A; Read should now proceed.
	close(release)

	appendedAt := <-appendDone
	readAt := <-readDone
	if readAt.Before(appendedAt) {
		t.Errorf("Read returned before Append: read=%s append=%s", readAt, appendedAt)
	}
}

// gatedOntology lets a test arm a one-shot block on Validate. Initial
// calls pass through; after armBlock() the NEXT Validate blocks until
// release is closed.
type gatedOntology struct {
	release      chan struct{}
	armed        atomic.Bool
	blockedCount atomic.Int64
}

func (g *gatedOntology) Schema(_ context.Context, _ string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}

func (g *gatedOntology) Register(_ context.Context, _ kernel.Schema) error {
	return kernel.ErrNotImplemented
}

func (g *gatedOntology) List(_ context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}

func (g *gatedOntology) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	if g.armed.CompareAndSwap(true, false) {
		g.blockedCount.Add(1)
		<-g.release
	}
	return nil
}

func (g *gatedOntology) armBlock() { g.armed.Store(true) }

func TestIntegration_CanceledAfterAcquiringLock(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ont := &blockingOntology{release: make(chan struct{})}
	w := kernel.NewFileEventLog(dataDir, ont)

	// Goroutine A holds the mutex inside Validate.
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		if _, err := w.Append(testutil.TestContext(t), []kernel.Event{sampleEvent()}); err != nil {
			t.Errorf("A.Append: %v", err)
		}
	}()

	// Wait for A to enter Validate so we know it owns the mutex.
	deadline := time.Now().Add(2 * time.Second)
	for ont.called.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("A never entered Validate")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// B enters Append; it will block on mu.Lock until A releases.
	ctxB, cancelB := context.WithCancel(testutil.TestContext(t))
	bErr := make(chan error, 1)
	go func() {
		_, err := w.Append(ctxB, []kernel.Event{sampleEvent()})
		bErr <- err
	}()

	// Give B a moment to queue on the mutex, then cancel its context.
	time.Sleep(50 * time.Millisecond)
	cancelB()

	// Release A; B now acquires the mutex and must observe the canceled
	// context before any I/O happens.
	close(ont.release)
	<-aDone

	select {
	case err := <-bErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("B: want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B did not return")
	}

	// Disk should hold A's single event and nothing from B.
	total := countLines(t, dataDir)
	if total != 1 {
		t.Errorf("want 1 line on disk (A only), got %d", total)
	}
}
