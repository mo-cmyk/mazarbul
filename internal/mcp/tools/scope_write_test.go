package tools_test

// scope_write_test.go covers the team-scope write parameter shared by the
// relate and fact tools (issue #213, D2.3): scope parsing, the team-scope
// "self"-sentinel guard, and the Append-time ErrPermission -> E_PERMISSION
// mapping. Reuses relateEntityResolver / captureRelateAppender / wantRelateCode
// from relate_test.go (same tools_test package).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// denyingAppender simulates the event log rejecting a team-scope write by a
// non-member: prepare()'s TeamWriteAuthority gate returns kernel.ErrPermission,
// which Append surfaces wrapped. The tool layer must map it to E_PERMISSION.
type denyingAppender struct{}

func (denyingAppender) Append(_ context.Context, _ []kernel.Event) ([]kernel.Receipt, error) {
	return nil, fmt.Errorf("eventlog: %w", kernel.ErrPermission)
}

// --- scope parsing ---

func TestRelate_UnknownScope_Validation(t *testing.T) {
	t.Parallel()
	r := tools.NewRelate(&captureRelateAppender{}, relateEntityResolver{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1","scope":"galaxy"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

func TestFact_UnknownScope_Validation(t *testing.T) {
	t.Parallel()
	f := tools.NewFact(&captureRelateAppender{}, relateEntityResolver{})
	_, err := f.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","stateKey":"state.task.status","value":"pending","scope":"galaxy"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

// --- scope is stamped onto the appended event ---

func TestRelate_TeamScope_StampedOnEvent(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})
	if _, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1","scope":"team"}`)); err != nil {
		t.Fatalf("team relate with ULID endpoints should be accepted, got: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	if app.events[0].Scope != kernel.ScopeTeam {
		t.Fatalf("event scope = %q, want %q", app.events[0].Scope, kernel.ScopeTeam)
	}
}

func TestFact_ProjectScope_StampedOnEvent(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	f := tools.NewFact(app, relateEntityResolver{})
	if _, err := f.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","stateKey":"state.task.status","value":"pending","scope":"project"}`)); err != nil {
		t.Fatalf("project fact should be accepted, got: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	if app.events[0].Scope != kernel.ScopeProject {
		t.Fatalf("event scope = %q, want %q", app.events[0].Scope, kernel.ScopeProject)
	}
}

// Default scope (omitted) leaves Event.Scope empty so the event log backfills
// personal — today's behaviour is preserved.
func TestRelate_NoScope_LeavesEventScopeEmpty(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})
	if _, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1"}`)); err != nil {
		t.Fatalf("scopeless relate should be accepted, got: %v", err)
	}
	if app.events[0].Scope != "" {
		t.Fatalf("event scope = %q, want empty (log backfills personal)", app.events[0].Scope)
	}
}

// --- team-scope "self" sentinel guard ---

// With no self-binder wired, the "self" sentinel does not rebind to a
// principal; a team-scope write referencing it must be refused (it would leak a
// device-local literal into a shared scope), while personal scope still accepts
// it.
func TestRelate_TeamScope_SelfSentinel_Rejected(t *testing.T) {
	t.Parallel()
	r := tools.NewRelate(&captureRelateAppender{}, relateEntityResolver{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

func TestRelate_TeamScope_SelfObject_Rejected(t *testing.T) {
	t.Parallel()
	r := tools.NewRelate(&captureRelateAppender{}, relateEntityResolver{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"self","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

func TestFact_TeamScope_SelfSubject_Rejected(t *testing.T) {
	t.Parallel()
	f := tools.NewFact(&captureRelateAppender{}, relateEntityResolver{})
	_, err := f.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","stateKey":"state.task.status","value":"pending","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

// Personal scope still accepts the "self" sentinel verbatim (unchanged path).
func TestFact_PersonalScope_SelfSubject_Accepted(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	f := tools.NewFact(app, relateEntityResolver{})
	if _, err := f.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","stateKey":"preference.coffee.milk","value":"oat","scope":"personal"}`)); err != nil {
		t.Fatalf("personal self fact should be accepted, got: %v", err)
	}
}

// --- Append-time ErrPermission -> E_PERMISSION mapping ---

func TestRelate_NonMemberTeamWrite_Permission(t *testing.T) {
	t.Parallel()
	r := tools.NewRelate(denyingAppender{}, relateEntityResolver{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodePermission)
}

func TestFact_NonMemberTeamWrite_Permission(t *testing.T) {
	t.Parallel()
	f := tools.NewFact(denyingAppender{}, relateEntityResolver{})
	_, err := f.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","stateKey":"state.task.status","value":"pending","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodePermission)
}

// CreateEntity shares the same append boundary: a team-scope mint by a
// non-member must surface as E_PERMISSION via mapAppendError, not the
// misleading E_INTERNAL it returned before. A nil kinds resolver skips the
// registry check for the core "person" kind so the call reaches the append.
func TestCreateEntity_NonMemberTeamWrite_Permission(t *testing.T) {
	t.Parallel()
	c := tools.NewCreateEntity(denyingAppender{}, nil)
	_, err := c.Invoke(context.Background(), json.RawMessage(
		`{"kind":"person","displayName":"Eowyn","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodePermission)
}

// lockedAppender simulates a transient SQLite lock contention on Append, the
// kind of error that used to surface as the misleading E_INTERNAL.
type lockedAppender struct{}

func (lockedAppender) Append(_ context.Context, _ []kernel.Event) ([]kernel.Receipt, error) {
	return nil, fmt.Errorf("eventlog: %w", errors.New("database is locked"))
}

// --- Append-time lock contention -> E_UNAVAILABLE mapping (mapAppendError) ---

// TestMapAppendError_LockContention_Unavailable verifies that a transient
// database-lock error from the event log Append surfaces as E_UNAVAILABLE
// through the relate tool's mapAppendError path, not as the misleading
// E_INTERNAL it produced before this fix.
func TestMapAppendError_LockContention_Unavailable(t *testing.T) {
	t.Parallel()
	r := tools.NewRelate(lockedAppender{}, relateEntityResolver{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1"}`))
	wantRelateCode(t, err, mcp.CodeUnavailable)
}

// TestMapAppendError_Permission_Still_Works asserts the ErrPermission branch is
// unaffected by the lock-contention addition (regression guard).
func TestMapAppendError_Permission_Still_Works(t *testing.T) {
	t.Parallel()
	r := tools.NewRelate(denyingAppender{}, relateEntityResolver{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1","scope":"team"}`))
	wantRelateCode(t, err, mcp.CodePermission)
}

// TestMapAppendError_GenericError_StillInternal guards against over-routing:
// a non-lock, non-permission error must still produce E_INTERNAL.
type failingAppender struct{ msg string }

func (f failingAppender) Append(_ context.Context, _ []kernel.Event) ([]kernel.Receipt, error) {
	return nil, fmt.Errorf("eventlog: %s", f.msg)
}

func TestMapAppendError_GenericError_StillInternal(t *testing.T) {
	t.Parallel()
	r := tools.NewRelate(failingAppender{msg: "disk full"}, relateEntityResolver{})
	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"01ARZ3NDEKTSV4RRFFQ69G5FB0","predicate":"knows","object":"01ARZ3NDEKTSV4RRFFQ69G5FB1"}`))
	wantRelateCode(t, err, mcp.CodeInternal)
}
