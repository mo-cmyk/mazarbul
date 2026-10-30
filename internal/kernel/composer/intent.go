package composer

import (
	"regexp"
	"strings"
)

// TemporalIntent classifies the temporal shape of a recall question so
// the render layer can route entries through the right shaping pipeline:
// chronological queries get pre-sorted with ordinal labels, duration
// queries get a pre-computed delta hint, etc. Routing is lexical — no
// LLM call — so it is cheap, deterministic, and reviewable.
//
// The enum is intentionally small. Each new bucket costs a prompt-level
// rule + a render-layer branch; only add one when a failing test
// demonstrates the model can't recover from a missing routing.
type TemporalIntent int

const (
	// IntentAgnostic is the default: no temporal signal, render in fused
	// score order with no extra shaping. Most "who/what/where" questions
	// land here.
	IntentAgnostic TemporalIntent = iota

	// IntentCurrent signals "what is true now?" — render prefers the
	// [LATEST] marker and demotes [SUPERSEDED] entries.
	IntentCurrent

	// IntentHistorical signals "what was true originally / before?" —
	// render prefers earlier entries; superseded entries become useful
	// rather than noise.
	IntentHistorical

	// IntentChronological signals "list these in order" — the render
	// layer sorts hits by CreateTime DESC and prefixes each entry with
	// an ordinal label "[k/N latest]" … "[k/N oldest]" so qwen2.5:7b
	// doesn't have to sort timestamps itself (arXiv 2511.14214 shows
	// non-reasoning <8B models hard-fail past 5 dated events).
	IntentChronological

	// IntentDuration signals "how long ago / how many weeks" — the
	// render layer emits a DURATION HINT block with pre-computed
	// deltas in days/weeks/months so the model can echo the answer
	// rather than derive it.
	IntentDuration
)

// String renders the intent for log lines.
func (t TemporalIntent) String() string {
	switch t {
	case IntentCurrent:
		return "current"
	case IntentHistorical:
		return "historical"
	case IntentChronological:
		return "chronological"
	case IntentDuration:
		return "duration"
	default:
		return "agnostic"
	}
}

// Lexical rules. Order matters: the first matching pattern wins, so
// more specific patterns sit above more general ones. Each pattern is
// case-insensitive and anchored on word boundaries to avoid matching
// inside unrelated tokens (e.g. "now" shouldn't match "snowing").
//
// The lists were tuned against the user-reported failure modes:
//   - Current: TEST 4 ("Which country do I live in") and the
//     "where do I work now" / "what is my name" family.
//   - Historical: the "where did I originally / used to" family.
//   - Chronological: TEST 6 ("List my names chronologically").
//   - Duration: TEST 5 ("how long ago did I work at X") and
//     "how many weeks/days/months" arithmetic.
var (
	reChronological = regexp.MustCompile(`(?i)\b(chronological|chronologically|in order|over time|oldest to newest|newest to oldest|list.*(names|jobs|places|cities)|timeline)\b`)
	reDuration      = regexp.MustCompile(`(?i)\b(how long ago|how many (days|weeks|months|years)|when did|since when|days ago|weeks ago|months ago|years ago)\b`)
	reHistorical    = regexp.MustCompile(`(?i)\b(historically|originally|original|first(?:ly)?|previously|used to|in the past|before|earlier)\b`)
	reCurrent       = regexp.MustCompile(`(?i)\b(currently|now|latest|today|right now|at the moment|present(?:ly)?)\b`)
)

// ClassifyIntent buckets a question into a TemporalIntent. Returns
// IntentAgnostic when no rule fires; that bucket is the safe default
// because the render layer renders it identically to today.
func ClassifyIntent(question string) TemporalIntent {
	q := strings.TrimSpace(question)
	if q == "" {
		return IntentAgnostic
	}
	switch {
	case reChronological.MatchString(q):
		return IntentChronological
	case reDuration.MatchString(q):
		return IntentDuration
	case reHistorical.MatchString(q):
		return IntentHistorical
	case reCurrent.MatchString(q):
		return IntentCurrent
	default:
		return IntentAgnostic
	}
}
