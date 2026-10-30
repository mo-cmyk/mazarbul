package navigate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ContextPin is one user-authored context pin or correction read off the
// context_pins table. It is durable steering the cold-start brief surfaces:
// a pin (Type "pin") is a free-floating or entity-anchored note, and a
// correction (Type "correction") overrides a surfaced item (TargetRef carries
// that item's id). A retired pin (unpinned) has Active=false; ContextPins
// returns only live rows, while GetContextPin returns either.
type ContextPin struct {
	UID            string `json:"uid"`
	Name           string `json:"name"` // contextPins/{uid}
	Type           string `json:"type"` // pin | correction
	Text           string `json:"text"`
	TargetRef      string `json:"targetRef,omitempty"` // pin: optional entity/resource ref; correction: corrected item id
	Active         bool   `json:"active"`
	SourceEventUID string `json:"sourceEventUid"`
	CreateTime     string `json:"createTime"`
	Scope          string `json:"scope"`
}

// contextPinColumns is the shared SELECT list for the context_pins table, in
// scanContextPin order. Target_ref is nullable and coalesces to the empty
// string for a uniform wire shape; every other column is NOT NULL.
const contextPinColumns = `uid, pin_type, text, COALESCE(target_ref, ''),
	active, source_event_uid, create_time, scope`

// scanContextPin reads one context_pins row in contextPinColumns order, maps
// the integer active flag to a bool, and stamps the AIP resource name.
func scanContextPin(s rowScanner) (ContextPin, error) {
	var (
		p      ContextPin
		active int
	)
	if err := s.Scan(&p.UID, &p.Type, &p.Text, &p.TargetRef,
		&active, &p.SourceEventUID, &p.CreateTime, &p.Scope); err != nil {
		return ContextPin{}, err
	}
	p.Active = active != 0
	p.Name = "contextPins/" + p.UID
	return p, nil
}

// ContextPins returns the LIVE (active) pins and corrections visible in the
// navigator's scope span, newest first. The read is scope-isolated: the
// navigator's scope predicate is ANDed into the query so an empty scope set
// fails closed (#206), exactly like ContextPins' sibling readers.
//
// Both pin_type='pin' and pin_type='correction' rows are returned; a caller
// (the brief assembler) discriminates on ContextPin.Type and, for corrections,
// resolves the overlay as the most recent correction per TargetRef. Retired
// pins (active=0, tombstoned by contextUnpinned) are excluded. The result is
// capped at defaultLimit to bound the brief's context budget.
func (n *Navigator) ContextPins(ctx context.Context) ([]ContextPin, error) {
	scope, scopeArgs := n.scopeCond("")
	//nolint:gosec // G202: scope is constant fragments + bound ? params; values are parameterized
	q := `SELECT ` + contextPinColumns + ` FROM context_pins
	      WHERE active = 1 AND ` + scope + `
	      ORDER BY create_time DESC, uid DESC
	      LIMIT ?`
	args := append([]any{}, scopeArgs...)
	args = append(args, defaultLimit)

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list context pins: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ContextPin, 0, defaultLimit)
	for rows.Next() {
		p, serr := scanContextPin(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan context pin: %w", serr)
		}
		out = append(out, p)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate context pins: %w", rerr)
	}
	return out, nil
}

// GetContextPin returns the pin or correction with uid, or ErrNotFound. The read
// is scope-isolated like GetInsight. A retired pin (active=0) is still returned
// — get is not the live brief index, so every fetchable row is accessible
// regardless of active state (a consumer reads ContextPin.Active to tell).
func (n *Navigator) GetContextPin(ctx context.Context, uid string) (ContextPin, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT ` + contextPinColumns + ` FROM context_pins WHERE uid = ? AND ` + scope
	p, err := scanContextPin(n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return ContextPin{}, fmt.Errorf("context pin %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return ContextPin{}, fmt.Errorf("get context pin %q: %w", uid, err)
	}
	return p, nil
}
