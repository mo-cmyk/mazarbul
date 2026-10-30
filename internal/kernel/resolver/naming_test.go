package resolver

import (
	"reflect"
	"testing"
)

func TestParseCanonicalName_ValidPick(t *testing.T) {
	t.Parallel()
	raw := `{"canonical": "Frodo Baggins"}`
	allowed := []string{"F. Baggins", "Frodo Baggins", "frodo"}
	got, ok := parseCanonicalName(raw, allowed)
	if !ok {
		t.Fatalf("expected ok, got false")
	}
	if got != "Frodo Baggins" {
		t.Errorf("got %q, want %q", got, "Frodo Baggins")
	}
}

func TestParseCanonicalName_PickNotInAllowed(t *testing.T) {
	t.Parallel()
	raw := `{"canonical": "Frodo The Hacker"}`
	if _, ok := parseCanonicalName(raw, []string{"F. Baggins", "frodo"}); ok {
		t.Fatal("expected false for pick not in allowed list")
	}
}

func TestParseCanonicalName_MalformedJSON(t *testing.T) {
	t.Parallel()
	if _, ok := parseCanonicalName("not json", []string{"a"}); ok {
		t.Fatal("expected false on malformed JSON")
	}
}

func TestParseCanonicalName_EmptyInput(t *testing.T) {
	t.Parallel()
	if _, ok := parseCanonicalName("   ", []string{"a"}); ok {
		t.Fatal("expected false on blank input")
	}
}

func TestMergeAlternateNames_PromotesPreviousAndDedupes(t *testing.T) {
	t.Parallel()
	existing := []string{"F. Baggins", "Frodo Baggins", "frodo"}
	got := mergeAlternateNames(existing, "Frodo", "Frodo Baggins")
	want := []string{"Frodo", "F. Baggins", "frodo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeAlternateNames_SamePreviousAndPicked(t *testing.T) {
	t.Parallel()
	got := mergeAlternateNames([]string{"a", "b"}, "x", "x")
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
