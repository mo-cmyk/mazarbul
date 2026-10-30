package ontology

import (
	"time"
)

// Access is one element of a memory's monotonic access set, as materialized
// from a memoryAccessed event. AccessedAt is the instant recall surfaced the
// memory.
type Access struct {
	AccessedAt time.Time
}

// MemoryStrength is the materialized accumulator for one target's access set —
// the order-independent state that the memory_strength row stores. It carries
// NO decay term: the time-dependent e^(-lambda*age) multiplier is recomputed at
// query time from LastAccessedAt (like the recency boost), which keeps this
// stored state replay-stable.
type MemoryStrength struct {
	// AccessCount is the number of distinct accesses (set cardinality). The
	// dispatcher dedups by access event UID via INSERT OR IGNORE, so this is the
	// cardinality of the OR-set, idempotent under re-application.
	AccessCount int

	// FirstAccessedAt is the MIN of all access instants (zero if the set is
	// empty). Commutative.
	FirstAccessedAt time.Time

	// LastAccessedAt is the MAX of all access instants (zero if the set is
	// empty). Commutative. The query-time decay is anchored on this value.
	LastAccessedAt time.Time
}

// ReduceAccesses folds a target's FULL access set into the canonical
// MemoryStrength accumulator that the memory_strength row materializes.
//
// The fold is the load-bearing piece of convergent reinforcement. It is a pure,
// deterministic function of its inputs so that two paired devices replaying the
// same set of memoryAccessed events in different orders converge to
// byte-identical state, and so that `maz rebuild` reaches the same end-state:
//
//   - access_count      <- the set cardinality (count of accesses).
//   - first_accessed_at  <- MIN(accessedAt) across the set.
//   - last_accessed_at   <- MAX(accessedAt) across the set.
//
// Cardinality is order-free; MIN and MAX are commutative, associative, and
// idempotent. So the result is independent of access order and stable under
// re-application. AccessedAt values are normalized to UTC so the stored state
// is byte-identical regardless of the originating device's timezone.
func ReduceAccesses(accesses []Access) MemoryStrength {
	ms := MemoryStrength{AccessCount: len(accesses)}
	for i := range accesses {
		ts := accesses[i].AccessedAt.UTC()
		if ms.FirstAccessedAt.IsZero() || ts.Before(ms.FirstAccessedAt) {
			ms.FirstAccessedAt = ts
		}
		if ms.LastAccessedAt.IsZero() || ts.After(ms.LastAccessedAt) {
			ms.LastAccessedAt = ts
		}
	}
	return ms
}
