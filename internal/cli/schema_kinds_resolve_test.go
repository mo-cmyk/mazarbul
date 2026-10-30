package cli

// schema_kinds_resolve_test.go pins the kind-name resolution contract shared by
// the schema-kinds authoring surface. The split is load-bearing:
//
//   - mintKindName namespaces a NEW bare kind to user: (write path: kinds add).
//   - resolveRegisteredKind resolves an EXISTING bare name against the registry
//     using describeType's exact/local/ambiguous model (read/update paths: show,
//     alias, --extends).
//
// The earlier surface used the minting helper on every path, so `show vehicle`
// looked for user:vehicle even when only extension:vehicle was registered, and
// alias silently mutated a guessed user: default for an ambiguous bare name.

import (
	"reflect"
	"testing"
)

func TestMintKindName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare non-core gets user prefix", "vehicle", "user:vehicle"},
		{"already namespaced passes through", "user:vehicle", "user:vehicle"},
		{"extension namespace passes through", "extension:vehicle", "extension:vehicle"},
		{"core kind passes through bare", "person", "person"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mintKindName(tc.in); got != tc.want {
				t.Errorf("mintKindName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLocalKindSegment(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"vehicle", "vehicle"},
		{"user:vehicle", "vehicle"},
		{"extension:foo:vehicle", "vehicle"},
		{"person", "person"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := localKindSegment(tc.in); got != tc.want {
				t.Errorf("localKindSegment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveRegisteredKind(t *testing.T) {
	registered := []string{"person", "user:vehicle", "extension:research_paper"}

	t.Run("exact full name wins outright", func(t *testing.T) {
		got, amb, found := resolveRegisteredKind(registered, "user:vehicle")
		if !found || got != "user:vehicle" || amb != nil {
			t.Fatalf("got (%q, %v, %v), want (user:vehicle, nil, true)", got, amb, found)
		}
	})

	t.Run("bare local name resolves to single namespaced kind", func(t *testing.T) {
		got, amb, found := resolveRegisteredKind(registered, "vehicle")
		if !found || got != "user:vehicle" || amb != nil {
			t.Fatalf("got (%q, %v, %v), want (user:vehicle, nil, true)", got, amb, found)
		}
	})

	t.Run("bare core name resolves exact", func(t *testing.T) {
		got, _, found := resolveRegisteredKind(registered, "person")
		if !found || got != "person" {
			t.Fatalf("got (%q, _, %v), want (person, _, true)", got, found)
		}
	})

	t.Run("unknown bare name is not found", func(t *testing.T) {
		got, amb, found := resolveRegisteredKind(registered, "spaceship")
		if found || got != "" || amb != nil {
			t.Fatalf("got (%q, %v, %v), want (\"\", nil, false)", got, amb, found)
		}
	})

	t.Run("ambiguous bare name returns candidate set", func(t *testing.T) {
		dup := []string{"user:vehicle", "extension:vehicle"}
		got, amb, found := resolveRegisteredKind(dup, "vehicle")
		if found || got != "" {
			t.Fatalf("got (%q, _, %v), want (\"\", _, false)", got, found)
		}
		want := []string{"user:vehicle", "extension:vehicle"}
		if !reflect.DeepEqual(amb, want) {
			t.Fatalf("ambiguous candidates = %v, want %v", amb, want)
		}
	})
}
