package extractor

import (
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// maxPasses is the hard cap on extraction passes per episode chunk. Even when
// every heuristic says another pass would help, we never exceed this limit so
// token costs stay bounded.
const maxPasses = 3

// needsSecondPass returns true when the first-pass result does not meet the
// quality bar that would let us skip subsequent passes.
//
// Two signals trigger a second pass (mirroring LangExtract's class-conditional
// inference logic):
//  1. Any surviving Mention has groundingScore < 0.95 (near-miss grounding
//     suggests the model produced a slightly-off surface form that a focused
//     re-run may improve).
//  2. The relation-to-mention ratio is below 0.3 on episodes with >= 2 mentions
//     (a model that listed people but skipped the obvious works_at or member_of
//     edges likely gave up early; a second pass focused on relations helps).
//
// Both conditions must still hold for the SECOND pass to trigger a THIRD;
// capping at maxPasses (3) prevents infinite loops on pathological input.
func needsSecondPass(mentions []ontology.Mention, candidates []ontology.RelationCandidate) bool {
	if len(mentions) == 0 {
		return false
	}
	for _, m := range mentions {
		if m.GroundingScore < 0.95 {
			return true
		}
	}
	if len(mentions) >= 2 {
		ratio := float64(len(candidates)) / float64(len(mentions))
		if ratio < 0.3 {
			return true
		}
	}
	return false
}

// allWellGrounded returns true when every Mention in ms has a GroundingScore
// >= 0.95. A true result means the first pass was high-confidence and no
// second pass is needed (adaptive skip).
func allWellGrounded(ms []ontology.Mention) bool {
	for _, m := range ms {
		if m.GroundingScore < 0.95 {
			return false
		}
	}
	return true
}

// lowConfidenceKinds returns the distinct EntityKind values that appear among
// Mentions with groundingScore < 0.95. The second pass focuses the prompt on
// these kinds so the model re-examines only the weak areas rather than
// re-running the full extraction (saves ~40% of tokens on typical episodes).
func lowConfidenceKinds(ms []ontology.Mention) []ontology.EntityKind {
	seen := make(map[ontology.EntityKind]bool)
	for _, m := range ms {
		if m.GroundingScore < 0.95 {
			seen[m.Kind] = true
		}
	}
	kinds := make([]ontology.EntityKind, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	return kinds
}

// spanMergeOverlapThreshold is the minimum overlap-over-min ratio at which
// two Mentions from successive passes are considered the same. The metric is
// containment-style (intersection / smaller-span length), not Jaccard/IOU:
// the typical second-pass collision is one mention being a substring of the
// other ("Frodo" at (9,16) inside "Frodo Baggins" at (9,24)). IOU would
// compute 7/15 = 0.47 there and miss the merge even though the model clearly
// produced two surface forms for the same span. Containment gives 7/7 = 1.0
// for that case while still rejecting disjoint or barely-touching spans.
// 0.7 balances "fold near-identical surface forms" vs. "do not collapse
// distinct overlapping mentions" (e.g. "Frodo Baggins" against "Baggins" inside
// it where the latter is the surname-only reference).
const spanMergeOverlapThreshold = 0.7

// spansOverlap returns the intersection-over-min ratio of two SourceSpans
// (1.0 = one fully contains the other) or 0 when either span is empty or
// unlocated. Symmetric: callers may pass spans in either order.
//
// Empty or zero-width spans never overlap: the grounding pass uses (0,0) as
// the sentinel for "extraction text not located", and folding two unlocated
// mentions together would silently drop one of them.
func spansOverlap(a, b ontology.SourceSpan) float64 {
	aLen := a.EndChar - a.StartChar
	bLen := b.EndChar - b.StartChar
	if aLen <= 0 || bLen <= 0 {
		return 0
	}
	lo := a.StartChar
	if b.StartChar > lo {
		lo = b.StartChar
	}
	hi := a.EndChar
	if b.EndChar < hi {
		hi = b.EndChar
	}
	if hi <= lo {
		return 0
	}
	intersection := hi - lo
	minLen := aLen
	if bLen < minLen {
		minLen = bLen
	}
	return float64(intersection) / float64(minLen)
}

// findOverlappingMention returns the index of the merged mention that shares
// at least spanMergeOverlapThreshold IOU with m, or -1 when no merged mention
// overlaps enough. Kind must match: a person mention and a place mention with
// the same span are still distinct entities (e.g. "Berlin" the person vs.
// "Berlin" the city).
func findOverlappingMention(merged []ontology.Mention, m ontology.Mention) int {
	bestIdx := -1
	bestIOU := spanMergeOverlapThreshold
	for i, existing := range merged {
		if existing.Kind != m.Kind {
			continue
		}
		iou := spansOverlap(existing.SourceSpan, m.SourceSpan)
		if iou >= bestIOU {
			bestIdx = i
			bestIOU = iou
		}
	}
	return bestIdx
}

// mergePasses combines the results of two extraction passes. Two pass2
// mentions match a pass1 mention when their SourceSpans cover the same range
// (IOU >= spanMergeOverlapThreshold) with the same Kind: the second pass may
// have produced a slightly different displayName ("Frodo" vs "Frodo
// Schmidt") for the same surface region, and we want to keep the longer
// grounded variant rather than emit a duplicate. The merge is index-stable:
// pass1 mentions keep their original indices so pass1 candidates remain valid.
func mergePasses(
	mentions1 []ontology.Mention, cands1 []ontology.RelationCandidate,
	mentions2 []ontology.Mention, cands2 []ontology.RelationCandidate,
) ([]ontology.Mention, []ontology.RelationCandidate) {
	merged := make([]ontology.Mention, len(mentions1))
	copy(merged, mentions1)

	remap := make(map[int]int, len(mentions2))
	for i, m := range mentions2 {
		if idx := findOverlappingMention(merged, m); idx >= 0 {
			// Already present at idx; keep the higher-grounding variant. Ties
			// resolve to pass1 (first-pass-wins) because pass1 saw the full
			// prompt before focus hints biased pass2 toward a subset of kinds.
			if m.GroundingScore > merged[idx].GroundingScore {
				merged[idx] = m
			}
			remap[i] = idx
		} else {
			remap[i] = len(merged)
			merged = append(merged, m)
		}
	}

	// Merge candidates: pass1 first, then pass2 with remapped indices.
	type ckey struct {
		subj int
		pred string
		obj  int
	}
	cSeen := make(map[ckey]bool, len(cands1))
	out := make([]ontology.RelationCandidate, 0, len(cands1)+len(cands2))
	for _, c := range cands1 {
		k := ckey{c.SubjectMentionIdx, c.Predicate, c.ObjectMentionIdx}
		if !cSeen[k] {
			cSeen[k] = true
			out = append(out, c)
		}
	}
	for _, c := range cands2 {
		newSubj, okS := remap[c.SubjectMentionIdx]
		newObj, okO := remap[c.ObjectMentionIdx]
		if !okS || !okO {
			continue
		}
		k := ckey{newSubj, c.Predicate, newObj}
		if !cSeen[k] {
			cSeen[k] = true
			out = append(out, ontology.RelationCandidate{
				SubjectMentionIdx: newSubj,
				Predicate:         c.Predicate,
				ObjectMentionIdx:  newObj,
				ValidFrom:         c.ValidFrom,
			})
		}
	}
	return merged, out
}
