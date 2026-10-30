package extractor

import (
	"strings"
	"unicode/utf8"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// defaultChunkThreshold is the byte length above which an episode is split
// into overlapping chunks before extraction. 6000 bytes leaves comfortable
// room inside a typical 8k-token context window after system prompt + few-shot
// examples (~2000 tokens).
const defaultChunkThreshold = 6000

// defaultChunkOverlap is the byte length of the overlap appended to each
// chunk from the start of the next chunk. Overlap preserves cross-boundary
// mentions so a span that straddles the cut point appears fully in at least
// one chunk.
const defaultChunkOverlap = 200

// Chunk splits text into overlapping chunks if it exceeds threshold bytes.
// Chunks break at sentence boundaries ('. ', '! ', '? ', '\n') when possible.
// Each chunk carries a byteOffset that maps chunk-relative SourceSpan values
// back to global episode coordinates. If text is short enough it is returned
// as a single chunk with byteOffset 0.
type Chunk struct {
	Text       string
	ByteOffset int // inclusive start in the original episode text
}

// ChunkText splits text into overlapping chunks. Threshold and overlap use the
// package defaults when <= 0.
func ChunkText(text string, threshold, overlap int) []Chunk {
	if threshold <= 0 {
		threshold = defaultChunkThreshold
	}
	if overlap <= 0 {
		overlap = defaultChunkOverlap
	}
	if len(text) <= threshold {
		return []Chunk{{Text: text, ByteOffset: 0}}
	}
	return splitIntoChunks(text, threshold, overlap)
}

// splitIntoChunks performs the actual chunking. It advances a start pointer
// through text in steps of approximately (threshold - overlap) bytes, snapping
// each boundary to the nearest sentence terminator ('. ', '! ', '? ', '\n')
// within a 100-byte look-ahead window so spans are not split mid-sentence.
func splitIntoChunks(text string, threshold, overlap int) []Chunk {
	var chunks []Chunk
	start := 0
	textLen := len(text)

	for start < textLen {
		end := start + threshold
		if end >= textLen {
			chunks = append(chunks, Chunk{Text: text[start:], ByteOffset: start})
			break
		}

		// Snap end to a sentence boundary within a look-ahead window.
		snapEnd := snapToSentenceBoundary(text, end, 100)
		chunks = append(chunks, Chunk{Text: text[start:snapEnd], ByteOffset: start})

		// Advance start by (threshold - overlap), snapped to a boundary.
		nextStart := start + threshold - overlap
		if nextStart <= start {
			nextStart = start + 1 // guard against infinite loop on degenerate input
		}
		nextStart = snapToSentenceBoundary(text, nextStart, 100)
		if nextStart <= start {
			nextStart = start + 1
		}
		start = nextStart
	}
	return chunks
}

// snapToSentenceBoundary looks for a sentence-ending sequence ('. ', '! ',
// '? ', or '\n') within [pos, pos+window) and returns the byte position
// immediately after the first such terminator. If no terminator is found it
// returns pos unchanged so the caller falls back to a hard cut.
func snapToSentenceBoundary(text string, pos, window int) int {
	end := pos + window
	if end > len(text) {
		end = len(text)
	}
	sub := text[pos:end]
	for _, sep := range []string{". ", "! ", "? ", "\n"} {
		if i := strings.Index(sub, sep); i >= 0 {
			candidate := pos + i + len(sep)
			// Ensure we landed on a valid UTF-8 boundary.
			if candidate <= len(text) && utf8.Valid([]byte(text[pos:candidate])) {
				return candidate
			}
		}
	}
	return pos
}

// AdjustSpans translates the SourceSpan values on each mention from
// chunk-relative byte offsets back to global episode offsets by adding
// chunk.ByteOffset. It returns a new slice; the input is not modified.
func AdjustSpans(ms []ontology.Mention, byteOffset int) []ontology.Mention {
	if byteOffset == 0 {
		return ms
	}
	out := make([]ontology.Mention, len(ms))
	for i, m := range ms {
		m.SourceSpan.StartChar += byteOffset
		m.SourceSpan.EndChar += byteOffset
		out[i] = m
	}
	return out
}

// DeduplicateMentions removes duplicate Mentions from a merged multi-chunk
// result. Two Mentions are considered duplicates when they share the same
// (DisplayName, Kind). Span offsets are intentionally excluded from the key
// because AdjustSpans shifts spans by each chunk's ByteOffset, so the same
// entity extracted from two overlapping chunks lands at different global
// positions. In a tie the Mention with the higher GroundingScore is kept.
// Order is preserved for the first occurrence of each unique key.
func DeduplicateMentions(ms []ontology.Mention) []ontology.Mention {
	type key struct {
		displayName string
		kind        ontology.EntityKind
	}
	seen := make(map[key]int) // key -> index in out
	out := ms[:0:0]
	for _, m := range ms {
		k := key{
			displayName: m.DisplayName,
			kind:        m.Kind,
		}
		if idx, ok := seen[k]; ok {
			if m.GroundingScore > out[idx].GroundingScore {
				out[idx] = m
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, m)
	}
	return out
}
