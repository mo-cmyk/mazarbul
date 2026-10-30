package kernel_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fakeStatsStore counts the IncEvent / IncBytes calls so the Append
// instrumentation can be asserted without standing up a memStore.
type fakeStatsStore struct {
	mu     sync.Mutex
	events atomic.Int64
	bytes  atomic.Int64
	byKind map[string]int
}

func newFakeStatsStore() *fakeStatsStore { return &fakeStatsStore{byKind: map[string]int{}} }

func (f *fakeStatsStore) IncEvent(kind string) {
	f.events.Add(1)
	f.mu.Lock()
	f.byKind[kind]++
	f.mu.Unlock()
}
func (f *fakeStatsStore) IncBytes(n int64)                  { f.bytes.Add(n) }
func (f *fakeStatsStore) IncRecall([]string, time.Duration) {}
func (f *fakeStatsStore) IncEmbed(bool)                     {}
func (f *fakeStatsStore) IncTokens(stats.TokenKey, int64)   {}
func (f *fakeStatsStore) ObserveTopEntity(string, string)   {}
func (f *fakeStatsStore) IncDedup(string)                   {}
func (f *fakeStatsStore) Snapshot(context.Context, stats.Window) (stats.Snapshot, error) {
	return stats.Snapshot{}, nil
}

func (f *fakeStatsStore) Top(context.Context, int, stats.Window) ([]stats.TopItem, error) {
	return nil, nil
}
func (f *fakeStatsStore) Close(context.Context) error { return nil }

// TestAppend_IncEventAndIncBytes is the regression guard for the
// user-reported "Usage / MCP call counts stay at 0" gap. The
// FileEventLog already had a `statsStore` field and a `WithStats` option,
// but Append never called either counter; the Usage panel rendered zero
// regardless of how many remembers the user issued. This test fails
// before the wiring lands and passes after.
func TestAppend_IncEventAndIncBytes(t *testing.T) {
	ctx := testutil.TestContext(t)
	st := newFakeStatsStore()
	w := newTestWriter(t, kernel.WithStats(st))

	if _, err := w.Append(ctx, []kernel.Event{sampleEvent(), sampleEvent()}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if got := st.events.Load(); got != 2 {
		t.Errorf("IncEvent count = %d, want 2", got)
	}
	if st.byKind["remember"] != 2 {
		t.Errorf("IncEvent[\"remember\"] = %d, want 2 (byKind=%v)", st.byKind["remember"], st.byKind)
	}
	if st.bytes.Load() <= 0 {
		t.Errorf("IncBytes total = %d, want > 0 (payload bytes were ignored)", st.bytes.Load())
	}
}
