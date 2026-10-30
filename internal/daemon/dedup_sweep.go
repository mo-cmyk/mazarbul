package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// sweepAppender is the subset of the event log the dedup sweep uses to
// append entityMerged events. The kernel.EventLog satisfies this; the narrow
// interface keeps DedupSweep free of the full EventLog import in tests.
type sweepAppender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// DedupSweep is the post-consume duplicate-detection sweep. After a
// team-scope graft batch lands new events from a remote device, Run is called
// with the entity UIDs touched by that batch. For each entity it executes
// three entitymatch lanes:
//
//   - Lane (a) exact-name: same kind + case-insensitive display name.
//     On a hit, the pair is inserted into entity_dup_candidates for human
//     review. No auto-merge — homonyms like two different "John Smith" people
//     must not be collapsed automatically.
//
//   - Lane (b) strong-identifier: an email, username, or domain identifier
//     shared between two entities of the same kind. This is the same signal
//     createEntity already treats as confident reuse, so the sweep AUTO-MERGES:
//     it appends an entityMerged event with the lexicographically smaller UID
//     as survivor (MIN uid = older entity). No candidate row is written.
//
//   - Lane (c) name-KNN: advisory cosine-similarity lane (only when an
//     embedder is wired). Hits above the advisory threshold are inserted into
//     entity_dup_candidates. No auto-merge.
//
// entity_dup_candidates is a DERIVED table (migration 0051): safe to truncate;
// the next sweep pass rebuilds it.
type DedupSweep struct {
	reader   *sql.DB
	writer   *sql.DB
	appender sweepAppender
	lookup   *entitymatch.Lookup
	logger   *zap.Logger
}

// NewDedupSweep constructs the sweep. Returns nil when any required field is
// absent so callers can treat a nil DedupSweep as "disabled" uniformly.
func NewDedupSweep(reader, writer *sql.DB, appender sweepAppender, lookup *entitymatch.Lookup, logger *zap.Logger) *DedupSweep {
	if reader == nil || writer == nil || appender == nil || lookup == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DedupSweep{
		reader:   reader,
		writer:   writer,
		appender: appender,
		lookup:   lookup,
		logger:   logger,
	}
}

// Run sweeps the provided entity UIDs for cross-device duplicates. It is
// idempotent and best-effort: a per-entity failure is logged and skipped
// without aborting the remaining entities.
func (d *DedupSweep) Run(ctx context.Context, entityUIDs []string) {
	if d == nil || len(entityUIDs) == 0 {
		return
	}
	for _, uid := range dedupStringSlice(entityUIDs) {
		if cerr := ctx.Err(); cerr != nil {
			return
		}
		d.sweepOne(ctx, uid)
	}
}

// dedupStringSlice removes repeated values while preserving order.
func dedupStringSlice(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// sweepOne performs all three lanes for a single entity UID.
func (d *DedupSweep) sweepOne(ctx context.Context, uid string) {
	name, kind, scope, ok, err := d.loadEntity(ctx, uid)
	if err != nil {
		d.logger.Warn("daemon: dedup sweep load entity", zap.String("uid", uid), zap.Error(err))
		return
	}
	if !ok {
		// Entity absent: already merged away or dispatcher lag. Nothing to sweep.
		return
	}

	// Lane (a): exact normalized-name + same entity kind.
	d.sweepExactName(ctx, uid, name, kind)

	// Lane (b): strong-identifier equality.
	d.sweepIdentifiers(ctx, uid, kind, scope)

	// Lane (c): name-KNN advisory (no-op when embedder absent).
	d.sweepNameKNN(ctx, uid, name, kind)
}

// sweepExactName runs lane (a): look for another entity with the same
// case-insensitive display name AND the same entity kind, excluding the entity
// being swept. Uses a direct query rather than entitymatch.ExactNameMatch
// because ExactNameMatch does not exclude the input UID, so it would always
// find the entity itself on the case-sensitive pass and the guard would exit.
func (d *DedupSweep) sweepExactName(ctx context.Context, uid, name, kind string) {
	var matchUID string
	err := d.reader.QueryRowContext(ctx,
		`SELECT uid FROM entities
		  WHERE LOWER(display_name) = LOWER(?) AND kind = ? AND uid != ?
		  ORDER BY uid
		  LIMIT 1`,
		name, kind, uid,
	).Scan(&matchUID)
	switch {
	case err == nil:
		// Hit — record advisory candidate.
	case errors.Is(err, sql.ErrNoRows):
		return
	default:
		d.logger.Warn("daemon: dedup sweep exact-name lane", zap.String("uid", uid), zap.Error(err))
		return
	}
	if rerr := d.recordCandidate(ctx, uid, matchUID, "exact_name", 1.0); rerr != nil {
		d.logger.Warn("daemon: dedup sweep record exact-name candidate",
			zap.String("uid_a", uid), zap.String("uid_b", matchUID), zap.Error(rerr))
	}
}

// sweepIdentifiers runs lane (b): for each strong identifier on this entity,
// check whether another entity carries the same value. On a kind-matching hit,
// auto-merge by appending an entityMerged event (MIN uid survives).
func (d *DedupSweep) sweepIdentifiers(ctx context.Context, uid, kind, scope string) {
	ids, err := d.loadIdentifiers(ctx, uid)
	if err != nil {
		d.logger.Warn("daemon: dedup sweep load identifiers", zap.String("uid", uid), zap.Error(err))
		return
	}
	seen := make(map[string]struct{}) // avoid merging the same peer twice in one pass
	for _, id := range ids {
		// Skip alias and generic "id" identifiers — only typed strong identifiers
		// (email, username, domain kinds) are treated as authoritative anchors.
		if id.kind == string(ontology.IdentifierKindAlias) ||
			id.kind == string(ontology.IdentifierKindID) {
			continue
		}
		if len(id.value) < entitymatch.StrongIdentifierMinLength {
			continue
		}
		matchUID, matchKind, merr := d.lookup.IdentifierMatch(ctx, id.value, "")
		if merr != nil {
			d.logger.Warn("daemon: dedup sweep identifier lane",
				zap.String("uid", uid), zap.String("identifier", id.value), zap.Error(merr))
			continue
		}
		if matchUID == "" || matchUID == uid {
			continue
		}
		// Auto-merge only when entity kinds agree — strong-id equality for
		// mismatched kinds (e.g. a username shared by a person and a project)
		// is plausible data rather than a certain duplicate.
		if matchKind != kind {
			continue
		}
		if _, done := seen[matchUID]; done {
			continue
		}
		seen[matchUID] = struct{}{}
		d.autoMerge(ctx, uid, matchUID, scope)
	}
}

// sweepNameKNN runs lane (c): advisory cosine-similarity search. Adds
// candidate rows for hits above the advisory threshold; never auto-merges.
func (d *DedupSweep) sweepNameKNN(ctx context.Context, uid, name, kind string) {
	candidates, err := d.lookup.NameKNN(ctx, name, kind, 5)
	if err != nil {
		d.logger.Warn("daemon: dedup sweep name-KNN lane", zap.String("uid", uid), zap.Error(err))
		return
	}
	for _, c := range candidates {
		if c.UID == uid {
			continue
		}
		if c.Similarity < d.lookup.AdvisoryThreshold {
			continue
		}
		if rerr := d.recordCandidate(ctx, uid, c.UID, "name_emb", c.Similarity); rerr != nil {
			d.logger.Warn("daemon: dedup sweep record name-KNN candidate",
				zap.String("uid_a", uid), zap.String("uid_b", c.UID), zap.Error(rerr))
		}
	}
}

// autoMerge appends an entityMerged event through the daemon's normal append
// path. The lexicographically smaller UID is the survivor (MIN uid = older
// entity, per ULID monotonicity). The source entity's scope is forwarded so
// the entityMerged event carries the right scope for team-scope ACL checks.
func (d *DedupSweep) autoMerge(ctx context.Context, uidA, uidB, scope string) {
	// Survivor = lexicographically smaller UID (chronologically older).
	target, source := uidA, uidB
	if uidB < uidA {
		target, source = uidB, uidA
	}
	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: source,
		TargetEntityUID: target,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		d.logger.Error("daemon: dedup sweep marshal entityMerged", zap.Error(err))
		return
	}
	_, aerr := d.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "daemon/dedupSweep",
		Confidence: 1.0,
		Scope:      kernel.Scope(scope),
		Payload:    payload,
	}})
	if aerr != nil {
		d.logger.Warn("daemon: dedup sweep auto-merge append",
			zap.String("source", source), zap.String("target", target), zap.Error(aerr))
		return
	}
	d.logger.Info("daemon: dedup sweep auto-merged duplicate entity (strong-identifier match)",
		zap.String("source", source), zap.String("target", target))
}

// recordCandidate upserts a pair into entity_dup_candidates. The pair is
// stored with the lexicographically smaller UID in entity_uid_a to satisfy
// the CHECK constraint and deduplicate (a,b) vs (b,a) permutations.
func (d *DedupSweep) recordCandidate(ctx context.Context, uidA, uidB, reason string, score float64) error {
	a, b := uidA, uidB
	if b < a {
		a, b = b, a
	}
	_, err := d.writer.ExecContext(ctx,
		`INSERT INTO entity_dup_candidates
		     (entity_uid_a, entity_uid_b, reason, score, detected_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(entity_uid_a, entity_uid_b)
		 DO UPDATE SET reason=excluded.reason, score=excluded.score, detected_at=excluded.detected_at`,
		a, b, reason, score, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("upsert dup candidate (%s, %s): %w", a, b, err)
	}
	return nil
}

// loadEntity reads an entity's display name, kind, and scope by UID.
// Returns ok=false when the entity is absent (merged away or not yet projected).
func (d *DedupSweep) loadEntity(ctx context.Context, uid string) (name, kind, scope string, ok bool, err error) {
	err = d.reader.QueryRowContext(ctx,
		`SELECT display_name, kind, COALESCE(scope, 'personal') FROM entities WHERE uid = ? LIMIT 1`,
		uid,
	).Scan(&name, &kind, &scope)
	switch {
	case err == nil:
		return name, kind, scope, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", "", "", false, nil
	default:
		return "", "", "", false, fmt.Errorf("load entity %s: %w", uid, err)
	}
}

// identifierEntry holds one row from entity_identifiers.
type identifierEntry struct {
	value string
	kind  string
}

// loadIdentifiers returns all identifier rows for an entity.
func (d *DedupSweep) loadIdentifiers(ctx context.Context, entityUID string) ([]identifierEntry, error) {
	rows, err := d.reader.QueryContext(ctx,
		`SELECT identifier, kind FROM entity_identifiers WHERE entity_uid = ?`,
		entityUID,
	)
	if err != nil {
		return nil, fmt.Errorf("query identifiers for %s: %w", entityUID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []identifierEntry
	for rows.Next() {
		var e identifierEntry
		if serr := rows.Scan(&e.value, &e.kind); serr != nil {
			return nil, fmt.Errorf("scan identifier: %w", serr)
		}
		out = append(out, e)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate identifiers: %w", rerr)
	}
	return out, nil
}
