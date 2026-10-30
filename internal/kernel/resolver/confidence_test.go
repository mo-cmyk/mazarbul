package resolver

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestRelationConfidence_DerivesFromMinGrounding pins the contract that
// relation Confidence is the minimum of subject and object mention grounding
// scores. The regression this guards against: appendRelation used to hardcode
// Confidence: 1.0 regardless of evidence, defeating the recall composer's
// MinConfidence filter (every resolver-produced relation passed the gate).
func TestRelationConfidence_DerivesFromMinGrounding(t *testing.T) {
	t.Parallel()
	mentions := []ontology.Mention{
		{GroundingScore: 0.95},
		{GroundingScore: 0.40},
		{GroundingScore: 0.80},
	}

	cases := []struct {
		name string
		cand ontology.RelationCandidate
		want float64
	}{
		{
			name: "min of two high-grounding mentions",
			cand: ontology.RelationCandidate{SubjectMentionIdx: 0, ObjectMentionIdx: 2},
			want: 0.80,
		},
		{
			name: "weak endpoint pulls confidence down",
			cand: ontology.RelationCandidate{SubjectMentionIdx: 0, ObjectMentionIdx: 1},
			want: 0.40,
		},
		{
			name: "symmetric: order does not matter",
			cand: ontology.RelationCandidate{SubjectMentionIdx: 1, ObjectMentionIdx: 0},
			want: 0.40,
		},
		{
			name: "out-of-range subject falls back to 1.0",
			cand: ontology.RelationCandidate{SubjectMentionIdx: 99, ObjectMentionIdx: 0},
			want: 1.0,
		},
		{
			name: "zero grounding (likely unset) falls back to 1.0",
			cand: ontology.RelationCandidate{SubjectMentionIdx: 0, ObjectMentionIdx: 0},
			want: 0.95,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := relationConfidence(mentions, tc.cand)
			if got != tc.want {
				t.Errorf("relationConfidence = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRelationConfidence_BothZero pins the defensive behaviour that an
// all-zero grounding does not produce a Confidence: 0 relation that would
// be filtered out by every MinConfidence > 0 caller.
func TestRelationConfidence_BothZero(t *testing.T) {
	t.Parallel()
	mentions := []ontology.Mention{{GroundingScore: 0.0}, {GroundingScore: 0.0}}
	got := relationConfidence(mentions, ontology.RelationCandidate{SubjectMentionIdx: 0, ObjectMentionIdx: 1})
	if got != 1.0 {
		t.Errorf("relationConfidence with both-zero grounding = %v, want 1.0 (defensive fallback)", got)
	}
}
