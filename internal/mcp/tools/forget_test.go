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

// fakeChecker satisfies the (unexported) existenceChecker the Forget tool
// consumes. found controls whether the target resolves; when false both getters
// return navigate.ErrNotFound, exercising the "nothing to forget" guard.
type fakeChecker struct{ found bool }

func (c fakeChecker) GetEpisode(context.Context, string) (navigate.Episode, error) {
	if c.found {
		return navigate.Episode{}, nil
	}
	return navigate.Episode{}, navigate.ErrNotFound
}

func (c fakeChecker) GetRelation(context.Context, string) (navigate.Relation, error) {
	if c.found {
		return navigate.Relation{}, nil
	}
	return navigate.Relation{}, navigate.ErrNotFound
}

func (c fakeChecker) GetFact(context.Context, string) (navigate.Fact, error) {
	if c.found {
		return navigate.Fact{}, nil
	}
	return navigate.Fact{}, navigate.ErrNotFound
}

func TestForget_Metadata(t *testing.T) {
	t.Parallel()
	f := tools.NewForget(&fakeAppender{})
	if got := f.Name(); got != "forget" {
		t.Errorf("Name() = %q, want %q", got, "forget")
	}
	if f.Description() == "" {
		t.Error("Description() is empty")
	}
	if len(f.InputSchema()) == 0 {
		t.Error("InputSchema() is empty")
	}
	if len(f.OutputSchema()) == 0 {
		t.Error("OutputSchema() is empty")
	}
}

func TestForget_AppendsTombstoneEvent(t *testing.T) {
	t.Parallel()
	const uid = "01HZZZZZZZZZZZZZZZZZZZFORG1"
	fake := &fakeAppender{
		receipts: []kernel.Receipt{{UID: uid}},
	}
	f := tools.NewForget(fake)

	args := mustMarshalJSON(t, map[string]any{
		"episodeUid": uid,
		"reason":     "user invocation",
	})
	body, err := f.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if !fake.called {
		t.Fatal("appender not called")
	}
	if len(fake.gotEvents) != 1 {
		t.Fatalf("got %d events, want 1", len(fake.gotEvents))
	}
	ev := fake.gotEvents[0]
	if ev.Kind != ontology.KindEpisodeForgotten {
		t.Errorf("event kind = %q, want %q", ev.Kind, ontology.KindEpisodeForgotten)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["name"] != "episodes/"+uid {
		t.Errorf("name = %q, want %q", got["name"], "episodes/"+uid)
	}
	if got["status"] != "forgotten" {
		t.Errorf("status = %q, want %q", got["status"], "forgotten")
	}

	var payload ontology.EpisodeForgottenEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.EpisodeUID != uid {
		t.Errorf("payload EpisodeUID = %q, want %q", payload.EpisodeUID, uid)
	}
	if payload.Reason != "user invocation" {
		t.Errorf("payload Reason = %q, want %q", payload.Reason, "user invocation")
	}
	if payload.ForgottenAt.IsZero() {
		t.Error("payload ForgottenAt is zero; tool should stamp time.Now().UTC()")
	}
}

func TestForget_NonexistentEpisode_NotFound_NoTombstone(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{}
	f := tools.NewForget(fake)
	f.SetExistenceChecker(fakeChecker{found: false})

	args := mustMarshalJSON(t, map[string]any{"episodeUid": "01HZZZZZZZZZZZZZZZZZGHOST1"})
	_, err := f.Invoke(context.Background(), args)
	var terr *mcp.ToolError
	if !errors.As(err, &terr) || terr.Code != mcp.CodeNotFound {
		t.Fatalf("error = %v, want E_NOT_FOUND", err)
	}
	if fake.called {
		t.Error("appender must NOT be called for a non-existent episode (no fabricated tombstone)")
	}
}

func TestForget_NonexistentRelation_NotFound_NoTombstone(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{}
	f := tools.NewForget(fake)
	f.SetExistenceChecker(fakeChecker{found: false})

	args := mustMarshalJSON(t, map[string]any{"relationUid": "01HZZZZZZZZZZZZZZZZGHOST2"})
	_, err := f.Invoke(context.Background(), args)
	var terr *mcp.ToolError
	if !errors.As(err, &terr) || terr.Code != mcp.CodeNotFound {
		t.Fatalf("error = %v, want E_NOT_FOUND", err)
	}
	if fake.called {
		t.Error("appender must NOT be called for a non-existent relation")
	}
}

func TestForget_Fact_WithChecker_Tombstones(t *testing.T) {
	t.Parallel()
	const uid = "01HZZZZZZZZZZZZZZZZZZFACT9"
	fake := &fakeAppender{receipts: []kernel.Receipt{{UID: uid}}}
	f := tools.NewForget(fake)
	f.SetExistenceChecker(fakeChecker{found: true})

	args := mustMarshalJSON(t, map[string]any{"factUid": uid, "reason": "wrong slot"})
	body, err := f.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.gotEvents) != 1 {
		t.Fatalf("got %d events, want 1", len(fake.gotEvents))
	}
	if fake.gotEvents[0].Kind != ontology.KindFactForgotten {
		t.Errorf("event kind = %q, want %q", fake.gotEvents[0].Kind, ontology.KindFactForgotten)
	}
	var got map[string]any
	if uerr := json.Unmarshal(body, &got); uerr != nil {
		t.Fatalf("decode response: %v", uerr)
	}
	if got["name"] != "facts/"+uid {
		t.Errorf("name = %q, want facts/%s", got["name"], uid)
	}
	if got["status"] != "forgotten" {
		t.Errorf("status = %q, want forgotten", got["status"])
	}
	var payload ontology.FactForgottenEvent
	if uerr := json.Unmarshal(fake.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("decode payload: %v", uerr)
	}
	if payload.FactUID != uid {
		t.Errorf("payload FactUID = %q, want %q", payload.FactUID, uid)
	}
	if payload.Reason != "wrong slot" {
		t.Errorf("payload Reason = %q, want %q", payload.Reason, "wrong slot")
	}
	if payload.ForgottenAt.IsZero() {
		t.Error("payload ForgottenAt is zero")
	}
}

func TestForget_NonexistentFact_NotFound_NoTombstone(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{}
	f := tools.NewForget(fake)
	f.SetExistenceChecker(fakeChecker{found: false})

	args := mustMarshalJSON(t, map[string]any{"factUid": "01HZZZZZZZZZZZZZZZZGHOST3"})
	_, err := f.Invoke(context.Background(), args)
	var terr *mcp.ToolError
	if !errors.As(err, &terr) || terr.Code != mcp.CodeNotFound {
		t.Fatalf("error = %v, want E_NOT_FOUND", err)
	}
	if fake.called {
		t.Error("appender must NOT be called for a non-existent fact")
	}
}

func TestForget_MutuallyExclusiveModes(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{}
	f := tools.NewForget(fake)

	args := mustMarshalJSON(t, map[string]any{
		"episodeUid": "01HZZZZZZZZZZZZZZZZZZLIVE1",
		"factUid":    "01HZZZZZZZZZZZZZZZZZZFACT9",
	})
	_, err := f.Invoke(context.Background(), args)
	var terr *mcp.ToolError
	if !errors.As(err, &terr) || terr.Code != mcp.CodeValidation {
		t.Fatalf("error = %v, want E_VALIDATION", err)
	}
	if fake.called {
		t.Error("appender should not be called when modes conflict")
	}
}

func TestForget_ExistingEpisode_WithChecker_Tombstones(t *testing.T) {
	t.Parallel()
	const uid = "01HZZZZZZZZZZZZZZZZZZLIVE1"
	fake := &fakeAppender{receipts: []kernel.Receipt{{UID: uid}}}
	f := tools.NewForget(fake)
	f.SetExistenceChecker(fakeChecker{found: true})

	args := mustMarshalJSON(t, map[string]any{"episodeUid": uid})
	if _, err := f.Invoke(context.Background(), args); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !fake.called || len(fake.gotEvents) != 1 {
		t.Fatalf("expected one tombstone append for an existing episode, got called=%v events=%d", fake.called, len(fake.gotEvents))
	}
}

func TestForget_RejectsMissingUID(t *testing.T) {
	t.Parallel()
	fake := &fakeAppender{}
	f := tools.NewForget(fake)

	args := mustMarshalJSON(t, map[string]any{})
	_, err := f.Invoke(context.Background(), args)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var terr *mcp.ToolError
	if !errors.As(err, &terr) || terr.Code != mcp.CodeValidation {
		t.Errorf("error code = %v, want %v", err, mcp.CodeValidation)
	}
	if fake.called {
		t.Error("appender should not have been called on validation error")
	}
}

func TestForget_AppendErrorBubblesUp(t *testing.T) {
	t.Parallel()
	boom := errors.New("ledger full")
	f := tools.NewForget(&fakeAppender{err: boom})

	args := mustMarshalJSON(t, map[string]any{
		"episodeUid": "01HZZZZZZZZZZZZZZZZZZZBOOM1",
	})
	_, err := f.Invoke(context.Background(), args)
	if err == nil {
		t.Fatal("expected error from append failure, got nil")
	}
	var terr *mcp.ToolError
	if !errors.As(err, &terr) || terr.Code != mcp.CodeInternal {
		t.Errorf("error code = %v, want CodeInternal", err)
	}
}

func mustMarshalJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
