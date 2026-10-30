package chunk

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
)

// EventSource is the read surface the sealer pulls events from.
// Kernel.FileEventLog satisfies this via its Read method.
type EventSource interface {
	Read(ctx context.Context, opts kernel.ReadOpts) iter.Seq2[kernel.Event, error]
}

// CursorStore tracks the set of event UIDs that have already been
// sealed into a chunk for a given scope so the sealer can resume
// across daemon restarts without re-sealing events the remote has
// already received. Set semantics (rather than a single
// highest-UID watermark) handle backfill correctly: an event whose
// UID sorts below the highest-sealed UID but was appended later —
// caller-supplied CreateTime, replayed import, late connector — is
// still sealed because membership is checked per-UID, not against a
// watermark.
//
// Implementations cache the set in memory and persist it on
// MarkSealed; production wiring lands a filesystem JSONL file under
// <dataDir>/sync/<scope>/sealed.jsonl with one UID per line.
type CursorStore interface {
	// Sealed returns the set of UIDs already sealed for scope.
	// Returning the same map across calls is acceptable; the caller
	// treats it as read-only.
	Sealed(ctx context.Context, scope kernel.Scope) (map[string]struct{}, error)

	// MarkSealed records that uids are now sealed. Implementations
	// persist atomically — a partial record after a crash must not
	// leave the on-disk state inconsistent with the in-memory set.
	MarkSealed(ctx context.Context, scope kernel.Scope, uids []string) error
}

// SealOnce reads every event from src whose UID is strictly greater
// than cursor.Get(scope), filters by scope, batches in groups of
// maxEvents (or the full remainder), seals each batch into a chunk,
// writes the chunk via store.Put, and finally advances the cursor.
//
// Returns the number of chunks emitted plus the highest UID covered.
// A zero-event run is a no-op (no cursor update, no chunk).
//
// This is the unit the daemon's sealer goroutine calls periodically
// (every ~1 hour) and on graceful shutdown. Idempotent: calling it
// twice with no events in between produces no extra chunks.
func SealOnce(
	ctx context.Context,
	src EventSource,
	scope kernel.Scope,
	scopeKey []byte,
	store Store,
	cursor CursorStore,
	maxEvents int,
) (chunksWritten int, newCursor string, err error) {
	return SealOnceWith(ctx, src, scope, scopeKey, store, cursor, maxEvents, "")
}

// SealOnceWith is SealOnce with an additional device tag that
// populates Header.Device on every chunk emitted. Caller-friendly
// when the sealer is invoked outside the daemon supervisor (CLI
// `maz sync push`, integration tests, scripts).
func SealOnceWith(
	ctx context.Context,
	src EventSource,
	scope kernel.Scope,
	scopeKey []byte,
	store Store,
	cursor CursorStore,
	maxEvents int,
	device string,
) (chunksWritten int, newCursor string, err error) {
	return SealOnceWithEpoch(ctx, src, scope, scopeKey, 0, store, cursor, maxEvents, device)
}

// SealOnceWithEpoch is SealOnceWith with an explicit DEK epoch stamped into
// every chunk header. Envelope-mode team scopes seal under a per-epoch
// data-encryption key; the epoch is plaintext header metadata (and is covered
// by the header-AAD authentication) so a reader can select the right DEK to
// Open the chunk. The dekEpoch arg is ignored for plaintext (scopeKey==nil)
// and Watchword-derived scopes, which always use epoch 0.
func SealOnceWithEpoch(
	ctx context.Context,
	src EventSource,
	scope kernel.Scope,
	scopeKey []byte,
	dekEpoch int,
	store Store,
	cursor CursorStore,
	maxEvents int,
	device string,
) (chunksWritten int, newCursor string, err error) {
	if maxEvents <= 0 {
		return 0, "", errors.New("chunk: maxEvents must be > 0")
	}
	if !scope.Valid() {
		return 0, "", fmt.Errorf("chunk: invalid scope %q", scope)
	}
	sealed, err := cursor.Sealed(ctx, scope)
	if err != nil {
		return 0, "", fmt.Errorf("chunk: read cursor: %w", err)
	}
	st := &sealState{
		ctx:       ctx,
		scope:     scope,
		scopeKey:  scopeKey,
		dekEpoch:  dekEpoch,
		device:    device,
		store:     store,
		cursor:    cursor,
		sealed:    sealed,
		maxEvents: maxEvents,
		batch:     make([]kernel.Event, 0, maxEvents),
		batchUIDs: make([]string, 0, maxEvents),
	}
	if err := st.consume(src); err != nil {
		return st.chunksWritten, st.lastUID, fmt.Errorf("chunk: read events: %w", err)
	}
	if err := st.flush(); err != nil {
		return st.chunksWritten, st.lastUID, err
	}
	return st.chunksWritten, st.lastUID, nil
}

// sealState carries the loop variables SealOnce mutates. Keeping
// them on a struct (rather than as closure captures) keeps the
// cognitive complexity of the top-level function under the linter
// gate without sacrificing the iterator-callback style kernel.Read
// requires.
type sealState struct {
	ctx           context.Context //nolint:containedctx // matches the seal-call lifetime; not stored long-term
	scope         kernel.Scope
	scopeKey      []byte
	dekEpoch      int
	device        string
	store         Store
	cursor        CursorStore
	sealed        map[string]struct{} // set of UIDs already sealed; mutated as new chunks flush
	maxEvents     int
	batch         []kernel.Event
	batchUIDs     []string
	chunksWritten int
	lastUID       string
}

func (st *sealState) consume(src EventSource) error {
	seq := src.Read(st.ctx, kernel.ReadOpts{})
	var iterErr error
	seq(func(ev kernel.Event, e error) bool {
		if e != nil {
			iterErr = e
			return false
		}
		if !st.accept(ev) {
			return true
		}
		st.batch = append(st.batch, ev)
		st.batchUIDs = append(st.batchUIDs, ev.UID)
		if len(st.batch) >= st.maxEvents {
			if flushErr := st.flush(); flushErr != nil {
				iterErr = flushErr
				return false
			}
		}
		return true
	})
	return iterErr
}

// accept filters one event against the scope and the sealed-UID set.
// Membership-based (not watermark-based) so a later-appended event
// with an older UID — backfill, replay, late connector — is still
// sealed if it has not been recorded yet.
func (st *sealState) accept(ev kernel.Event) bool {
	if ev.Scope != st.scope {
		return false
	}
	if !st.ownsEvent(ev) {
		return false
	}
	if _, already := st.sealed[ev.UID]; already {
		return false
	}
	if st.skipPlaintextSensitive(ev) {
		return false
	}
	return true
}

// skipPlaintextSensitive reports whether ev must be skipped rather than sealed:
// it is a sensitive payload bound for a permanently-plaintext scope where
// EncodeWithOpts would refuse it (ErrSensitivePayloadPlaintext). The condition
// mirrors firstSensitivePlaintext exactly — plaintext path (no scope key),
// non-project scope, and a registered privacy marker trips — so the sealer
// never hands flush() a batch that Encode would reject.
//
// Skipping (not batching) is the fix for the permanently-plaintext scopes:
// personal and draft never acquire a scope key, so a sensitive connector
// event there can NEVER seal. Letting it into the batch made EncodeWithOpts
// fail the whole pass, wedging the cursor so every following event stalled
// behind it. Skipping leaves the event durable in the local log — the source
// of truth — but keeps it off the plaintext sync remote, which is exactly the
// privacy contract for connector-sourced data, while the innocent events in
// the same pass seal and the cursor advances.
//
// Project (intentionally public, seals sensitive payloads in cleartext by
// design) and encrypted scopes (scopeKey != nil, i.e. team — the payload
// seals inside an envelope) are both excluded, matching the Encode guard.
func (st *sealState) skipPlaintextSensitive(ev kernel.Event) bool {
	if st.scopeKey != nil {
		return false
	}
	if st.scope == kernel.ScopeProject {
		return false
	}
	return privacy.IsSensitivePayload(ev)
}

// ownsEvent decides whether this device may seal an event. Every scope with
// a consume loop — team AND personal/project since the all-scope consume
// wave — is multi-writer: peer events are grafted into the local log (see
// FileEventLog.GraftSealed), and a device that re-sealed a peer's grafted
// event would re-tag the chunk with its OWN device. Every other device
// (including the original author) would then reject that chunk at the
// replay boundary because the chunk's Header.Device no longer matches the
// event's signed Author.DeviceID (chunk.Header.AuthorsMatchDevice). So a
// device-tagged seal covers only events this device authored.
//
// Pre-identity events (Author.PreIdentity: the first-boot ontology
// bootstrap, signed by the public local-default placeholder) never seal
// into a device-tagged chunk: their "local" device marker violates the same
// header binding, and every device self-bootstraps its own registry, so
// peers have no use for them.
//
// A blank device tag (CLI/test seals) keeps the original behaviour and
// seals everything: Header.Device stays empty, so there is no device
// binding to violate. Legacy events with a blank Author.DeviceID are ours
// by definition (grafted peer events always carry a signed device).
func (st *sealState) ownsEvent(ev kernel.Event) bool {
	if st.device == "" {
		return true
	}
	if ev.Author.PreIdentity() {
		return false
	}
	return ev.Author.DeviceID == "" || ev.Author.DeviceID == st.device
}

func (st *sealState) flush() error {
	if len(st.batch) == 0 {
		return nil
	}
	blob, hdr, encErr := EncodeWithOpts(st.batch, EncodeOpts{
		Scope:    st.scope,
		ScopeKey: st.scopeKey,
		Device:   st.device,
		DEKEpoch: st.dekEpoch,
	})
	if encErr != nil {
		return encErr
	}
	if putErr := st.store.Put(st.ctx, st.scope, hdr, blob); putErr != nil {
		return putErr
	}
	// MarkSealed must run AFTER a successful Put so a crash between
	// the two does not record a chunk as sealed when the bytes never
	// landed on disk. A crash before MarkSealed leaves the chunk on
	// disk and the cursor lagging — the next SealOnce re-seals the
	// same UIDs into a new chunk, which the chunk-store-on-remote
	// deduplicates by ChunkID. Idempotent.
	if cerr := st.cursor.MarkSealed(st.ctx, st.scope, st.batchUIDs); cerr != nil {
		return fmt.Errorf("chunk: persist cursor: %w", cerr)
	}
	for _, uid := range st.batchUIDs {
		st.sealed[uid] = struct{}{}
	}
	st.chunksWritten++
	st.lastUID = hdr.LastULID
	st.batch = st.batch[:0]
	st.batchUIDs = st.batchUIDs[:0]
	return nil
}
