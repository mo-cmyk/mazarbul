// Package stats aggregates Mazarbul's runtime counters: events
// written, recalls performed, embed cache hits, latency histograms, and
// per-(provider, model) token totals. The Usage and Cost hub panels
// consume the same Store via Deps.
//
// The current implementation is in-memory only — counters reset on
// daemon restart. A future extension could add persistent daily roll-ups
// so the 30-day sparkline and month-to-date cost survive process restarts.
package stats

import (
	"context"
	"sync"
	"time"
)

// Window identifies the aggregation horizon for Snapshot reads.
type Window int

const (
	// Today aggregates from local midnight to now.
	Today Window = iota
	// Last7Days aggregates from now-7d to now.
	Last7Days
	// Last30Days aggregates from now-30d to now.
	Last30Days
	// Lifetime aggregates everything the store has seen.
	Lifetime
)

// Direction is in-bound or out-bound token flow for IncTokens.
type Direction string

const (
	// DirIn is input tokens flowing into the model.
	DirIn Direction = "in"
	// DirOut is output tokens flowing out of the model.
	DirOut Direction = "out"
)

// Kind classifies a token charge by Anthropic's API line items.
type Kind string

const (
	// KindRegular is plain input/output, no cache involvement.
	KindRegular Kind = "regular"
	// KindCacheRead bills against the Anthropic prompt cache hit path.
	KindCacheRead Kind = "cache-read"
	// KindCacheCreate bills against the Anthropic prompt cache write path.
	KindCacheCreate Kind = "cache-create"
	// KindEmbed bills against an embedding model.
	KindEmbed Kind = "embed"
)

// TokenKey identifies a single token-counter bucket.
type TokenKey struct {
	Provider  string
	Model     string
	Direction Direction
	Kind      Kind
	Op        string
}

// Snapshot is the panel-facing aggregate over a Window.
type Snapshot struct {
	Window               Window
	Events               int64
	BytesIngested        int64
	Recalls              int64
	RecallLatencyP50Ms   float64
	RecallLatencyP95Ms   float64
	RecallLatencyP99Ms   float64
	EmbedCacheHits       int64
	EmbedCacheMisses     int64
	EmbedCacheHitRate    float64
	TokensByKey          map[TokenKey]int64
	TopEntitiesByRecalls []TopItem
	// DedupSkipsByReason counts how often a write was suppressed at the
	// dispatcher because an existing record made it redundant. Phase 2
	// mem0-parity uses "semantic" to mark cosine-similarity NOOPs;
	// future entries (e.g. "exact-hash") follow the same pattern.
	DedupSkipsByReason map[string]int64
}

// TopItem is one entry in a top-K list.
type TopItem struct {
	UID         string
	Name        string
	RecallCount int64
}

// Store is the runtime counter sink. Implementations must be safe for
// concurrent use across the daemon's goroutines.
type Store interface {
	IncEvent(kind string)
	IncBytes(n int64)
	IncRecall(lanes []string, latency time.Duration)
	IncEmbed(cacheHit bool)
	IncTokens(key TokenKey, n int64)
	ObserveTopEntity(uid, name string)
	// IncDedup records a write that was suppressed because a
	// near-identical record already existed. `reason` is a short tag
	// like "semantic" (cosine similarity > threshold) or "exact-hash"
	// (SHA256 content-hash collision). Multiple reasons aggregate
	// independently so the doctor command can report which dedup path
	// fired.
	IncDedup(reason string)

	Snapshot(ctx context.Context, w Window) (Snapshot, error)
	Top(ctx context.Context, n int, w Window) ([]TopItem, error)

	Close(ctx context.Context) error
}

// NewInMemoryStore returns a thread-safe in-memory Store. A
// persistence-backed wrapper that flushes daily could wrap it later.
func NewInMemoryStore() Store {
	return &memStore{
		live:    newLiveState(),
		started: time.Now(),
	}
}

// memStore is the in-memory implementation: pure in-memory counters,
// no persistence. Concurrency-safe under a single RWMutex.
type memStore struct {
	mu      sync.RWMutex
	live    *liveState
	started time.Time
}

func (s *memStore) IncEvent(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live.events++
	if kind != "" {
		s.live.eventsByKind[kind]++
	}
}

func (s *memStore) IncBytes(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live.bytes += n
}

func (s *memStore) IncRecall(lanes []string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live.recalls++
	for _, l := range lanes {
		s.live.recallsByLane[l]++
	}
	s.live.latency.observe(latency)
}

func (s *memStore) IncEmbed(cacheHit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cacheHit {
		s.live.cacheHits++
	} else {
		s.live.cacheMisses++
	}
}

func (s *memStore) IncTokens(key TokenKey, n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live.tokens == nil {
		s.live.tokens = map[TokenKey]int64{}
	}
	s.live.tokens[key] += n
}

func (s *memStore) IncDedup(reason string) {
	if reason == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live.dedupSkips == nil {
		s.live.dedupSkips = map[string]int64{}
	}
	s.live.dedupSkips[reason]++
}

func (s *memStore) ObserveTopEntity(uid, name string) {
	if uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live.topEntities == nil {
		s.live.topEntities = map[string]*topEntityState{}
	}
	st, ok := s.live.topEntities[uid]
	if !ok {
		st = &topEntityState{UID: uid, Name: name}
		s.live.topEntities[uid] = st
	}
	st.Count++
	if st.Name == "" && name != "" {
		st.Name = name
	}
}

func (s *memStore) Snapshot(_ context.Context, w Window) (Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := Snapshot{
		Window:           w,
		Events:           s.live.events,
		BytesIngested:    s.live.bytes,
		Recalls:          s.live.recalls,
		EmbedCacheHits:   s.live.cacheHits,
		EmbedCacheMisses: s.live.cacheMisses,
	}
	total := snap.EmbedCacheHits + snap.EmbedCacheMisses
	if total > 0 {
		snap.EmbedCacheHitRate = float64(snap.EmbedCacheHits) / float64(total)
	}
	if s.live.latency.count > 0 {
		snap.RecallLatencyP50Ms = s.live.latency.percentile(0.50)
		snap.RecallLatencyP95Ms = s.live.latency.percentile(0.95)
		snap.RecallLatencyP99Ms = s.live.latency.percentile(0.99)
	}
	if len(s.live.tokens) > 0 {
		snap.TokensByKey = make(map[TokenKey]int64, len(s.live.tokens))
		for k, v := range s.live.tokens {
			snap.TokensByKey[k] = v
		}
	}
	if items := s.live.topK(10); len(items) > 0 {
		snap.TopEntitiesByRecalls = items
	}
	if len(s.live.dedupSkips) > 0 {
		snap.DedupSkipsByReason = make(map[string]int64, len(s.live.dedupSkips))
		for k, v := range s.live.dedupSkips {
			snap.DedupSkipsByReason[k] = v
		}
	}
	return snap, nil
}

func (s *memStore) Top(_ context.Context, n int, _ Window) ([]TopItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.live.topK(n), nil
}

func (s *memStore) Close(_ context.Context) error { return nil }

// Noop returns a Store whose methods do nothing. Used in tests and the
// --minimal-boot daemon path where stats are deliberately disabled.
func Noop() Store { return noopStore{} }

type noopStore struct{}

func (noopStore) IncEvent(string)                                    {}
func (noopStore) IncBytes(int64)                                     {}
func (noopStore) IncRecall([]string, time.Duration)                  {}
func (noopStore) IncEmbed(bool)                                      {}
func (noopStore) IncTokens(TokenKey, int64)                          {}
func (noopStore) ObserveTopEntity(string, string)                    {}
func (noopStore) IncDedup(string)                                    {}
func (noopStore) Snapshot(context.Context, Window) (Snapshot, error) { return Snapshot{}, nil }
func (noopStore) Top(context.Context, int, Window) ([]TopItem, error) {
	return nil, nil
}
func (noopStore) Close(context.Context) error { return nil }
