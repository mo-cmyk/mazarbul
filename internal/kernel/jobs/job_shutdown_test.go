package jobs

import (
	"context"
	"sync"
	"testing"
)

// TestRegistry_ConcurrentPersistDuringClose reproduces the shutdown race the
// live MCP load test surfaced: background enrichment goroutines call
// rep.Update / rep.Done / rep.Fail (→ enqueuePersist) while the daemon shuts
// down and Close() closes the persist channel. Before the persistMu guard this
// panicked with "send on closed channel" on a background goroutine which —
// absent the eventlog's panic recovery — took down the whole daemon process.
//
// The test hammers Begin + Update + Done/Fail from several goroutines while
// Close runs concurrently, then keeps sending after Close has returned. It must
// complete without a panic. Run under -race to also catch the unsynchronized
// close-vs-send as a data race.
func TestRegistry_ConcurrentPersistDuringClose(t *testing.T) {
	t.Parallel()
	r := NewRegistryWithStore(t.TempDir())

	const writers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	stop := make(chan struct{})

	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start // line all writers up so sends straddle Close
			for {
				select {
				case <-stop:
					return
				default:
				}
				rep, _ := r.Begin(context.Background(), "extraction", "load")
				rep.Update(0.5, "mid")
				if n%2 == 0 {
					rep.Done()
				} else {
					rep.Fail(context.Canceled)
				}
			}
		}(i)
	}

	close(start)
	// Close while sends are in flight — the exact shutdown race.
	r.Close()
	// Late senders (post-Close) must drop their snapshot, not panic.
	rep, _ := r.Begin(context.Background(), "extraction", "after-close")
	rep.Update(0.9, "late")
	rep.Done()

	close(stop)
	wg.Wait()

	// Close is idempotent: a second call must be a safe no-op.
	r.Close()
}
