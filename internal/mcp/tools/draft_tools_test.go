package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// wantDraftCode asserts err is a *mcp.ToolError carrying the expected code.
func wantDraftCode(t *testing.T, err error, code mcp.ToolErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", code)
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not *mcp.ToolError", err)
	}
	if te.Code != code {
		t.Errorf("error code = %v, want %v", te.Code, code)
	}
}

// fakeNav is a minimal Navigator stub for draft-tool tests. It returns a
// canned episode from GetEpisode when the uid matches draftUID, and
// ErrNotFound otherwise. ListEpisodes returns the canned episode.
type fakeNav struct {
	draftUID string
}

func (f *fakeNav) GetEntity(_ context.Context, _ string) (navigate.Entity, error) {
	return navigate.Entity{}, navigate.ErrNotFound
}

func (f *fakeNav) GetEpisode(_ context.Context, uid string) (navigate.Episode, error) {
	if uid == f.draftUID {
		return navigate.Episode{UID: uid, Text: "test episode"}, nil
	}
	return navigate.Episode{}, navigate.ErrNotFound
}

func (f *fakeNav) GetRelation(_ context.Context, _ string) (navigate.Relation, error) {
	return navigate.Relation{}, navigate.ErrNotFound
}

func (f *fakeNav) EntityRelations(_ context.Context, _ string, _ navigate.RelationFilter) ([]navigate.Relation, error) {
	return nil, nil
}

func (f *fakeNav) Neighbors(_ context.Context, _ string, _ int, _ navigate.RelationFilter) ([]navigate.Neighbor, error) {
	return nil, nil
}

func (f *fakeNav) ListEntities(_ context.Context, _ navigate.EntityFilter) ([]navigate.Entity, error) {
	return nil, nil
}

func (f *fakeNav) CountEntities(_ context.Context, _ navigate.EntityFilter) (int64, error) {
	return 0, nil
}

func (f *fakeNav) AggregateEntities(_ context.Context, _ navigate.AggregateSpec) ([]navigate.GroupCount, error) {
	return nil, nil
}

func (f *fakeNav) EpisodesCitingEntity(_ context.Context, _ string, _ int) ([]navigate.Episode, error) {
	return nil, nil
}

func (f *fakeNav) EventProvenance(_ context.Context, _ string) ([]navigate.Provenance, error) {
	return nil, nil
}

func (f *fakeNav) EntityProvenance(_ context.Context, _ string) ([]navigate.Provenance, error) {
	return nil, nil
}

func (f *fakeNav) Predicates(_ context.Context) ([]navigate.Predicate, error) {
	return nil, nil
}

func (f *fakeNav) EntityKinds(_ context.Context) ([]navigate.KindCount, error) {
	return nil, nil
}

func (f *fakeNav) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	return nil, nil
}

func (f *fakeNav) GetFact(_ context.Context, _ string) (navigate.Fact, error) {
	return navigate.Fact{}, navigate.ErrNotFound
}

func (f *fakeNav) FactsForSubject(_ context.Context, _ string, _ navigate.FactFilter) ([]navigate.Fact, error) {
	return nil, nil
}

func (f *fakeNav) EntityIdentifiers(_ context.Context, _ string) ([]navigate.EntityIdentifier, error) {
	return nil, nil
}

func (f *fakeNav) GetSourceRef(_ context.Context, _ string) (navigate.SourceRef, error) {
	return navigate.SourceRef{}, navigate.ErrNotFound
}

func (f *fakeNav) GetInsight(_ context.Context, _ string) (navigate.Insight, error) {
	return navigate.Insight{}, navigate.ErrNotFound
}

func (f *fakeNav) GetConsolidatedMemory(_ context.Context, _ string) (navigate.ConsolidatedMemory, error) {
	return navigate.ConsolidatedMemory{}, navigate.ErrNotFound
}

func (f *fakeNav) ListEpisodes(_ context.Context, _ navigate.EpisodeFilter) ([]navigate.Episode, error) {
	if f.draftUID != "" {
		return []navigate.Episode{{UID: f.draftUID, Text: "test episode"}}, nil
	}
	return nil, nil
}

const testDraftUID = "01HZZZZZZZZZZZZZZZZZZDRAFT1"

// --- PromoteDraft tests -------------------------------------------------------

func TestPromoteDraft_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{
		receipts: []kernel.Receipt{{UID: "01HZZZZZZZZZZZZZZZZZZPROM01", CreateTime: time.Now().UTC()}},
	}
	nav := &fakeNav{draftUID: testDraftUID}
	tool := tools.NewPromoteDraft(fake, nav)

	req, _ := json.Marshal(map[string]any{"name": "episodes/" + testDraftUID})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got := out["status"]; got != "promoted" {
		t.Errorf("status = %q, want \"promoted\"", got)
	}
	if got := out["toScope"]; got != "personal" {
		t.Errorf("toScope = %q, want \"personal\"", got)
	}
	if !fake.called {
		t.Error("Append was not called")
	}
}

func TestPromoteDraft_NotFoundReturnsCodeNotFound(t *testing.T) {
	t.Parallel()
	nav := &fakeNav{draftUID: "different-uid"}
	tool := tools.NewPromoteDraft(&fakeAppender{}, nav)

	req, _ := json.Marshal(map[string]any{"name": "episodes/" + testDraftUID})
	_, err := tool.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing draft episode, got nil")
	}
	wantDraftCode(t, err, mcp.CodeNotFound)
}

func TestPromoteDraft_MissingNameReturnsValidation(t *testing.T) {
	t.Parallel()
	tool := tools.NewPromoteDraft(&fakeAppender{}, &fakeNav{})

	req, _ := json.Marshal(map[string]any{})
	_, err := tool.Invoke(context.Background(), req)
	wantDraftCode(t, err, mcp.CodeValidation)
}

func TestPromoteDraft_AppendErrorMapsCorrectly(t *testing.T) {
	t.Parallel()
	nav := &fakeNav{draftUID: testDraftUID}
	fake := &fakeAppender{err: kernel.ErrPermission}
	tool := tools.NewPromoteDraft(fake, nav)

	req, _ := json.Marshal(map[string]any{"name": "episodes/" + testDraftUID})
	_, err := tool.Invoke(context.Background(), req)
	wantDraftCode(t, err, mcp.CodePermission)
}

func TestPromoteDraft_Unlisted(t *testing.T) {
	t.Parallel()
	tool := tools.NewPromoteDraft(&fakeAppender{}, &fakeNav{})
	if !tool.Unlisted() {
		t.Error("PromoteDraft should be Unlisted()")
	}
}

// --- DiscardDraft tests -------------------------------------------------------

func TestDiscardDraft_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{
		receipts: []kernel.Receipt{{UID: "01HZZZZZZZZZZZZZZZZZZDISC01", CreateTime: time.Now().UTC()}},
	}
	nav := &fakeNav{draftUID: testDraftUID}
	tool := tools.NewDiscardDraft(fake, nav)

	req, _ := json.Marshal(map[string]any{"name": "episodes/" + testDraftUID, "reason": "testing"})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got := out["status"]; got != "discarded" {
		t.Errorf("status = %q, want \"discarded\"", got)
	}
	if !fake.called {
		t.Error("Append was not called")
	}
}

func TestDiscardDraft_NotFoundReturnsCodeNotFound(t *testing.T) {
	t.Parallel()
	nav := &fakeNav{draftUID: "different-uid"}
	tool := tools.NewDiscardDraft(&fakeAppender{}, nav)

	req, _ := json.Marshal(map[string]any{"name": "episodes/" + testDraftUID})
	_, err := tool.Invoke(context.Background(), req)
	wantDraftCode(t, err, mcp.CodeNotFound)
}

func TestDiscardDraft_MissingNameReturnsValidation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDiscardDraft(&fakeAppender{}, &fakeNav{})

	req, _ := json.Marshal(map[string]any{})
	_, err := tool.Invoke(context.Background(), req)
	wantDraftCode(t, err, mcp.CodeValidation)
}

func TestDiscardDraft_Unlisted(t *testing.T) {
	t.Parallel()
	tool := tools.NewDiscardDraft(&fakeAppender{}, &fakeNav{})
	if !tool.Unlisted() {
		t.Error("DiscardDraft should be Unlisted()")
	}
}

// --- DraftStage tests ---------------------------------------------------------

func TestDraftStage_ListView(t *testing.T) {
	t.Parallel()
	nav := &fakeNav{draftUID: testDraftUID}
	tool := tools.NewDraftStage(nav)

	req, _ := json.Marshal(map[string]any{"view": "list"})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := out["view"]; got != "list" {
		t.Errorf("view = %q, want \"list\"", got)
	}
	if out["draft"] == nil {
		t.Error("draft key missing from list response")
	}
	if out["personal"] != nil {
		t.Error("personal key must be absent in list view")
	}
}

func TestDraftStage_DiffView(t *testing.T) {
	t.Parallel()
	nav := &fakeNav{draftUID: testDraftUID}
	tool := tools.NewDraftStage(nav)

	req, _ := json.Marshal(map[string]any{"view": "diff"})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := out["view"]; got != "diff" {
		t.Errorf("view = %q, want \"diff\"", got)
	}
	if out["draft"] == nil {
		t.Error("draft key missing from diff response")
	}
	// personal is present in diff view (even if empty).
	if out["personal"] == nil {
		t.Error("personal key must be present in diff view")
	}
}

func TestDraftStage_InvalidViewReturnsValidation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDraftStage(&fakeNav{})

	req, _ := json.Marshal(map[string]any{"view": "badview"})
	_, err := tool.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	wantDraftCode(t, err, mcp.CodeValidation)
}

func TestDraftStage_Unlisted(t *testing.T) {
	t.Parallel()
	tool := tools.NewDraftStage(&fakeNav{})
	if !tool.Unlisted() {
		t.Error("DraftStage should be Unlisted()")
	}
}
