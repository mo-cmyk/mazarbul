package extractor

import (
	"regexp"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// proceduralLine matches a list step header at the start of a trimmed
// line — "1.", "1)", or "Step 1:" forms. Mem0 v3 treats assistant-emitted
// numbered traces as procedural memory; the heuristic requires three or
// more consecutive numbered lines so a passing reference like "see step
// 4 below" doesn't trigger the classification.
var proceduralLine = regexp.MustCompile(`^\s*(?:step\s+)?\d+[.)](?:\s|$)`)

// firstPersonClause matches first-person predicates Mazarbul's
// self-statement detector keys off ("I am", "I live", "my name is",
// "I'm a"). The classifier widens this slightly beyond the
// self-statement detector's relation-oriented patterns because a
// classifier signal doesn't require a typed object — "I love oat milk"
// is semantically self-describing even though the predicate (love) is
// outside the four-relation set the detector knows.
// FirstPersonClause permits one optional adverb between the "I" and the
// verb so common intensifiers ("I really love", "I always drive") still
// trip the classifier. The adverb slot is non-greedy and bounded to a
// single token so a long episodic narration ("I went to the store and
// later love…") cannot mis-trip.
var firstPersonClause = regexp.MustCompile(`(?i)\b(?:i\s+(?:\w+\s+)?(?:am|'m|live|work|study|love|hate|prefer|like|dislike|enjoy|grew\s+up|was\s+born|have|own|use|drink|eat|play|drive)\b|my\s+(?:name|favourite|favorite|hometown|address|email|phone|partner|spouse|wife|husband|child|kids?|job|role|company|employer)\s+(?:is|are)\b|i'?ve\s+(?:been|lived|worked|studied)\b)`)

// ClassifyEpisode applies a deterministic heuristic to assign an
// EpisodeClass before the LLM extractor runs. The three signals are:
//
//   - ActorAssistant + three or more consecutive lines matching
//     proceduralLine → ClassProcedural
//   - firstPersonClause matches anywhere in the trimmed text →
//     ClassSemantic
//   - Otherwise → ClassEpisodic (the default for session turns)
//
// The classifier is intentionally pure (no LLM, no I/O) so it adds zero
// latency to the write path. Misclassifications degrade gracefully: the
// retrieval type-weight multiplier defaults the unknown-class branch to
// the episodic weight (0.3 with mem0 defaults).
//
// The function never mutates ep; callers assign the return value to
// ep.Class before persistence.
func ClassifyEpisode(ep ontology.Episode) ontology.EpisodeClass {
	text := strings.TrimSpace(ep.Text)
	if text == "" {
		return ontology.ClassEpisodic
	}
	if ep.Actor == ontology.ActorAssistant && looksProcedural(text) {
		return ontology.ClassProcedural
	}
	if firstPersonClause.MatchString(text) {
		return ontology.ClassSemantic
	}
	return ontology.ClassEpisodic
}

// looksProcedural reports whether text contains three or more
// consecutive lines matching proceduralLine. A passing reference like
// "step 4 of the recipe" inside an episodic narration won't trigger
// the classification because only one line matches.
func looksProcedural(text string) bool {
	streak, peak := 0, 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if proceduralLine.MatchString(line) {
			streak++
			if streak > peak {
				peak = streak
			}
		} else {
			streak = 0
		}
	}
	return peak >= 3
}
