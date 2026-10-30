package plugin_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/plugin"
)

// TestCheckAPICompatibility exercises the install-time host-API gate across the
// opt-out, exact-match, normalised (no leading v), incompatible-major,
// host-too-old, and malformed cases.
//
// The table is written against HostAPIVersion = "v1.0.0"; the guard below fails
// loudly if that constant is bumped so the cases get revisited alongside it.
func TestCheckAPICompatibility(t *testing.T) {
	t.Parallel()

	if plugin.HostAPIVersion != "v1.0.0" {
		t.Fatalf("HostAPIVersion changed to %q; revisit the compatibility cases in this test", plugin.HostAPIVersion)
	}

	cases := []struct {
		name       string
		apiVersion string
		wantErr    error // nil, ErrInvalidAPIVersion, or ErrIncompatibleAPIVersion
	}{
		{"empty opts out of the gate", "", nil},
		{"exact match", "v1.0.0", nil},
		{"normalises a missing leading v", "1.0.0", nil},
		{"older minor than host is too old", "v1.1.0", plugin.ErrIncompatibleAPIVersion},
		{"newer major", "v2.0.0", plugin.ErrIncompatibleAPIVersion},
		{"older major", "v0.9.0", plugin.ErrIncompatibleAPIVersion},
		{"malformed string", "not-a-version", plugin.ErrInvalidAPIVersion},
		{"four-component is not semver", "1.2.3.4", plugin.ErrInvalidAPIVersion},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := plugin.CheckAPICompatibility(tc.apiVersion)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("CheckAPICompatibility(%q) = %v, want nil", tc.apiVersion, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CheckAPICompatibility(%q) = %v, want sentinel %v", tc.apiVersion, err, tc.wantErr)
			}
		})
	}
}

// TestCheckAPICompatibilityMessageNamesHostAndRequired verifies an incompatible
// version produces a message that names BOTH the required and the host version,
// so the install flow can surface a self-explanatory refusal.
func TestCheckAPICompatibilityMessageNamesHostAndRequired(t *testing.T) {
	t.Parallel()
	err := plugin.CheckAPICompatibility("v2.0.0")
	if err == nil {
		t.Fatal("expected an incompatibility error for v2.0.0")
	}
	msg := err.Error()
	for _, want := range []string{"v2.0.0", plugin.HostAPIVersion} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should mention %q", msg, want)
		}
	}
}
