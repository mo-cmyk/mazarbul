package cli

import (
	"encoding/json"
	"testing"
)

// TestNormalizeCountGroups_EmptyInput verifies that an empty/nil raw message
// is rewritten to {"groups":[]}.
func TestNormalizeCountGroups_EmptyInput(t *testing.T) {
	t.Parallel()
	got := normalizeCountGroups(nil)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("result is not valid JSON: %v — got %s", err, got)
	}
	var groups []any
	if err := json.Unmarshal(obj["groups"], &groups); err != nil {
		t.Fatalf("groups field is not a JSON array: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("groups = %v, want empty array", groups)
	}
}

// TestNormalizeCountGroups_BareObject verifies that {} (the empty-result
// response from the count MCP tool when no rows match a group-by query)
// is rewritten to {"groups":[]}.
func TestNormalizeCountGroups_BareObject(t *testing.T) {
	t.Parallel()
	got := normalizeCountGroups(json.RawMessage(`{}`))
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("result is not valid JSON: %v — got %s", err, got)
	}
	var groups []any
	if err := json.Unmarshal(obj["groups"], &groups); err != nil {
		t.Fatalf("groups field is not a JSON array: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("groups = %v, want empty array", groups)
	}
}

// TestNormalizeCountGroups_ExistingGroups verifies that a response that
// already has a non-empty groups array is passed through unchanged.
func TestNormalizeCountGroups_ExistingGroups(t *testing.T) {
	t.Parallel()
	input := json.RawMessage(`{"groups":[{"value":"person","count":3}]}`)
	got := normalizeCountGroups(input)
	var result struct {
		Groups []struct {
			Value string `json:"value"`
			Count int    `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("result is not valid JSON: %v — got %s", err, got)
	}
	if len(result.Groups) != 1 {
		t.Fatalf("groups len = %d, want 1", len(result.Groups))
	}
	if result.Groups[0].Value != "person" || result.Groups[0].Count != 3 {
		t.Errorf("groups[0] = %+v, want {person 3}", result.Groups[0])
	}
}

// TestNormalizeCountGroups_EmptyGroupsArray verifies that an explicit
// {"groups":[]} is left unchanged (the key is already present).
func TestNormalizeCountGroups_EmptyGroupsArray(t *testing.T) {
	t.Parallel()
	input := json.RawMessage(`{"groups":[]}`)
	got := normalizeCountGroups(input)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if _, ok := result["groups"]; !ok {
		t.Error("groups key must be present")
	}
}

// TestNormalizeCountGroups_TotalCount verifies that a scalar count response
// (no groups, just {"count":42}) is passed through unchanged.
func TestNormalizeCountGroups_TotalCount(t *testing.T) {
	t.Parallel()
	input := json.RawMessage(`{"count":42}`)
	got := normalizeCountGroups(input)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	// groups may or may not be injected here; the important invariant is
	// that the count value survived.
	var count int
	if err := json.Unmarshal(result["count"], &count); err != nil {
		t.Fatalf("count field not readable: %v", err)
	}
	if count != 42 {
		t.Errorf("count = %d, want 42", count)
	}
}
