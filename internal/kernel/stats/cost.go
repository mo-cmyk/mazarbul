package stats

import "sort"

// CostBreakdown is the panel-facing aggregate of token spend for a
// Window. Mirrors the M5b cost drill-down shape: totals, per-model,
// per-operation, per-source, cache analytics, projection.
type CostBreakdown struct {
	Window        string
	TotalUSD      float64
	ByModel       []ModelCost
	ByOperation   []OpCost
	BySource      []SourceCost
	CacheAnalyt   CacheAnalytics
	ProjectionUSD float64
}

// ModelCost is per-(provider, model) totals + estimated USD.
type ModelCost struct {
	Provider       string
	Model          string
	InputTok       int64
	OutputTok      int64
	CacheReadTok   int64
	CacheCreateTok int64
	EmbedTok       int64
	USD            float64
}

// OpCost is per-operation USD subtotal (chat, recall, ingest, …).
type OpCost struct {
	Op  string
	USD float64
}

// SourceCost is per-source USD subtotal (manual, gmail, slack, …).
type SourceCost struct {
	Source string
	USD    float64
}

// CacheAnalytics surfaces Anthropic prompt-cache and embedding-cache
// hit rates side-by-side.
type CacheAnalytics struct {
	AnthropicCacheReadTok   int64
	AnthropicCacheCreateTok int64
	AnthropicCacheHitRate   float64
	EmbedCacheHitRate       float64
}

// ComputeCost builds a CostBreakdown from a Snapshot + price table.
// Pure function — no I/O — so the same logic backs the MCP tool, the
// /cost slash turn, and the cost drill-down panel.
func ComputeCost(snap Snapshot, table []Price, windowLabel string) CostBreakdown {
	cb := CostBreakdown{Window: windowLabel}

	type modelKey struct {
		provider, model string
	}
	modelTotals := map[modelKey]*ModelCost{}
	opTotals := map[string]*OpCost{}

	var anthropicCacheRead, anthropicCacheCreate int64

	for k, n := range snap.TokensByKey {
		mk := modelKey{k.Provider, k.Model}
		mc := modelTotals[mk]
		if mc == nil {
			mc = &ModelCost{Provider: k.Provider, Model: k.Model}
			modelTotals[mk] = mc
		}
		switch {
		case k.Kind == KindEmbed:
			mc.EmbedTok += n
		case k.Kind == KindCacheRead:
			mc.CacheReadTok += n
			if k.Provider == "anthropic" {
				anthropicCacheRead += n
			}
		case k.Kind == KindCacheCreate:
			mc.CacheCreateTok += n
			if k.Provider == "anthropic" {
				anthropicCacheCreate += n
			}
		case k.Direction == DirIn:
			mc.InputTok += n
		case k.Direction == DirOut:
			mc.OutputTok += n
		}

		usd := tokenUSD(table, k.Provider, k.Model, k.Kind, n)
		mc.USD += usd
		cb.TotalUSD += usd

		if k.Op != "" {
			op := opTotals[k.Op]
			if op == nil {
				op = &OpCost{Op: k.Op}
				opTotals[k.Op] = op
			}
			op.USD += usd
		}
	}

	cb.ByModel = make([]ModelCost, 0, len(modelTotals))
	for _, m := range modelTotals {
		cb.ByModel = append(cb.ByModel, *m)
	}
	sort.Slice(cb.ByModel, func(i, j int) bool {
		return cb.ByModel[i].USD > cb.ByModel[j].USD
	})

	cb.ByOperation = make([]OpCost, 0, len(opTotals))
	for _, o := range opTotals {
		cb.ByOperation = append(cb.ByOperation, *o)
	}
	sort.Slice(cb.ByOperation, func(i, j int) bool {
		return cb.ByOperation[i].USD > cb.ByOperation[j].USD
	})

	cb.CacheAnalyt = CacheAnalytics{
		AnthropicCacheReadTok:   anthropicCacheRead,
		AnthropicCacheCreateTok: anthropicCacheCreate,
		EmbedCacheHitRate:       snap.EmbedCacheHitRate,
	}
	if total := anthropicCacheRead + anthropicCacheCreate; total > 0 {
		cb.CacheAnalyt.AnthropicCacheHitRate = float64(anthropicCacheRead) / float64(total)
	}

	return cb
}

// tokenUSD returns the USD cost of n tokens at the given (provider,
// model, kind) rate. Unknown prices contribute 0.
func tokenUSD(table []Price, provider, model string, kind Kind, n int64) float64 {
	rate, ok := LookupPrice(table, provider, model, kind)
	if !ok || rate == 0 || n <= 0 {
		return 0
	}
	return rate * float64(n) / 1_000_000
}
