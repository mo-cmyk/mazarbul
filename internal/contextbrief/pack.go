// Package contextbrief assembles a deterministic, token-budgeted markdown
// context brief from the unified knowledge graph — the cold-start working set a
// session loads at open (plan §C). No LLM runs in this path: the brief is pinned
// documents → recently-updated documents → top entities → (when a query is
// given) query-relevant hits, greedily packed by priority into a token budget,
// with the lowest-priority tail dropped behind an explicit marker — never
// silently.
package contextbrief

import (
	"fmt"
	"strings"
)

// bytesPerToken is the coarse tokens≈bytes/4 heuristic the brief uses to stay
// LLM-free in the hot path. It intentionally over-counts for ASCII prose (real
// tokenizers average ~4 chars/token) so the budget is a conservative ceiling.
const bytesPerToken = 4

// tokensFor estimates the token count of s as ceil(len(s)/bytesPerToken).
func tokensFor(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + bytesPerToken - 1) / bytesPerToken
}

// block is one packable unit of the brief: a markdown fragment (typically a
// bullet plus optional body) tagged with the section it belongs to. Blocks are
// supplied in priority order (highest first); the packer preserves that order
// and groups consecutive same-section blocks under one header.
type block struct {
	section string // section heading, e.g. "Pinned"
	text    string // the block's markdown, including its trailing newline
}

// packResult is the outcome of packing blocks into a budget.
type packResult struct {
	markdown string
	tokens   int // estimated tokens of markdown (always <= budget)
	included int // blocks written
	dropped  int // lowest-priority blocks omitted to fit
}

// sectionBudgets maps a section title to its maximum token share of the global
// budget. A section absent from the map is unbounded (governed only by the
// global greedy pack). It is the reusable per-section cap mechanism #210 ships
// for the untrusted "Operating instructions" section; later sections add their
// own entries.
type sectionBudgets map[string]int

// clampSections drops the lowest-priority TAIL of each capped section so that
// section's blocks fit within its per-section token budget, and returns the
// trimmed blocks plus the number dropped by section-capping. Blocks arrive in
// priority order, so trimming the tail of a section drops its lowest-priority
// items. It runs BEFORE pack and only ever removes blocks, so the global
// "never exceed budget" invariant in pack holds unchanged; the returned drop
// count is folded into the brief's total. Per-block cost uses the same
// tokensFor heuristic as pack, counting each section's "## <title>" header once.
func clampSections(blocks []block, caps sectionBudgets) (kept []block, dropped int) {
	if len(caps) == 0 {
		return blocks, 0
	}
	kept = make([]block, 0, len(blocks))
	spent := make(map[string]int, len(caps))
	headerSeen := make(map[string]bool, len(caps))
	for i := range blocks {
		sec := blocks[i].section
		capTokens, capped := caps[sec]
		if !capped {
			kept = append(kept, blocks[i])
			continue
		}
		cost := tokensFor(blocks[i].text)
		if !headerSeen[sec] {
			cost += tokensFor("\n## " + sec + "\n")
		}
		if spent[sec]+cost > capTokens {
			dropped++
			continue
		}
		spent[sec] += cost
		headerSeen[sec] = true
		kept = append(kept, blocks[i])
	}
	return kept, dropped
}

// pack greedily writes title then blocks (in the given priority order) until the
// next block would exceed budget tokens, then drops the remaining tail behind a
// marker. The returned markdown NEVER exceeds budget tokens — even for absurdly
// small budgets the title and marker are themselves budget-gated and dropped if
// they do not fit. When nothing is dropped no marker is emitted and the full
// budget is available to content; when the tail is dropped, markerReserveTokens
// is held back so the marker fits.
func pack(title string, blocks []block, budget int) packResult {
	// First pass at the full budget: the common case where everything fits.
	res := packPass(title, blocks, budget)
	if res.dropped == 0 {
		return res
	}
	// Truncating: reserve exactly the worst-case marker size (the marker names a
	// dropped count of at most len(blocks)), re-pack, then append the real
	// marker. Reserving the actual marker size — not a fixed slab — keeps small
	// budgets from over-dropping. The final fit check makes the never-exceed
	// invariant absolute even when the budget cannot hold the marker at all.
	reserved := budget - tokensFor(markerText(len(blocks), budget))
	if reserved < 0 {
		reserved = 0
	}
	res = packPass(title, blocks, reserved)
	if res.dropped == 0 {
		return res
	}
	marker := markerText(res.dropped, budget)
	if res.tokens+tokensFor(marker) <= budget {
		res.markdown += marker
		res.tokens += tokensFor(marker)
	}
	return res
}

// markerText renders the truncation marker naming how many blocks were dropped.
func markerText(dropped, budget int) string {
	return fmt.Sprintf("\n_… %d lower-priority item(s) omitted to fit the %d-token budget._\n",
		dropped, budget)
}

// packPass is one greedy fill of title + blocks against budget, with no marker.
// The title is written only when it fits; a budget too small for the title (and
// therefore for any block) yields an empty brief rather than an overflow.
func packPass(title string, blocks []block, budget int) packResult {
	var b strings.Builder
	running := 0
	if t := tokensFor(title); t <= budget {
		b.WriteString(title)
		running = t
	}

	lastSection := ""
	included := 0
	for i := range blocks {
		var add strings.Builder
		if blocks[i].section != lastSection {
			add.WriteString("\n## " + blocks[i].section + "\n")
		}
		add.WriteString(blocks[i].text)
		addTokens := tokensFor(add.String())
		if running+addTokens > budget {
			return packResult{markdown: b.String(), tokens: running, included: included, dropped: len(blocks) - i}
		}
		b.WriteString(add.String())
		running += addTokens
		lastSection = blocks[i].section
		included++
	}
	return packResult{markdown: b.String(), tokens: running, included: included, dropped: 0}
}
