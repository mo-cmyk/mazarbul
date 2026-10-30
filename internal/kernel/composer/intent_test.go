package composer

import "testing"

// TestClassifyIntent covers the lexical bucketing rules. Each case is
// an example from the user-reported failure modes (TEST 5/6) plus the
// canonical Agnostic fallback. Tests pin behaviour so future prompt
// rewrites don't silently shift the routing.
func TestClassifyIntent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		q    string
		want TemporalIntent
	}{
		// Current — "now / latest / today" patterns
		{"What is my name currently?", IntentCurrent},
		{"Where do I live now?", IntentCurrent},
		{"What is my latest job?", IntentCurrent},
		{"Where do I work today?", IntentCurrent},

		// Historical — origin / original phrasing
		{"What was my original name?", IntentHistorical},
		{"Historically where have I lived?", IntentHistorical},
		{"What did I do previously?", IntentHistorical},
		{"What name did I use first?", IntentHistorical},

		// Chronological — sorted-list questions
		{"List my names chronologically", IntentChronological},
		{"My names in chronological order", IntentChronological},
		{"List my names in order from oldest to newest", IntentChronological},
		{"Show me my jobs over time", IntentChronological},

		// Duration — date-arithmetic questions
		{"How long ago did I work at Erebor?", IntentDuration},
		{"How many weeks ago did I move?", IntentDuration},
		{"How many days have passed since I started?", IntentDuration},
		{"When did I last change my name?", IntentDuration},

		// Agnostic — no temporal signal
		{"Who is my manager?", IntentAgnostic},
		{"What is the capital of France?", IntentAgnostic},
		{"", IntentAgnostic},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			t.Parallel()
			got := ClassifyIntent(tc.q)
			if got != tc.want {
				t.Errorf("ClassifyIntent(%q) = %v, want %v", tc.q, got, tc.want)
			}
		})
	}
}
