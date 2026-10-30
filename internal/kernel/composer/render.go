package composer

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// SupersededMarker is the literal string the render layer appends after a
// superseded episode's snippet. The compose-v1 prompt's "Currency" rule
// keys off this exact token, so the marker and the prompt rule must stay
// in lockstep; bumping one without the other silently demotes the
// supersede signal.
const SupersededMarker = "[SUPERSEDED]"

// LatestMarker is the literal string the render layer appends after the
// snippet of the single entry with the highest CreateTime when there
// are 2+ hits. The compose-v1 prompt's "RECENCY MARKER" rule keys off
// this token to short-circuit qwen2.5:7b's position-as-recency
// heuristic ("most recently [N]" where [N] is the lowest citation
// number rather than the newest createTime).
const LatestMarker = "[LATEST]"

// FormatContextBlock is the back-compat wrapper around
// FormatContextBlockWithIntent. It routes through IntentAgnostic so
// existing callers get the intent-agnostic behaviour unchanged. New code
// should pass an explicit intent so chronological / duration questions
// get the right shaping.
func FormatContextBlock(hits []kernel.Hit, maxRunes int) (string, []Citation) {
	return FormatContextBlockWithIntent(hits, maxRunes, IntentAgnostic)
}

// FormatContextBlockWithIntent renders the recall hits as a numbered
// context block for the compose-v1 prompt, with intent-aware shaping.
//
// Layouts by intent:
//
//   - IntentAgnostic / IntentCurrent / IntentHistorical / IntentDuration:
//     "[N] (episodeUID, createTime):\nsnippet" — preserves input order
//     (typically fused-score DESC). Adds LatestMarker on the newest
//     entry when len(hits) >= 2 so qwen2.5:7b's "lowest-index ==
//     latest" heuristic doesn't fire.
//
//   - IntentChronological: hits are stable-sorted by CreateTime DESC
//     and each entry is prefixed with "[k/N latest]" (k=1),
//     "[k/N oldest]" (k=N), or "[k/N]" (in between). The ordinal
//     labels are canonical; qwen2.5:7b can echo them in prose without
//     having to derive the order from ISO timestamps (arXiv 2511.14214
//     shows non-reasoning <8B models hard-fail past 5 dated events).
//
// Returned []Citation is parallel-indexed with the rendered entries:
// citations[0] corresponds to the first entry in the block, etc.
// kernel.Hit.Superseded suffixes SupersededMarker for the compose-v1
// "Currency" rule.
func FormatContextBlockWithIntent(hits []kernel.Hit, maxRunes int, intent TemporalIntent) (string, []Citation) {
	if len(hits) == 0 {
		return "(no context)", nil
	}

	// Chronological intent owns its own shaping path: sort + ordinal
	// labels. Other intents share the legacy numbered layout because
	// they don't need cross-entry ordering — they need a marker on the
	// right entry (LATEST/SUPERSEDED) or a hint block elsewhere in the
	// prompt (DURATION HINT, handled at the prompt layer).
	if intent == IntentChronological {
		return renderChronological(hits, maxRunes)
	}
	return renderNumbered(hits, maxRunes)
}

// renderNumbered is the legacy "[N]" layout used by every non-
// chronological intent. Preserves input order and stamps the LATEST
// marker on the single newest entry when there are 2+ hits.
func renderNumbered(hits []kernel.Hit, maxRunes int) (string, []Citation) {
	var sb strings.Builder
	citations := make([]Citation, 0, len(hits))

	latestIdx := -1
	if len(hits) >= 2 {
		latestIdx = indexOfLatest(hits)
	}

	for i, h := range hits {
		n := i + 1
		snippet := truncateRunesLocal(h.Snippet, maxRunes)
		ts := h.CreateTime.UTC().Format(time.RFC3339)

		fmt.Fprintf(&sb, "[%d] (%s, %s):\n%s", n, hitSourceLabel(&hits[i]), ts, snippet)
		if h.Superseded {
			fmt.Fprintf(&sb, "\n%s", SupersededMarker)
		}
		if i == latestIdx {
			fmt.Fprintf(&sb, "\n%s", LatestMarker)
		}
		sb.WriteString("\n\n")

		citations = append(citations, newCitation(n, &hits[i], snippet))
	}
	return strings.TrimRight(sb.String(), "\n"), citations
}

// renderChronological sorts hits by CreateTime DESC (newest first) and
// prefixes each entry with an ordinal label. The label format is
// "[k/N latest]" for k=1, "[k/N oldest]" for k=N, and "[k/N]" in
// between. Sort is stable so hits with identical createTime preserve
// their input order — important when two events share a timestamp and
// the upstream fused-score ordering carries useful signal.
func renderChronological(hits []kernel.Hit, maxRunes int) (string, []Citation) {
	sorted := make([]kernel.Hit, len(hits))
	copy(sorted, hits)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreateTime.After(sorted[j].CreateTime)
	})

	var sb strings.Builder
	citations := make([]Citation, 0, len(sorted))
	total := len(sorted)

	for i, h := range sorted {
		k := i + 1
		label := fmt.Sprintf("[%d/%d]", k, total)
		switch {
		case k == 1 && total > 1:
			label = fmt.Sprintf("[%d/%d latest]", k, total)
		case k == total && total > 1:
			label = fmt.Sprintf("[%d/%d oldest]", k, total)
		}
		snippet := truncateRunesLocal(h.Snippet, maxRunes)
		ts := h.CreateTime.UTC().Format(time.RFC3339)

		fmt.Fprintf(&sb, "%s (%s, %s):\n%s", label, hitSourceLabel(&sorted[i]), ts, snippet)
		if h.Superseded {
			fmt.Fprintf(&sb, "\n%s", SupersededMarker)
		}
		sb.WriteString("\n\n")

		citations = append(citations, newCitation(k, &sorted[i], snippet))
	}
	return strings.TrimRight(sb.String(), "\n"), citations
}

// hitSourceLabel is the identity shown in a context-entry header: the event
// UID for episodes, the vault location ("notes/foo.md › ## Goals") for
// document hits — so the model grounds document evidence in its source file
// and the prose can cite it naturally.
func hitSourceLabel(h *kernel.Hit) string {
	if h.Source == nil {
		return h.UID
	}
	if h.Source.Heading != "" {
		return h.Source.Path + " › " + h.Source.Heading
	}
	return h.Source.Path
}

// newCitation builds the Citation for one rendered entry, carrying the
// document location through for document hits.
func newCitation(index int, h *kernel.Hit, snippet string) Citation {
	c := Citation{
		Index:      index,
		EpisodeUID: h.UID,
		Snippet:    snippet,
		CreateTime: h.CreateTime.UTC(),
	}
	if h.Source != nil {
		c.Path = h.Source.Path
		c.Heading = h.Source.Heading
	}
	return c
}

// indexOfLatest returns the index of the hit with the most recent
// CreateTime. Ties resolve to the earliest index — deterministic and
// stable across renders of the same hit slice.
func indexOfLatest(hits []kernel.Hit) int {
	best := 0
	for i := 1; i < len(hits); i++ {
		if hits[i].CreateTime.After(hits[best].CreateTime) {
			best = i
		}
	}
	return best
}

// truncateRunesLocal returns s capped at runesMax runes. When the input is
// shorter than the cap it is returned unchanged. A non-positive cap returns
// the empty string. Kept local (not exported) to avoid polluting the package
// API; the render path is the only caller.
func truncateRunesLocal(s string, runesMax int) string {
	if runesMax <= 0 {
		return ""
	}
	count := utf8.RuneCountInString(s)
	if count <= runesMax {
		return s
	}
	var out strings.Builder
	out.Grow(runesMax * 4)
	n := 0
	for _, r := range s {
		if n >= runesMax {
			break
		}
		out.WriteRune(r)
		n++
	}
	return out.String()
}
