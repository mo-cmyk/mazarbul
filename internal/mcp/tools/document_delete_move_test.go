package tools

// document_delete_move_test.go: table-driven tests for deleteDocument and
// moveDocument covering the happy path and all documented error codes
// (E_NOT_FOUND, E_VALIDATION).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

// ---- fakes for deleteDocument / moveDocument ----

type fakeDeleter struct {
	deleteResult document.DeleteResult
	deleteErr    error
	moveResult   document.MoveResult
	moveErr      error

	// gotReason records the reason argument the last Delete call received, so
	// tests can assert the MCP tool forwards a caller-supplied audit reason.
	gotReason string
	// gotScope records the scope disambiguator the last Delete/Move call received,
	// so tests can assert the MCP tool threads a caller-supplied scope through.
	gotScope string
}

func (f *fakeDeleter) Delete(_ context.Context, _, _, reason, scope string) (document.DeleteResult, error) {
	f.gotReason = reason
	f.gotScope = scope
	return f.deleteResult, f.deleteErr
}

func (f *fakeDeleter) Move(_ context.Context, _, _, _, scope string) (document.MoveResult, error) {
	f.gotScope = scope
	return f.moveResult, f.moveErr
}

// fakeMoveProjector satisfies moveProjector with no-op vault operations and
// records SyncMove calls so tests can assert an idempotent no-op retry never
// triggers a (potentially destructive) materialization.
type fakeMoveProjector struct {
	fakeVaultProjector
	syncMoveCalls int
}

type fakeVaultProjector struct{}

func (fakeVaultProjector) SyncAll(_ context.Context) (vault.Stats, error) { return vault.Stats{}, nil }
func (fakeVaultProjector) SyncPath(_ context.Context, _, _ string) (vault.Stats, error) {
	return vault.Stats{}, nil
}

func (fakeVaultProjector) FilePath(rel, scope string) (string, error) {
	return "/vault/" + scope + "/" + rel, nil
}
func (fakeVaultProjector) Root() string { return "/vault" }

func (f *fakeMoveProjector) SyncMove(_ context.Context, _, _, _ string) (vault.Stats, error) {
	f.syncMoveCalls++
	return vault.Stats{}, nil
}

// ---- deleteDocument tests ----

func TestDeleteDocument_HappyPath(t *testing.T) {
	deleter := &fakeDeleter{
		deleteResult: document.DeleteResult{
			DocumentUID: "01HXYZ",
			Path:        "notes/gone.md",
			DisplayName: "Gone",
			Status:      "deleted",
		},
	}
	tool := NewDeleteDocument(deleter, nil)

	req, _ := json.Marshal(map[string]any{"path": "notes/gone.md"})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Status string `json:"status"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "deleted" {
		t.Errorf("status = %q, want deleted", out.Status)
	}
	if out.Path != "notes/gone.md" {
		t.Errorf("path = %q, want notes/gone.md", out.Path)
	}
}

func TestDeleteDocument_NotFound(t *testing.T) {
	deleter := &fakeDeleter{
		deleteErr: errors.Join(errors.New("no document"), navigate.ErrNotFound),
	}
	tool := NewDeleteDocument(deleter, nil)

	req, _ := json.Marshal(map[string]any{"path": "notes/ghost.md"})
	_, err := tool.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("want error for not-found, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("want ToolError, got %T: %v", err, err)
	}
	if te.Code != mcp.CodeNotFound {
		t.Errorf("code = %q, want CodeNotFound (%q)", te.Code, mcp.CodeNotFound)
	}
}

func TestDeleteDocument_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		req  map[string]any
	}{
		{"both path and uid", map[string]any{"path": "a.md", "documentUid": "01ABC"}},
		{"neither path nor uid", map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewDeleteDocument(&fakeDeleter{}, nil)
			req, _ := json.Marshal(tc.req)
			_, err := tool.Invoke(context.Background(), req)
			if err == nil {
				t.Fatal("want validation error, got nil")
			}
			var te *mcp.ToolError
			if !errors.As(err, &te) {
				t.Fatalf("want ToolError, got %T", err)
			}
			if te.Code != mcp.CodeValidation {
				t.Errorf("code = %q, want CodeValidation (%q)", te.Code, mcp.CodeValidation)
			}
		})
	}
}

func TestDeleteDocument_Unlisted(t *testing.T) {
	tool := NewDeleteDocument(&fakeDeleter{}, nil)
	if !tool.Unlisted() {
		t.Error("deleteDocument must be unlisted")
	}
}

// TestDeleteDocument_ForwardsReason verifies that a caller-supplied audit
// reason is passed through to the importer rather than dropped (the input
// schema documents reason as stored in the event log for audit).
func TestDeleteDocument_ForwardsReason(t *testing.T) {
	deleter := &fakeDeleter{
		deleteResult: document.DeleteResult{
			DocumentUID: "01HXYZ", Path: "notes/gone.md", Status: "deleted",
		},
	}
	tool := NewDeleteDocument(deleter, nil)

	req, _ := json.Marshal(map[string]any{"path": "notes/gone.md", "reason": "duplicate of notes/keep.md"})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if deleter.gotReason != "duplicate of notes/keep.md" {
		t.Errorf("forwarded reason = %q, want %q", deleter.gotReason, "duplicate of notes/keep.md")
	}
}

// TestDeleteDocument_ForwardsScope verifies the tool threads a caller-supplied
// scope disambiguator to the importer, so a delete can target one of several
// same-path documents; an unknown scope maps to E_VALIDATION (parseWriteScope).
func TestDeleteDocument_ForwardsScope(t *testing.T) {
	deleter := &fakeDeleter{deleteResult: document.DeleteResult{DocumentUID: "01HXYZ", Path: "notes/dup.md", Status: "deleted"}}
	tool := NewDeleteDocument(deleter, nil)

	// scope must be declared in the input schema (validated before Invoke).
	if !strings.Contains(string(tool.InputSchema()), `"scope"`) {
		t.Fatal("deleteDocument input schema does not declare scope; a caller's scope would be rejected before Invoke")
	}

	req, _ := json.Marshal(map[string]any{"path": "notes/dup.md", "scope": "team"})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if deleter.gotScope != "team" {
		t.Errorf("forwarded scope = %q, want team", deleter.gotScope)
	}

	bad, _ := json.Marshal(map[string]any{"path": "notes/dup.md", "scope": "galaxy"})
	_, err := tool.Invoke(context.Background(), bad)
	var te *mcp.ToolError
	if !errors.As(err, &te) || te.Code != mcp.CodeValidation {
		t.Errorf("invalid scope: got err %v, want E_VALIDATION", err)
	}
}

// TestMoveDocument_ForwardsScope verifies moveDocument threads the scope
// disambiguator to the importer's Move.
func TestMoveDocument_ForwardsScope(t *testing.T) {
	mover := &fakeDeleter{moveResult: document.MoveResult{
		DocumentUID: "01HXYZ", OldPath: "notes/dup.md", NewPath: "notes/moved.md", RevisionUID: "01HNEWREV", Scope: "team", Status: "moved",
	}}
	tool := NewMoveDocument(nil, mover, &fakeMoveProjector{})

	// The scope disambiguator must be DECLARED in the input schema: the MCP server
	// validates input against InputSchema() (additionalProperties:false) BEFORE
	// Invoke runs, so a struct field the schema omits is unreachable end-to-end.
	if !strings.Contains(string(tool.InputSchema()), `"scope"`) {
		t.Fatal("moveDocument input schema does not declare scope; a caller's scope would be rejected before Invoke")
	}

	req, _ := json.Marshal(map[string]any{"path": "notes/dup.md", "newPath": "notes/moved.md", "scope": "team"})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if mover.gotScope != "team" {
		t.Errorf("forwarded scope = %q, want team", mover.gotScope)
	}
}

// TestMapDeleteMoveErr_PathValidation verifies that CleanVaultPath validation
// errors (absolute path, non-markdown extension, vault escape, empty path) are
// reported as E_VALIDATION rather than E_INTERNAL, and that a genuinely
// internal error still maps to E_INTERNAL.
func TestMapDeleteMoveErr_PathValidation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want mcp.ToolErrorCode
	}{
		{"absolute path", errors.New(`document path "/etc/x.md" must be vault-relative, not absolute`), mcp.CodeValidation},
		{"non markdown", errors.New(`document path "notes/x.png": only markdown documents (.md, .markdown) are supported`), mcp.CodeValidation},
		{"escapes root", errors.New(`document path "../x.md" escapes the vault root`), mcp.CodeValidation},
		{"empty path", errors.New("document path is empty"), mcp.CodeValidation},
		{"collision", errors.New(`a document already exists at "notes/x.md" (uid 01ABC); choose a different path`), mcp.CodeValidation},
		{"same path", errors.New(`newPath is the same as the current path "notes/x.md"`), mcp.CodeValidation},
		{"not found", errors.Join(errors.New("no document"), navigate.ErrNotFound), mcp.CodeNotFound},
		{"internal", errors.New("append events: disk full"), mcp.CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := mapDeleteMoveErr(tc.err)
			var te *mcp.ToolError
			if !errors.As(err, &te) {
				t.Fatalf("want ToolError, got %T: %v", err, err)
			}
			if te.Code != tc.want {
				t.Errorf("code = %q, want %q", te.Code, tc.want)
			}
		})
	}
}

// ---- moveDocument tests ----

func TestMoveDocument_HappyPath(t *testing.T) {
	mover := &fakeDeleter{
		moveResult: document.MoveResult{
			DocumentUID: "01HXYZ",
			OldPath:     "notes/old.md",
			NewPath:     "notes/new.md",
			RevisionUID: "01HNEWREV",
			Status:      "moved",
		},
	}
	proj := &fakeMoveProjector{}
	tool := NewMoveDocument(nil, mover, proj)

	req, _ := json.Marshal(map[string]any{"path": "notes/old.md", "newPath": "notes/new.md"})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "moved" {
		t.Errorf("status = %q, want moved", out.Status)
	}
	if out.OldPath != "notes/old.md" || out.NewPath != "notes/new.md" {
		t.Errorf("paths = %q → %q, want notes/old.md → notes/new.md", out.OldPath, out.NewPath)
	}
	if proj.syncMoveCalls != 1 {
		t.Errorf("SyncMove calls = %d, want 1 for a real move", proj.syncMoveCalls)
	}
}

// TestMoveDocument_NoOpRetryStillMaterializes verifies that an idempotent
// move-by-uid retry (OldPath == NewPath) STILL calls SyncMove. A prior move
// could have appended the revision and then failed during SyncMove (projection
// errors are non-fatal), leaving the new file un-written; the retry must repair
// the vault. Projector.SyncMove handles equal paths safely (write/adopt, never
// remove — see TestSyncMove_SamePathKeepsFile), so this is safe.
func TestMoveDocument_NoOpRetryStillMaterializes(t *testing.T) {
	mover := &fakeDeleter{
		moveResult: document.MoveResult{
			DocumentUID: "01HXYZ",
			OldPath:     "notes/same.md",
			NewPath:     "notes/same.md",
			RevisionUID: "01HOLDREV",
			Status:      "moved",
		},
	}
	proj := &fakeMoveProjector{}
	tool := NewMoveDocument(nil, mover, proj)

	req, _ := json.Marshal(map[string]any{"documentUid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "newPath": "notes/same.md"})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if proj.syncMoveCalls != 1 {
		t.Errorf("SyncMove calls = %d, want 1 (retry must repair the vault)", proj.syncMoveCalls)
	}
}

func TestMoveDocument_NotFound(t *testing.T) {
	mover := &fakeDeleter{
		moveErr: errors.Join(errors.New("no document"), navigate.ErrNotFound),
	}
	tool := NewMoveDocument(nil, mover, &fakeMoveProjector{})

	req, _ := json.Marshal(map[string]any{"path": "notes/ghost.md", "newPath": "notes/dest.md"})
	_, err := tool.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("want error for not-found, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("want ToolError, got %T", err)
	}
	if te.Code != mcp.CodeNotFound {
		t.Errorf("code = %q, want CodeNotFound (%q)", te.Code, mcp.CodeNotFound)
	}
}

func TestMoveDocument_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		req  map[string]any
	}{
		{"missing newPath", map[string]any{"path": "notes/old.md"}},
		{"both path and uid", map[string]any{"path": "a.md", "documentUid": "01ABC", "newPath": "b.md"}},
		{"neither path nor uid", map[string]any{"newPath": "b.md"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewMoveDocument(nil, &fakeDeleter{}, &fakeMoveProjector{})
			req, _ := json.Marshal(tc.req)
			_, err := tool.Invoke(context.Background(), req)
			if err == nil {
				t.Fatal("want validation error, got nil")
			}
			var te *mcp.ToolError
			if !errors.As(err, &te) {
				t.Fatalf("want ToolError, got %T", err)
			}
			if te.Code != mcp.CodeValidation {
				t.Errorf("code = %q, want CodeValidation (%q)", te.Code, mcp.CodeValidation)
			}
		})
	}
}

func TestMoveDocument_ConflictMapsToValidation(t *testing.T) {
	mover := &fakeDeleter{
		moveErr: errors.New("a document already exists at \"notes/b.md\""),
	}
	tool := NewMoveDocument(nil, mover, &fakeMoveProjector{})

	req, _ := json.Marshal(map[string]any{"path": "notes/a.md", "newPath": "notes/b.md"})
	_, err := tool.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("want error for collision, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("want ToolError, got %T", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("code = %q, want CodeValidation (%q)", te.Code, mcp.CodeValidation)
	}
}

func TestMoveDocument_Unlisted(t *testing.T) {
	tool := NewMoveDocument(nil, &fakeDeleter{}, &fakeMoveProjector{})
	if !tool.Unlisted() {
		t.Error("moveDocument must be unlisted")
	}
}
