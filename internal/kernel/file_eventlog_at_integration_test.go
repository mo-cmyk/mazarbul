//go:build integration

package kernel_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestIntegration_ConcurrentAtAndAppendRaceClean drives Append and At
// concurrently on the same FileEventLog and asserts neither -race nor
// the result-equality check trips. Append takes the writer mutex; At
// reads through bbolt without it and relies on bbolt's RW transaction
// semantics plus the append-only nature of the JSONL files.
func TestIntegration_ConcurrentAtAndAppendRaceClean(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Seed one event so an initial UID is available before the At
	// goroutines start spinning.
	seed, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	const writers = 4
	const reads = 8
	const opsPerWriter = 100
	const opsPerReader = 200

	var (
		uids    sync.Map
		uidList atomic.Value // []string snapshot for readers
	)
	uids.Store(seed[0].UID, struct{}{})
	uidList.Store([]string{seed[0].UID})

	var wg sync.WaitGroup
	var failures atomic.Int64

	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range opsPerWriter {
				rs, aerr := w.Append(ctx, []kernel.Event{sampleEvent()})
				if aerr != nil {
					failures.Add(1)
					t.Errorf("Append: %v", aerr)
					return
				}
				for _, r := range rs {
					uids.Store(r.UID, struct{}{})
				}
				snap := make([]string, 0)
				uids.Range(func(k, _ any) bool {
					snap = append(snap, k.(string))
					return true
				})
				uidList.Store(snap)
			}
		}()
	}

	for range reads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range opsPerReader {
				snap, _ := uidList.Load().([]string)
				if len(snap) == 0 {
					continue
				}
				uid := snap[len(snap)-1]
				if _, lerr := w.At(ctx, uid); lerr != nil && !errors.Is(lerr, kernel.ErrNotFound) {
					failures.Add(1)
					t.Errorf("At %s: %v", uid, lerr)
					return
				}
			}
		}()
	}

	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d failures during concurrent At+Append", n)
	}

	// Final correctness sweep: every recorded UID still resolves.
	var sweepErr error
	uids.Range(func(k, _ any) bool {
		uid := k.(string)
		if _, aerr := w.At(ctx, uid); aerr != nil {
			sweepErr = aerr
			return false
		}
		return true
	})
	if sweepErr != nil {
		t.Errorf("final sweep: %v", sweepErr)
	}
}

// TestIntegration_AtSurvivesProcessRestart writes events with one
// FileEventLog, closes it, and constructs a fresh one over the same
// dataDir. The reopened sidecar must yield the same events.
func TestIntegration_AtSurvivesProcessRestart(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	w1 := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	const n = 50
	batch := make([]kernel.Event, n)
	for i := range batch {
		batch[i] = sampleEvent()
	}
	receipts, err := w1.Append(ctx, batch)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if cerr := w1.Close(); cerr != nil {
		t.Fatalf("close w1: %v", cerr)
	}

	w2 := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	t.Cleanup(func() { _ = w2.Close() })

	for _, r := range receipts {
		ev, lerr := w2.At(ctx, r.UID)
		if lerr != nil {
			t.Fatalf("At %s after restart: %v", r.UID, lerr)
		}
		if ev.UID != r.UID {
			t.Errorf("UID drift after restart: got %q, want %q", ev.UID, r.UID)
		}
	}
}
