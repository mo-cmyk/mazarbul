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

// deleteNav is a test double for the entityDeleteNav interface that
// DeleteEntity uses to confirm existence and enumerate active relations.
type deleteNav struct {
	entity navigate.Entity
	rels   []navigate.Relation
	getErr error
	relErr error
}

func (d *deleteNav) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	if d.getErr != nil {
		return navigate.Entity{}, d.getErr
	}
	if d.entity.UID == uid {
		return d.entity, nil
	}
	return navigate.Entity{}, navigate.ErrNotFound
}

func (d *deleteNav) EntityRelations(_ context.Context, _ string, _ navigate.RelationFilter) ([]navigate.Relation, error) {
	if d.relErr != nil {
		return nil, d.relErr
	}
	return d.rels, nil
}

const (
	testDeleteUID = "01HZZZZZZZZZZZZZZZZZDELETE"
	testRelUID1   = "01HZZZZZZZZZZZZZZZZZRREL01"
	testRelUID2   = "01HZZZZZZZZZZZZZZZZZRREL02"
)

// orphanNav returns a deleteNav for an entity with no active relations.
func orphanNav() *deleteNav {
	return &deleteNav{
		entity: navigate.Entity{UID: testDeleteUID, Kind: "person", DisplayName: "Orphan"},
		rels:   nil,
	}
}

// connectedNav returns a deleteNav for an entity with two active relations.
func connectedNav() *deleteNav {
	return &deleteNav{
		entity: navigate.Entity{UID: testDeleteUID, Kind: "person", DisplayName: "Connected"},
		rels: []navigate.Relation{
			{UID: testRelUID1, SubjectUID: testDeleteUID, Predicate: "knows", ObjectUID: "01HZZZZZZZZZZZZZZZZZOTHER1"},
			{UID: testRelUID2, SubjectUID: "01HZZZZZZZZZZZZZZZZZOTHER2", Predicate: "employs", ObjectUID: testDeleteUID},
		},
	}
}

// deleteAppender is a recording Appender for delete tests that always
// returns a single receipt so the "no receipt" guard does not fire.
type deleteAppender struct {
	events []kernel.Event
}

func (d *deleteAppender) Append(_ context.Context, evs []kernel.Event) ([]kernel.Receipt, error) {
	d.events = append(d.events, evs...)
	return []kernel.Receipt{{}}, nil
}

// TestDeleteEntity_DeletesOrphanEntity verifies that deleting an entity with
// no active relations appends exactly one entityDeleted event and returns
// status "deleted" with forgottenRelations==0.
func TestDeleteEntity_DeletesOrphanEntity(t *testing.T) {
	t.Parallel()
	rec := &deleteAppender{}
	tool := tools.NewDeleteEntity(rec, orphanNav())

	req := mustMarshalJSON(t, map[string]any{"entityUid": testDeleteUID})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name               string `json:"name"`
		Status             string `json:"status"`
		ForgottenRelations int    `json:"forgottenRelations"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "deleted" {
		t.Errorf("status = %q, want deleted", out.Status)
	}
	if out.Name != "entities/"+testDeleteUID {
		t.Errorf("name = %q, want entities/%s", out.Name, testDeleteUID)
	}
	if out.ForgottenRelations != 0 {
		t.Errorf("forgottenRelations = %d, want 0", out.ForgottenRelations)
	}
	if len(rec.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(rec.events))
	}
	if string(rec.events[0].Kind) != ontology.KindEntityDeleted {
		t.Errorf("event kind = %q, want %s", rec.events[0].Kind, ontology.KindEntityDeleted)
	}
}

// TestDeleteEntity_RefusesActiveRelationsWithoutCascade verifies E_VALIDATION
// is returned (code assert included) when the entity has active relations
// and cascade=false, and that no event is appended.
func TestDeleteEntity_RefusesActiveRelationsWithoutCascade(t *testing.T) {
	t.Parallel()
	rec := &deleteAppender{}
	tool := tools.NewDeleteEntity(rec, connectedNav())

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": testDeleteUID,
		"cascade":   false,
	})
	_, err := tool.Invoke(context.Background(), req)

	// E_VALIDATION is required by convention; assert the code explicitly.
	wantCode(t, err, mcp.CodeValidation)

	if len(rec.events) != 0 {
		t.Errorf("appended %d events, want 0 when refused", len(rec.events))
	}
}

// TestDeleteEntity_CascadeTombstonesRelationsThenDeletes verifies that with
// cascade=true and two active relations the batch is:
// [relationForgotten, relationForgotten, entityDeleted] and the output carries
// forgottenRelations==2.
func TestDeleteEntity_CascadeTombstonesRelationsThenDeletes(t *testing.T) {
	t.Parallel()
	rec := &deleteAppender{}
	tool := tools.NewDeleteEntity(rec, connectedNav())

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": testDeleteUID,
		"cascade":   true,
		"reason":    "test cascade",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status             string `json:"status"`
		ForgottenRelations int    `json:"forgottenRelations"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "deleted" {
		t.Errorf("status = %q, want deleted", out.Status)
	}
	if out.ForgottenRelations != 2 {
		t.Errorf("forgottenRelations = %d, want 2", out.ForgottenRelations)
	}

	// Verify event batch order: 2 x relationForgotten then 1 x entityDeleted.
	if len(rec.events) != 3 {
		t.Fatalf("appended %d events, want 3 (2 relationForgotten + 1 entityDeleted)", len(rec.events))
	}
	for i, ev := range rec.events[:2] {
		if string(ev.Kind) != ontology.KindRelationForgotten {
			t.Errorf("event[%d] kind = %q, want %s", i, ev.Kind, ontology.KindRelationForgotten)
		}
	}
	if string(rec.events[2].Kind) != ontology.KindEntityDeleted {
		t.Errorf("event[2] kind = %q, want %s", rec.events[2].Kind, ontology.KindEntityDeleted)
	}
}

// TestDeleteEntity_UnknownEntityIsNotFound verifies E_NOT_FOUND is returned
// (code assert included) when GetEntity returns ErrNotFound, and no event
// is appended.
func TestDeleteEntity_UnknownEntityIsNotFound(t *testing.T) {
	t.Parallel()
	// nav knows about a different UID; testDeleteUID will hit ErrNotFound.
	nav := &deleteNav{
		entity: navigate.Entity{UID: "01HZZZZZZZZZZZZZZZZZOTHER3", Kind: "person"},
	}
	rec := &deleteAppender{}
	tool := tools.NewDeleteEntity(rec, nav)

	req := mustMarshalJSON(t, map[string]any{"entityUid": testDeleteUID})
	_, err := tool.Invoke(context.Background(), req)

	// E_NOT_FOUND is required by convention; assert the code explicitly.
	wantCode(t, err, mcp.CodeNotFound)

	if len(rec.events) != 0 {
		t.Errorf("appended %d events, want 0 on not-found", len(rec.events))
	}
}

// TestDeleteEntity_NilNavSkipsGuard verifies that when no nav is wired
// NewDeleteEntity(rec, nil) the existence and active-edge guards are skipped
// and the delete proceeds (status "deleted").
func TestDeleteEntity_NilNavSkipsGuard(t *testing.T) {
	t.Parallel()
	rec := &deleteAppender{}
	tool := tools.NewDeleteEntity(rec, nil)

	req := mustMarshalJSON(t, map[string]any{"entityUid": testDeleteUID})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke with nil nav: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "deleted" {
		t.Errorf("status = %q, want deleted (nil nav skips guard)", out.Status)
	}
	if len(rec.events) == 0 {
		t.Error("no events appended, want entityDeleted")
	}
}

// TestDeleteEntity_ErrorCodeMapping asserts E_VALIDATION and E_NOT_FOUND are
// surfaced with their documented codes per repo conventions.
func TestDeleteEntity_ErrorCodeMapping(t *testing.T) {
	t.Parallel()

	t.Run("E VALIDATION on active relations without cascade", func(t *testing.T) {
		t.Parallel()
		rec := &deleteAppender{}
		tool := tools.NewDeleteEntity(rec, connectedNav())
		req := mustMarshalJSON(t, map[string]any{"entityUid": testDeleteUID})
		_, err := tool.Invoke(context.Background(), req)
		wantCode(t, err, mcp.CodeValidation)
	})

	t.Run("E NOT FOUND on unknown entity uid", func(t *testing.T) {
		t.Parallel()
		nav := &deleteNav{getErr: navigate.ErrNotFound}
		rec := &deleteAppender{}
		tool := tools.NewDeleteEntity(rec, nav)
		req := mustMarshalJSON(t, map[string]any{"entityUid": testDeleteUID})
		_, err := tool.Invoke(context.Background(), req)
		wantCode(t, err, mcp.CodeNotFound)
	})

	// An Append that fails with ErrPermission (e.g. a non-member deleting a
	// team-scoped entity) must surface E_PERMISSION via mapAppendError, not a
	// bare internal error — mirrors mergeEntities' permission mapping. The
	// entity is an orphan so the active-relation guard passes and Append runs.
	t.Run("E PERMISSION on append permission denied", func(t *testing.T) {
		t.Parallel()
		fa := &fakeAppender{err: kernel.ErrPermission}
		tool := tools.NewDeleteEntity(fa, orphanNav())
		req := mustMarshalJSON(t, map[string]any{"entityUid": testDeleteUID})
		_, err := tool.Invoke(context.Background(), req)
		wantCode(t, err, mcp.CodePermission)
	})
}

// TestDeleteEntity_StampsEntityScopeOnTombstones verifies that every tombstone
// the cascade delete appends — the per-edge relationForgotten events AND the
// final entityDeleted — carries the deleted entity's storage scope. Deleting a
// team-scoped entity (and cascading its edges) must be gated in team scope, not
// the personal default Append applies to an unset scope, so a non-member cannot
// destroy team data via the personal lane.
func TestDeleteEntity_StampsEntityScopeOnTombstones(t *testing.T) {
	t.Parallel()
	rec := &deleteAppender{}
	nav := &deleteNav{
		entity: navigate.Entity{UID: testDeleteUID, Kind: "person", DisplayName: "TeamPerson", Scope: "team"},
		rels: []navigate.Relation{
			{UID: testRelUID1, SubjectUID: testDeleteUID, Predicate: "knows", ObjectUID: "01HZZZZZZZZZZZZZZZZZOTHER1"},
			{UID: testRelUID2, SubjectUID: "01HZZZZZZZZZZZZZZZZZOTHER2", Predicate: "employs", ObjectUID: testDeleteUID},
		},
	}
	tool := tools.NewDeleteEntity(rec, nav)

	req := mustMarshalJSON(t, map[string]any{
		"entityUid": testDeleteUID,
		"cascade":   true,
	})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(rec.events) != 3 {
		t.Fatalf("appended %d events, want 3 (2 relationForgotten + 1 entityDeleted)", len(rec.events))
	}
	for i, ev := range rec.events {
		if ev.Scope != kernel.ScopeTeam {
			t.Errorf("event[%d] (%s) scope = %q, want %q", i, ev.Kind, ev.Scope, kernel.ScopeTeam)
		}
	}
}

// Ensure ontology event kind constants referenced in assertions are reachable.
var (
	_ = ontology.KindEntityDeleted
	_ = ontology.KindRelationForgotten
)

// Ensure errors package is used via ErrNotFound wrapping in deleteNav.
var _ = errors.New
