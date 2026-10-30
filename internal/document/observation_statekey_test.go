package document

import (
	"regexp"
	"testing"
)

// segmentPattern mirrors the StateKey validation pattern on ontology.Fact
// (kernel schema): every dot-separated segment must start with a letter.
var segmentPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// TestObservationStateKeySegmentsStartWithLetter guards the schema invariant
// that every StateKey segment matches ^[a-z][a-z0-9_]*. The category segment is
// derived from a bullet's [category] label, which is author-supplied and may
// start with a digit (e.g. an index line like "- [0035 migration ...](...)").
// A digit-leading category must not produce an invalid slot key that crashes
// the whole import batch.
func TestObservationStateKeySegmentsStartWithLetter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		category string
		value    string
	}{
		{"plain category", "Decision", "ship Friday"},
		{"digit-leading category", "0035 migration collision RESOLVED", "was a dup"},
		{"underscore-leading category", "___weird", "x"},
		{"all-stripped category", "!!! ???", "y"},
		{"empty category", "", "z"},
		{"digits only", "123", "w"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := observationStateKey(tc.category, tc.value)
			if !segmentPattern.MatchString(got) {
				t.Errorf("observationStateKey(%q, %q) = %q, which violates segment pattern %s",
					tc.category, tc.value, got, segmentPattern)
			}
		})
	}
}

// TestObservationStateKeyNoSegmentCollision guards that two DISTINCT categories
// which sanitize to the SAME segment never share a StateKey slot when their
// values match. The segment filter is intentionally lossy ("a-b" and "ab" both
// reduce to segment "ab"; "123" and "c123" both reduce to "c123"), so a
// value-only discriminator would silently collapse the pair onto one slot and
// the supersede pipeline would drop one observation. Folding the normalized
// category into the discriminator keeps the slots distinct.
func TestObservationStateKeyNoSegmentCollision(t *testing.T) {
	t.Parallel()
	const v = "same observation text"
	pairs := [][2]string{
		{"a-b", "ab"},   // pre-existing lossy-filter collision
		{"123", "c123"}, // digit-leading (c-prefixed) vs naturally c-prefixed
	}
	for _, p := range pairs {
		if a, b := observationStateKey(p[0], v), observationStateKey(p[1], v); a == b {
			t.Errorf("distinct categories %q and %q collided on one StateKey %q", p[0], p[1], a)
		}
	}
	// Same (category, value) modulo case/whitespace must still map to ONE slot so
	// an identical re-assert stays idempotent.
	if got, want := observationStateKey("  Decision ", v), observationStateKey("decision", v); got != want {
		t.Errorf("case/whitespace split a slot: %q != %q", got, want)
	}
}
