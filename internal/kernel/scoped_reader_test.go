package kernel

import (
	"testing"
)

func TestScopeSet_Predicate_FailsClosedWhenEmpty(t *testing.T) {
	var empty ScopeSet // zero value
	if !empty.IsEmpty() {
		t.Fatalf("zero-value ScopeSet should be empty")
	}
	sql, args := empty.Predicate("e")
	// The fail-closed guard must reference the scope column (so the SQL-trace
	// enforcement test recognises it) and admit zero rows (NULL never matches IN).
	if sql != "e.scope IN (NULL)" {
		t.Errorf("empty-set predicate = %q, want %q", sql, "e.scope IN (NULL)")
	}
	if len(args) != 0 {
		t.Errorf("empty-set predicate args = %v, want none", args)
	}
}

func TestScopeSet_Predicate_SortedArgsAndPlaceholders(t *testing.T) {
	tests := []struct {
		name     string
		set      ScopeSet
		alias    string
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "single scope no alias",
			set:      NewScopeSet(ScopePersonal),
			alias:    "",
			wantSQL:  "scope IN (?)",
			wantArgs: []any{"personal"},
		},
		{
			name:     "default read set is sorted",
			set:      DefaultReadSet(),
			alias:    "e",
			wantSQL:  "e.scope IN (?, ?)",
			wantArgs: []any{"personal", "project"}, // alphabetical: personal < project
		},
		{
			name:     "all scopes",
			set:      AllScopes(),
			alias:    "r",
			wantSQL:  "r.scope IN (?, ?, ?)",
			wantArgs: []any{"personal", "project", "team"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSQL, gotArgs := tt.set.Predicate(tt.alias)
			if gotSQL != tt.wantSQL {
				t.Errorf("SQL = %q, want %q", gotSQL, tt.wantSQL)
			}
			if len(gotArgs) != len(tt.wantArgs) {
				t.Fatalf("args len = %d, want %d (%v)", len(gotArgs), len(tt.wantArgs), gotArgs)
			}
			for i := range tt.wantArgs {
				if gotArgs[i] != tt.wantArgs[i] {
					t.Errorf("args[%d] = %v, want %v", i, gotArgs[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestNewScopeSet_DedupesAndDropsInvalid(t *testing.T) {
	s := NewScopeSet(ScopePersonal, ScopePersonal, Scope("bogus"), ScopeProject, "")
	if !s.Equal(DefaultReadSet()) {
		t.Errorf("NewScopeSet deduped/cleaned = %s, want %s", s, DefaultReadSet())
	}
}

func TestParseScopeSet(t *testing.T) {
	t.Run("valid scopes parse", func(t *testing.T) {
		got, err := ParseScopeSet([]string{"team", "personal"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Equal(NewScopeSet(ScopeTeam, ScopePersonal)) {
			t.Errorf("parsed = %s", got)
		}
	})
	t.Run("unknown scope is rejected", func(t *testing.T) {
		_, err := ParseScopeSet([]string{"personal", "marketing"})
		if err == nil {
			t.Fatalf("expected error for unknown scope")
		}
	})
	t.Run("empty input yields fail-closed empty set", func(t *testing.T) {
		got, err := ParseScopeSet(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.IsEmpty() {
			t.Errorf("empty input should yield empty set, got %s", got)
		}
	})
}

func TestScopeDraft_ValidButExcludedFromDefaultAndNavigationReads(t *testing.T) {
	// draft is a recognised scope (writable, explicitly readable)...
	if !ScopeDraft.Valid() {
		t.Fatalf("ScopeDraft should be a Valid scope")
	}
	// ...but it must never appear in the default recall span...
	if DefaultReadSet().Contains(ScopeDraft) {
		t.Errorf("DefaultReadSet must NOT contain draft: speculative writes would pollute recall")
	}
	// ...nor in AllScopes, which backs graph navigation (Navigator uses
	// AllScopes); including it there would leak drafts through neighbors/get.
	if AllScopes().Contains(ScopeDraft) {
		t.Errorf("AllScopes must NOT contain draft: navigation would leak staged drafts")
	}
}

func TestParseScopeSet_AcceptsDraftForExplicitOptIn(t *testing.T) {
	// draft is reachable only by naming it explicitly — the `maz scope draft`
	// surface parses it; no default path widens into it.
	got, err := ParseScopeSet([]string{"draft"})
	if err != nil {
		t.Fatalf("draft should parse as an explicit scope: %v", err)
	}
	if !got.Equal(NewScopeSet(ScopeDraft)) {
		t.Errorf("parsed = %s, want {draft}", got)
	}
	sql, args := got.Predicate("e")
	if sql != "e.scope IN (?)" || len(args) != 1 || args[0] != "draft" {
		t.Errorf("draft predicate = %q args %v, want \"e.scope IN (?)\" [draft]", sql, args)
	}
}

func TestScopeSet_ContainsAndScopes(t *testing.T) {
	s := DefaultReadSet()
	if !s.Contains(ScopeProject) || !s.Contains(ScopePersonal) {
		t.Errorf("default set should contain project + personal")
	}
	if s.Contains(ScopeTeam) {
		t.Errorf("default set should not contain team")
	}
	// Scopes returns a copy; mutating it must not affect the set.
	cp := s.Scopes()
	cp[0] = ScopeTeam
	if s.Contains(ScopeTeam) {
		t.Errorf("Scopes() must return a defensive copy")
	}
}
