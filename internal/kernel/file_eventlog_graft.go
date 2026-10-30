package kernel

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// GraftSealed persists a batch of already-authored events VERBATIM into the
// log and projects them into the derived stores. It is the consume-side
// counterpart to the sealer: a pulling device decodes a sealed team chunk
// (internal/sync/chunk) into kernel.Events that were signed on ANOTHER device,
// and grafts them here so the local graph reflects the shared brain.
//
// Why this is NOT Append. Append mints a fresh signed identity for every
// event — it stampAuthors (re-signing, which would destroy the original remote
// signature), rejects pre-existing UIDs (a re-pull re-presents every chunk's
// UIDs), and enforces live team-write authority (the local device is not the
// author). A graft must do the opposite of all three: preserve the original
// UID + Author + Sig byte-for-byte, treat a UID collision as an idempotent
// skip, and trust the signed author rather than the local writer.
//
// Per-event, in ULID order. Each event is verified, persisted, then PROJECTED
// before the next is verified — the same loop shape as Rebuild. The ordering
// is load-bearing for team bootstrap: a genesis self-grant publishes the
// team's root signing key, and its projection enrolls that key into the
// devices table so the very next event (another grant, a fact) can have its
// signature resolved. A batch-verify-then-apply design would fail that chain
// (it would verify the grant-of-B before the genesis self-grant that vouches
// the granter's key had applied).
//
// Trust gate. Every team-scope event passes the same l.verifier the Rebuild
// replay path uses (verifyTeamEvent) BEFORE it is persisted: the signed
// EventAuthor is the only trust anchor for a remote event. A failed
// verification is dropped and reported in GraftResult.Rejections, never
// written. Non-team events (no cross-device trust requirement) pass trivially;
// in practice GraftSealed is only fed team chunks.
//
// Idempotency. Every pull re-decodes every local chunk, so GraftSealed is
// called repeatedly with overlapping batches. UIDs already in the log are
// filtered out (filterAbsentUIDs) and counted as Skipped; only the genuinely
// new tail is persisted + projected.
func (l *FileEventLog) GraftSealed(ctx context.Context, events []Event) (GraftResult, error) {
	var res GraftResult
	if len(events) == 0 {
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("eventlog: graft: context: %w", err)
	}

	// Seed the HLC before the loop (same rationale as Append: the fallback
	// day-file scan must not race appendUnderLock's write lock).
	l.ensureHLCInit(ctx)

	// Sort ascending by UID (ULIDs are time-ordered) so bootstrap-rooting
	// events — the lowest-ULID genesis self-grant — graft and project before
	// the events whose verification depends on the keys they enroll.
	ordered := sortEventsByUID(events)
	valid := make([]Event, 0, len(ordered))
	for i := range ordered {
		if rej, ok := graftPrecheck(&ordered[i]); ok {
			valid = append(valid, ordered[i])
		} else {
			res.Rejections = append(res.Rejections, rej)
		}
	}

	// Drop UIDs already in the log once, up front (idempotent re-pull). The
	// surviving set is processed in order below.
	fresh, err := l.filterAbsentUIDs(ctx, valid)
	if err != nil {
		return res, err
	}
	res.Skipped = len(valid) - len(fresh)

	var maxObservedHLC string
	for i := range fresh {
		ev := fresh[i]
		if verr := l.verifyTeamEvent(ctx, &ev); verr != nil {
			res.Rejections = append(res.Rejections, GraftRejection{UID: ev.UID, Err: verr})
			logging.FromContext(ctx).Warn("eventlog: graft rejected unverifiable team event",
				zap.String("event_uid", ev.UID), zap.Error(verr))
			continue
		}
		if perr := l.graftPersistOne(ctx, ev); perr != nil {
			return res, perr
		}
		// Track the maximum HLC seen across the batch so the local clock can
		// be advanced after the loop (causality: later local writes sort after
		// all observed remote state).
		if h := ev.EffectiveHLC(); h > maxObservedHLC {
			maxObservedHLC = h
		}
		// Project synchronously (detached, bounded ctx — same as Append's
		// applyToStorage) so a key this event enrolls is visible to the next
		// event's verification, and so a stalled projection cannot pin the
		// writer mutex (graftPersistOne already released it).
		l.applyToStorage(ctx, []Event{ev})
		l.statsStore.IncEvent(string(ev.Kind))
		l.statsStore.IncBytes(int64(len(ev.Payload)))
		res.Grafted++
	}

	// Advance the local HLC past the maximum remote stamp and persist it so
	// subsequent local writes sort after all observed remote state. The cap
	// (l.maxSkew) prevents a far-future remote HLC from freezing local ordering.
	if maxObservedHLC != "" {
		l.hlcClock.Observe(maxObservedHLC, l.clock(), l.maxSkew)
		if state := l.hlcClock.State(); state != "" {
			if err := l.putHLCLast(state); err != nil {
				logging.FromContext(ctx).Debug("eventlog: persist hlc after graft failed (best-effort)",
					zap.Error(err))
			}
		}
	}
	return res, nil
}

// GraftResult reports the outcome of a GraftSealed call. Grafted is the number
// of genuinely new events persisted; Skipped is the number already in the log
// (idempotent re-pull); Rejections lists events the trust gate or UID
// validation refused. A caller surfaces Rejections but does not treat them as
// fatal — one tampered event must not strand the rest of a chunk.
type GraftResult struct {
	Grafted    int
	Skipped    int
	Rejections []GraftRejection
}

// GraftRejection records one event a graft refused, with the reason. UID is ""
// when the event arrived without a parseable identifier.
type GraftRejection struct {
	UID string
	Err error
}

// graftPrecheck validates an event has a well-formed ULID before it is
// considered for grafting. Returns ok=false with a rejection otherwise.
func graftPrecheck(ev *Event) (GraftRejection, bool) {
	if ev.UID == "" {
		return GraftRejection{Err: errors.New("graft: event missing uid")}, false
	}
	if _, err := ulid.Parse(ev.UID); err != nil {
		return GraftRejection{UID: ev.UID, Err: fmt.Errorf("graft: invalid uid: %w", err)}, false
	}
	return GraftRejection{}, true
}

// graftPersistOne writes one already-verified event verbatim into the JSONL
// log + index under l.mu, using the same spine as Append (planBuckets →
// commitPlans → putIndex) but WITHOUT prepare — no UID minting, no re-signing,
// no live authority enforcement — so the original UID + Author + Sig survive.
func (l *FileEventLog) graftPersistOne(ctx context.Context, ev Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("eventlog: graft: context: %w", err)
	}
	plans, err := l.planBuckets([]Event{ev})
	if err != nil {
		return err
	}
	entries, err := l.commitPlans(plans)
	if err != nil {
		return err
	}
	if err := l.putIndex(entries); err != nil {
		// Same recovery contract as Append: the JSONL bytes are durable but the
		// sidecar is now behind the log; invalidate it so the next index read
		// rebuilds from JSONL before trusting a uniqueness verdict.
		if ierr := l.invalidateIndex(); ierr != nil {
			return fmt.Errorf("eventlog: graft: update index: %w (also failed to invalidate: %w)", err, ierr)
		}
		return fmt.Errorf("eventlog: graft: update index: %w", err)
	}
	return nil
}

// sortEventsByUID returns a copy of events sorted ascending by UID (ULID time
// order). Stable so events minted within the same millisecond keep their input
// order.
func sortEventsByUID(events []Event) []Event {
	out := make([]Event, len(events))
	copy(out, events)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// filterAbsentUIDs returns the subset of events whose UID is NOT already in the
// index — the new tail a graft must persist. It mirrors rejectUIDsInIndex but
// inverts the verdict: a present UID is filtered out (idempotent skip) rather
// than erroring. Ensures the sidecar reflects the JSONL first so an
// operator-deleted or stale index cannot wrongly classify a present UID as
// absent and duplicate a JSONL line. Preserves input order.
func (l *FileEventLog) filterAbsentUIDs(ctx context.Context, events []Event) ([]Event, error) {
	if err := l.ensureIndexReflectsLog(ctx); err != nil {
		return nil, err
	}
	absent := make([]Event, 0, len(events))
	verr := l.withIndex(func(db *bolt.DB) error {
		return db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(indexBucket())
			for i := range events {
				key, kerr := encodeIndexKey(events[i].UID)
				if kerr != nil {
					return kerr
				}
				if b == nil || b.Get(key[:]) == nil {
					absent = append(absent, events[i])
				}
			}
			return nil
		})
	})
	if verr != nil {
		return nil, fmt.Errorf("eventlog: graft: filter present uids: %w", verr)
	}
	return absent, nil
}
