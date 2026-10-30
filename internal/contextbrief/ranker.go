package contextbrief

import (
	"context"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
)

// EntityRanker is the optional ranking surface the Assembler uses to order the
// "Key entities" section by recall usage rather than creation time. Satisfied
// by StatsRanker; a fake in tests only needs to implement this one method.
//
// Implementations MUST return entities in descending recall-count order with a
// deterministic tie-break (recallCount DESC, then UID ASC) so the brief remains
// byte-stable across identical inputs.
//
// When the store has no usage data yet (cold start), implementations MUST fall
// back to recency ordering (createTime DESC, UID ASC) so the section never goes
// empty or useless.
type EntityRanker interface {
	TopEntities(ctx context.Context, n int) ([]navigate.Entity, error)
}

// statsTopLister is the stats.Store slice the StatsRanker needs. Narrowed so
// tests can inject a stub without importing the whole stats package.
type statsTopLister interface {
	Top(ctx context.Context, n int, w stats.Window) ([]stats.TopItem, error)
}

// entityGetter fetches one entity by UID, returning navigate.ErrNotFound (wrapped)
// when the entity does not exist. Satisfied by *navigate.Navigator.
type entityGetter interface {
	GetEntity(ctx context.Context, uid string) (navigate.Entity, error)
	ListEntities(ctx context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error)
}

// StatsRanker implements EntityRanker by pulling the top-N recall items from a
// stats store and hydrating each via the navigator. Entities that have been
// forgotten since their last recall are silently dropped (they have no navigator
// row). When the stats store returns zero items (cold start with no usage data),
// StatsRanker falls back to ListEntities (createTime DESC) so the section
// always contains useful content.
type StatsRanker struct {
	stats  statsTopLister
	nav    entityGetter
	window stats.Window
}

// NewStatsRanker constructs a StatsRanker. The window controls the recall
// aggregation horizon; pass stats.Last7Days for the same default as the
// entities MCP tool.
func NewStatsRanker(s statsTopLister, nav entityGetter, window stats.Window) *StatsRanker {
	return &StatsRanker{stats: s, nav: nav, window: window}
}

// TopEntities returns up to n entities ordered by recall count (descending)
// for the configured window. Tie-breaking is deterministic: recallCount DESC,
// then UID ASC (the order stats.Top returns within a count tier) so
// byte-identical briefs are produced across two calls with the same state.
//
// On a cold store (Top returns nothing) it falls back to ListEntities
// (createTime DESC, implicit in the navigator's default ordering) so the brief
// section never goes empty.
func (r *StatsRanker) TopEntities(ctx context.Context, n int) ([]navigate.Entity, error) {
	if n <= 0 {
		return nil, nil
	}

	// Hydrate the recall-ranked UIDs. Stats rows may outlive entities (an entity
	// can be forgotten after it was recalled); those rows drop out during
	// hydration. Asking the stats store for exactly n rows underfills the
	// section when some of the top-n recalled entities were forgotten but
	// lower-ranked live entities still exist. Grow the fetch — deduping by UID —
	// until n live entities are collected or the store is exhausted, capped so a
	// store full of forgotten rows cannot scan unbounded. Mirrors the entities
	// tool's recallCount-sort fetch loop.
	const recallScanCap = 4096
	out := make([]navigate.Entity, 0, n)
	seen := make(map[string]struct{})
	for fetchN := n; len(out) < n; {
		top, err := r.stats.Top(ctx, fetchN, r.window)
		if err != nil {
			return nil, fmt.Errorf("contextbrief: top entities from stats: %w", err)
		}
		if len(top) == 0 {
			break // cold start: no usage data yet
		}
		out, err = r.hydrateBatch(ctx, top, out, seen, n)
		if err != nil {
			return nil, err
		}
		// The store returned fewer rows than asked (exhausted) or we hit the
		// scan ceiling — stop growing.
		if len(top) < fetchN || fetchN >= recallScanCap {
			break
		}
		if fetchN *= 2; fetchN > recallScanCap {
			fetchN = recallScanCap
		}
	}

	// Cold start (no usage data) or every recalled entity has since been
	// forgotten: fall back to recency ordering so the section never goes empty
	// or useless.
	if len(out) == 0 {
		ents, lErr := r.nav.ListEntities(ctx, navigate.EntityFilter{Limit: n})
		if lErr != nil {
			return nil, fmt.Errorf("contextbrief: list entities (fallback): %w", lErr)
		}
		return ents, nil
	}

	return out, nil
}

// hydrateBatch appends entities from one stats.Top batch to out, deduping by
// UID via seen and stopping once out reaches n. Forgotten rows (ErrNotFound)
// are skipped silently; any other GetEntity error is fatal.
func (r *StatsRanker) hydrateBatch(ctx context.Context, top []stats.TopItem, out []navigate.Entity, seen map[string]struct{}, n int) ([]navigate.Entity, error) {
	for _, item := range top {
		if len(out) >= n {
			break
		}
		if _, dup := seen[item.UID]; dup {
			continue
		}
		seen[item.UID] = struct{}{}
		ent, err := r.nav.GetEntity(ctx, item.UID)
		if errors.Is(err, navigate.ErrNotFound) {
			continue // forgotten entity; skip
		}
		if err != nil {
			return nil, fmt.Errorf("contextbrief: hydrate entity %q: %w", item.UID, err)
		}
		out = append(out, ent)
	}
	return out, nil
}
