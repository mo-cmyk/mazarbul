package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// decodeCountOutput unmarshals a count tool response for assertions.
func decodeCountOutput(t *testing.T, body json.RawMessage) countOutput {
	t.Helper()
	var out countOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal count output: %v", err)
	}
	return out
}

func TestCount_Total(t *testing.T) {
	t.Parallel()
	c := NewCount(&fakeNavigator{count: 7})
	body, err := c.Invoke(context.Background(), json.RawMessage(`{"filter":"kind = \"person\""}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decodeCountOutput(t, body)
	if out.Count == nil || *out.Count != 7 {
		t.Errorf("count = %v, want 7", out.Count)
	}
	if out.Groups != nil {
		t.Errorf("groups = %v, want nil when no groupBy", out.Groups)
	}
}

func TestCount_GroupBy(t *testing.T) {
	t.Parallel()
	c := NewCount(&fakeNavigator{groups: []navigate.GroupCount{{Value: "pending", Count: 2}, {Value: "active", Count: 1}}})
	body, err := c.Invoke(context.Background(), json.RawMessage(`{"groupBy":"status"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decodeCountOutput(t, body)
	if out.Count != nil {
		t.Errorf("count = %v, want nil under groupBy", *out.Count)
	}
	if len(out.Groups) != 2 || out.Groups[0].Value != "pending" || out.Groups[0].Count != 2 {
		t.Errorf("groups = %+v, want [{pending 2} {active 1}]", out.Groups)
	}
}

// TestCount_BadFilter_Validation pins the error mapping: a filter the compiler
// rejects (navigate.ErrInvalidFilter) surfaces as E_VALIDATION so the agent can
// fix the expression, not an opaque internal error.
func TestCount_BadFilter_Validation(t *testing.T) {
	t.Parallel()
	c := NewCount(&fakeNavigator{err: fmt.Errorf("%w: at <input>:1:6", navigate.ErrInvalidFilter)})
	_, err := c.Invoke(context.Background(), json.RawMessage(`{"filter":"kind ="}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestCount_InternalError maps a non-filter failure (e.g. the reader is down) to
// E_INTERNAL.
func TestCount_InternalError(t *testing.T) {
	t.Parallel()
	c := NewCount(&fakeNavigator{err: errors.New("reader closed")})
	_, err := c.Invoke(context.Background(), json.RawMessage(`{}`))
	wantCode(t, err, mcp.CodeInternal)
}

// TestCount_GroupByBadFilter_Validation pins the same mapping on the aggregate
// path.
func TestCount_GroupByBadFilter_Validation(t *testing.T) {
	t.Parallel()
	c := NewCount(&fakeNavigator{err: fmt.Errorf("%w: unexpected token", navigate.ErrInvalidFilter)})
	_, err := c.Invoke(context.Background(), json.RawMessage(`{"groupBy":"status","filter":"("}`))
	wantCode(t, err, mcp.CodeValidation)
}

// --- scope enforcement: navWithScopes isolates team entities ---

// TestCount_TeamScopeHidden proves that team-scoped entities are invisible to
// the count tool when no scopes param is given (defaults to DefaultReadSet =
// {project, personal}) and visible when scopes:["team"] is explicitly passed.
func TestCount_TeamScopeHidden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Use a unique kind absent from all migrations so the count is unambiguous.
	if _, serr := store.Writer().ExecContext(ctx,
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`,
		"count-team-ent", "test_scope_sentinel", "Team Member", 1.0, "confirmed", now, "team",
	); serr != nil {
		t.Fatalf("seed team entity: %v", serr)
	}

	// AllScopes navigator: the base nav can see everything; scope narrowing
	// happens per-request inside Count.Invoke via navWithScopes.
	nav, nerr := navigate.NewAllScopes(store.Reader())
	if nerr != nil {
		t.Fatalf("navigator: %v", nerr)
	}
	c := NewCount(nav)

	// Default (no scopes) → DefaultReadSet = {project, personal}: team hidden, count = 0.
	body, ierr := c.Invoke(ctx, json.RawMessage(`{"kind":"test_scope_sentinel"}`))
	if ierr != nil {
		t.Fatalf("Invoke (no scopes): %v", ierr)
	}
	out := decodeCountOutput(t, body)
	if out.Count == nil || *out.Count != 0 {
		t.Errorf("no scopes: count=%v, want 0 (team entity hidden by DefaultReadSet)", out.Count)
	}

	// scopes:["team"] → team entity surfaces, count = 1.
	body2, ierr2 := c.Invoke(ctx, json.RawMessage(`{"kind":"test_scope_sentinel","scopes":["team"]}`))
	if ierr2 != nil {
		t.Fatalf("Invoke (team scope): %v", ierr2)
	}
	out2 := decodeCountOutput(t, body2)
	if out2.Count == nil || *out2.Count != 1 {
		t.Errorf("team scope: count=%v, want 1", out2.Count)
	}
}

// TestCount_InvalidScope_Validation checks that an unrecognised scope value
// returns E_VALIDATION rather than an opaque internal error.
func TestCount_InvalidScope_Validation(t *testing.T) {
	t.Parallel()
	c := NewCount(&fakeNavigator{count: 0})
	_, err := c.Invoke(context.Background(), json.RawMessage(`{"scopes":["invalid_scope"]}`))
	wantCode(t, err, mcp.CodeValidation)
}
