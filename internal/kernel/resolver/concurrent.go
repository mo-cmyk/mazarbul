package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// FunctionalPredicate is the narrow contract DetectConcurrentSupersedeOn
// requires: given a predicate name, return whether the registry marks it
// as functional. The Resolver's predicate Registry satisfies this; CLI
// tools that need to detect conflicts without assembling the full
// resolver (chat, embedder, vec) load a thin predicate-cache that
// satisfies this interface instead. Keeps the conflict-detection path
// callable without dragging in every Deps dependency.
type FunctionalPredicate interface {
	IsFunctional(predicate string) bool
}

// ConcurrentSupersedeConflict is a single detected pair of open
// relations that share a functional slot. The resolver returns one of
// these per offending sibling: a slot with three open relations after
// replay produces two ConcurrentSupersedeConflict records, all
// referencing the same primary (lowest-ULID) arm.
//
// The struct is the input the caller hands to the event-log appender
// to materialise a relationSupersedeConflict event. Keeping detection
// pure (no event log dependency) makes the function trivially
// testable against an in-memory SQLite reader.
type ConcurrentSupersedeConflict struct {
	// PrimaryRelationUID is the lowest-ULID open relation in the slot.
	// Deterministic tiebreak only — not a quality judgement.
	PrimaryRelationUID string

	// ConflictingRelationUID is the sibling open relation flagged as
	// a conflict against the primary.
	ConflictingRelationUID string

	// SubjectUID is the entity UID shared by both relations.
	SubjectUID string

	// Predicate is the functional predicate name.
	Predicate string

	// Reason is the detection trigger tag matching the ontology event
	// payload field of the same name.
	Reason string
}

// concurrentSupersedeReason is the only reason tag currently emitted.
// Kept as a typed constant so future detection heuristics can
// disambiguate without bumping the schema version.
const concurrentSupersedeReason = "multiple_open_functional"

// DetectConcurrentSupersede scans the relations table for functional
// slots — (subject_uid, predicate) pairs where the predicate registry
// marks predicate as functional — that contain more than one OPEN
// (valid_until IS NULL) relation. Functional slots must have at most
// one open arm by definition; multiple opens indicate two devices
// independently superseded the same prior relation along incompatible
// paths and the replay merged both arms.
//
// The function emits one ConcurrentSupersedeConflict per conflicting
// sibling (slot with N opens → N-1 conflicts, all referencing the
// lowest-ULID open relation as primary). Sorting by ULID is the only
// deterministic primary-pick available without operator input; the
// follow-up /review conflicts UI lets the operator pick the real
// winner before issuing a normal relationForgotten on the loser.
//
// Pure read; safe to call any time. Production callers run it once
// at the end of the post-pull replay pass before the dispatcher seals
// the merge. Tests call it directly against a seeded reader.
func (r *Resolver) DetectConcurrentSupersede(ctx context.Context) ([]ConcurrentSupersedeConflict, error) {
	return DetectConcurrentSupersedeOn(ctx, r.reader, r.predicates)
}

// DetectConcurrentSupersedeOn is the standalone detection routine the
// Resolver method delegates to. Exposing it without the full Resolver
// surface lets the CLI's `maz review conflicts` command (and any
// future post-pull replay hook in the daemon) materialise conflicts
// without assembling Deps.Embedder / Deps.Chat / Deps.Vec — every
// dependency the inner scan does not actually use.
func DetectConcurrentSupersedeOn(
	ctx context.Context, reader *sql.DB, predicates FunctionalPredicate,
) ([]ConcurrentSupersedeConflict, error) {
	rows, err := reader.QueryContext(ctx,
		`SELECT subject_uid, predicate, uid FROM relations
		  WHERE valid_until IS NULL
		  ORDER BY subject_uid, predicate, uid`,
	)
	if err != nil {
		return nil, fmt.Errorf("detect concurrent supersede query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type openRow struct {
		subjectUID string
		predicate  string
		uid        string
	}
	bySlot := make(map[string][]openRow, 16)
	for rows.Next() {
		var row openRow
		if err := rows.Scan(&row.subjectUID, &row.predicate, &row.uid); err != nil {
			return nil, fmt.Errorf("detect concurrent supersede scan: %w", err)
		}
		key := row.subjectUID + "\x00" + row.predicate
		bySlot[key] = append(bySlot[key], row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("detect concurrent supersede iterate: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("detect concurrent supersede close rows: %w", err)
	}

	// Stable order: scan slot keys sorted lexicographically so test
	// expectations and event-log replay sequences are deterministic.
	keys := make([]string, 0, len(bySlot))
	for k := range bySlot {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var conflicts []ConcurrentSupersedeConflict
	for _, k := range keys {
		opens := bySlot[k]
		if len(opens) < 2 {
			continue
		}
		sort.Slice(opens, func(i, j int) bool { return opens[i].uid < opens[j].uid })
		if !predicates.IsFunctional(opens[0].predicate) {
			continue
		}
		primary := opens[0]
		for _, sibling := range opens[1:] {
			conflicts = append(conflicts, ConcurrentSupersedeConflict{
				PrimaryRelationUID:     primary.uid,
				ConflictingRelationUID: sibling.uid,
				SubjectUID:             primary.subjectUID,
				Predicate:              primary.predicate,
				Reason:                 concurrentSupersedeReason,
			})
		}
	}
	return conflicts, nil
}

// EmitConcurrentSupersedeConflicts appends one
// relationSupersedeConflict event per detected conflict that has not
// already been materialised in the event log. Returns the number of
// NEW events emitted plus the first error encountered, if any.
//
// Idempotency: each call reads existing relationSupersedeConflict
// events from the log and builds a set of already-seen
// (PrimaryRelationUID, ConflictingRelationUID) pairs. Pairs already
// present are skipped. Without this guard, every post-pull replay
// pass would emit a fresh conflict event for the same unresolved
// arms, polluting /review conflicts with duplicates.
//
// Source is the Event.Source string the caller wants stamped on the
// emitted events (e.g. "sync:pull"). The resolver does not invent a
// source on the caller's behalf so the audit trail stays explicit
// about which subsystem flagged the conflict.
func (r *Resolver) EmitConcurrentSupersedeConflicts(
	ctx context.Context, conflicts []ConcurrentSupersedeConflict, source string,
) (int, error) {
	return EmitConcurrentSupersedeConflictsTo(ctx, r.eventLog, r.logger, conflicts, source, time.Now)
}

// EmitConcurrentSupersedeConflictsTo is the standalone emitter the Resolver
// method delegates to. Exposing it without the Resolver surface lets the
// CLI's `maz review conflicts --emit` materialise events using only an
// already-opened event log and a logger — no embedder / chat / vec / registry
// boot needed. Now is a clock injection point so tests can pin DetectedAt;
// pass time.Now in production.
func EmitConcurrentSupersedeConflictsTo(
	ctx context.Context,
	log kernel.EventLog,
	logger *zap.Logger,
	conflicts []ConcurrentSupersedeConflict,
	source string,
	now func() time.Time,
) (int, error) {
	if len(conflicts) == 0 {
		return 0, nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if now == nil {
		now = time.Now
	}
	seen, err := existingConflictPairsOn(ctx, log, logger)
	if err != nil {
		return 0, fmt.Errorf("load existing supersede-conflicts: %w", err)
	}
	emitted := 0
	for _, c := range conflicts {
		key := conflictKey(c.PrimaryRelationUID, c.ConflictingRelationUID)
		if _, dup := seen[key]; dup {
			continue
		}
		payload := ontology.RelationSupersedeConflictEvent{
			PrimaryRelationUID:     ontology.ULID(c.PrimaryRelationUID),
			ConflictingRelationUID: ontology.ULID(c.ConflictingRelationUID),
			SubjectUID:             ontology.ULID(c.SubjectUID),
			Predicate:              c.Predicate,
			Reason:                 c.Reason,
			DetectedAt:             now().UTC(),
		}
		raw, mErr := json.Marshal(&payload)
		if mErr != nil {
			return emitted, fmt.Errorf("marshal supersede-conflict %d: %w", emitted, mErr)
		}
		ev := kernel.Event{
			Kind:       kernel.EventKind(ontology.KindRelationSupersedeConflict),
			Source:     source,
			Confidence: 1.0,
			Payload:    raw,
		}
		receipts, aErr := log.Append(ctx, []kernel.Event{ev})
		if aErr != nil {
			return emitted, fmt.Errorf("append supersede-conflict %d: %w", emitted, aErr)
		}
		uid := ""
		if len(receipts) > 0 {
			uid = receipts[0].UID
		}
		seen[key] = struct{}{}
		logger.Warn("Resolver: emitted relationSupersedeConflict",
			zap.String("event_uid", uid),
			zap.String("primary_relation_uid", c.PrimaryRelationUID),
			zap.String("conflicting_relation_uid", c.ConflictingRelationUID),
			zap.String("subject_uid", c.SubjectUID),
			zap.String("predicate", c.Predicate),
			zap.String("reason", c.Reason),
		)
		emitted++
	}
	return emitted, nil
}

// conflictKey is the stable string identity of a conflict pair. The
// primary/conflicting UIDs are joined with a NUL so two distinct
// (primary, conflicting) tuples cannot accidentally hash to the same
// key (the inputs are ULIDs, never NUL-containing).
func conflictKey(primaryUID, conflictingUID string) string {
	return primaryUID + "\x00" + conflictingUID
}

// existingConflictPairsOn walks the event log for previously-materialised
// relationSupersedeConflict events and returns the set of pairs already
// on disk so EmitConcurrentSupersedeConflictsTo can skip them. Reading
// the full event-kind range is acceptable: replay frequency is bounded
// by pull cadence (one per network sync, ~30 min) and the conflict
// event count is bounded by the count of unresolved functional slots,
// which is small in practice. Storage-agnostic so CLI callers can pass
// a FileEventLog opened via kernelboot and tests can pass an in-memory
// fake.
func existingConflictPairsOn(
	ctx context.Context, log kernel.EventLog, logger *zap.Logger,
) (map[string]struct{}, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	seen := map[string]struct{}{}
	iter := log.Read(ctx, kernel.ReadOpts{
		Kinds: []kernel.EventKind{kernel.EventKind(ontology.KindRelationSupersedeConflict)},
	})
	var iterErr error
	iter(func(ev kernel.Event, e error) bool {
		if e != nil {
			iterErr = e
			return false
		}
		var payload ontology.RelationSupersedeConflictEvent
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			logger.Warn("Resolver: skipping corrupt supersede-conflict event during dedup",
				zap.String("event_uid", ev.UID),
				zap.Error(err))
			return true
		}
		seen[conflictKey(string(payload.PrimaryRelationUID), string(payload.ConflictingRelationUID))] = struct{}{}
		return true
	})
	if iterErr != nil {
		return nil, iterErr
	}
	return seen, nil
}
