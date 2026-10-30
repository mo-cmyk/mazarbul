package extractor

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestChunkText_ShortText(t *testing.T) {
	t.Parallel()
	text := "Short episode."
	chunks := ChunkText(text, 6000, 200)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for short text, got %d", len(chunks))
	}
	if chunks[0].Text != text {
		t.Errorf("chunk text mismatch")
	}
	if chunks[0].ByteOffset != 0 {
		t.Errorf("expected byteOffset 0, got %d", chunks[0].ByteOffset)
	}
}

func TestChunkText_LongText_ProducesMultipleChunks(t *testing.T) {
	t.Parallel()
	// Build a text clearly over 6000 bytes.
	para := strings.Repeat("This is a sentence about nothing in particular. ", 200)
	chunks := ChunkText(para, 6000, 200)
	if len(chunks) < 2 {
		t.Fatalf("expected >= 2 chunks for long text (%d bytes), got %d", len(para), len(chunks))
	}
}

func TestChunkText_OverlapPreservesContent(t *testing.T) {
	t.Parallel()
	// Build text where two consecutive chunks should share a window.
	para := strings.Repeat("Hello world. ", 600) // ~7800 bytes
	chunks := ChunkText(para, 6000, 200)
	if len(chunks) < 2 {
		t.Skip("expected multiple chunks")
	}
	// The end of chunk[0] and the start of chunk[1] should overlap.
	end0 := chunks[0].ByteOffset + len(chunks[0].Text)
	start1 := chunks[1].ByteOffset
	if start1 >= end0 {
		t.Errorf("expected overlap: chunk[1] starts at %d but chunk[0] ends at %d (no overlap)", start1, end0)
	}
}

func TestChunkText_OffsetMonotonic(t *testing.T) {
	t.Parallel()
	para := strings.Repeat("Sentence here. ", 700)
	chunks := ChunkText(para, 6000, 200)
	for i := 1; i < len(chunks); i++ {
		if chunks[i].ByteOffset <= chunks[i-1].ByteOffset {
			t.Errorf("chunk[%d].ByteOffset %d <= chunk[%d].ByteOffset %d", i, chunks[i].ByteOffset, i-1, chunks[i-1].ByteOffset)
		}
	}
}

func TestAdjustSpans(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{SourceSpan: ontology.SourceSpan{StartChar: 5, EndChar: 10}},
		{SourceSpan: ontology.SourceSpan{StartChar: 20, EndChar: 25}},
	}
	got := AdjustSpans(ms, 100)
	if got[0].SourceSpan.StartChar != 105 || got[0].SourceSpan.EndChar != 110 {
		t.Errorf("unexpected span after adjust: %+v", got[0].SourceSpan)
	}
	if got[1].SourceSpan.StartChar != 120 || got[1].SourceSpan.EndChar != 125 {
		t.Errorf("unexpected span after adjust: %+v", got[1].SourceSpan)
	}
}

func TestAdjustSpans_ZeroOffset(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{SourceSpan: ontology.SourceSpan{StartChar: 3, EndChar: 7}},
	}
	got := AdjustSpans(ms, 0)
	if got[0].SourceSpan.StartChar != 3 {
		t.Errorf("zero offset should not change span")
	}
}

func TestDeduplicateMentions(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{DisplayName: "Frodo", Kind: ontology.EntityKindPerson, SourceSpan: ontology.SourceSpan{StartChar: 0, EndChar: 7}, GroundingScore: 0.9},
		{DisplayName: "Frodo", Kind: ontology.EntityKindPerson, SourceSpan: ontology.SourceSpan{StartChar: 0, EndChar: 7}, GroundingScore: 1.0},
		{DisplayName: "Rivendell", Kind: ontology.EntityKindOrganization, SourceSpan: ontology.SourceSpan{StartChar: 20, EndChar: 30}, GroundingScore: 1.0},
	}
	got := DeduplicateMentions(ms)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique mentions, got %d", len(got))
	}
	// The higher-scored Frodo should win.
	if got[0].GroundingScore != 1.0 {
		t.Errorf("expected higher-scored mention to be kept, got %v", got[0].GroundingScore)
	}
}
