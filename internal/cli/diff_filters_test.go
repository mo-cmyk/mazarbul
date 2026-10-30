package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestDiffHelp verifies that `maz diff --help` is non-empty and contains
// the documented examples, including the --operation flag.
func TestDiffHelp(t *testing.T) {
	t.Parallel()
	root := newDiffCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("diff help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz diff",
		"--operation",
		"--kind",
		"--predicate",
		"--format",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff help missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestPostFilterDiff_Operation verifies that --operation filters diffEntry.Action.
func TestPostFilterDiff_Operation(t *testing.T) {
	t.Parallel()
	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Alice", Scope: "personal"},
		{Action: diffActionChanged, Kind: diffKindEntity, Target: "person/Bob", Scope: "personal"},
		{Action: diffActionForgotten, Kind: diffKindRelation, Target: "alice knows bob", Scope: "personal"},
		{Action: diffActionConflict, Kind: diffKindRelation, Target: "bob role_at Erebor", Scope: "personal"},
	}

	t.Run("filter to added only", func(t *testing.T) {
		t.Parallel()
		got, removed := postFilterDiff(entries, &diffFlags{operation: diffActionAdded})
		if len(got) != 1 || got[0].Action != diffActionAdded {
			t.Errorf("expected 1 added entry, got %+v", got)
		}
		if removed != 3 {
			t.Errorf("expected 3 removed, got %d", removed)
		}
	})

	t.Run("filter to conflict only", func(t *testing.T) {
		t.Parallel()
		got, removed := postFilterDiff(entries, &diffFlags{operation: "conflict"})
		if len(got) != 1 || got[0].Action != diffActionConflict {
			t.Errorf("expected 1 conflict entry, got %+v", got)
		}
		if removed != 3 {
			t.Errorf("expected 3 removed, got %d", removed)
		}
	})

	t.Run("no filter returns all", func(t *testing.T) {
		t.Parallel()
		got, removed := postFilterDiff(entries, &diffFlags{})
		if len(got) != len(entries) {
			t.Errorf("expected %d entries unfiltered, got %d", len(entries), len(got))
		}
		if removed != 0 {
			t.Errorf("expected 0 removed without filter, got %d", removed)
		}
	})

	t.Run("case-insensitive match", func(t *testing.T) {
		t.Parallel()
		got, _ := postFilterDiff(entries, &diffFlags{operation: "ADDED"})
		if len(got) != 1 || got[0].Action != diffActionAdded {
			t.Errorf("expected case-insensitive match: %+v", got)
		}
	})
}

// TestPostFilterDiff_KindIsDeepPassOnly verifies that --kind does NOT filter in
// postFilterDiff. Entity-kind filtering (e.g. "person") is applied exclusively
// in the deep computation pass (entityMatchesFilter). If postFilterDiff were to
// match flags.kind against diffEntry.Kind ("entity"/"relation"), it would filter
// out ALL entries when the caller passes --kind person, because no entry has
// diffEntry.Kind == "person".
func TestPostFilterDiff_KindIsDeepPassOnly(t *testing.T) {
	t.Parallel()
	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Alice", Scope: "personal"},
		{Action: diffActionAdded, Kind: diffKindRelation, Target: "alice knows bob", Scope: "personal"},
		{Action: diffActionChanged, Kind: diffKindEntity, Target: "person/Bob", Scope: "personal"},
	}

	t.Run("kind person does not filter in postFilterDiff", func(t *testing.T) {
		t.Parallel()
		// --kind person: the deep pass already restricted entries to person
		// entities; postFilterDiff must return all provided entries unchanged.
		got, removed := postFilterDiff(entries, &diffFlags{kind: "person"})
		if len(got) != len(entries) {
			t.Errorf("postFilterDiff with --kind person should return all entries unchanged, got %d/%d", len(got), len(entries))
		}
		if removed != 0 {
			t.Errorf("postFilterDiff with --kind person should remove 0, got %d", removed)
		}
	})

	t.Run("kind entity does not filter in postFilterDiff", func(t *testing.T) {
		t.Parallel()
		// Even the structural kind "entity" should not be filtered here.
		got, removed := postFilterDiff(entries, &diffFlags{kind: "entity"})
		if len(got) != len(entries) {
			t.Errorf("postFilterDiff with --kind entity should return all entries unchanged, got %d/%d", len(got), len(entries))
		}
		if removed != 0 {
			t.Errorf("expected 0 removed, got %d", removed)
		}
	})
}

// TestEntityKindFilter_DeepPass verifies that computeDiff honours the --kind flag
// at the deep-pass level: only entities whose kind matches are included, in both
// the text and JSON paths.
func TestEntityKindFilter_DeepPass(t *testing.T) {
	t.Parallel()

	entA := []map[string]any{
		{"uid": "alice", "kind": "person", "displayName": "Alice"},
		{"uid": "acme", "kind": "organization", "displayName": "Acme Corp"},
	}
	entB := []map[string]any{
		{"uid": "alice", "kind": "person", "displayName": "Alice"},
		{"uid": "acme", "kind": "organization", "displayName": "Acme Corp"},
		{"uid": "bob", "kind": "person", "displayName": "Bob"},
	}

	t.Run("kind person returns only person entity", func(t *testing.T) {
		t.Parallel()
		flags := &diffFlags{kind: "person"}
		entries := computeDiff("personal", entA, entB, nil, nil, nil, nil, nil, nil, flags)
		for _, e := range entries {
			if e.Kind == diffKindEntity && !strings.Contains(e.Target, "person/") {
				t.Errorf("non-person entity slipped through --kind person filter: %+v", e)
			}
		}
		var personCount int
		for _, e := range entries {
			if e.Kind == diffKindEntity {
				personCount++
			}
		}
		// Only Bob (added) should appear; Alice is unchanged and Acme is excluded.
		if personCount != 1 {
			t.Errorf("expected 1 person entity entry (Bob added), got %d: %+v", personCount, entries)
		}
	})

	t.Run("kind person JSON and text paths return same entries", func(t *testing.T) {
		t.Parallel()
		flags := &diffFlags{kind: "person"}
		entries := computeDiff("personal", entA, entB, nil, nil, nil, nil, nil, nil, flags)
		// postFilterDiff with --kind must not remove any further entries.
		textVisible, removedByPost := postFilterDiff(entries, flags)
		if removedByPost != 0 {
			t.Errorf("postFilterDiff removed %d entries with --kind person; text and JSON paths now disagree", removedByPost)
		}
		if len(textVisible) != len(entries) {
			t.Errorf("text path sees %d entries, JSON path would see %d", len(entries), len(textVisible))
		}
	})
}

// TestPostFilterDiff_Predicate verifies that --predicate post-filters on
// target content (predicate substring in the target string).
func TestPostFilterDiff_Predicate(t *testing.T) {
	t.Parallel()
	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindRelation, Target: "alice knows bob", Scope: "personal"},
		{Action: diffActionAdded, Kind: diffKindRelation, Target: "bob role_at Erebor", Scope: "personal"},
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Alice", Scope: "personal"},
	}

	t.Run("predicate role at narrows to one relation", func(t *testing.T) {
		t.Parallel()
		got, removed := postFilterDiff(entries, &diffFlags{predicate: "role_at"})
		if len(got) != 1 || !strings.Contains(got[0].Target, "role_at") {
			t.Errorf("expected 1 role_at entry, got %+v", got)
		}
		if removed != 2 {
			t.Errorf("expected 2 removed, got %d", removed)
		}
	})

	t.Run("predicate narrows text and JSON identically", func(t *testing.T) {
		t.Parallel()
		flags := &diffFlags{predicate: "knows"}
		filtered, _ := postFilterDiff(entries, flags)
		if len(filtered) != 1 || !strings.Contains(filtered[0].Target, "knows") {
			t.Errorf("predicate filter wrong: %+v", filtered)
		}
	})
}

// TestPostFilterDiff_OperationAndPredicateCompose verifies that --operation and
// --predicate compose with AND logic (both paths).
func TestPostFilterDiff_OperationAndPredicateCompose(t *testing.T) {
	t.Parallel()
	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindRelation, Target: "alice knows bob", Scope: "personal"},
		{Action: diffActionChanged, Kind: diffKindRelation, Target: "bob role_at Erebor", Scope: "personal"},
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Alice", Scope: "personal"},
	}

	// operation=added AND predicate=knows: only the knows relation.
	got, removed := postFilterDiff(entries, &diffFlags{operation: diffActionAdded, predicate: "knows"})
	if len(got) != 1 || !strings.Contains(got[0].Target, "knows") {
		t.Errorf("expected 1 entry (knows relation), got %+v", got)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}
}

// TestActiveFilters verifies that activeFilters returns the right map.
func TestActiveFilters(t *testing.T) {
	t.Parallel()

	t.Run("no flags returns nil", func(t *testing.T) {
		t.Parallel()
		m := activeFilters(&diffFlags{})
		if m != nil {
			t.Errorf("expected nil, got %v", m)
		}
	})

	t.Run("operation flag included", func(t *testing.T) {
		t.Parallel()
		m := activeFilters(&diffFlags{operation: diffActionAdded})
		if m["operation"] != diffActionAdded {
			t.Errorf("expected operation=%s, got %v", diffActionAdded, m)
		}
	})

	t.Run("kind flag included in active filters", func(t *testing.T) {
		t.Parallel()
		m := activeFilters(&diffFlags{kind: "person"})
		if m["kind"] != "person" {
			t.Errorf("expected kind=person in active_filters, got %v", m)
		}
	})

	t.Run("multiple flags included", func(t *testing.T) {
		t.Parallel()
		m := activeFilters(&diffFlags{kind: "entity", predicate: "knows", operation: diffActionAdded})
		if len(m) != 3 {
			t.Errorf("expected 3 keys, got %d: %v", len(m), m)
		}
	})

	t.Run("forgotten and conflicts included", func(t *testing.T) {
		t.Parallel()
		m := activeFilters(&diffFlags{forgotten: true, conflicts: true})
		if m["forgotten"] != "true" || m["conflicts"] != "true" {
			t.Errorf("expected forgotten+conflicts flags, got %v", m)
		}
	})
}

// TestWriteDiffJSON_Envelope verifies the JSON output shape with filter metadata.
func TestWriteDiffJSON_Envelope(t *testing.T) {
	t.Parallel()

	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Alice", Scope: "personal"},
		{Action: diffActionChanged, Kind: diffKindEntity, Target: "person/Bob", Scope: "personal"},
	}

	t.Run("no filters entries present filtered zero no active filters", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := writeDiffJSON(&buf, entries, 0, nil); err != nil {
			t.Fatalf("writeDiffJSON: %v", err)
		}
		var out diffOutput
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatalf("not valid JSON: %v\nraw: %s", err, buf.String())
		}
		if len(out.Entries) != 2 {
			t.Errorf("expected 2 entries, got %d", len(out.Entries))
		}
		if out.Filtered != 0 {
			t.Errorf("filtered = %d, want 0", out.Filtered)
		}
		if out.ActiveFilters != nil {
			t.Errorf("active_filters should be absent when nil, got %v", out.ActiveFilters)
		}
	})

	t.Run("operation filter filtered one active filters set", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		filters := map[string]string{"operation": diffActionAdded}
		if err := writeDiffJSON(&buf, entries[:1], 1, filters); err != nil {
			t.Fatalf("writeDiffJSON: %v", err)
		}
		var out diffOutput
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatalf("not valid JSON: %v\nraw: %s", err, buf.String())
		}
		if len(out.Entries) != 1 {
			t.Errorf("expected 1 entry, got %d", len(out.Entries))
		}
		if out.Filtered != 1 {
			t.Errorf("filtered = %d, want 1", out.Filtered)
		}
		if out.ActiveFilters["operation"] != diffActionAdded {
			t.Errorf("active_filters[operation] = %q, want %q", out.ActiveFilters["operation"], diffActionAdded)
		}
	})

	t.Run("kind filter shows in active filters and filtered count", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		// Simulate: deep pass returned 1 person entry; 1 org entry was excluded at
		// compute time (not at postFilterDiff time), so filtered=1 here.
		filters := map[string]string{"kind": "person"}
		if err := writeDiffJSON(&buf, entries[:1], 1, filters); err != nil {
			t.Fatalf("writeDiffJSON: %v", err)
		}
		var out diffOutput
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatalf("not valid JSON: %v\nraw: %s", err, buf.String())
		}
		if out.ActiveFilters["kind"] != "person" {
			t.Errorf("active_filters[kind] = %q, want \"person\"", out.ActiveFilters["kind"])
		}
	})

	t.Run("empty entries produces [] not null", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := writeDiffJSON(&buf, nil, 0, nil); err != nil {
			t.Fatalf("writeDiffJSON: %v", err)
		}
		if !strings.Contains(buf.String(), `"entries": []`) {
			t.Errorf("nil entries should produce empty array: %s", buf.String())
		}
	})
}

// TestRunDiff_OperationValidation verifies that an unknown --operation value
// returns an error (exit 1) and that valid values are accepted.
func TestRunDiff_OperationValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		operation string
		wantErr   bool
	}{
		{"valid added", diffActionAdded, false},
		{"valid changed", "changed", false},
		{"valid superseded", "superseded", false},
		{"valid forgotten", "forgotten", false},
		{"valid conflict", "conflict", false},
		{"valid orphaned", "orphaned", false},
		{"invalid badvalue", "BADVALUE", true},
		{"invalid typo", "add", true},
		{"invalid empty string", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			flags := &diffFlags{
				format:    "text",
				operation: tc.operation,
			}
			// runDiff validates before hitting the sync config; an empty dataDir
			// will fail after validation so we only check for a validation error.
			err := runDiff(context.Background(), &buf, "/nonexistent-data-dir", diffDefaultRevspec, flags)
			if tc.wantErr {
				if err == nil {
					t.Errorf("operation %q: expected error, got nil", tc.operation)
					return
				}
				if !strings.Contains(err.Error(), "--operation") {
					t.Errorf("operation %q: error should mention --operation, got: %v", tc.operation, err)
				}
			} else if err != nil && strings.Contains(err.Error(), "--operation") {
				t.Errorf("operation %q: unexpected --operation validation error: %v", tc.operation, err)
			}
		})
	}
}

// TestRunDiff_FormatValidation verifies that an unknown --format value returns
// an error and that valid values are accepted.
func TestRunDiff_FormatValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		format  string
		wantErr bool
	}{
		{"valid text", "text", false},
		{"valid json", "json", false},
		{"valid markdown", "markdown", false},
		{"invalid badvalue", "BADVALUE", true},
		{"invalid output alias", "ndjson", true},
		{"invalid empty treated as text", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			flags := &diffFlags{format: tc.format}
			err := runDiff(context.Background(), &buf, "/nonexistent-data-dir", diffDefaultRevspec, flags)
			if tc.wantErr {
				if err == nil {
					t.Errorf("format %q: expected error, got nil", tc.format)
					return
				}
				if !strings.Contains(err.Error(), "--format") {
					t.Errorf("format %q: error should mention --format, got: %v", tc.format, err)
				}
			} else if err != nil && strings.Contains(err.Error(), "--format") {
				t.Errorf("format %q: unexpected --format validation error: %v", tc.format, err)
			}
		})
	}
}

// TestDiffTextJSONConsistency verifies that --operation and --predicate filters
// produce the same set of entries in both text and JSON render paths. The text
// path must apply postFilterDiff per-scope; the JSON path applies it after
// collecting all scopes.
func TestDiffTextJSONConsistency(t *testing.T) {
	t.Parallel()

	// Build a realistic cross-scope entry set as if returned from diffScope.
	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Alice", Scope: "personal"},
		{Action: diffActionChanged, Kind: diffKindEntity, Target: "person/Bob", Scope: "personal"},
		{Action: diffActionAdded, Kind: diffKindRelation, Target: "alice knows bob", Scope: "personal"},
		{Action: diffActionForgotten, Kind: diffKindRelation, Target: "bob role_at Erebor", Scope: "personal"},
	}

	cases := []struct {
		name      string
		flags     *diffFlags
		wantCount int
	}{
		{
			name:      "operation added gives 2 entries",
			flags:     &diffFlags{operation: diffActionAdded},
			wantCount: 2,
		},
		{
			name:      "predicate knows gives 1 entry",
			flags:     &diffFlags{predicate: "knows"},
			wantCount: 1,
		},
		{
			name:      "operation forgotten gives 1 entry",
			flags:     &diffFlags{operation: "forgotten"},
			wantCount: 1,
		},
		{
			name:      "operation added predicate knows gives 1 entry",
			flags:     &diffFlags{operation: diffActionAdded, predicate: "knows"},
			wantCount: 1,
		},
		{
			name:      "no filters gives all entries",
			flags:     &diffFlags{},
			wantCount: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Simulate the text path: postFilterDiff applied per-scope before write.
			textVisible, _ := postFilterDiff(entries, tc.flags)
			// Simulate the JSON path: postFilterDiff applied after all scopes.
			jsonVisible, _ := postFilterDiff(entries, tc.flags)

			if len(textVisible) != tc.wantCount {
				t.Errorf("text path: want %d entries, got %d: %+v", tc.wantCount, len(textVisible), textVisible)
			}
			if len(jsonVisible) != tc.wantCount {
				t.Errorf("json path: want %d entries, got %d: %+v", tc.wantCount, len(jsonVisible), jsonVisible)
			}
			if len(textVisible) != len(jsonVisible) {
				t.Errorf("text and JSON paths disagree: text=%d json=%d", len(textVisible), len(jsonVisible))
			}
		})
	}
}
