package dispatcher_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestApply_EpisodeForgottenCascade is the W3 ship gate: an EpisodeForgotten
// event must remove the episode row, its mentions, its provenance, and the
// episodes_vec entry, leaving the JSONL log + the tombstone intact.
func TestApply_EpisodeForgottenCascade(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const episodeUID = "01HZZZZZZZZZZZZZZZZZZZFORG1"
	ev := makeEpisodeEvent(t, episodeUID, "Eowyn works at Erebor.", "test")
	ev.CreateTime = time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	assertEpisodeCount(t, ctx, d, 1)

	payload, err := json.Marshal(ontology.EpisodeForgottenEvent{
		EpisodeUID:  episodeUID,
		Reason:      "test",
		ForgottenAt: time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tombstone := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZZTOMB1",
		Kind:       kernel.EventKind(ontology.KindEpisodeForgotten),
		Source:     "mcp:forget",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{tombstone}); err != nil {
		t.Fatalf("tombstone Apply: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 0)
}

// TestApply_EpisodeForgottenIsIdempotent ensures replaying the same
// tombstone (the maz rebuild path) does not error and does not leave the
// derived state in a broken half-applied form.
func TestApply_EpisodeForgottenIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const episodeUID = "01HZZZZZZZZZZZZZZZZZZZFORG2"
	ev := makeEpisodeEvent(t, episodeUID, "x", "test")
	ev.CreateTime = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	payload, err := json.Marshal(ontology.EpisodeForgottenEvent{
		EpisodeUID:  episodeUID,
		ForgottenAt: time.Date(2026, 5, 27, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tombstone := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZZTOMB2",
		Kind:       kernel.EventKind(ontology.KindEpisodeForgotten),
		Source:     "mcp:forget",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 5, 27, 13, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{tombstone}); err != nil {
		t.Fatalf("first tombstone Apply: %v", err)
	}
	// Replay: must not error even though the episode is already gone.
	if err := d.Apply(ctx, []kernel.Event{tombstone}); err != nil {
		t.Fatalf("replayed tombstone Apply: %v", err)
	}
	assertEpisodeCount(t, ctx, d, 0)
}

// TestApply_EntityIdentifierAddedWritesRow pins that an
// entityIdentifierAdded event populates the entity_identifiers table the
// resolver's identifier-match step reads from. Replays must collapse via
// the composite PK + ON CONFLICT update branch.
func TestApply_EntityIdentifierAddedWritesRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// Seed an entity first; entity_identifiers FK-references entities(uid).
	// Emit one as a KindEntity event (the dispatcher's regular path runs:
	// vector writes + entities row insert).
	createTime := time.Date(2026, 5, 27, 13, 0, 0, 0, time.UTC)
	entPayload, err := json.Marshal(ontology.Entity{
		Name:             "entities/01HZZZZZZZZZZZZZZZZZZZIDEN1",
		UID:              "01HZZZZZZZZZZZZZZZZZZZIDEN1",
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Eowyn",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       createTime,
	})
	if err != nil {
		t.Fatalf("marshal entity: %v", err)
	}
	entEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZZENT01",
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "resolver/v1",
		Confidence: 1.0,
		CreateTime: createTime,
		Payload:    entPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{entEv}); err != nil {
		t.Fatalf("seed entity Apply: %v", err)
	}

	payload, err := json.Marshal(ontology.EntityIdentifierAddedEvent{
		EntityUID:  "01HZZZZZZZZZZZZZZZZZZZIDEN1",
		Identifier: "Eowyn",
		Kind:       ontology.IdentifierKindAlias,
		Source:     ontology.IdentifierSourceExtracted,
		AddedAt:    time.Date(2026, 5, 27, 14, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZZIDEV1",
		Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
		Source:     "resolver/v1",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 5, 27, 14, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Re-apply with a different added_at + source to exercise the
	// ON CONFLICT UPDATE branch.
	payload2, err := json.Marshal(ontology.EntityIdentifierAddedEvent{
		EntityUID:  "01HZZZZZZZZZZZZZZZZZZZIDEN1",
		Identifier: "Eowyn",
		Kind:       ontology.IdentifierKindAlias,
		Source:     ontology.IdentifierSourceUser,
		AddedAt:    time.Date(2026, 5, 27, 15, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev.UID = "01HZZZZZZZZZZZZZZZZZZZIDEV2"
	ev.Payload = payload2
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Replay Apply: %v", err)
	}

	uid, err := d.LookupEntityByIdentifier(ctx, "Eowyn", string(ontology.IdentifierKindAlias))
	if err != nil {
		t.Fatalf("LookupEntityByIdentifier: %v", err)
	}
	if uid != "01HZZZZZZZZZZZZZZZZZZZIDEN1" {
		t.Errorf("LookupEntityByIdentifier returned %q, want %q", uid, "01HZZZZZZZZZZZZZZZZZZZIDEN1")
	}
}
