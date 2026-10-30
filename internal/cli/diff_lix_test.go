package cli

import (
	"strings"
	"testing"
)

// TestComputeDiff_SupersedeForgottenIdentifier pins the lix-style entity
// diff vocabulary against the renderer's output layout:
//
//   - the renderer REMOVES a relation row from relations.jsonl when it is
//     superseded or forgotten, then writes the historical row to
//     superseded-relations.jsonl / forgotten-relations.jsonl;
//   - diff must surface that as ⤳ (superseded) and ⊗ (forgotten),
//     not as a silent drop.
//   - an entity that gains an identifier (entityIdentifierAdded event,
//     e.g. a new email) must surface as ~ entity with "identifier added"
//     in details, just like alias additions.
//
// Without this coverage `maz diff HEAD~1..HEAD` against a real two-push
// scenario shows only the "+ relation" for the new edge while silently
// dropping the supersede and forget that the user explicitly asked
// about.
func TestComputeDiff_SupersedeForgottenIdentifier(t *testing.T) {
	t.Parallel()

	relA := []map[string]any{
		{"uid": "rel-ceo", "subject": "rosie", "predicate": "role_at", "object": "erebor"},
		{"uid": "rel-knows", "subject": "rosie", "predicate": "knows", "object": "bob"},
	}
	relB := []map[string]any{
		{"uid": "rel-cfo", "subject": "rosie", "predicate": "role_at", "object": "erebor"},
	}
	supersededA := []map[string]any(nil)
	supersededB := []map[string]any{
		{"uid": "rel-ceo", "subject": "rosie", "predicate": "role_at", "object": "erebor", "superseded_by": "rel-cfo"},
	}
	forgottenA := []map[string]any(nil)
	forgottenB := []map[string]any{
		{"uid": "rel-knows", "subject": "rosie", "predicate": "knows", "object": "bob", "reason": "tentative; downgrading"},
	}

	entA := []map[string]any{
		{
			"uid": "rosie", "kind": "person", "displayName": "Rosie Cotton",
			"aliases": []any{"Rosie"},
		},
	}
	entB := []map[string]any{
		{
			"uid": "rosie", "kind": "person", "displayName": "Rosie Cotton",
			"aliases": []any{"Rosie"},
			"identifiers": []any{
				map[string]any{"type": "email", "value": "rosie@erebor.example"},
			},
		},
	}

	entries := computeDiff(
		"personal",
		entA, entB,
		relA, relB,
		supersededA, supersededB,
		forgottenA, forgottenB,
		&diffFlags{},
	)

	var seenSupersede, seenForget, seenIdentifier bool
	for _, e := range entries {
		switch e.Action {
		case diffActionSuperseded:
			if e.Kind == "relation" && strings.Contains(e.Target, "role_at") {
				seenSupersede = true
				if len(e.Refs) == 0 || e.Refs[0] != "rel-cfo" {
					t.Errorf("supersede entry missing successor ref rel-cfo: %+v", e)
				}
			}
		case diffActionForgotten:
			if e.Kind == "relation" && strings.Contains(e.Target, "knows") {
				seenForget = true
				if !strings.Contains(e.Details, "tentative") {
					t.Errorf("forgotten entry missing reason: %+v", e)
				}
			}
		case diffActionChanged:
			if e.Kind == "entity" && strings.Contains(e.Details, "identifier") {
				seenIdentifier = true
			}
		}
	}
	if !seenSupersede {
		t.Errorf("missing ⤳ supersede entry for rel-ceo in entries: %+v", entries)
	}
	if !seenForget {
		t.Errorf("missing ⊗ forgotten entry for rel-knows in entries: %+v", entries)
	}
	if !seenIdentifier {
		t.Errorf("missing ~ entity identifier-added entry: %+v", entries)
	}
}
