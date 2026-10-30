package contextbrief

import (
	"context"
	"fmt"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
)

// fakeStatsLister is a stub for statsTopLister that returns a fixed top list.
type fakeStatsLister struct {
	items []stats.TopItem
	err   error
}

func (f *fakeStatsLister) Top(_ context.Context, n int, _ stats.Window) ([]stats.TopItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	if n < len(f.items) {
		return f.items[:n], nil
	}
	return f.items, nil
}

// fakeStatsCounter is a fakeStatsLister that records how many times Top was
// called and with which n, so a test can assert the grow-the-fetch loop fires.
type fakeStatsCounter struct {
	items    []stats.TopItem
	calls    int
	lastN    int
	maxFetch int // largest n it has been asked for
}

func (f *fakeStatsCounter) Top(_ context.Context, n int, _ stats.Window) ([]stats.TopItem, error) {
	f.calls++
	f.lastN = n
	if n > f.maxFetch {
		f.maxFetch = n
	}
	if n < len(f.items) {
		return f.items[:n], nil
	}
	return f.items, nil
}

// fakeEntityNav is a stub for entityGetter that serves entities from a map.
type fakeEntityNav struct {
	byUID    map[string]navigate.Entity
	fallback []navigate.Entity // returned by ListEntities
}

func (f *fakeEntityNav) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	e, ok := f.byUID[uid]
	if !ok {
		return navigate.Entity{}, fmt.Errorf("entity %q: %w", uid, navigate.ErrNotFound)
	}
	return e, nil
}

//nolint:gocritic // hugeParam: EntityFilter passed by value to satisfy the entityGetter interface.
func (f *fakeEntityNav) ListEntities(_ context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	if opts.Limit > 0 && len(f.fallback) > opts.Limit {
		return f.fallback[:opts.Limit], nil
	}
	return f.fallback, nil
}

func TestStatsRanker_RanksByRecallCount(t *testing.T) {
	t.Parallel()
	// Three entities with distinct recall counts; stats returns them in
	// descending order (as the real store does). The ranker must preserve that
	// order in its output.
	navFake := &fakeEntityNav{
		byUID: map[string]navigate.Entity{
			"uid-alpha": {UID: "uid-alpha", Kind: "person", DisplayName: "Merry"},
			"uid-beta":  {UID: "uid-beta", Kind: "person", DisplayName: "Sam"},
			"uid-gamma": {UID: "uid-gamma", Kind: "org", DisplayName: "Gamma Corp"},
		},
	}
	statsFake := &fakeStatsLister{items: []stats.TopItem{
		{UID: "uid-alpha", RecallCount: 42},
		{UID: "uid-beta", RecallCount: 17},
		{UID: "uid-gamma", RecallCount: 3},
	}}

	r := NewStatsRanker(statsFake, navFake, stats.Last7Days)
	ents, err := r.TopEntities(context.Background(), 10)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(ents) != 3 {
		t.Fatalf("want 3 entities, got %d", len(ents))
	}
	if ents[0].UID != "uid-alpha" {
		t.Errorf("rank 0: want uid-alpha, got %s", ents[0].UID)
	}
	if ents[1].UID != "uid-beta" {
		t.Errorf("rank 1: want uid-beta, got %s", ents[1].UID)
	}
	if ents[2].UID != "uid-gamma" {
		t.Errorf("rank 2: want uid-gamma, got %s", ents[2].UID)
	}
}

func TestStatsRanker_LimitHonoured(t *testing.T) {
	t.Parallel()
	navFake := &fakeEntityNav{
		byUID: map[string]navigate.Entity{
			"e1": {UID: "e1", Kind: "person", DisplayName: "One"},
			"e2": {UID: "e2", Kind: "person", DisplayName: "Two"},
			"e3": {UID: "e3", Kind: "person", DisplayName: "Three"},
		},
	}
	statsFake := &fakeStatsLister{items: []stats.TopItem{
		{UID: "e1", RecallCount: 10},
		{UID: "e2", RecallCount: 5},
		{UID: "e3", RecallCount: 1},
	}}

	r := NewStatsRanker(statsFake, navFake, stats.Last7Days)
	ents, err := r.TopEntities(context.Background(), 2)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(ents) != 2 {
		t.Errorf("want 2, got %d", len(ents))
	}
}

func TestStatsRanker_ZeroStatsFallsBackToRecency(t *testing.T) {
	t.Parallel()
	// When the stats store is empty (cold start), TopEntities must return the
	// recency list from ListEntities rather than an empty slice.
	fallback := []navigate.Entity{
		{UID: "r1", Kind: "person", DisplayName: "Recency One"},
		{UID: "r2", Kind: "org", DisplayName: "Recency Two"},
	}
	navFake := &fakeEntityNav{fallback: fallback}
	statsFake := &fakeStatsLister{items: nil} // cold store — no usage data

	r := NewStatsRanker(statsFake, navFake, stats.Last7Days)
	ents, err := r.TopEntities(context.Background(), 10)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("want fallback 2 entities, got %d: %v", len(ents), ents)
	}
	if ents[0].UID != "r1" {
		t.Errorf("fallback order: want r1 first, got %s", ents[0].UID)
	}
}

func TestStatsRanker_SkipsForgottenEntities(t *testing.T) {
	t.Parallel()
	// uid-gone is in the stats store but has been forgotten from the navigator.
	// The ranker must skip it silently and return the remaining live entities.
	navFake := &fakeEntityNav{
		byUID: map[string]navigate.Entity{
			"uid-live": {UID: "uid-live", Kind: "person", DisplayName: "Alive"},
		},
		fallback: []navigate.Entity{{UID: "uid-live", Kind: "person", DisplayName: "Alive"}},
	}
	statsFake := &fakeStatsLister{items: []stats.TopItem{
		{UID: "uid-gone", RecallCount: 99}, // forgotten
		{UID: "uid-live", RecallCount: 5},
	}}

	r := NewStatsRanker(statsFake, navFake, stats.Last7Days)
	ents, err := r.TopEntities(context.Background(), 10)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("want 1 entity, got %d", len(ents))
	}
	if ents[0].UID != "uid-live" {
		t.Errorf("want uid-live, got %s", ents[0].UID)
	}
}

func TestStatsRanker_GrowsFetchWhenTopRowsForgotten(t *testing.T) {
	t.Parallel()
	// The single highest-ranked stats row is forgotten, but two lower-ranked
	// live entities exist. Asking the store for exactly n=2 and dropping the
	// forgotten row would underfill to 1. The ranker must grow the fetch so it
	// still returns 2 live entities.
	navFake := &fakeEntityNav{
		byUID: map[string]navigate.Entity{
			"live-a": {UID: "live-a", Kind: "person", DisplayName: "A"},
			"live-b": {UID: "live-b", Kind: "person", DisplayName: "B"},
		},
	}
	statsFake := &fakeStatsCounter{items: []stats.TopItem{
		{UID: "ghost", RecallCount: 100}, // forgotten — drops during hydration
		{UID: "live-a", RecallCount: 50},
		{UID: "live-b", RecallCount: 25},
	}}

	r := NewStatsRanker(statsFake, navFake, stats.Last7Days)
	ents, err := r.TopEntities(context.Background(), 2)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("want 2 live entities (grew past forgotten top row), got %d: %v", len(ents), ents)
	}
	if ents[0].UID != "live-a" || ents[1].UID != "live-b" {
		t.Errorf("want [live-a live-b], got [%s %s]", ents[0].UID, ents[1].UID)
	}
	if statsFake.maxFetch <= 2 {
		t.Errorf("expected the fetch to grow past 2, max fetch was %d", statsFake.maxFetch)
	}
}

func TestStatsRanker_ZeroLimitReturnsNil(t *testing.T) {
	t.Parallel()
	statsFake := &fakeStatsCounter{items: []stats.TopItem{{UID: "x", RecallCount: 1}}}
	navFake := &fakeEntityNav{}
	r := NewStatsRanker(statsFake, navFake, stats.Last7Days)
	ents, err := r.TopEntities(context.Background(), 0)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("want no entities for n=0, got %d", len(ents))
	}
	if statsFake.calls != 0 {
		t.Errorf("want no stats fetch for n=0, got %d calls", statsFake.calls)
	}
}

func TestStatsRanker_AllForgottenFallsBackToRecency(t *testing.T) {
	t.Parallel()
	// Every entity in the stats store has been forgotten (navigator returns
	// ErrNotFound for all). The ranker must fall back to recency.
	fallback := []navigate.Entity{
		{UID: "fresh", Kind: "person", DisplayName: "Fresh"},
	}
	navFake := &fakeEntityNav{
		byUID:    map[string]navigate.Entity{}, // none hydrate
		fallback: fallback,
	}
	statsFake := &fakeStatsLister{items: []stats.TopItem{
		{UID: "ghost1", RecallCount: 10},
		{UID: "ghost2", RecallCount: 7},
	}}

	r := NewStatsRanker(statsFake, navFake, stats.Last7Days)
	ents, err := r.TopEntities(context.Background(), 10)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(ents) != 1 || ents[0].UID != "fresh" {
		t.Errorf("expected fallback to recency list, got %v", ents)
	}
}
