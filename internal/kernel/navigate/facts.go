package navigate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Fact is a scalar state slot read off the facts table: a value addressed by a
// hierarchical state_key for a subject entity. Unlike a Relation (a typed edge
// between two entities) a Fact carries a single scalar — it is where an
// entity's attributes (status, first name, a preference) live. The bi-temporal
// window mirrors Relation: an open Fact (the slot's current value) has
// EventEnd == "".
type Fact struct {
	UID             string `json:"uid"`
	Name            string `json:"name"` // facts/{uid}
	SubjectUID      string `json:"subjectUid"`
	StateKey        string `json:"stateKey"`
	Value           string `json:"value"`
	EventStart      string `json:"eventStart,omitempty"`
	EventEnd        string `json:"eventEnd,omitempty"`
	CreateTime      string `json:"createTime,omitempty"`
	SourceEventUID  string `json:"sourceEventUid,omitempty"` // origin event; walk back to it for provenance
	SupersededByUID string `json:"supersededByUid,omitempty"`
	// Source is the event source identifier (e.g. "mcp:fact", "cli:fact").
	// Populated by migration 0018; empty for facts written before that migration.
	Source string `json:"source,omitempty"`
	// Confidence is the event confidence score for this fact.
	// Populated by migration 0018; zero for facts written before that migration.
	Confidence float64 `json:"confidence,omitempty"`
}

// factColumns is the shared SELECT list for the facts table, in scanFact order.
// Event_start / event_end / superseded_by_uid are nullable, so they coalesce to
// the empty string for a uniform wire shape. Source_event_uid is NOT NULL.
// Source + confidence were added by migration 0018; COALESCE to defaults for
// rows written before that migration.
const factColumns = `uid, subject_uid, state_key, value,
	COALESCE(event_start, ''), COALESCE(event_end, ''),
	create_time, source_event_uid, COALESCE(superseded_by_uid, ''),
	COALESCE(source, ''), COALESCE(confidence, 0.0)`

// factRowScanner is the row interface shared by *sql.Row and *sql.Rows so a
// single scanFact serves both GetFact and FactsForSubject.
type factRowScanner interface {
	Scan(dest ...any) error
}

// scanFact reads one facts row in factColumns order and stamps the AIP name.
func scanFact(s factRowScanner) (Fact, error) {
	var f Fact
	if err := s.Scan(&f.UID, &f.SubjectUID, &f.StateKey, &f.Value,
		&f.EventStart, &f.EventEnd, &f.CreateTime, &f.SourceEventUID, &f.SupersededByUID,
		&f.Source, &f.Confidence); err != nil {
		return Fact{}, err
	}
	f.Name = "facts/" + f.UID
	return f, nil
}

// GetFact returns the Fact with uid, or ErrNotFound.
func (n *Navigator) GetFact(ctx context.Context, uid string) (Fact, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT ` + factColumns + ` FROM facts WHERE uid = ? AND ` + scope
	f, err := scanFact(n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, fmt.Errorf("fact %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return Fact{}, fmt.Errorf("get fact %q: %w", uid, err)
	}
	return f, nil
}

// FactFilter narrows FactsForSubject along the two orthogonal bi-temporal axes
// ratified in ADR-0001 (agent-docs/adr/0001-bitemporal-read-semantics.md). The
// zero value returns each slot's value live NOW and not superseded ("current
// truth"). At moves the valid-time anchor to a point in time (the half-open
// window [start, end) must contain At); AsOf bounds transaction time (drop facts
// recorded after the instant); IncludeInactive returns the full history of every
// slot, bypassing the valid-window AND supersession gates — but never the AsOf
// recording bound.
type FactFilter struct {
	At              *time.Time
	AsOf            *time.Time
	IncludeInactive bool
}

// FactsForSubject returns the Facts about subjectUID honoring a bi-temporal
// filter (ADR-0001). Ordered by state_key, then chronologically within a slot.
// The anchors bind this fact sub-query, never the entity-identity row (the seed
// the caller already holds). This is the temporal-aware read behind get's
// --at / --as-of / --include-inactive: the anchor reaches the SQL here.
func (n *Navigator) FactsForSubject(ctx context.Context, subjectUID string, opts FactFilter) ([]Fact, error) {
	scope, scopeArgs := n.scopeCond("")
	conds := []string{"subject_uid = ?", scope}
	args := append([]any{subjectUID}, scopeArgs...)
	conds, args = appendFactFilter(conds, args, opts)
	//nolint:gosec // G202: conds are constant fragments + bound ? params; values are parameterized
	q := `SELECT ` + factColumns + ` FROM facts WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY state_key ASC, COALESCE(event_start, create_time) ASC, uid ASC`

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("facts for %q: %w", subjectUID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Fact
	for rows.Next() {
		f, serr := scanFact(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan fact: %w", serr)
		}
		out = append(out, f)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate facts: %w", rerr)
	}
	return out, nil
}

// appendFactFilter folds a FactFilter's valid-time + transaction-time predicates
// into a facts query, per ADR-0001. It is the fact-table counterpart of
// appendRelationFilter; together they are the single home for each timelined
// table's temporal SQL (no read surface re-inlines it). The valid window is the
// half-open interval [start, end): a fact is live at @at when
// COALESCE(event_start, create_time) <= @at AND @at < event_end (or the slot is
// still open). @at defaults to now ("current truth"). The IS NULL OR = ” guards
// mirror the relation predicate so legacy rows with empty validity bounds resolve
// identically. Timestamps compare via julianday() so RFC 3339 values of differing
// sub-second precision still order correctly. IncludeInactive bypasses the
// valid-window and supersession gates; AsOf is always honored.
func appendFactFilter(conds []string, args []any, opts FactFilter) ([]string, []any) {
	if !opts.IncludeInactive {
		anchor := time.Now().UTC()
		if opts.At != nil {
			anchor = opts.At.UTC()
		}
		at := anchor.Format(time.RFC3339Nano)
		conds = append(conds,
			"julianday(COALESCE(NULLIF(event_start, ''), create_time)) <= julianday(?)",
			"(event_end IS NULL OR event_end = '' OR julianday(?) < julianday(event_end))",
			"(superseded_by_uid IS NULL OR superseded_by_uid = '')")
		args = append(args, at, at)
	}
	if opts.AsOf != nil {
		conds = append(conds, "julianday(create_time) <= julianday(?)")
		args = append(args, opts.AsOf.UTC().Format(time.RFC3339Nano))
	}
	return conds, args
}
