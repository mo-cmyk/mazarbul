package extractor

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestDetectSelfStatements covers the lexical patterns the self-
// statement detector recognises. Each case is grounded in the user-
// reported failure mode: "What is my name?" / "Where do I live?" /
// "Where do I work now?" — questions the existing extractor returns
// zero relation candidates for because its GROUNDING CONTRACT
// excludes first-person speech.
//
// The detector receives the episode text plus the grounded mentions
// the LLM extractor produced for that episode, and emits synthetic
// RelationCandidates with subject="self" (a sentinel the resolver
// pipeline maps to the canonical self entity UID).
func TestDetectSelfStatements(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		text     string
		mentions []ontology.Mention
		want     []syntheticSelfCandidate
	}{
		{
			name: "name statement",
			text: "I changed my name to Lucas yesterday.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindPerson, DisplayName: "Lucas"},
			},
			want: []syntheticSelfCandidate{
				{ObjectMentionIdx: 0, Predicate: "has_name"},
			},
		},
		{
			name: "lives_in",
			text: "I live in Berlin and love it.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindPlace, DisplayName: "Berlin"},
			},
			want: []syntheticSelfCandidate{
				{ObjectMentionIdx: 0, Predicate: "lives_in"},
			},
		},
		{
			name: "works_at",
			text: "I work at Rivendell now.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindOrganization, DisplayName: "Rivendell"},
			},
			want: []syntheticSelfCandidate{
				{ObjectMentionIdx: 0, Predicate: "works_at"},
			},
		},
		{
			name: "joined Org",
			text: "I joined Erebor Inc. last month.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindOrganization, DisplayName: "Erebor Inc."},
			},
			want: []syntheticSelfCandidate{
				{ObjectMentionIdx: 0, Predicate: "works_at"},
			},
		},
		{
			name: "my name is",
			text: "My name is Arwen.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindPerson, DisplayName: "Arwen"},
			},
			want: []syntheticSelfCandidate{
				{ObjectMentionIdx: 0, Predicate: "has_name"},
			},
		},
		{
			name: "no first-person",
			text: "Frodo Baggins works at Erebor Inc.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindPerson, DisplayName: "Frodo Baggins"},
				{Kind: ontology.EntityKindOrganization, DisplayName: "Erebor Inc."},
			},
			want: nil,
		},
		{
			name: "name pattern but object kind wrong (place can't be name)",
			text: "My name is Berlin.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindPlace, DisplayName: "Berlin"},
			},
			want: nil,
		},
		{
			name: "lives_in pattern but no place mention",
			text: "I live in Berlin and love it.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindPerson, DisplayName: "Berlin"},
			},
			want: nil,
		},
		{
			name: "case-insensitive trigger",
			text: "i'm from PARIS originally.",
			mentions: []ontology.Mention{
				{Kind: ontology.EntityKindPlace, DisplayName: "Paris"},
			},
			want: []syntheticSelfCandidate{
				{ObjectMentionIdx: 0, Predicate: "lives_in"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := detectSelfStatements(tc.text, tc.mentions)
			if len(got) != len(tc.want) {
				t.Fatalf("detectSelfStatements(%q) = %d candidates, want %d (got: %+v)",
					tc.text, len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].ObjectMentionIdx != tc.want[i].ObjectMentionIdx ||
					got[i].Predicate != tc.want[i].Predicate {
					t.Errorf("cand[%d]=%+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
