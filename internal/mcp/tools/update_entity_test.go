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

// fakeEntityGetter stubs the entityGetter interface for update/merge tool tests.
type fakeEntityGetter struct {
	entity *navigate.Entity
	err    error
}

func (f *fakeEntityGetter) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	if f.err != nil {
		return navigate.Entity{}, f.err
	}
	if f.entity != nil {
		return *f.entity, nil
	}
	for _, e := range []navigate.Entity{} {
		if e.UID == uid {
			return e, nil
		}
	}
	return navigate.Entity{}, navigate.ErrNotFound
}

// existingEntity returns a fakeEntityGetter that resolves any uid to the given entity.
func existingEntity(uid string) *fakeEntityGetter {
	return &fakeEntityGetter{entity: &navigate.Entity{UID: uid, Kind: "person", DisplayName: "Old Name"}}
}

// missingEntity returns a fakeEntityGetter that always returns ErrNotFound.
func missingEntity() *fakeEntityGetter {
	return &fakeEntityGetter{}
}

const testEntityUID = "01HZZZZZZZZZZZZZZZZZZZUPD1"

// TestUpdateEntity_HappyPath_Kind verifies that updating only kind appends one
// entityUpdated event and returns status=updated.
func TestUpdateEntity_HappyPath_Kind(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewUpdateEntity(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": testEntityUID,
		"kind":      "organization",
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
	if out.Status != "updated" {
		t.Errorf("status = %q, want updated", out.Status)
	}
	if out.Name != "entities/"+testEntityUID {
		t.Errorf("name = %q, want entities/%s", out.Name, testEntityUID)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if string(fa.gotEvents[0].Kind) != ontology.KindEntityUpdated {
		t.Errorf("event kind = %q, want %s", fa.gotEvents[0].Kind, ontology.KindEntityUpdated)
	}
}

// TestUpdateEntity_HappyPath_DisplayName verifies updating only displayName.
func TestUpdateEntity_HappyPath_DisplayName(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewUpdateEntity(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid":   testEntityUID,
		"displayName": "New Name",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	var payload map[string]any
	if err := json.Unmarshal(fa.gotEvents[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["displayName"] != "New Name" {
		t.Errorf("payload displayName = %v, want New Name", payload["displayName"])
	}
}

// TestUpdateEntity_NotFound_NoNav returns E_NOT_FOUND when entity is absent.
func TestUpdateEntity_NotFound_NoNav(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewUpdateEntity(fa, missingEntity())

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": "01HZZZZZZZZZZZZZZZZZZZGONE1",
		"kind":      "organization",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on not-found", len(fa.gotEvents))
	}
}

// TestUpdateEntity_Validation_NoFields rejects when neither kind nor displayName given.
func TestUpdateEntity_Validation_NoFields(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewUpdateEntity(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{"entityUid": testEntityUID})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("appended %d events, want 0 on validation failure", len(fa.gotEvents))
	}
}

// TestUpdateEntity_Validation_MissingUID rejects an empty entityUid.
func TestUpdateEntity_Validation_MissingUID(t *testing.T) {
	t.Parallel()
	tool := tools.NewUpdateEntity(&fakeAppender{}, nil)
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"kind":"person"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestUpdateEntity_AppendError_Internal maps a storage failure to E_INTERNAL.
func TestUpdateEntity_AppendError_Internal(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("disk full")}
	tool := tools.NewUpdateEntity(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": testEntityUID,
		"kind":      "organization",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}

// TestUpdateEntity_AppendError_Permission maps ErrPermission to E_PERMISSION.
func TestUpdateEntity_AppendError_Permission(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: kernel.ErrPermission}
	tool := tools.NewUpdateEntity(fa, existingEntity(testEntityUID))

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": testEntityUID,
		"kind":      "organization",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodePermission)
}

// TestUpdateEntity_NilNav_SkipsExistenceCheck skips the existence check when
// no navigator is wired (minimal-boot path).
func TestUpdateEntity_NilNav_SkipsExistenceCheck(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewUpdateEntity(fa, nil)

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": testEntityUID,
		"kind":      "person",
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke with nil nav: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
}
