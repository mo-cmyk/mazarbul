package ontology

import "time"

// Tombstone is one element of a relation's monotonic retraction set, as
// materialized from a relationRetracted event. SupersededByUID is the ULID of
// the replacing relation, or empty for a pure temporal close — a relationship
// that simply ended with no replacement (the unrelate tool / `maz unrelate`),
// in which case the edge is closed by valid_until alone and superseded_by_uid
// stays NULL. RetractedAt is the instant the close was applied.
type Tombstone struct {
	SupersededByUID string
	RetractedAt     time.Time
}

// ReduceRetractions folds a relation's FULL tombstone set — plus any
// extractor-set validity bound on the relation row itself — into the
// canonical (supersededByUID, validUntil) that the relations row and graph
// edge materialize.
//
// The fold is the load-bearing piece of convergent retraction. It must be a
// pure, deterministic function of its inputs so that SQLite, the graph edge,
// and the git-render projection all derive identical state, and so that two
// paired devices replaying the same set of relationRetracted events in
// different orders converge:
//
//   - superseded_by_uid ← MIN(superseded_by_uid) by ULID lexical order across
//     all tombstones carrying a non-empty superseder. nil when the set is
//     empty or holds only pure-forget tombstones.
//   - valid_until      ← MIN(extractedValidUntil, MIN(retracted_at)). The
//     earliest close wins (bi-temporally correct: the fact stopped being true
//     at the earliest known transition), and folding against the extracted
//     bound preserves a legitimate extractor-set validity window instead of
//     clobbering it. nil when there is neither a tombstone nor an extracted
//     bound (the relation is open).
//
// MIN is commutative, associative, and idempotent, so the result is
// independent of tombstone order and stable under re-application. The returned
// pointers are nil to signal SQL NULL (open / no replacement); callers bind
// them directly.
func ReduceRetractions(extractedValidUntil *time.Time, tombstones []Tombstone) (supersededByUID *string, validUntil *time.Time) {
	if extractedValidUntil != nil {
		vu := extractedValidUntil.UTC()
		validUntil = &vu
	}
	for i := range tombstones {
		t := tombstones[i]
		if t.SupersededByUID != "" {
			if supersededByUID == nil || t.SupersededByUID < *supersededByUID {
				uid := t.SupersededByUID
				supersededByUID = &uid
			}
		}
		ra := t.RetractedAt.UTC()
		if validUntil == nil || ra.Before(*validUntil) {
			validUntil = &ra
		}
	}
	return supersededByUID, validUntil
}
