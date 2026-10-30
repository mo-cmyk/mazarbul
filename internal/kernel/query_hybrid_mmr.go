package kernel

import (
	"strings"
)

// applyMMR re-ranks the input hits via Maximal Marginal Relevance
// (Carbonell & Goldstein 1998) so near-duplicates are demoted in favour
// of more diverse candidates. Returns the top-K of the reranked set.
//
// Algorithm:
//
//	pick = argmax_{d in remaining} λ·rel(d) − (1−λ)·max_{s in picked} sim(d, s)
//
// where rel(d) is the input FusedScore (already RRF-fused and floor-
// filtered upstream) and sim(d, s) is a snippet-level Jaccard
// similarity over character 3-grams. Jaccard was chosen over cosine on
// the vector lane because it is deterministic, dependency-free, runs
// in pure CPU at top-100 scale, and reliably catches the "Arwen
// working at Rivendell #1..#N" near-duplicate pattern that motivated
// MMR in the first place. Cosine-on-stored-embeddings can replace this
// later without changing the algorithm.
//
// Lambda contract:
//   - λ <= 0:   no-op, return input unchanged (used as the disable knob
//     via config; treat negative lambdas as zero defensively).
//   - λ == 1:   pure relevance, reduces to FusedScore order.
//   - λ ∈ (0,1): trade off relevance vs diversity; 0.7 is the
//     production-consensus default.
//
// The output is capped at k; when k <= 0 or k > len(hits) the whole
// reranked list is returned. The input slice is treated as immutable
// — applyMMR allocates its own picked + remaining state.
func applyMMR(hits []Hit, lambda float64, k int) []Hit {
	if lambda <= 0 || len(hits) <= 1 {
		// Disabled (λ=0) or trivially small — pass through.
		if k > 0 && k < len(hits) {
			out := make([]Hit, k)
			copy(out, hits)
			return out
		}
		return hits
	}

	limit := k
	if limit <= 0 || limit > len(hits) {
		limit = len(hits)
	}

	// Precompute 3-gram sets once per hit so each sim() call is O(|A|+|B|).
	grams := make([]map[string]struct{}, len(hits))
	for i, h := range hits {
		grams[i] = charNGramSet(h.Snippet, 3)
	}

	picked := make([]Hit, 0, limit)
	pickedIdx := make([]int, 0, limit)
	used := make([]bool, len(hits))

	for len(picked) < limit {
		bestIdx := pickBestMMR(hits, grams, pickedIdx, used, lambda)
		if bestIdx == -1 {
			break
		}
		used[bestIdx] = true
		picked = append(picked, hits[bestIdx])
		pickedIdx = append(pickedIdx, bestIdx)
	}
	return picked
}

// pickBestMMR scans the unused hits and returns the index with the
// highest MMR score (λ·rel − (1−λ)·max_sim). Returns -1 when all hits
// are used.
func pickBestMMR(hits []Hit, grams []map[string]struct{}, pickedIdx []int, used []bool, lambda float64) int {
	bestIdx := -1
	var bestScore float64
	for i, h := range hits {
		if used[i] {
			continue
		}
		mmrScore := lambda*float64(h.FusedScore) - (1.0-lambda)*maxSimAgainst(grams, i, pickedIdx)
		if bestIdx == -1 || mmrScore > bestScore {
			bestIdx = i
			bestScore = mmrScore
		}
	}
	return bestIdx
}

// maxSimAgainst returns the maximum Jaccard similarity between grams[i]
// and any previously-picked candidate. Returns 0 when no picks yet (the
// first MMR iteration has no diversity penalty).
func maxSimAgainst(grams []map[string]struct{}, i int, pickedIdx []int) float64 {
	var maxSim float64
	for _, pi := range pickedIdx {
		s := jaccard(grams[i], grams[pi])
		if s > maxSim {
			maxSim = s
		}
	}
	return maxSim
}

// charNGramSet returns the set of n-character substrings of s after
// lowercasing and collapsing whitespace. Used as a cheap text-similarity
// signal for MMR; deterministic and dependency-free.
func charNGramSet(s string, n int) map[string]struct{} {
	if n <= 0 {
		return nil
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(s), " "))
	if len(normalized) < n {
		out := map[string]struct{}{}
		if normalized != "" {
			out[normalized] = struct{}{}
		}
		return out
	}
	out := make(map[string]struct{}, len(normalized)-n+1)
	for i := 0; i <= len(normalized)-n; i++ {
		out[normalized[i:i+n]] = struct{}{}
	}
	return out
}

// jaccard returns |A ∩ B| / |A ∪ B| ∈ [0, 1]. Empty sets return 0.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var inter int
	smaller, larger := a, b
	if len(b) < len(a) {
		smaller, larger = b, a
	}
	for k := range smaller {
		if _, ok := larger[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
