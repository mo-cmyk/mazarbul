package stats

import (
	"math/rand/v2"
	"sort"
	"time"
)

// reservoirLatency is a fixed-size reservoir of recall latencies (ms).
// Reservoir sampling beats an HDR histogram for simplicity: no
// extra dep, O(1) per observation, and the panel only needs p50/p95/p99.
type reservoirLatency struct {
	cap    int
	count  int64
	values []float64
}

func newReservoirLatency(capacity int) *reservoirLatency {
	if capacity <= 0 {
		capacity = 1024
	}
	return &reservoirLatency{cap: capacity}
}

func (r *reservoirLatency) observe(d time.Duration) {
	if r == nil {
		return
	}
	r.count++
	ms := float64(d) / float64(time.Millisecond)
	if len(r.values) < r.cap {
		r.values = append(r.values, ms)
		return
	}
	idx := rand.IntN(int(r.count))
	if idx < r.cap {
		r.values[idx] = ms
	}
}

// percentile returns the requested percentile (0..1) of the sampled
// latencies, in milliseconds. Returns 0 when no samples have been seen.
func (r *reservoirLatency) percentile(p float64) float64 {
	if r == nil || len(r.values) == 0 {
		return 0
	}
	vs := make([]float64, len(r.values))
	copy(vs, r.values)
	sort.Float64s(vs)
	if p <= 0 {
		return vs[0]
	}
	if p >= 1 {
		return vs[len(vs)-1]
	}
	idx := int(float64(len(vs)-1) * p)
	return vs[idx]
}
