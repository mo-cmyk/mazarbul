package kernel

import (
	"context"
	"fmt"
)

// PrimarySourceQuery is the optional recall surface backing recall's
// `--has-primary-source` / hasPrimarySource filter (feature #14, issue #208).
// Given a set of record UIDs it reports which are grounded in a primary source:
// the subject of a source_ref provenance row (#207) or of an active
// (not-yet-superseded) has_primary_source relation edge (#208). HybridQuery
// implements it; the recall MCP tool type-asserts for it only when the caller
// passes hasPrimarySource=true, so the extra lookup stays off the default recall
// hot path. Kept separate from Query (mirroring ReportingQuery) so the streaming
// iterator contract and existing Query fakes are untouched.
type PrimarySourceQuery interface {
	SubjectsWithPrimarySource(ctx context.Context, uids, scopes []string) (map[string]bool, error)
}

// hasPrimarySourcePredicate is the seed predicate (#208) whose presence grounds
// a local record in an external_ref. Matches the genesis seed in
// internal/kernelboot/genesis.go; recall's primary-source filter keys off it.
const hasPrimarySourcePredicate = "has_primary_source"

// SubjectsWithPrimarySource implements PrimarySourceQuery. It returns the subset
// of uids that are the subject of a source_ref row OR of an active
// has_primary_source relation edge, read scope-isolated under the same scope
// resolution recall uses (caller scopes, else {project,personal}, fail-closed on
// an unknown token). An empty uids slice short-circuits to an empty set.
func (h *HybridQuery) SubjectsWithPrimarySource(ctx context.Context, uids, scopes []string) (map[string]bool, error) {
	have := make(map[string]bool, len(uids))
	uniq := dedupeNonEmpty(uids)
	if len(uniq) == 0 {
		return have, nil
	}

	ss := scopeSetForRequest(scopes)
	scopePred, scopeArgs := ss.Predicate("")
	inClause, inArgs := inClauseStrings(uniq)

	// Both queries are built by concatenating constant SQL with the "?"-only
	// placeholder string from inClauseStrings + the scope predicate; every value
	// is bound, never spliced (no gosec G201 surface).

	// source_refs: any provenance row pointing at the subject (#207).
	srQuery := "SELECT DISTINCT subject_uid FROM source_refs WHERE subject_uid IN (" +
		inClause + ") AND " + scopePred
	srArgs := append(append([]any{}, inArgs...), scopeArgs...)
	if err := h.collectSubjectUIDs(ctx, "source_refs", srQuery, srArgs, have); err != nil {
		return nil, err
	}

	// relations: an active has_primary_source edge from the subject (#208). An
	// edge whose valid_until is set has been retracted/superseded, so it no
	// longer grounds the record. The dispatcher stores an unset valid_until as
	// the empty string (not SQL NULL), so an "active" edge is NULL-or-empty —
	// matching valid_until IS NULL alone would treat every live edge as ended.
	relQuery := "SELECT DISTINCT subject_uid FROM relations " +
		"WHERE predicate = ? AND (valid_until IS NULL OR valid_until = '') " +
		"AND subject_uid IN (" + inClause + ") AND " + scopePred
	relArgs := append([]any{hasPrimarySourcePredicate}, inArgs...)
	relArgs = append(relArgs, scopeArgs...)
	if err := h.collectSubjectUIDs(ctx, "relations", relQuery, relArgs, have); err != nil {
		return nil, err
	}
	return have, nil
}

// collectSubjectUIDs runs query with args and marks each returned subject_uid
// true in have. `what` names the table for error context. Shared by the
// source_refs and relations lookups.
func (h *HybridQuery) collectSubjectUIDs(ctx context.Context, what, query string, args []any, have map[string]bool) error {
	rows, err := h.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: primary-source %s query: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return fmt.Errorf("kernel: hybrid: primary-source %s scan: %w", what, err)
		}
		have[uid] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: primary-source %s iterate: %w", what, err)
	}
	return nil
}

// dedupeNonEmpty returns vals with empty strings and duplicates removed,
// preserving first-seen order. Keeps the IN list (and bound-arg count) minimal
// when a hit and its entity anchors repeat a UID across the candidate set.
func dedupeNonEmpty(vals []string) []string {
	out := make([]string, 0, len(vals))
	seen := make(map[string]bool, len(vals))
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
