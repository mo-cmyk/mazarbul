package extractor

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestGround_ExactVerbatimMatch(t *testing.T) {
	t.Parallel()
	source := "Met with Frodo yesterday."
	m := ontology.Mention{ExtractionText: "Frodo"}
	got := Ground(m, source)
	if got.GroundingScore != 1.0 {
		t.Errorf("expected groundingScore 1.0, got %v", got.GroundingScore)
	}
	if got.SourceSpan.StartChar != 9 || got.SourceSpan.EndChar != 14 {
		t.Errorf("expected span [9,14), got [%d,%d)", got.SourceSpan.StartChar, got.SourceSpan.EndChar)
	}
}

func TestGround_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()
	source := "met with frodo yesterday."
	m := ontology.Mention{ExtractionText: "Frodo"}
	got := Ground(m, source)
	if got.GroundingScore < 0.9 {
		t.Errorf("expected groundingScore >= 0.9 for case-insensitive match, got %v", got.GroundingScore)
	}
	if got.SourceSpan.StartChar != 9 {
		t.Errorf("expected span start 9, got %d", got.SourceSpan.StartChar)
	}
}

func TestGround_OneCharTypo(t *testing.T) {
	t.Parallel()
	source := "Met with Frodo yesterday."
	// "Frodi" has edit distance 1 from "Frodo"
	m := ontology.Mention{ExtractionText: "Frodi"}
	got := Ground(m, source)
	if got.GroundingScore <= 0 {
		t.Errorf("expected fuzzy match score > 0 for one-char typo, got %v", got.GroundingScore)
	}
	if got.GroundingScore >= 1.0 {
		t.Errorf("expected score < 1.0 for non-exact match, got %v", got.GroundingScore)
	}
}

func TestGround_Confabulation(t *testing.T) {
	t.Parallel()
	source := "The quarterly report looks good."
	m := ontology.Mention{ExtractionText: "Frodo Baggins"}
	got := Ground(m, source)
	if got.GroundingScore != 0 {
		t.Errorf("expected groundingScore 0 for confabulation, got %v", got.GroundingScore)
	}
	if got.SourceSpan.StartChar != 0 || got.SourceSpan.EndChar != 0 {
		t.Errorf("expected zero span for confabulation, got [%d,%d)", got.SourceSpan.StartChar, got.SourceSpan.EndChar)
	}
}

func TestGround_MultipleOccurrences_FirstPicked(t *testing.T) {
	t.Parallel()
	source := "Ana spoke to Ana about the project."
	m := ontology.Mention{ExtractionText: "Ana"}
	got := Ground(m, source)
	if got.GroundingScore != 1.0 {
		t.Errorf("expected score 1.0, got %v", got.GroundingScore)
	}
	// First occurrence of "Ana" is at index 0.
	if got.SourceSpan.StartChar != 0 {
		t.Errorf("expected first occurrence at 0, got %d", got.SourceSpan.StartChar)
	}
}

func TestGround_EmptyExtractionText(t *testing.T) {
	t.Parallel()
	m := ontology.Mention{ExtractionText: ""}
	got := Ground(m, "some source text")
	if got.GroundingScore != 0 {
		t.Errorf("expected score 0 for empty extractionText, got %v", got.GroundingScore)
	}
}

func TestGround_EmptySource(t *testing.T) {
	t.Parallel()
	m := ontology.Mention{ExtractionText: "Frodo"}
	got := Ground(m, "")
	if got.GroundingScore != 0 {
		t.Errorf("expected score 0 for empty source, got %v", got.GroundingScore)
	}
}

func TestFilterByThreshold(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{ExtractionText: "a", GroundingScore: 1.0},
		{ExtractionText: "b", GroundingScore: 0.8},
		{ExtractionText: "c", GroundingScore: 0.5},
		{ExtractionText: "d", GroundingScore: 0.0},
	}
	got := FilterByThreshold(ms, 0.6)
	if len(got) != 2 {
		t.Errorf("expected 2 mentions above 0.6, got %d", len(got))
	}
	for _, m := range got {
		if m.GroundingScore < 0.6 {
			t.Errorf("mention %q has score %v below threshold", m.ExtractionText, m.GroundingScore)
		}
	}
}

func TestFilterByThreshold_DefaultUsedWhenZero(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{ExtractionText: "a", GroundingScore: 0.7},
		{ExtractionText: "b", GroundingScore: 0.5},
	}
	// threshold=0 should use default (0.6)
	got := FilterByThreshold(ms, 0)
	if len(got) != 1 {
		t.Errorf("expected 1 mention above default threshold 0.6, got %d", len(got))
	}
}

func TestLevenshtein(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"kitten", "sitting", 3},
		{"Frodo", "Frodi", 1},
	}
	for _, tc := range tests {
		got := levenshtein([]rune(tc.a), []rune(tc.b))
		if got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
