package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// fakeChunkTransport is a minimal transport.Transport whose List reports a
// fixed listing (with content hashes) and whose Get returns caller-supplied
// bytes per chunk — so a test can hand pullChunks a chunk whose bytes do not
// match the recorded hash and assert it is rejected before reaching the store.
type fakeChunkTransport struct {
	listing transport.Listing
	bodies  map[string][]byte
}

func (f *fakeChunkTransport) Push(context.Context, kernel.Scope, []transport.Blob) (int, error) {
	return 0, nil
}

func (f *fakeChunkTransport) List(context.Context, kernel.Scope) (transport.Listing, error) {
	return f.listing, nil
}

func (f *fakeChunkTransport) Get(_ context.Context, _ kernel.Scope, chunkID string) (transport.Blob, error) {
	b, ok := f.bodies[chunkID]
	if !ok {
		return transport.Blob{}, fmt.Errorf("fake transport: no body for %s", chunkID)
	}
	return transport.Blob{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func (f *fakeChunkTransport) Presence(context.Context, kernel.Scope) ([]transport.Device, error) {
	return nil, nil
}

func (f *fakeChunkTransport) Name() string { return "fake" }

// integrityFixtureChunk seals one plaintext chunk and returns its bytes+header.
func integrityFixtureChunk(t *testing.T) ([]byte, chunk.Header) {
	t.Helper()
	ev := kernel.Event{
		UID:        "01HKJZNBp1111111111111111",
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      kernel.ScopePersonal,
		Payload:    []byte(`{"text":"hello"}`),
	}
	body, hdr, err := chunk.EncodePlaintext([]kernel.Event{ev}, kernel.ScopePersonal, "device-a")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	return body, hdr
}

func storedChunkCount(t *testing.T, store *chunk.FSStore) int {
	t.Helper()
	refs, err := store.List(context.Background(), kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	return len(refs)
}

// TestPullChunks_RejectsTamperedChunk is the integrity regression: a chunk
// whose bytes disagree with the hash the remote recorded in MANIFEST must be
// refused at the pull boundary and never reach the local store (so it can
// never be grafted). Before the contentHash guard this tampered chunk would
// have been stored and silently replayed.
func TestPullChunks_RejectsTamperedChunk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body, hdr := integrityFixtureChunk(t)

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0xFF // flip one byte; line count unchanged

	store, err := chunk.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	s := &syncSupervisor{
		store: store,
		trans: &fakeChunkTransport{
			listing: transport.Listing{
				Scope:         kernel.ScopePersonal,
				Chunks:        []chunk.Header{hdr},
				ContentHashes: map[string]string{hdr.ChunkID: chunk.ContentHash(body)},
			},
			bodies: map[string][]byte{hdr.ChunkID: tampered},
		},
		logger: zap.NewNop(),
	}
	if err := s.pullChunks(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("pullChunks: %v", err)
	}
	if n := storedChunkCount(t, store); n != 0 {
		t.Errorf("stored %d chunks; tampered chunk must be rejected (0)", n)
	}
}

// TestPullChunks_StoresVerifiedChunk is the control: a chunk whose bytes match
// the recorded hash is accepted normally.
func TestPullChunks_StoresVerifiedChunk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body, hdr := integrityFixtureChunk(t)

	store, err := chunk.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	s := &syncSupervisor{
		store: store,
		trans: &fakeChunkTransport{
			listing: transport.Listing{
				Scope:         kernel.ScopePersonal,
				Chunks:        []chunk.Header{hdr},
				ContentHashes: map[string]string{hdr.ChunkID: chunk.ContentHash(body)},
			},
			bodies: map[string][]byte{hdr.ChunkID: body},
		},
		logger: zap.NewNop(),
	}
	if err := s.pullChunks(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("pullChunks: %v", err)
	}
	if n := storedChunkCount(t, store); n != 1 {
		t.Errorf("stored %d chunks; verified chunk must be accepted (1)", n)
	}
}

// TestPullChunks_LegacyManifestUnverifiedIsStored pins the upgrade path: a
// remote whose MANIFEST records no hash for a chunk (pre-v2) is treated as
// unverified — the chunk is stored, not rejected — so an old remote keeps
// working after the contentHash landing.
func TestPullChunks_LegacyManifestUnverifiedIsStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body, hdr := integrityFixtureChunk(t)

	store, err := chunk.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	s := &syncSupervisor{
		store: store,
		trans: &fakeChunkTransport{
			listing: transport.Listing{
				Scope:  kernel.ScopePersonal,
				Chunks: []chunk.Header{hdr},
				// No ContentHashes entry → unverified, not tampered.
			},
			bodies: map[string][]byte{hdr.ChunkID: body},
		},
		logger: zap.NewNop(),
	}
	if err := s.pullChunks(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("pullChunks: %v", err)
	}
	if n := storedChunkCount(t, store); n != 1 {
		t.Errorf("stored %d chunks; an unverified (legacy) chunk must still be accepted (1)", n)
	}
}
