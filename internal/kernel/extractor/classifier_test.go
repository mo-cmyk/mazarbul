package extractor

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestClassifyEpisode_Heuristic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ep   ontology.Episode
		want ontology.EpisodeClass
		why  string
	}{
		{
			name: "self_statement_promoted_to_semantic",
			ep:   ontology.Episode{Text: "I live in Berlin and work as a designer.", Actor: ontology.ActorUser},
			want: ontology.ClassSemantic,
			why:  "First-person 'I live' clause is the canonical semantic signal.",
		},
		{
			name: "my_name_is_promoted_to_semantic",
			ep:   ontology.Episode{Text: "My name is Gandalf.", Actor: ontology.ActorUser},
			want: ontology.ClassSemantic,
			why:  "'My name is' is the strongest possible self-statement.",
		},
		{
			name: "preference_promoted_to_semantic",
			ep:   ontology.Episode{Text: "I really love oat milk in my coffee.", Actor: ontology.ActorUser},
			want: ontology.ClassSemantic,
			why:  "Preferences are context-stable facts; mem0 v3 classifies them semantic.",
		},
		{
			name: "assistant_numbered_trace_promoted_to_procedural",
			ep: ontology.Episode{
				Text: "1. Open the file you want to edit.\n" +
					"2. Find the relevant section near the imports.\n" +
					"3. Replace the import with the new alias.\n" +
					"4. Save and re-run the tests.",
				Actor: ontology.ActorAssistant,
			},
			want: ontology.ClassProcedural,
			why:  "Three or more consecutive numbered lines from the assistant is a procedural trace.",
		},
		{
			name: "assistant_short_numbered_list_below_streak_threshold",
			ep: ontology.Episode{
				Text:  "1. Open the file.\n2. Edit the line.",
				Actor: ontology.ActorAssistant,
			},
			want: ontology.ClassEpisodic,
			why:  "Two-step lists do not clear the streak threshold; default to episodic.",
		},
		{
			name: "user_numbered_list_stays_episodic",
			ep: ontology.Episode{
				Text: "1. Picked up the kids.\n" +
					"2. Stopped for groceries.\n" +
					"3. Cooked dinner together.",
				Actor: ontology.ActorUser,
			},
			want: ontology.ClassEpisodic,
			why:  "User-actor numbered narratives are session memories, not procedures.",
		},
		{
			name: "session_turn_stays_episodic",
			ep:   ontology.Episode{Text: "We went to lunch and discussed the project roadmap.", Actor: ontology.ActorUser},
			want: ontology.ClassEpisodic,
			why:  "Third-person narration with no self-statement is episodic chatter.",
		},
		{
			name: "empty_text_defaults_to_episodic",
			ep:   ontology.Episode{Text: "", Actor: ontology.ActorUser},
			want: ontology.ClassEpisodic,
			why:  "Empty text has no signal; default matches the storage column default.",
		},
		{
			name: "whitespace_only_defaults_to_episodic",
			ep:   ontology.Episode{Text: "   \n  \t ", Actor: ontology.ActorUser},
			want: ontology.ClassEpisodic,
			why:  "Whitespace-only text trims to empty; same fallback as the empty case.",
		},
		{
			name: "single_step_reference_does_not_trigger_procedural",
			ep: ontology.Episode{
				Text:  "Followed step 4 of the recipe and the bread came out great.",
				Actor: ontology.ActorAssistant,
			},
			want: ontology.ClassEpisodic,
			why:  "One-off 'step 4' reference with no first-person clause is plain episodic narration.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyEpisode(tc.ep)
			if got != tc.want {
				t.Fatalf("ClassifyEpisode(%q, actor=%s) = %s, want %s (%s)", tc.ep.Text, tc.ep.Actor, got, tc.want, tc.why)
			}
		})
	}
}
