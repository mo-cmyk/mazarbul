package ontology_test

import (
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// accAt builds one Access at the given UTC day-of-2026.
func accAt(day int) ontology.Access {
	return ontology.Access{AccessedAt: time.Date(2026, 1, day, 12, 0, 0, 0, time.UTC)}
}

// permute yields every permutation of in (Heap-free recursive swap). N is small
// in these tests (<=5), so 5! = 120 permutations is cheap and exhaustive.
func permute(in []ontology.Access) [][]ontology.Access {
	var out [][]ontology.Access
	var rec func(k int, cur []ontology.Access)
	rec = func(k int, cur []ontology.Access) {
		if k == len(cur) {
			cp := make([]ontology.Access, len(cur))
			copy(cp, cur)
			out = append(out, cp)
			return
		}
		for i := k; i < len(cur); i++ {
			cur[k], cur[i] = cur[i], cur[k]
			rec(k+1, cur)
			cur[k], cur[i] = cur[i], cur[k]
		}
	}
	seed := make([]ontology.Access, len(in))
	copy(seed, in)
	rec(0, seed)
	return out
}

func sameStrength(a, b ontology.MemoryStrength) bool {
	return a.AccessCount == b.AccessCount &&
		a.FirstAccessedAt.Equal(b.FirstAccessedAt) &&
		a.LastAccessedAt.Equal(b.LastAccessedAt)
}

// TestReduceAccesses_OrderIndependent is the convergence AC: the same access
// set folded in EVERY permutation yields byte-identical MemoryStrength. This is
// what lets two paired devices replaying the same memoryAccessed events in
// different orders converge to the same memory_strength row.
func TestReduceAccesses_OrderIndependent(t *testing.T) {
	t.Parallel()
	set := []ontology.Access{accAt(3), accAt(20), accAt(1), accAt(11), accAt(7)}
	canonical := ontology.ReduceAccesses(set)

	for i, perm := range permute(set) {
		got := ontology.ReduceAccesses(perm)
		if !sameStrength(got, canonical) {
			t.Fatalf("permutation %d folded to %+v, want %+v (order must not matter)", i, got, canonical)
		}
	}
	// And the canonical fold itself is correct: count = cardinality, MIN/MAX days.
	if canonical.AccessCount != 5 {
		t.Errorf("AccessCount = %d, want 5", canonical.AccessCount)
	}
	if canonical.FirstAccessedAt.Day() != 1 {
		t.Errorf("FirstAccessedAt day = %d, want 1 (MIN)", canonical.FirstAccessedAt.Day())
	}
	if canonical.LastAccessedAt.Day() != 20 {
		t.Errorf("LastAccessedAt day = %d, want 20 (MAX)", canonical.LastAccessedAt.Day())
	}
}

// TestReduceAccesses_Empty folds an empty set to the zero accumulator: count 0,
// zero timestamps. The dispatcher relies on this (no accesses => no strength
// row materialized with garbage).
func TestReduceAccesses_Empty(t *testing.T) {
	t.Parallel()
	got := ontology.ReduceAccesses(nil)
	if got.AccessCount != 0 {
		t.Errorf("AccessCount = %d, want 0", got.AccessCount)
	}
	if !got.FirstAccessedAt.IsZero() || !got.LastAccessedAt.IsZero() {
		t.Errorf("empty set must leave timestamps zero, got first=%v last=%v",
			got.FirstAccessedAt, got.LastAccessedAt)
	}
}

// TestReduceAccesses_NormalizesToUTC pins that a non-UTC access instant folds to
// the same UTC accumulator a UTC instant would — so a device in a different
// timezone converges to byte-identical stored state.
func TestReduceAccesses_NormalizesToUTC(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("UTC+5", 5*60*60)
	local := time.Date(2026, 1, 10, 17, 0, 0, 0, loc) // == 2026-01-10T12:00:00Z
	utc := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

	gotLocal := ontology.ReduceAccesses([]ontology.Access{{AccessedAt: local}})
	gotUTC := ontology.ReduceAccesses([]ontology.Access{{AccessedAt: utc}})

	if !sameStrength(gotLocal, gotUTC) {
		t.Fatalf("timezone affected fold: local=%+v utc=%+v", gotLocal, gotUTC)
	}
	if gotLocal.LastAccessedAt.Location() != time.UTC {
		t.Errorf("LastAccessedAt not normalized to UTC: %v", gotLocal.LastAccessedAt.Location())
	}
}

// TestReduceAccesses_SingleAccess folds a one-element set: first == last == the
// only instant, count 1.
func TestReduceAccesses_SingleAccess(t *testing.T) {
	t.Parallel()
	got := ontology.ReduceAccesses([]ontology.Access{accAt(9)})
	if got.AccessCount != 1 {
		t.Errorf("AccessCount = %d, want 1", got.AccessCount)
	}
	if !got.FirstAccessedAt.Equal(got.LastAccessedAt) {
		t.Errorf("single access: first %v != last %v", got.FirstAccessedAt, got.LastAccessedAt)
	}
}
