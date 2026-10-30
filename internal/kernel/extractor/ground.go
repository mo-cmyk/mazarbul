package extractor

import (
	"strings"
	"unicode/utf8"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// defaultGroundingThreshold is the minimum groundingScore a Mention must carry
// to survive the filter pass. Mentions below this threshold are dropped and
// never produce events. Configurable via Config.GroundingThreshold.
const defaultGroundingThreshold = 0.6

// FilterByThreshold returns only the Mentions from ms whose GroundingScore is
// >= threshold. When threshold is <= 0 the defaultGroundingThreshold (0.6) is
// used. Mentions with score == 0 (confabulations) are always excluded.
func FilterByThreshold(ms []ontology.Mention, threshold float64) []ontology.Mention {
	t := threshold
	if t <= 0 {
		t = defaultGroundingThreshold
	}
	out := ms[:0:0]
	for _, m := range ms {
		if m.GroundingScore >= t {
			out = append(out, m)
		}
	}
	return out
}

// Ground locates extractionText inside sourceText and returns a grounded
// Mention with SourceSpan and GroundingScore set. It does not modify any
// other field on m; callers must set Kind, DisplayName, etc. before calling.
//
// Grounding strategy (mirrors LangExtract verbatim-match contract):
//  1. Exact substring match -> score 1.0, span = first occurrence.
//  2. Case-insensitive exact match -> score 0.95, span = first occurrence.
//  3. Levenshtein distance <= 2 over sliding windows of the same length ->
//     score = max(0, 1 - dist/len(extractionText)), span = best-match window.
//  4. No plausible match anywhere -> score 0.0, span = {0,0}. Caller
//     filters; confabulated extractions never produce Mention events.
func Ground(m ontology.Mention, sourceText string) ontology.Mention {
	et := m.ExtractionText
	if et == "" || sourceText == "" {
		m.GroundingScore = 0
		m.SourceSpan = ontology.SourceSpan{}
		return m
	}

	// Pass 1: exact substring.
	if idx := strings.Index(sourceText, et); idx >= 0 {
		m.GroundingScore = 1.0
		m.SourceSpan = ontology.SourceSpan{
			StartChar: idx,
			EndChar:   idx + len(et),
		}
		return m
	}

	// Pass 2: case-insensitive exact match.
	lowerSrc := strings.ToLower(sourceText)
	lowerET := strings.ToLower(et)
	if idx := strings.Index(lowerSrc, lowerET); idx >= 0 {
		m.GroundingScore = 0.95
		m.SourceSpan = ontology.SourceSpan{
			StartChar: idx,
			EndChar:   idx + len(et),
		}
		return m
	}

	// Pass 3: Levenshtein fuzzy match over sliding windows.
	etLen := utf8.RuneCountInString(et)
	srcRunes := []rune(sourceText)
	srcLen := len(srcRunes)

	if etLen == 0 || srcLen < etLen {
		m.GroundingScore = 0
		m.SourceSpan = ontology.SourceSpan{}
		return m
	}

	etRunes := []rune(et)
	bestDist := etLen + 1
	bestStart := -1

	// Slide a window of len(etRunes) across srcRunes. Only consider windows
	// where dist <= 2; anything larger is a confabulation for our purposes.
	for i := 0; i <= srcLen-etLen; i++ {
		window := srcRunes[i : i+etLen]
		d := levenshtein(etRunes, window)
		if d < bestDist {
			bestDist = d
			bestStart = i
		}
		if bestDist == 0 {
			break
		}
	}

	if bestDist > 2 || bestStart < 0 {
		m.GroundingScore = 0
		m.SourceSpan = ontology.SourceSpan{}
		return m
	}

	// Convert rune offset to byte offset for SourceSpan.
	byteStart := runeOffsetToByteOffset(sourceText, bestStart)
	byteEnd := runeOffsetToByteOffset(sourceText, bestStart+etLen)

	score := 1.0 - float64(bestDist)/float64(etLen)
	if score < 0 {
		score = 0
	}
	m.GroundingScore = score
	m.SourceSpan = ontology.SourceSpan{
		StartChar: byteStart,
		EndChar:   byteEnd,
	}
	return m
}

// runeOffsetToByteOffset converts a rune-based index into sourceText into the
// corresponding byte index. Pure-ASCII strings are O(1) due to the fast path;
// multibyte strings scan up to runeIdx runes.
func runeOffsetToByteOffset(s string, runeIdx int) int {
	if runeIdx <= 0 {
		return 0
	}
	byteIdx := 0
	for i := range s {
		if runeIdx == 0 {
			return byteIdx
		}
		byteIdx = i
		runeIdx--
	}
	// runeIdx reached 0 at the last rune; return end of string.
	if runeIdx == 0 {
		return len(s)
	}
	return len(s)
}

// levenshtein returns the edit distance between two rune slices using the
// standard two-row DP algorithm. Both slices are assumed non-empty by the
// caller (Ground enforces this). The implementation is bounded O(len(a)*len(b))
// and allocates two rows; for typical mention lengths (<= 50 chars) this is
// effectively free.
func levenshtein(a, b []rune) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			curr[j] = minInt(del, minInt(ins, sub))
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
