// Package transport defines the pluggable sync transport contract.
// Implementations move sealed chunks (see internal/sync/chunk) across
// some remote substrate — a git remote in v0.x, an HTTPS-served chunk
// store later, an Imladris hosted endpoint after that. The interface
// stays narrow on purpose: the transport never sees plaintext events,
// only the encrypted chunk blobs and their plaintext headers.
//
// Layering:
//
//	kernel events
//	    └─ sealer → sealed chunks (encrypted)
//	         └─ Transport.Push(chunks) → remote
//	         └─ Transport.Pull(remote, since) → []Chunk → dispatcher replay
//
// The transport is responsible for naming, durability, and presence;
// it is NOT responsible for batching policy (sealer owns that) or
// for conflict resolution (resolver owns that). A Transport
// implementation is expected to be stateless across calls; per-remote
// state (last-pulled marker, auth credentials) is held by the caller.
package transport

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// ErrTransport is the base wrap for every transport-emitted error so
// callers can distinguish "the remote misbehaved" from "the local
// chunk format is corrupt". Wrapped with %w in every implementation;
// errors.Is(err, ErrTransport) is the right reachability check.
var ErrTransport = errors.New("transport: remote operation failed")

// ErrNotConfigured is returned when a Transport is asked to operate
// against a scope whose remote has not been set up. Distinct from
// ErrTransport so the CLI can print a "run `maz sync configure`"
// hint instead of a stack trace.
var ErrNotConfigured = errors.New("transport: scope has no remote configured")

// ErrPushContention is returned when a Push exhausts all retry attempts
// because a non-fast-forward rejection persists across every attempt —
// concurrent pushers are consistently ahead of this device. Callers
// should back off further or surface the error to the operator.
// Wraps ErrTransport so errors.Is(err, ErrTransport) is still true.
var ErrPushContention = errors.New("transport: push contention exhausted")

// ErrHistoryRewritten is returned when a fast-forward detects that the
// remote's commit history has been rewritten (force-push): the previously
// recorded remote HEAD is no longer an ancestor of the current origin/main.
// The local mirror is left untouched — recovery is explicit operator action:
// run `maz sync repair --scope <scope>` to delete the mirror, then
// `maz sync pull` + `maz rebuild` to re-clone and replay.
// Wraps ErrTransport so errors.Is(err, ErrTransport) is still true.
var ErrHistoryRewritten = errors.New("transport: remote history was rewritten")

// Blob is one sealed chunk in transit: the plaintext header (parsed
// from the chunk's framing prefix) plus the raw byte stream. The
// transport never inspects the body; it only positions, names, and
// moves it. Body MUST be closed by the caller.
type Blob struct {
	Header chunk.Header
	Body   io.ReadCloser
}

// Listing is the result of Transport.List: the remote's view of which
// chunks exist for a scope, ordered by ChunkID (ULID lex = seal-time
// order). The caller diffs Listing against its local chunk store to
// decide what to download.
type Listing struct {
	Scope  kernel.Scope
	Chunks []chunk.Header

	// ContentHashes maps a chunkID to the hex SHA-256 of the chunk's
	// canonical bytes, as recorded by the producer in the remote's
	// integrity manifest. Optional: a nil/empty map (or a chunk absent
	// from it) means the transport has no recorded hash to verify
	// against, and the caller MUST treat that chunk as unverified rather
	// than tampered. Used by the consumer to detect a chunk hand-edited
	// outside Mazarbul before grafting it.
	ContentHashes map[string]string
}

// Device is what Presence returns: every device that has touched the
// remote recently, with timestamps so the operator can see stale
// pairings.
type Device struct {
	ID         string
	Name       string
	LastSeenAt time.Time
}

// Transport is the pluggable sync surface. Each scope is treated
// independently — Push for scope=personal does not affect scope=team.
// Concrete implementations: internal/sync/transport/git.
type Transport interface {
	// Push uploads every Blob from blobs to the remote for scope.
	// Idempotent: blobs whose ChunkID already exists on the remote
	// MUST be skipped, not errored. Returns the count of blobs the
	// remote accepted (newly added). Errors wrap ErrTransport.
	Push(ctx context.Context, scope kernel.Scope, blobs []Blob) (int, error)

	// List returns the remote's view of chunks for scope. Used by
	// the caller to compute the diff against the local chunk store
	// before calling Get.
	List(ctx context.Context, scope kernel.Scope) (Listing, error)

	// Get downloads a specific chunk from the remote. Returns
	// ErrNotConfigured if the scope's remote is not set up; wraps
	// ErrTransport for transient remote failures.
	Get(ctx context.Context, scope kernel.Scope, chunkID string) (Blob, error)

	// Presence lists devices that have touched the remote recently.
	// Used by the TUI Sync tab.
	Presence(ctx context.Context, scope kernel.Scope) ([]Device, error)

	// Name returns a stable identifier for the configured remote
	// ("git+ssh://git@github.com/...", "git+file:///tmp/maz.git", ...).
	// Used in log lines and `maz sync remote`.
	Name() string
}
