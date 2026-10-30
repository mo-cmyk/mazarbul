package ontology_test

import (
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestExternalRefUID_Deterministic asserts the same (scope, uri) pair always
// derives the same ULID — the property that makes the reference tool
// mint-or-return rather than minting a duplicate on every call.
func TestExternalRefUID_Deterministic(t *testing.T) {
	t.Parallel()
	const (
		scope = "personal"
		uri   = "https://github.com/org/repo/issues/42"
	)
	a := ontology.ExternalRefUID(scope, uri)
	b := ontology.ExternalRefUID(scope, uri)
	if a != b {
		t.Fatalf("ExternalRefUID not deterministic: %q != %q", a, b)
	}
}

// TestExternalRefUID_ScopeSensitive asserts the same URI in two scopes derives
// two distinct ULIDs. This is the dedup-within-scope guarantee: a personal
// pointer never collapses into a project-scope entity that renders to git.
func TestExternalRefUID_ScopeSensitive(t *testing.T) {
	t.Parallel()
	const uri = "https://github.com/org/repo/issues/42"
	personal := ontology.ExternalRefUID("personal", uri)
	project := ontology.ExternalRefUID("project", uri)
	if personal == project {
		t.Fatalf("ExternalRefUID collapsed across scopes: personal=%q project=%q", personal, project)
	}
}

// TestExternalRefUID_URISensitive asserts distinct URIs in one scope derive
// distinct ULIDs (no accidental collision for different pointers).
func TestExternalRefUID_URISensitive(t *testing.T) {
	t.Parallel()
	a := ontology.ExternalRefUID("personal", "https://example.com/a")
	b := ontology.ExternalRefUID("personal", "https://example.com/b")
	if a == b {
		t.Fatalf("distinct URIs derived the same ULID: %q", a)
	}
}

// TestExternalRefUID_ParsesAsULID asserts the derived id is a well-formed
// 26-char Crockford ULID, so it round-trips through ulid.Parse and is accepted
// verbatim as a relate() endpoint with no relate.go change.
func TestExternalRefUID_ParsesAsULID(t *testing.T) {
	t.Parallel()
	// Exercise a high-bit input (0xFF-leading hash) and a plain one.
	for _, tc := range []struct{ scope, uri string }{
		{"personal", "https://example.com/x"},
		{"project", "mailto:alice@example.com"},
		{"team", "file:///repo/path/to/file.go"},
	} {
		uid := ontology.ExternalRefUID(tc.scope, tc.uri)
		if len(string(uid)) != 26 {
			t.Errorf("uid %q is not 26 chars (got %d)", uid, len(string(uid)))
		}
		if _, err := ulid.Parse(string(uid)); err != nil {
			t.Errorf("ulid.Parse(%q): %v", uid, err)
		}
	}
}
