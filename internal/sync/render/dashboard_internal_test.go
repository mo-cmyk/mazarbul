package render

import (
	"strings"
	"testing"
)

func TestMermaidEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Alice Smith", "Alice Smith"},
		{"quote", `The "Boss"`, "The #quot;Boss#quot;"},
		{"pipe", "has|role", "has#124;role"},
		{"percent", "100% sure", "100#37; sure"},
		{"newline", "line1\nline2", "line1<br/>line2"},
		{"crlf dropped", "a\r\nb", "a<br/>b"},
		{"combined", "x\"%\n", "x#quot;#37;<br/>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mermaidEscape(tc.in); got != tc.want {
				t.Errorf("mermaidEscape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCrockfordDecode_AlphabetBoundaries(t *testing.T) {
	t.Parallel()
	// Crockford Base32 excludes I, L, O, U, so the letter runs are not
	// contiguous. The boundaries around the gaps (H/J, K/M, N/P, T/V) are where
	// a naive contiguous-range decoder drifts off by one.
	want := map[byte]byte{
		'0': 0, '9': 9, 'A': 10, 'H': 17,
		'J': 18, 'K': 19, 'M': 20, 'N': 21,
		'P': 22, 'T': 26, 'V': 27, 'Z': 31,
	}
	for c, v := range want {
		if got := crockfordDecode(c); got != v {
			t.Errorf("crockfordDecode(%q) = %d, want %d", c, got, v)
		}
	}
}

func TestYearMonthFromULID_DecodesTimestamp(t *testing.T) {
	t.Parallel()
	// Regression: this ULID's embedded timestamp is 2026-06-22T20:27:15Z. A
	// crockfordDecode that treats J..N as a five-letter run mis-decodes the
	// 'M'/'V' digits and reports 2026-07, drifting the dashboard activity
	// buckets (and the changelog month) a month forward.
	y, m := yearMonthFromULID("01KVRG80MHE9B8BVBB9B0R1PD8")
	if y != 2026 || m != 6 {
		t.Errorf("yearMonthFromULID = %04d-%02d, want 2026-06", y, m)
	}
	// Short input degrades to a fixed sentinel rather than panicking.
	if y, m := yearMonthFromULID("01K"); y != 2000 || m != 1 {
		t.Errorf("short ULID = %04d-%02d, want 2000-01", y, m)
	}
}

func TestSparklineRow(t *testing.T) {
	t.Parallel()
	// Ascending counts must map monotonically up the five-glyph ramp; the max
	// reaches the top glyph and zero stays at the bottom.
	got := sparklineRow([]int{0, 1, 2, 4, 8})
	bars := []rune(got)
	if len(bars) != 5 {
		t.Fatalf("sparkline length = %d, want 5", len(bars))
	}
	if bars[0] != '▁' {
		t.Errorf("zero count glyph = %q, want ▁", string(bars[0]))
	}
	if bars[4] != '▇' {
		t.Errorf("max count glyph = %q, want ▇", string(bars[4]))
	}
	// All-zero series renders as all-lowest (no divide-by-zero).
	if z := sparklineRow([]int{0, 0, 0}); z != "▁▁▁" {
		t.Errorf("all-zero sparkline = %q, want ▁▁▁", z)
	}
}

func TestTopConnectedEntities_DegreeRankUIDTiebreak(t *testing.T) {
	t.Parallel()
	// hub has the highest degree; the two leaves tie on degree 1 and must order
	// by UID ascending.
	state := newGraphState()
	for _, uid := range []string{"hub", "aaa", "bbb"} {
		state.Entities[uid] = &EntityRow{UID: uid, DisplayName: uid}
	}
	state.Relations["r1"] = &RelationRow{UID: "r1", SubjectUID: "hub", Predicate: "knows", ObjectUID: "aaa"}
	state.Relations["r2"] = &RelationRow{UID: "r2", SubjectUID: "hub", Predicate: "knows", ObjectUID: "bbb"}

	degrees := entityDegrees(state)
	top := topConnectedEntities(state, degrees, 10)
	want := []string{"hub", "aaa", "bbb"} // hub deg=2; aaa/bbb deg=1, UID-asc
	if strings.Join(top, ",") != strings.Join(want, ",") {
		t.Errorf("top = %v, want %v", top, want)
	}
}

func TestTopConnectedEntities_ExcludesIsolated(t *testing.T) {
	t.Parallel()
	// An entity with no live edge must not appear (it would be an orphan node).
	state := newGraphState()
	state.Entities["a"] = &EntityRow{UID: "a", DisplayName: "a"}
	state.Entities["b"] = &EntityRow{UID: "b", DisplayName: "b"}
	state.Entities["lonely"] = &EntityRow{UID: "lonely", DisplayName: "lonely"}
	state.Relations["r1"] = &RelationRow{UID: "r1", SubjectUID: "a", Predicate: "knows", ObjectUID: "b"}

	top := topConnectedEntities(state, entityDegrees(state), 10)
	for _, uid := range top {
		if uid == "lonely" {
			t.Errorf("isolated entity must be excluded from the graph, got %v", top)
		}
	}
}

func TestLiveRelationsSorted_SkipsForgottenAndSuperseded(t *testing.T) {
	t.Parallel()
	state := newGraphState()
	state.Relations["live"] = &RelationRow{UID: "live", SubjectUID: "a", ObjectUID: "b"}
	state.Relations["gone"] = &RelationRow{UID: "gone", SubjectUID: "a", ObjectUID: "b", Forgotten: true}
	state.Relations["old"] = &RelationRow{UID: "old", SubjectUID: "a", ObjectUID: "b", SupersededByUID: "live"}

	live := liveRelationsSorted(state)
	if len(live) != 1 || live[0].UID != "live" {
		t.Errorf("live relations = %v, want exactly [live]", live)
	}
}
