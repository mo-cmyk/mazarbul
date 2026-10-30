package extractor

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestNeedsSecondPass_AllWellGrounded(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{GroundingScore: 1.0},
		{GroundingScore: 0.98},
	}
	cs := []ontology.RelationCandidate{{Predicate: "works_at"}}
	if needsSecondPass(ms, cs) {
		t.Error("expected no second pass when all mentions are well-grounded")
	}
}

func TestNeedsSecondPass_LowGroundingScore(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{GroundingScore: 1.0},
		{GroundingScore: 0.7}, // below 0.95
	}
	cs := []ontology.RelationCandidate{{Predicate: "works_at"}}
	if !needsSecondPass(ms, cs) {
		t.Error("expected second pass when a mention scores below 0.95")
	}
}

func TestNeedsSecondPass_LowRelationRatio(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{GroundingScore: 1.0},
		{GroundingScore: 1.0},
		{GroundingScore: 1.0},
	}
	// 0 relations for 3 mentions: ratio = 0.0, well below 0.3
	var cs []ontology.RelationCandidate
	if !needsSecondPass(ms, cs) {
		t.Error("expected second pass when relation-to-mention ratio is below 0.3")
	}
}

func TestNeedsSecondPass_EmptyMentions(t *testing.T) {
	t.Parallel()
	if needsSecondPass(nil, nil) {
		t.Error("expected false for empty mention list")
	}
}

func TestAllWellGrounded_True(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{GroundingScore: 1.0},
		{GroundingScore: 0.97},
	}
	if !allWellGrounded(ms) {
		t.Error("expected allWellGrounded true")
	}
}

func TestAllWellGrounded_False(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{GroundingScore: 1.0},
		{GroundingScore: 0.8},
	}
	if allWellGrounded(ms) {
		t.Error("expected allWellGrounded false when any score < 0.95")
	}
}

func TestAllWellGrounded_Empty(t *testing.T) {
	t.Parallel()
	if !allWellGrounded(nil) {
		t.Error("empty slice should be considered well-grounded (vacuously true)")
	}
}

func TestLowConfidenceKinds(t *testing.T) {
	t.Parallel()
	ms := []ontology.Mention{
		{Kind: ontology.EntityKindPerson, GroundingScore: 1.0},
		{Kind: ontology.EntityKindOrganization, GroundingScore: 0.7},
		{Kind: ontology.EntityKindPlace, GroundingScore: 0.8},
	}
	kinds := lowConfidenceKinds(ms)
	if len(kinds) != 2 {
		t.Fatalf("expected 2 low-confidence kinds, got %d: %v", len(kinds), kinds)
	}
	kindSet := make(map[ontology.EntityKind]bool)
	for _, k := range kinds {
		kindSet[k] = true
	}
	if !kindSet[ontology.EntityKindOrganization] {
		t.Error("expected organization in low-confidence kinds")
	}
	if !kindSet[ontology.EntityKindPlace] {
		t.Error("expected place in low-confidence kinds")
	}
}

func TestMergePasses_DeduplicatesByOverlappingSpan(t *testing.T) {
	t.Parallel()
	ms1 := []ontology.Mention{
		{
			DisplayName:    "Frodo",
			Kind:           ontology.EntityKindPerson,
			GroundingScore: 0.9,
			SourceSpan:     ontology.SourceSpan{StartChar: 9, EndChar: 16},
		},
	}
	cs1 := []ontology.RelationCandidate{
		{SubjectMentionIdx: 0, Predicate: "works_at", ObjectMentionIdx: 0},
	}
	// Pass 2 covers the same span with a slightly longer surface form and a
	// higher grounding score; the longer variant must replace pass1's.
	ms2 := []ontology.Mention{
		{
			DisplayName:    "Frodo Baggins",
			Kind:           ontology.EntityKindPerson,
			GroundingScore: 1.0,
			SourceSpan:     ontology.SourceSpan{StartChar: 9, EndChar: 24},
		},
	}
	cs2 := []ontology.RelationCandidate{}

	merged, cands := mergePasses(ms1, cs1, ms2, cs2)
	if len(merged) != 1 {
		t.Fatalf("got %d merged mentions, want 1 (overlapping spans collapse)", len(merged))
	}
	if merged[0].DisplayName != "Frodo Baggins" {
		t.Errorf("got displayName %q, want %q (higher-grounded variant wins)", merged[0].DisplayName, "Frodo Baggins")
	}
	if merged[0].GroundingScore != 1.0 {
		t.Errorf("expected higher score to win, got %v", merged[0].GroundingScore)
	}
	if len(cands) != 1 {
		t.Errorf("expected 1 candidate preserved, got %d", len(cands))
	}
}

func TestMergePasses_DistinctSpansStayDistinct(t *testing.T) {
	t.Parallel()
	ms1 := []ontology.Mention{
		{
			DisplayName:    "Frodo",
			Kind:           ontology.EntityKindPerson,
			GroundingScore: 1.0,
			SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 7},
		},
	}
	ms2 := []ontology.Mention{
		{
			DisplayName:    "Rivendell",
			Kind:           ontology.EntityKindOrganization,
			GroundingScore: 1.0,
			SourceSpan:     ontology.SourceSpan{StartChar: 20, EndChar: 30},
		},
	}
	cs2 := []ontology.RelationCandidate{
		{SubjectMentionIdx: 0, Predicate: "located_in", ObjectMentionIdx: 0},
	}

	merged, _ := mergePasses(ms1, nil, ms2, cs2)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged mentions (Frodo + Rivendell), got %d", len(merged))
	}
}

func TestMergePasses_SameSpanDifferentKindStaysDistinct(t *testing.T) {
	t.Parallel()
	// "Berlin" the city vs. "Berlin" the person: same span, different kind,
	// must remain two distinct mentions.
	ms1 := []ontology.Mention{
		{
			DisplayName:    "Berlin",
			Kind:           ontology.EntityKindPlace,
			GroundingScore: 1.0,
			SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 6},
		},
	}
	ms2 := []ontology.Mention{
		{
			DisplayName:    "Berlin",
			Kind:           ontology.EntityKindPerson,
			GroundingScore: 1.0,
			SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 6},
		},
	}
	merged, _ := mergePasses(ms1, nil, ms2, nil)
	if len(merged) != 2 {
		t.Fatalf("expected 2 mentions (different kinds), got %d", len(merged))
	}
}

func TestMergePasses_ZeroSpansNeverOverlap(t *testing.T) {
	t.Parallel()
	// Two unlocated mentions (zero spans) must not collapse: each is a
	// separate ungrounded extraction the downstream filter has to deal with.
	ms1 := []ontology.Mention{
		{DisplayName: "Frodo", Kind: ontology.EntityKindPerson, GroundingScore: 0.5},
	}
	ms2 := []ontology.Mention{
		{DisplayName: "Frodo", Kind: ontology.EntityKindPerson, GroundingScore: 0.5},
	}
	merged, _ := mergePasses(ms1, nil, ms2, nil)
	if len(merged) != 2 {
		t.Errorf("zero-span mentions collapsed; got %d, want 2", len(merged))
	}
}

func TestSpansOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b ontology.SourceSpan
		want float64
	}{
		{"identical", ontology.SourceSpan{StartChar: 0, EndChar: 10}, ontology.SourceSpan{StartChar: 0, EndChar: 10}, 1.0},
		{"fully contained: smaller inside larger", ontology.SourceSpan{StartChar: 0, EndChar: 10}, ontology.SourceSpan{StartChar: 0, EndChar: 5}, 1.0},
		{"adjacent no overlap", ontology.SourceSpan{StartChar: 0, EndChar: 5}, ontology.SourceSpan{StartChar: 5, EndChar: 10}, 0},
		{"disjoint", ontology.SourceSpan{StartChar: 0, EndChar: 5}, ontology.SourceSpan{StartChar: 10, EndChar: 15}, 0},
		{"zero-width a", ontology.SourceSpan{StartChar: 5, EndChar: 5}, ontology.SourceSpan{StartChar: 0, EndChar: 10}, 0},
		{"zero-width b", ontology.SourceSpan{StartChar: 0, EndChar: 10}, ontology.SourceSpan{StartChar: 5, EndChar: 5}, 0},
		{"half-overlap: 5 of 10-min = 0.5", ontology.SourceSpan{StartChar: 0, EndChar: 10}, ontology.SourceSpan{StartChar: 5, EndChar: 15}, 0.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := spansOverlap(tc.a, tc.b)
			if absDelta(got, tc.want) > 1e-9 {
				t.Errorf("spansOverlap(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func absDelta(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
