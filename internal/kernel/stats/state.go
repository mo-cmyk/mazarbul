package stats

import "sort"

// liveState holds the hot-path counters; access is gated by memStore.mu.
type liveState struct {
	events        int64
	eventsByKind  map[string]int64
	bytes         int64
	recalls       int64
	recallsByLane map[string]int64
	cacheHits     int64
	cacheMisses   int64
	latency       *reservoirLatency
	tokens        map[TokenKey]int64
	topEntities   map[string]*topEntityState
	dedupSkips    map[string]int64
}

type topEntityState struct {
	UID   string
	Name  string
	Count int64
}

func newLiveState() *liveState {
	return &liveState{
		eventsByKind:  map[string]int64{},
		recallsByLane: map[string]int64{},
		latency:       newReservoirLatency(1024),
	}
}

func (s *liveState) topK(n int) []TopItem {
	if n <= 0 || len(s.topEntities) == 0 {
		return nil
	}
	items := make([]TopItem, 0, len(s.topEntities))
	for _, st := range s.topEntities {
		items = append(items, TopItem{UID: st.UID, Name: st.Name, RecallCount: st.Count})
	}
	// Range over a map yields nondeterministic order, so a plain count sort
	// would shuffle equal-count entities across identical states. Break ties on
	// UID ASC so the top list — and every consumer that ranks off it (the
	// entities recallCount sort, the context brief's "Key entities") — is
	// byte-stable.
	sort.Slice(items, func(i, j int) bool {
		if items[i].RecallCount != items[j].RecallCount {
			return items[i].RecallCount > items[j].RecallCount
		}
		return items[i].UID < items[j].UID
	})
	if len(items) > n {
		items = items[:n]
	}
	return items
}
