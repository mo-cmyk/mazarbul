package kernel_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
)

// recallStatsRecorder is a stats.Store fake recording IncRecall calls.
type recallStatsRecorder struct {
	mu      sync.Mutex
	calls   atomic.Int64
	lanes   [][]string
	latency []time.Duration
}

func newRecallStatsRecorder() *recallStatsRecorder { return &recallStatsRecorder{} }

func (r *recallStatsRecorder) IncEvent(string) {}
func (r *recallStatsRecorder) IncBytes(int64)  {}
func (r *recallStatsRecorder) IncRecall(lanes []string, latency time.Duration) {
	r.calls.Add(1)
	r.mu.Lock()
	r.lanes = append(r.lanes, append([]string(nil), lanes...))
	r.latency = append(r.latency, latency)
	r.mu.Unlock()
}
func (r *recallStatsRecorder) IncEmbed(bool)                   {}
func (r *recallStatsRecorder) IncTokens(stats.TokenKey, int64) {}
func (r *recallStatsRecorder) ObserveTopEntity(string, string) {}
func (r *recallStatsRecorder) IncDedup(string)                 {}
func (r *recallStatsRecorder) Snapshot(context.Context, stats.Window) (stats.Snapshot, error) {
	return stats.Snapshot{}, nil
}

func (r *recallStatsRecorder) Top(context.Context, int, stats.Window) ([]stats.TopItem, error) {
	return nil, nil
}
func (r *recallStatsRecorder) Close(context.Context) error { return nil }

// TestHybridQuery_Recall_IncrementsStats is the regression guard that
// Recall bumps stats.IncRecall on every invocation regardless of whether
// the lane returned hits. The user-reported "Usage / MCP call counts
// stay at 0" stems from this missing wire: the Stats store lived on
// Deps but no Recall call site ever called it. This test fails before
// the SetStats + IncRecall wiring lands and passes after.
func TestHybridQuery_Recall_IncrementsStats(t *testing.T) {
	rec := newRecallStatsRecorder()
	h := &kernel.HybridQuery{}
	h.SetStats(rec)

	// Empty query short-circuits inside Recall; we exercise the
	// instrumentation path that fires once per call regardless of hits.
	// (Using a non-empty query would force a full storage stack stand-up
	// which the unit test deliberately avoids.)
	for hit, err := range h.Recall(context.Background(), kernel.RecallRequest{Query: ""}) {
		_ = hit
		_ = err
	}

	if rec.calls.Load() != 1 {
		t.Errorf("IncRecall calls = %d, want 1 (instrumentation missing)", rec.calls.Load())
	}
}
