package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const (
	testSrcUID = "01HZZZZZZZZZZZZZZZZZZMERGS"
	testTgtUID = "01HZZZZZZZZZZZZZZZZZZMERGT"
)

// multiEntityGetter returns a fakeEntityGetter that resolves a fixed set of UIDs.
type multiEntityGetter struct {
	known map[string]navigate.Entity
}

func (m *multiEntityGetter) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	if e, ok := m.known[uid]; ok {
		return e, nil
	}
	return navigate.Entity{}, navigate.ErrNotFound
}

func bothExist() *multiEntityGetter {
	return &multiEntityGetter{known: map[string]navigate.Entity{
		testSrcUID: {UID: testSrcUID, Kind: "person", DisplayName: "Duplicate"},
		testTgtUID: {UID: testTgtUID, Kind: "person", DisplayName: "Canonical"},
	}}
}

func onlyTarget() *multiEntityGetter {
	return &multiEntityGetter{known: map[string]navigate.Entity{
		testTgtUID: {UID: testTgtUID, Kind: "person", DisplayName: "Canonical"},
	}}
}

// TestMergeEntity_HappyPath_Merged verifies a clean merge appends one
// entityMerged event and returns status=merged with the target resource name.
func TestMergeEntity_HappyPath_Merged(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewMergeEntity(fa, bothExist())

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testTgtUID,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "merged" {
		t.Errorf("status = %q, want merged", out.Status)
	}
	if out.Name != "entities/"+testTgtUID {
		t.Errorf("name = %q, want entities/%s", out.Name, testTgtUID)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if string(fa.gotEvents[0].Kind) != ontology.KindEntityMerged {
		t.Errorf("event kind = %q, want %s", fa.gotEvents[0].Kind, ontology.KindEntityMerged)
	}
}

// TestMergeEntity_Validation_SameUID rejects source==target with E_VALIDATION.
func TestMergeEntity_Validation_SameUID(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewMergeEntity(fa, bothExist())

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testSrcUID,
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on validation failure", len(fa.gotEvents))
	}
}

// TestMergeEntity_NotFound_SourceMissing returns E_NOT_FOUND when source is absent.
func TestMergeEntity_NotFound_SourceMissing(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewMergeEntity(fa, onlyTarget())

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testTgtUID,
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on not-found", len(fa.gotEvents))
	}
}

// TestMergeEntity_NotFound_TargetMissing returns E_NOT_FOUND when target is absent.
func TestMergeEntity_NotFound_TargetMissing(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	// nav knows source but not target
	nav := &multiEntityGetter{known: map[string]navigate.Entity{
		testSrcUID: {UID: testSrcUID, Kind: "person", DisplayName: "Duplicate"},
	}}
	tool := tools.NewMergeEntity(fa, nav)

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testTgtUID,
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
}

// TestMergeEntity_Validation_MissingFields rejects when required fields are absent.
func TestMergeEntity_Validation_MissingFields(t *testing.T) {
	t.Parallel()
	tool := tools.NewMergeEntity(&fakeAppender{}, nil)
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"sourceEntityUid":""}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestMergeEntity_AppendError_Internal maps a storage failure to E_INTERNAL.
func TestMergeEntity_AppendError_Internal(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("storage unavailable")}
	tool := tools.NewMergeEntity(fa, bothExist())

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testTgtUID,
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}

// TestMergeEntity_AppendError_Permission maps ErrPermission to E_PERMISSION.
func TestMergeEntity_AppendError_Permission(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: kernel.ErrPermission}
	tool := tools.NewMergeEntity(fa, bothExist())

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testTgtUID,
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodePermission)
}

// TestMergeEntity_StampsSourceScope verifies the entityMerged event carries the
// SOURCE entity's storage scope. A merge deletes the source and repoints its
// data onto the target, so — like deleteEntity — the operation must be gated in
// the scope of the entity being removed: merging a team-scoped entity away must
// not slip through the personal default Append applies to an unset scope.
func TestMergeEntity_StampsSourceScope(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	nav := &multiEntityGetter{known: map[string]navigate.Entity{
		testSrcUID: {UID: testSrcUID, Kind: "person", DisplayName: "Duplicate", Scope: "team"},
		testTgtUID: {UID: testTgtUID, Kind: "person", DisplayName: "Canonical", Scope: "personal"},
	}}
	tool := tools.NewMergeEntity(fa, nav)

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testTgtUID,
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if fa.gotEvents[0].Scope != kernel.ScopeTeam {
		t.Errorf("entityMerged scope = %q, want %q (the source entity's scope)", fa.gotEvents[0].Scope, kernel.ScopeTeam)
	}
}

// TestMergeEntity_NilNav_SkipsExistenceCheck accepts without a navigator
// (minimal-boot / test-fake path).
func TestMergeEntity_NilNav_SkipsExistenceCheck(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewMergeEntity(fa, nil)

	req := mustMarshalJSON(t, map[string]any{
		"sourceEntityUid": testSrcUID,
		"targetEntityUid": testTgtUID,
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke with nil nav: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
}
