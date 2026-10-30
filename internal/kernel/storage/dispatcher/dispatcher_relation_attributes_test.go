package dispatcher_test

import (
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
)

// TestMarshalAttributes_EmptyMapsCollapseToEmptyObject pins the column
// contract for relations.attributes: nil and empty maps both serialise
// to the literal "{}" so downstream readers (graph lane Cypher, future
// CLI inspectors) see a uniform shape regardless of whether the
// extractor emitted attributes.
func TestMarshalAttributes_EmptyMapsCollapseToEmptyObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input map[string]string
	}{
		{"nil", nil},
		{"empty", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dispatcher.MarshalAttributesForTest(tc.input)
			if got != "{}" {
				t.Errorf("got %q, want %q", got, "{}")
			}
		})
	}
}

// TestMarshalAttributes_PreservesRoleAndTitle covers the happy path:
// the extractor's structured attributes (role, title, employment_type)
// land as a JSON object string ready for SQLite + LadybugDB writes.
func TestMarshalAttributes_PreservesRoleAndTitle(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"role":            "CEO",
		"title":           "Chief Executive Officer",
		"employment_type": "full-time",
	}
	got := dispatcher.MarshalAttributesForTest(in)

	var roundtrip map[string]string
	if err := json.Unmarshal([]byte(got), &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k, want := range in {
		if roundtrip[k] != want {
			t.Errorf("roundtrip[%q] = %q, want %q", k, roundtrip[k], want)
		}
	}
}
