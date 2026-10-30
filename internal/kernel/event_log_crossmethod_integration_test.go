//go:build integration

package kernel_test

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// tornLastLine finds the lexicographically last JSONL file under
// dataDir/events/, reads it, and removes the trailing newline from the
// final line via Truncate + Sync. This simulates a crash that left a
// partial write with no terminating newline.
func tornLastLine(t *testing.T, dataDir string) {
	t.Helper()
	root := filepath.Join(dataDir, "events")
	var jsonlFiles []string
	if werr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == ".jsonl" {
			jsonlFiles = append(jsonlFiles, p)
		}
		return nil
	}); werr != nil {
		t.Fatalf("tornLastLine: walk: %v", werr)
	}
	if len(jsonlFiles) == 0 {
		t.Fatal("tornLastLine: no JSONL files found")
	}
	sort.Strings(jsonlFiles)
	target := jsonlFiles[len(jsonlFiles)-1]

	info, serr := os.Stat(target)
	if serr != nil {
		t.Fatalf("tornLastLine: stat %s: %v", target, serr)
	}
	if info.Size() == 0 {
		t.Fatalf("tornLastLine: file %s is empty", target)
	}
	// #nosec G304 -- test-controlled path.
	f, oerr := os.OpenFile(target, os.O_WRONLY, 0o600)
	if oerr != nil {
		t.Fatalf("tornLastLine: open %s: %v", target, oerr)
	}
	defer func() { _ = f.Close() }()
	// Remove the trailing newline from the last line.
	if terr := f.Truncate(info.Size() - 1); terr != nil {
		t.Fatalf("tornLastLine: truncate %s: %v", target, terr)
	}
	if serr2 := f.Sync(); serr2 != nil {
		t.Fatalf("tornLastLine: sync %s: %v", target, serr2)
	}
}

// TestIntegration_CrossMethod_ConcurrentAppendReadNoTornLines verifies that
// Read and Append are safe to call concurrently: the race detector is the
// primary concurrency-safety tool here. Lock-ordering isolation (Read cannot
// observe a partially-written event while Append holds the mutex) is covered
// by the dedicated TestIntegration_ReadWaitsForInFlightAppend test in
// file_eventlog_integration_test.go. This test focuses on the cross-method
// properties under concurrent load: no data corruption, correct final event
// count, and no torn JSONL lines on disk.
//
// 4 writer goroutines × 100 appends run alongside 2 looping reader goroutines.
// After all writers finish:
// (a) drains Read and asserts total yielded == 400,
// (b) walks every JSONL file under events/ and asserts every line ends with \n,
// (c) asserts each appended UID appears in the final drain.
func TestIntegration_CrossMethod_ConcurrentAppendReadNoTornLines(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	t.Cleanup(func() { _ = w.Close() })

	const writers = 4
	const eventsPerWriter = 100

	appended := make(chan string, writers*eventsPerWriter)
	done := make(chan struct{})

	var wWG sync.WaitGroup
	var appendFailures atomic.Int64
	for range writers {
		wWG.Add(1)
		go func() {
			defer wWG.Done()
			for range eventsPerWriter {
				receipts, err := w.Append(ctx, []kernel.Event{sampleEvent()})
				if err != nil {
					appendFailures.Add(1)
					t.Errorf("Append: %v", err)
					return
				}
				appended <- receipts[0].UID
			}
		}()
	}

	var rWG sync.WaitGroup
	var readFailures atomic.Int64
	for range 2 {
		rWG.Add(1)
		go func() {
			defer rWG.Done()
			for {
				for ev, err := range w.Read(ctx, kernel.ReadOpts{}) {
					if err != nil {
						readFailures.Add(1)
						t.Errorf("concurrent Read yielded error: %v", err)
						return
					}
					if ev.UID == "" {
						readFailures.Add(1)
						t.Errorf("concurrent Read yielded event with empty UID")
						return
					}
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}

	wWG.Wait()
	close(appended)
	close(done)
	rWG.Wait()

	if n := appendFailures.Load(); n != 0 {
		t.Fatalf("%d append failures", n)
	}
	if n := readFailures.Load(); n != 0 {
		t.Fatalf("%d read failures during concurrent phase", n)
	}

	// (a) Final drain must yield exactly 400 events.
	const totalEvents = writers * eventsPerWriter
	var finalCount int
	seenUIDs := make(map[string]struct{}, totalEvents)
	for ev, err := range w.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			t.Fatalf("final Read: %v", err)
		}
		if _, dup := seenUIDs[ev.UID]; dup {
			t.Errorf("final Read yielded duplicate UID %q", ev.UID)
		}
		seenUIDs[ev.UID] = struct{}{}
		finalCount++
	}
	if finalCount != totalEvents {
		t.Errorf("final Read count: got %d, want %d", finalCount, totalEvents)
	}

	// (b) Every JSONL line must end with \n (no torn lines).
	if err := filepath.WalkDir(filepath.Join(dataDir, "events"), func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || filepath.Ext(p) == ".bolt" {
			return nil
		}
		if filepath.Ext(p) != ".jsonl" {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if len(raw) == 0 {
			return nil
		}
		// Count newlines and check the final byte.
		if raw[len(raw)-1] != '\n' {
			t.Errorf("torn line in %s: file does not end with newline", p)
		}
		for i, b := range raw {
			if b == '\n' {
				continue
			}
			// Any \n is fine; we only care that lines end cleanly.
			// The above final-byte check is the torn-line assertion.
			_ = i
		}
		return nil
	}); err != nil {
		t.Fatalf("walk for torn lines: %v", err)
	}

	// (c) Every appended UID appeared exactly once in the final drain.
	wantUIDs := make(map[string]struct{}, writers*eventsPerWriter)
	for uid := range appended {
		wantUIDs[uid] = struct{}{}
	}
	for uid := range wantUIDs {
		if _, ok := seenUIDs[uid]; !ok {
			t.Errorf("appended UID %q not found in final Read", uid)
		}
	}
}

// TestIntegration_CrossMethod_TornLineAtFileTailSkippedByRead appends 10
// events, then simulates a crash by truncating the last JSONL file's final
// newline. A fresh FileEventLog over the same dataDir must yield exactly 9
// events and no error: the reader's isCompleteLine gate treats a line without
// a terminating \n as incomplete at EOF and silently skips it rather than
// surfacing a parse error. The first 9 fully-written UIDs must still resolve
// via At() because the bbolt sidecar is authoritative for random access.
func TestIntegration_CrossMethod_TornLineAtFileTailSkippedByRead(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	w1 := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	receipts := appendN(t, w1, 10)
	if cerr := w1.Close(); cerr != nil {
		t.Fatalf("w1.Close: %v", cerr)
	}

	// Corrupt: strip the trailing newline from the last JSONL line.
	tornLastLine(t, dataDir)

	// Open a fresh FileEventLog over the same dataDir.
	w2 := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	t.Cleanup(func() { _ = w2.Close() })

	// Read must yield exactly 9 events and no error. The torn final line is
	// not a complete line (isCompleteLine returns false) so it is skipped at
	// EOF without raising a parse error. Callers see a clean 9-event stream.
	got := make([]kernel.Event, 0, 10)
	for ev, err := range w2.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			t.Fatalf("Read yielded unexpected error: %v", err)
		}
		got = append(got, ev)
	}

	if len(got) != 9 {
		t.Errorf("events after torn line: got %d, want 9", len(got))
	}

	// At() for every fully-written UID must succeed. bbolt recorded their
	// offsets at Append time, so the torn tail does not affect random access.
	for _, r := range receipts[:9] {
		ev, aerr := w2.At(ctx, r.UID)
		if aerr != nil {
			t.Errorf("At(%s): %v", r.UID, aerr)
			continue
		}
		if ev.UID != r.UID {
			t.Errorf("At(%s) returned UID %q", r.UID, ev.UID)
		}
	}

	// At() for the torn UID (receipts[9]) also succeeds: bbolt recorded the
	// offset at Append time, and readEventAt uses ReadBytes which accepts EOF
	// after content, so the missing trailing newline does not block the read.
	tornUID := receipts[9].UID
	ev, aerr := w2.At(ctx, tornUID)
	if aerr != nil {
		t.Errorf("At(torn UID %s): %v", tornUID, aerr)
	} else if ev.UID != tornUID {
		t.Errorf("At(torn UID %s) returned wrong UID %q", tornUID, ev.UID)
	}
}

// TestIntegration_CrossMethod_AppendThenAtRandomAccess appends 200 events in 4
// batches of 50, then spawns 8 goroutines each calling At() on a random UID
// from the full set. Asserts zero errors, UID match, and race-clean.
func TestIntegration_CrossMethod_AppendThenAtRandomAccess(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	t.Cleanup(func() { _ = w.Close() })

	const batches = 4
	const perBatch = 50
	uids := make([]string, 0, batches*perBatch)
	for range batches {
		rs := appendN(t, w, perBatch)
		for _, r := range rs {
			uids = append(uids, r.UID)
		}
	}

	const readers = 8
	const readsPerReader = 50

	var wg sync.WaitGroup
	var failures atomic.Int64
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range readsPerReader {
				uid := uids[rand.IntN(len(uids))]
				ev, err := w.At(ctx, uid)
				if err != nil {
					failures.Add(1)
					t.Errorf("At(%s): %v", uid, err)
					return
				}
				if ev.UID != uid {
					failures.Add(1)
					t.Errorf("At(%s) returned wrong UID %q", uid, ev.UID)
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("%d At failures", n)
	}
}

// TestIntegration_CrossMethod_AppendRebuildIdempotencyLargeN appends 500 events
// through a FileEventLog with rebuildFakeStorage wired via WithStorage. It then
// wipes the fake's applied map and calls Rebuild twice, asserting idempotency:
// first pass applies 500 and reports zero errors; second pass skips all 500
// (HasEvent returns true) and applies 0.
func TestIntegration_CrossMethod_AppendRebuildIdempotencyLargeN(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	store := newRebuildFakeStorage()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithStorage(store))
	t.Cleanup(func() { _ = w.Close() })

	const n = 500
	appendN(t, w, n)

	// appendN drives Append which calls applyToStorage, so store.calls
	// already has n entries from the fan-out path. Snapshot the count now
	// so we can measure only the delta that Rebuild contributes.
	baselineCalls := len(store.callsSnapshot())
	if baselineCalls != n {
		t.Fatalf("baseline Apply calls after Append: got %d, want %d", baselineCalls, n)
	}

	// Wipe the applied map to force Rebuild to re-apply from scratch.
	store.mu.Lock()
	store.applied = make(map[string]bool)
	store.mu.Unlock()

	// First Rebuild pass: all 500 events must be applied (applied map is empty).
	errs, err := w.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("first Rebuild ApplyErrors: got %d, want 0", len(errs))
	}
	afterFirstRebuild := len(store.callsSnapshot())
	firstRebuildApplied := afterFirstRebuild - baselineCalls
	if firstRebuildApplied != n {
		t.Errorf("first Rebuild applied: got %d, want %d", firstRebuildApplied, n)
	}

	// Second Rebuild pass: HasEvent now returns true for all UIDs; Apply must
	// not be called again (idempotency).
	errs, err = w.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("second Rebuild ApplyErrors: got %d, want 0", len(errs))
	}
	afterSecondRebuild := len(store.callsSnapshot())
	if afterSecondRebuild != afterFirstRebuild {
		t.Errorf("second Rebuild must skip all (idempotent): Apply called %d additional times",
			afterSecondRebuild-afterFirstRebuild)
	}
}

// TestIntegration_CrossMethod_ReadFilterConsistencyAfterConcurrentAppend
// writes 100 alpha events and 100 beta events from separate concurrent
// goroutines, then asserts Read filter correctness:
// - ReadOpts{Kinds: ["alpha"]} yields exactly 100 events, all Kind=="alpha".
// - ReadOpts{Kinds: ["beta"]} yields exactly 100 events, all Kind=="beta".
// - ReadOpts{Limit: 50} yields exactly 50 events.
func TestIntegration_CrossMethod_ReadFilterConsistencyAfterConcurrentAppend(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{})
	t.Cleanup(func() { _ = w.Close() })

	const perKind = 100

	alphaEv := func() kernel.Event {
		ev := sampleEvent()
		ev.Kind = "alpha"
		return ev
	}
	betaEv := func() kernel.Event {
		ev := sampleEvent()
		ev.Kind = "beta"
		return ev
	}

	var wg sync.WaitGroup
	var failures atomic.Int64

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range perKind {
			if _, err := w.Append(ctx, []kernel.Event{alphaEv()}); err != nil {
				failures.Add(1)
				t.Errorf("alpha Append: %v", err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range perKind {
			if _, err := w.Append(ctx, []kernel.Event{betaEv()}); err != nil {
				failures.Add(1)
				t.Errorf("beta Append: %v", err)
				return
			}
		}
	}()

	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d append failures", n)
	}

	// Alpha filter.
	var alphaCount int
	for ev, err := range w.Read(ctx, kernel.ReadOpts{Kinds: []kernel.EventKind{"alpha"}}) {
		if err != nil {
			t.Fatalf("alpha Read: %v", err)
		}
		if ev.Kind != "alpha" {
			t.Errorf("alpha filter yielded Kind=%q", ev.Kind)
		}
		alphaCount++
	}
	if alphaCount != perKind {
		t.Errorf("alpha filter count: got %d, want %d", alphaCount, perKind)
	}

	// Beta filter.
	var betaCount int
	for ev, err := range w.Read(ctx, kernel.ReadOpts{Kinds: []kernel.EventKind{"beta"}}) {
		if err != nil {
			t.Fatalf("beta Read: %v", err)
		}
		if ev.Kind != "beta" {
			t.Errorf("beta filter yielded Kind=%q", ev.Kind)
		}
		betaCount++
	}
	if betaCount != perKind {
		t.Errorf("beta filter count: got %d, want %d", betaCount, perKind)
	}

	// Limit filter.
	var limitCount int
	for _, err := range w.Read(ctx, kernel.ReadOpts{Limit: 50}) {
		if err != nil {
			t.Fatalf("limit Read: %v", err)
		}
		limitCount++
	}
	if limitCount != 50 {
		t.Errorf("limit filter count: got %d, want 50", limitCount)
	}

	// Sanity: unfiltered total is 200.
	var total int
	for _, err := range w.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			t.Fatalf("unfiltered Read: %v", err)
		}
		total++
	}
	if total != perKind*2 {
		t.Errorf("unfiltered total: got %d, want %d", total, perKind*2)
	}

	// Verify torn-line invariant: no file in events/ ends without \n.
	eventsRoot := filepath.Join(dataDir, "events")
	if err := filepath.WalkDir(eventsRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			t.Errorf("torn line in %s", p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk for torn lines: %v", err)
	}
}

// TestIntegration_CrossMethod_ReadCursorLinearity asserts that a complete Read
// after concurrent appends yields events in a consistent (non-repeating,
// non-skipping) order. All events land on the same UTC day (fixed clock), so
// the UID ordering check is a global assertion over the single day bucket.
func TestIntegration_CrossMethod_ReadCursorLinearity(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	// newTestWriter pins the clock to 2026-05-24 12:00 UTC so all events land
	// in the same day bucket regardless of when the test runs.
	w := newTestWriter(t)
	t.Cleanup(func() { _ = w.Close() })

	const total = 200
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range total / 4 {
				if _, err := w.Append(ctx, []kernel.Event{sampleEvent()}); err != nil {
					t.Errorf("Append: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	evs := make([]kernel.Event, 0, total)
	for ev, err := range w.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		evs = append(evs, ev)
	}
	if len(evs) != total {
		t.Fatalf("Read count: got %d, want %d", len(evs), total)
	}

	seen := make(map[string]struct{}, total)
	for i, ev := range evs {
		if _, dup := seen[ev.UID]; dup {
			t.Errorf("Read yielded duplicate UID at index %d: %q", i, ev.UID)
		}
		seen[ev.UID] = struct{}{}
		if i > 0 && !evs[i-1].CreateTime.Before(ev.CreateTime) && evs[i-1].UID > ev.UID {
			// Same CreateTime: UID must be non-decreasing (secondary sort key).
			t.Errorf("Read order violation at index %d: UID[%d]=%q > UID[%d]=%q with equal CreateTime",
				i, i-1, evs[i-1].UID, i, ev.UID)
		}
	}
}
