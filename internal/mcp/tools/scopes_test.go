package tools

// Error-code coverage for the shared `scopes` param (#206). All scope-aware read
// tools (recall / getContext / get / neighbors / relations / entities) route
// their `scopes` array through scopesFromParam, so testing the helper plus one
// end-to-end Invoke pins the E_VALIDATION contract for the whole surface.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

func TestScopesFromParam(t *testing.T) {
	t.Parallel()

	t.Run("omitted scopes default to project and personal", func(t *testing.T) {
		t.Parallel()
		got, err := scopesFromParam(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Equal(kernel.DefaultReadSet()) {
			t.Errorf("scopesFromParam(nil) = %v, want DefaultReadSet %v", got, kernel.DefaultReadSet())
		}
	})

	t.Run("valid scopes parse", func(t *testing.T) {
		t.Parallel()
		got, err := scopesFromParam([]string{"team", "personal"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := kernel.NewScopeSet(kernel.ScopeTeam, kernel.ScopePersonal); !got.Equal(want) {
			t.Errorf("scopes = %v, want %v", got, want)
		}
	})

	t.Run("unknown scope is rejected with a validation error", func(t *testing.T) {
		t.Parallel()
		_, err := scopesFromParam([]string{"galaxy"})
		if err == nil {
			t.Fatal("want error for unknown scope, got nil")
		}
		var te *mcp.ToolError
		if !errors.As(err, &te) {
			t.Fatalf("err type = %T, want *mcp.ToolError", err)
		}
		if te.Code != mcp.CodeValidation {
			t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
		}
	})

	// F1 security gate: even though kernel.ParseScopeSet accepts "draft" (it is a
	// real, kernel-internal scope), the shared read verbs MUST refuse it. The
	// draft staging area is reachable only through draftStage/promoteDraft/
	// discardDraft — never a general read with scopes:["draft"].
	t.Run("draft scope is rejected with a validation error", func(t *testing.T) {
		t.Parallel()
		_, err := scopesFromParam([]string{"draft"})
		if err == nil {
			t.Fatal("want error for draft scope, got nil")
		}
		var te *mcp.ToolError
		if !errors.As(err, &te) {
			t.Fatalf("err type = %T, want *mcp.ToolError", err)
		}
		if te.Code != mcp.CodeValidation {
			t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
		}
	})

	t.Run("draft mixed with a valid scope is still rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := scopesFromParam([]string{"personal", "draft"}); err == nil {
			t.Fatal("want error when draft is mixed with a valid scope, got nil")
		}
	})
}

// TestNeighbors_UnknownScopeReturnsValidationError proves the error-code contract
// end to end at the tool boundary: scopesFromParam runs before the navigator is
// touched, so an unknown scope surfaces as an E_VALIDATION envelope even with no
// backing store.
func TestNeighbors_UnknownScopeReturnsValidationError(t *testing.T) {
	t.Parallel()
	nb := NewNeighbors(nil)
	_, err := nb.Invoke(context.Background(),
		json.RawMessage(`{"seed":"01ARZ3NDEKTSV4RRFFQ69G5FAV","scopes":["galaxy"]}`))
	if err == nil {
		t.Fatal("Invoke succeeded, want E_VALIDATION")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
	}
}
