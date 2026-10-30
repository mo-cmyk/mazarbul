package privacy_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
)

func ev(source, payload string) kernel.Event {
	return kernel.Event{UID: "01TESTUID0000000000000000", Source: source, Payload: []byte(payload)}
}

// TestIsSensitivePayload_ConnectorSource pins the connector-source seed
// marker: connector:* / connector/* are sensitive; the tool:verb sources
// every current writer emits are not.
func TestIsSensitivePayload_ConnectorSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source string
		want   bool
	}{
		{"connector colon prefix is sensitive", "connector:granola", true},
		{"connector slash prefix is sensitive", "connector/slack", true},
		{"cli source is not sensitive", "cli:remember", false},
		{"mcp source is not sensitive", "mcp:fact", false},
		{"extractor source is not sensitive", "extractor:m1", false},
		{"empty source is not sensitive", "", false},
		{"connector mid-string is not sensitive", "x-connector:foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := privacy.IsSensitivePayload(ev(tc.source, `{}`)); got != tc.want {
				t.Errorf("IsSensitivePayload(source=%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

// TestIsSensitivePayload_ExplicitFlag pins the opt-in payload flag: a
// top-level {"sensitive":true} trips; everything else (including a
// non-object payload) does not.
func TestIsSensitivePayload_ExplicitFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"sensitive true flag trips", `{"sensitive":true}`, true},
		{"sensitive true with siblings trips", `{"sensitive":true,"text":"x"}`, true},
		{"sensitive stringified true trips (fail-closed)", `{"sensitive":"true"}`, true},
		{"sensitive number trips (fail-closed)", `{"sensitive":1}`, true},
		{"sensitive false flag does not trip", `{"sensitive":false}`, false},
		{"sensitive null does not trip", `{"sensitive":null}`, false},
		{"absent flag does not trip", `{"text":"hi"}`, false},
		{"empty payload does not trip", ``, false},
		{"array payload does not trip", `["a","b"]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := privacy.IsSensitivePayload(ev("cli:remember", tc.payload)); got != tc.want {
				t.Errorf("IsSensitivePayload(payload=%q) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// TestMarkerNames_IncludesBuiltinsSorted verifies the two seed markers
// are present and the list is sorted. Tolerates extra names so a
// concurrent RegisterMarker test cannot make it flaky.
func TestMarkerNames_IncludesBuiltinsSorted(t *testing.T) {
	t.Parallel()
	names := privacy.MarkerNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("MarkerNames not sorted: %v", names)
		}
	}
	want := map[string]bool{"source.connector": false, "payload.explicit_flag": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("MarkerNames missing built-in %q; got %v", n, names)
		}
	}
}

// TestRegisterMarker_ExtendsPredicate documents the downstream extension
// contract: register one Marker and IsSensitivePayload reflects it
// immediately, with no change to any gate. Not parallel — it mutates the
// process-wide registry.
func TestRegisterMarker_ExtendsPredicate(t *testing.T) {
	privacy.RegisterMarker("test.extension_marker", func(e kernel.Event) bool {
		return e.Kind == "test_secret_kind"
	})
	if !privacy.IsSensitivePayload(kernel.Event{UID: "01EXT", Kind: "test_secret_kind"}) {
		t.Errorf("after RegisterMarker, IsSensitivePayload(kind=test_secret_kind) = false, want true")
	}
	found := false
	for _, n := range privacy.MarkerNames() {
		if n == "test.extension_marker" {
			found = true
		}
	}
	if !found {
		t.Errorf("MarkerNames missing the registered marker; got %v", privacy.MarkerNames())
	}
}

// TestRegisterMarker_NilPanics pins the fail-fast contract: a nil Marker
// is a programmer error that must surface at registration, never weaken
// the gate silently.
func TestRegisterMarker_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("RegisterMarker(nil) did not panic")
		}
	}()
	privacy.RegisterMarker("test.nil", nil)
}
