// Package sync defines the contracts for Mazarbul's remote sync layer
// (the "Moria" concept from agent-docs/synthesis.md and the decisions
// log). Concrete implementations land in M4; this file holds only the
// interfaces and a Noop fallback.
//
// The default and only required transport in v0.x is git. The interface
// is kept narrow so a future Imladris (hosted) or S3 / R2 / Backblaze
// chunk store can drop in without rewiring callers.
package sync

import (
	"context"
	"errors"
	"time"
)

// Event is the sync-layer view of an event log entry. The kernel's
// kernel.Event is the canonical shape; the sync layer copies only the
// fields needed for transport (the full payload is content-addressed
// and lives in the chunk store).
//
// We accept this duplication rather than import kernel here to avoid
// a circular dependency once kernel grows a sync hook for receipts.
type Event struct {
	UID        string
	CreateTime time.Time
	Kind       string
	ChunkRefs  []string
}

// Device is the public-key identity of a paired device.
type Device struct {
	ID         string
	Name       string
	PubKey     []byte
	PairedAt   time.Time
	LastSeenAt time.Time
}

// Remote is the contract for the remote that holds the canonical
// preserved copy of the event log (the "Moria" in the project's
// naming). Version 0.x ships
// with one implementation: git transport against any standard git
// remote.
type Remote interface {
	// Push uploads the named events to the remote. The remote takes
	// responsibility for durable storage from then on; the local kernel
	// may still keep its own copy. Returns the count of events the
	// remote actually accepted (zero if the remote already has them all).
	Push(ctx context.Context, events []Event) (int, error)

	// Pull fetches events created since `since`. Returns them in append
	// order. The kernel then applies each event through Storage.Apply.
	Pull(ctx context.Context, since time.Time) ([]Event, error)

	// Presence lists every device that has touched the remote recently.
	// Used for the `maz status` device summary and for stale-device
	// detection in M4+.
	Presence(ctx context.Context) ([]Device, error)

	// Name returns a stable identifier for the remote ("github:mo-cmyk/
	// mocmyk-moria", "self-hosted-gitea:moria/main", etc.). Used in logs
	// and `maz remote list`.
	Name() string
}

// ChunkStore is the content-addressed blob store. The event log
// references chunks by content hash; the chunk store holds the actual
// bytes. Version 0.x embeds the chunk store in the same git remote as
// the event log; later providers (S3, R2, IPFS) drop in here without
// touching the Remote.
type ChunkStore interface {
	// Put writes content to the store and returns its content hash
	// (sha256). Subsequent Put calls with the same content are idempotent.
	Put(ctx context.Context, content []byte) (string, error)

	// Get returns the content at the named hash, or ErrNotFound when no
	// such chunk exists.
	Get(ctx context.Context, hash string) ([]byte, error)

	// Has reports whether the named hash exists locally. Used by Push
	// to avoid re-uploading content the remote already has.
	Has(ctx context.Context, hash string) (bool, error)
}

// ErrNotFound is returned when a requested chunk or event does not exist.
var ErrNotFound = errors.New("sync: not found")

// ErrNotImplemented is returned by the Noop placeholders. Replaced in
// M4 by the real git transport and chunk store.
var ErrNotImplemented = errors.New("sync: not yet implemented")

// NoopRemote is the placeholder remote. Returns ErrNotImplemented
// from every operation. The name "noop://" is descriptive in logs.
type NoopRemote struct{}

// Push implements Remote. It returns ErrNotImplemented.
func (NoopRemote) Push(_ context.Context, _ []Event) (int, error) {
	return 0, ErrNotImplemented
}

// Pull implements Remote. It returns ErrNotImplemented.
func (NoopRemote) Pull(_ context.Context, _ time.Time) ([]Event, error) {
	return nil, ErrNotImplemented
}

// Presence implements Remote. It returns ErrNotImplemented so an unwired
// remote cannot be mistaken for a real "no devices" presence list.
func (NoopRemote) Presence(_ context.Context) ([]Device, error) {
	return nil, ErrNotImplemented
}

// Name implements Remote. It returns "noop://".
func (NoopRemote) Name() string { return "noop://" }

// NoopChunkStore is the placeholder chunk store. Returns
// ErrNotImplemented from every operation.
type NoopChunkStore struct{}

// Put implements ChunkStore. It returns ErrNotImplemented.
func (NoopChunkStore) Put(_ context.Context, _ []byte) (string, error) {
	return "", ErrNotImplemented
}

// Get implements ChunkStore. It returns ErrNotImplemented.
func (NoopChunkStore) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, ErrNotImplemented
}

// Has implements ChunkStore. It returns ErrNotImplemented so an unwired chunk
// store cannot be mistaken for a real "chunk not present" outcome.
func (NoopChunkStore) Has(_ context.Context, _ string) (bool, error) {
	return false, ErrNotImplemented
}
