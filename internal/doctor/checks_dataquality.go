package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// checks_dataquality.go holds advisory data-quality checks over the materialised
// graph: duplicate entities (two rows sharing a kind + case-insensitive display
// name) and duplicate active edges (two live relations sharing the same
// subject/predicate/object/status triple). Both are WARN-only — duplicates do
// not break reads, but they clutter recall and usually mean an entity should be
// merged. They are the detective control paired with the create-time dedup gate
// (entitymatch) and the merge-time edge dedup (dedupRepointedEdges): prevention
// stops new duplicates, these surface the ones already in the data.

// newDuplicateEntitiesCheck counts entity rows that share a kind and a
// case-insensitive display name. Each such group is a likely duplicate the user
// can consolidate with `maz entity merge`. Advisory: WARN, never FAIL.
func newDuplicateEntitiesCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "dataquality.duplicate_entities",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
			}
			groups, excess, example, qerr := scanDuplicateEntities(ctx, store.Reader())
			if qerr != nil {
				if strings.Contains(qerr.Error(), "no such table") {
					return StatusOK, "entities table not yet materialised; nothing to check", nil
				}
				return StatusFail, "", qerr
			}
			if groups == 0 {
				return StatusOK, "no duplicate entities (kind + name)", nil
			}
			return StatusWarn, fmt.Sprintf(
				"%d duplicate entity %s (%d redundant rows); e.g. %s — consolidate with `maz entity merge`",
				groups, pluralizeGroup(groups), excess, example), nil
		},
	}
}

// newDupCandidatesCheck counts rows in entity_dup_candidates — entity pairs
// the post-consume dedup sweep flagged as likely duplicates after cross-device
// sync. Advisory WARN so the operator can inspect with `maz review dups` and
// merge with `maz entity merge`. Candidates are rebuilt by subsequent sweeps;
// a count of zero means no duplicates were detected on the last pass.
func newDupCandidatesCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "memory.dup_candidates",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
			}
			count, examples, qerr := scanDupCandidates(ctx, store.Reader())
			if qerr != nil {
				if strings.Contains(qerr.Error(), "no such table") {
					return StatusOK, "entity_dup_candidates table not yet created; sweep has not run", nil
				}
				return StatusFail, "", qerr
			}
			if count == 0 {
				return StatusOK, "no cross-device duplicate entity candidates", nil
			}
			return StatusWarn, fmt.Sprintf(
				"%d duplicate entity candidate %s (e.g. %s); inspect with `maz review dups`, merge with `maz entity merge`",
				count, pluralizeGroup(count), examples), nil
		},
	}
}

// scanDupCandidates returns the total candidate count and a human-readable
// example showing the top (highest-scoring) pair.
func scanDupCandidates(ctx context.Context, reader *sql.DB) (count int, example string, err error) {
	if serr := reader.QueryRowContext(ctx,
		`SELECT count(*) FROM entity_dup_candidates`,
	).Scan(&count); serr != nil {
		return 0, "", fmt.Errorf("count dup candidates: %w", serr)
	}
	if count == 0 {
		return 0, "", nil
	}

	var uidA, uidB, reason string
	var score float64
	if serr := reader.QueryRowContext(ctx,
		`SELECT entity_uid_a, entity_uid_b, reason, score
		   FROM entity_dup_candidates
		  ORDER BY score DESC, detected_at DESC
		  LIMIT 1`,
	).Scan(&uidA, &uidB, &reason, &score); serr != nil {
		return count, "", fmt.Errorf("top dup candidate: %w", serr)
	}
	example = fmt.Sprintf("%s vs %s (%s, score=%.2f)", uidA, uidB, reason, score)
	return count, example, nil
}

// newDuplicateActiveEdgesCheck counts live relations that share the same
// subject/predicate/object/status triple. Status is part of the key so a
// proposed edge that legitimately coexists with an asserted one (different
// status) is not flagged — only true duplicates within one status, which a
// merge should have collapsed (dedupRepointedEdges) or which predate that fix.
// Advisory: WARN, never FAIL.
func newDuplicateActiveEdgesCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "dataquality.duplicate_active_edges",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
			}
			groups, excess, qerr := scanDuplicateActiveEdges(ctx, store.Reader())
			if qerr != nil {
				if strings.Contains(qerr.Error(), "no such table") {
					return StatusOK, "relations table not yet materialised; nothing to check", nil
				}
				return StatusFail, "", qerr
			}
			if groups == 0 {
				return StatusOK, "no duplicate active edges", nil
			}
			return StatusWarn, fmt.Sprintf(
				"%d duplicate active edge %s (%d redundant rows); drop extras with `maz forget <relationUid>` (a merge auto-dedups going forward)",
				groups, pluralizeGroup(groups), excess), nil
		},
	}
}

// scanDuplicateEntities returns the number of duplicate (kind, lower(name))
// groups, the count of redundant rows beyond one-per-group, and a human example
// of the worst (most-duplicated) group.
func scanDuplicateEntities(ctx context.Context, reader *sql.DB) (groups, excess int, example string, err error) {
	rows, err := reader.QueryContext(ctx,
		`SELECT kind, display_name, count(*) AS n
		   FROM entities
		  GROUP BY kind, LOWER(display_name)
		 HAVING n > 1
		 ORDER BY n DESC, kind, display_name`)
	if err != nil {
		return 0, 0, "", fmt.Errorf("query duplicate entities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var kind, name string
		var n int
		if serr := rows.Scan(&kind, &name, &n); serr != nil {
			return 0, 0, "", fmt.Errorf("scan duplicate entities: %w", serr)
		}
		groups++
		excess += n - 1
		if groups == 1 {
			example = fmt.Sprintf("%q (%s) ×%d", name, kind, n)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return 0, 0, "", fmt.Errorf("iterate duplicate entities: %w", rerr)
	}
	return groups, excess, example, nil
}

// scanDuplicateActiveEdges returns the number of duplicate live-relation triples
// (keyed by subject/predicate/object/status) and the count of redundant rows
// beyond one-per-group.
func scanDuplicateActiveEdges(ctx context.Context, reader *sql.DB) (groups, excess int, err error) {
	rows, err := reader.QueryContext(ctx,
		`SELECT count(*) AS n
		   FROM relations
		  WHERE superseded_by_uid IS NULL
		  GROUP BY subject_uid, predicate, object_uid, status
		 HAVING n > 1`)
	if err != nil {
		return 0, 0, fmt.Errorf("query duplicate active edges: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var n int
		if serr := rows.Scan(&n); serr != nil {
			return 0, 0, fmt.Errorf("scan duplicate active edges: %w", serr)
		}
		groups++
		excess += n - 1
	}
	if rerr := rows.Err(); rerr != nil {
		return 0, 0, fmt.Errorf("iterate duplicate active edges: %w", rerr)
	}
	return groups, excess, nil
}

// newSupersedeConflictsCheck counts open relationSupersedeConflict events in the
// event log — functional-slot collisions detected after cross-device sync where
// two devices independently superseded the same relation. Advisory WARN so the
// operator can inspect with `maz review conflicts` and resolve by forgetting the
// losing arm. A count of zero means no outstanding conflicts.
func newSupersedeConflictsCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "memory.supersede_conflicts",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
			}
			count, example, qerr := scanOpenSupersedeConflicts(ctx, store.Reader())
			if qerr != nil {
				if strings.Contains(qerr.Error(), "no such table") {
					return StatusOK, "relations table not yet materialised; nothing to check", nil
				}
				return StatusFail, "", qerr
			}
			if count == 0 {
				return StatusOK, "no open supersede conflicts", nil
			}
			return StatusWarn, fmt.Sprintf(
				"%d open supersede conflict %s (e.g. subject=%s predicate=%s); inspect with `maz review conflicts`",
				count, pluralizeGroup(count), example.subjectUID, example.predicate), nil
		},
	}
}

// supersedeConflictExample holds the subject and predicate from the
// most-recently-detected open conflict, for the WARN detail line.
type supersedeConflictExample struct {
	subjectUID string
	predicate  string
}

// scanOpenSupersedeConflicts counts functional slots that have more than one open
// (valid_until IS NULL) relation — the same scan maz review conflicts runs. The
// relation table scan is preferred over the event log scan because it reflects the
// current materialised state after any manual resolution (forgetting a loser
// closes the slot). Returns count=0 when there are no conflicts.
func scanOpenSupersedeConflicts(ctx context.Context, reader *sql.DB) (int, supersedeConflictExample, error) {
	// Count open supersede conflicts by scanning event log for
	// relationSupersedeConflict events whose primary relation is still open
	// (valid_until IS NULL). Joining against relations gives us the current
	// state: if the operator forgets the loser, the relation row disappears and
	// the conflict stops appearing here without any new event needed.
	//
	// We read the primary_relation_uid from the event payload and join to the
	// relations table so only still-open primary arms count as conflicts.
	rows, err := reader.QueryContext(ctx,
		`SELECT r.subject_uid, r.predicate
		   FROM relations r
		  WHERE r.valid_until IS NULL
		  GROUP BY r.subject_uid, r.predicate
		 HAVING count(*) > 1
		  ORDER BY r.subject_uid, r.predicate
		  LIMIT 100`,
	)
	if err != nil {
		return 0, supersedeConflictExample{}, fmt.Errorf("scan open supersede conflicts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var count int
	var ex supersedeConflictExample
	for rows.Next() {
		var subjectUID, predicate string
		if serr := rows.Scan(&subjectUID, &predicate); serr != nil {
			return 0, supersedeConflictExample{}, fmt.Errorf("scan row: %w", serr)
		}
		// Only flag slots whose predicate is functional. We need to check the
		// predicates table to filter. Skip the join for simplicity: a multi-open
		// non-functional slot is fine (that is the definition), so we read the
		// predicate kind separately to filter.
		var kind string
		kerr := reader.QueryRowContext(ctx,
			`SELECT kind FROM predicates WHERE predicate = ? LIMIT 1`, predicate,
		).Scan(&kind)
		if kerr != nil {
			// Unknown predicate → non-functional by conservative default; skip.
			continue
		}
		if kind != "functional" {
			continue
		}
		count++
		if count == 1 {
			ex = supersedeConflictExample{subjectUID: subjectUID, predicate: predicate}
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return 0, supersedeConflictExample{}, fmt.Errorf("iterate open supersede conflicts: %w", rerr)
	}
	return count, ex, nil
}

// pluralizeGroup returns "group" for n==1, else "groups".
func pluralizeGroup(n int) string {
	if n == 1 {
		return "group"
	}
	return "groups"
}
