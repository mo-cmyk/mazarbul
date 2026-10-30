package chunk_test

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// withDevice tags an event's signed author device so the sealer's team-scope
// ownership filter can reason about it.
func withDevice(ev kernel.Event, device string) kernel.Event {
	ev.Author.DeviceID = device
	return ev
}

func TestSealOnce_TeamScopeSealsOnlyOwnDeviceEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := deriveKey(t)
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	src := &listSource{events: []kernel.Event{
		withDevice(sampleEvent(t, kernel.ScopeTeam, "mine-1"), "me"),
		withDevice(sampleEvent(t, kernel.ScopeTeam, "peer"), "peer"),
		withDevice(sampleEvent(t, kernel.ScopeTeam, "mine-2"), "me"),
		withDevice(sampleEvent(t, kernel.ScopeTeam, "legacy"), ""), // pre-EventAuthor: still ours
	}}

	n, _, err := chunk.SealOnceWith(ctx, src, kernel.ScopeTeam, key, store, cursor, 100, "me")
	if err != nil {
		t.Fatalf("SealOnceWith: %v", err)
	}
	if n != 1 {
		t.Fatalf("chunksWritten = %d, want 1", n)
	}

	refs, err := store.List(ctx, kernel.ScopeTeam)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	blob, err := store.Get(ctx, kernel.ScopeTeam, refs[0].ChunkID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_, events, err := chunk.Decode(blob, kernel.ScopeTeam, key)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	// "peer" must be excluded; the two "me" events + the blank-device legacy
	// event survive (re-sealing a peer's event would re-tag the chunk and make
	// every other device reject it at AuthorsMatchDevice).
	if len(events) != 3 {
		t.Fatalf("sealed %d events, want 3 (peer excluded)", len(events))
	}
	for _, ev := range events {
		if ev.Author.DeviceID == "peer" {
			t.Errorf("a peer-authored team event was re-sealed: %s", ev.UID)
		}
	}
}

func TestSealOnce_PersonalScopeSealsOnlyOwnDeviceEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := deriveKey(t)
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	// Personal scope is multi-writer since the all-scope consume wave: a
	// grafted peer event re-sealed under this device's header would make every
	// consumer reject the chunk at AuthorsMatchDevice. Only own-device (and
	// blank-device legacy) events may seal.
	src := &listSource{events: []kernel.Event{
		withDevice(sampleEvent(t, kernel.ScopePersonal, "a"), "me"),
		withDevice(sampleEvent(t, kernel.ScopePersonal, "b"), "other-device"),
		withDevice(sampleEvent(t, kernel.ScopePersonal, "c"), ""),
	}}

	if _, _, err := chunk.SealOnceWith(ctx, src, kernel.ScopePersonal, key, store, cursor, 100, "me"); err != nil {
		t.Fatalf("SealOnceWith: %v", err)
	}
	refs, err := store.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if refs[0].Count != 2 {
		t.Errorf("personal scope sealed %d events, want 2 (own + blank-device legacy; peer excluded)", refs[0].Count)
	}
}

func TestSealOnce_DeviceTaggedSealExcludesPreIdentityEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := deriveKey(t)
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	// First-boot ontology bootstrap events are authored by the "local"
	// pre-identity placeholder. Sealing them under a real device header would
	// poison the whole chunk: AuthorsMatchDevice rejects any event whose
	// signed device differs from Header.Device.
	src := &listSource{events: []kernel.Event{
		withDevice(sampleEvent(t, kernel.ScopePersonal, "bootstrap"), "local"),
		withDevice(sampleEvent(t, kernel.ScopePersonal, "mine"), "me"),
	}}

	if _, _, err := chunk.SealOnceWith(ctx, src, kernel.ScopePersonal, key, store, cursor, 100, "me"); err != nil {
		t.Fatalf("SealOnceWith: %v", err)
	}
	refs, err := store.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	blob, err := store.Get(ctx, kernel.ScopePersonal, refs[0].ChunkID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	hdr, events, err := chunk.Decode(blob, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(events) != 1 || events[0].Author.DeviceID != "me" {
		t.Fatalf("sealed %d events, want exactly the own-device event (pre-identity excluded)", len(events))
	}
	// The surviving chunk must clear the replay-boundary binding check.
	if merr := hdr.AuthorsMatchDevice(events); merr != nil {
		t.Errorf("sealed chunk fails AuthorsMatchDevice: %v", merr)
	}
}
