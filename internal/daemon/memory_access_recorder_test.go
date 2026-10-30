package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// stubAppender is a no-op accessAppender; the debounce/prune paths never reach
// it, and the tests that exercise emit don't assert on its output.
type stubAppender struct{}

func (stubAppender) Append(_ context.Context, _ []kernel.Event) ([]kernel.Receipt, error) {
	return nil, nil
}

// TestMemoryAccessRecorder_DebouncePrunesStaleEntries pins the lastEmit-leak
// fix: a target accessed once and never again must not linger in the map for the
// daemon's lifetime. The amortized sweep (at most once per cooldown) evicts
// entries older than the cooldown, bounding the map to recently-accessed UIDs.
func TestMemoryAccessRecorder_DebouncePrunesStaleEntries(t *testing.T) {
	t.Parallel()
	r := newMemoryAccessRecorder(stubAppender{}, nil)
	r.cooldown = time.Minute

	base := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	r.debounce([]string{"a", "b"}, base)
	if got := len(r.lastEmit); got != 2 {
		t.Fatalf("after first batch lastEmit=%d, want 2", got)
	}

	// Well past the cooldown: a new access to a different target triggers the
	// amortized sweep, evicting the now-stale a/b entries.
	later := base.Add(2 * time.Minute)
	r.debounce([]string{"c"}, later)
	if _, ok := r.lastEmit["a"]; ok {
		t.Errorf("stale entry a not pruned")
	}
	if _, ok := r.lastEmit["b"]; ok {
		t.Errorf("stale entry b not pruned")
	}
	if _, ok := r.lastEmit["c"]; !ok {
		t.Errorf("fresh entry c missing after prune")
	}
	if got := len(r.lastEmit); got != 1 {
		t.Errorf("lastEmit=%d after prune, want 1 (bounded to recent)", got)
	}
}

// TestMemoryAccessRecorder_DebounceSuppressesWithinCooldown confirms the
// debounce contract the prune fix must not regress: a repeat access inside the
// cooldown window is still coalesced (returns no fresh target).
func TestMemoryAccessRecorder_DebounceSuppressesWithinCooldown(t *testing.T) {
	t.Parallel()
	r := newMemoryAccessRecorder(stubAppender{}, nil)
	r.cooldown = time.Minute

	base := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	if first := r.debounce([]string{"x"}, base); len(first) != 1 {
		t.Fatalf("first debounce returned %d, want 1", len(first))
	}
	if again := r.debounce([]string{"x"}, base.Add(30*time.Second)); len(again) != 0 {
		t.Errorf("within-cooldown debounce returned %d, want 0 (suppressed)", len(again))
	}
	// Past the cooldown the same target emits again.
	if reopened := r.debounce([]string{"x"}, base.Add(2*time.Minute)); len(reopened) != 1 {
		t.Errorf("post-cooldown debounce returned %d, want 1 (re-emit)", len(reopened))
	}
}
