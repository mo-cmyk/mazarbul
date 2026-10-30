package stats

import "testing"

// TestTopK_DeterministicTieBreak guards that equal-recall-count entities sort
// by UID ASC, not in nondeterministic map-iteration order. Both the entities
// recallCount sort and the context brief "Key entities" ranking promise a
// byte-stable order; an unstable count-only sort over a map breaks that.
func TestTopK_DeterministicTieBreak(t *testing.T) {
	t.Parallel()
	// Run repeatedly to defeat the chance that one map-iteration order happens
	// to coincide with the sorted order.
	const runs = 20
	want := []string{"a", "b", "c"} // all count 5 → UID ASC
	for i := range runs {
		s := newLiveState()
		s.topEntities = map[string]*topEntityState{
			"c": {UID: "c", Name: "C", Count: 5},
			"a": {UID: "a", Name: "A", Count: 5},
			"b": {UID: "b", Name: "B", Count: 5},
		}
		got := s.topK(10)
		if len(got) != 3 {
			t.Fatalf("run %d: want 3 items, got %d", i, len(got))
		}
		for j := range want {
			if got[j].UID != want[j] {
				t.Fatalf("run %d: tie order = %v, want %v", i, uids(got), want)
			}
		}
	}
}

// TestTopK_CountThenUID guards the full ordering: count DESC first, UID ASC
// only within a count tier.
func TestTopK_CountThenUID(t *testing.T) {
	t.Parallel()
	s := newLiveState()
	s.topEntities = map[string]*topEntityState{
		"z": {UID: "z", Count: 10},
		"a": {UID: "a", Count: 5},
		"y": {UID: "y", Count: 5},
		"m": {UID: "m", Count: 20},
	}
	got := uids(s.topK(10))
	want := []string{"m", "z", "a", "y"} // 20, 10, then tie(5) by UID asc
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func uids(items []TopItem) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = items[i].UID
	}
	return out
}
