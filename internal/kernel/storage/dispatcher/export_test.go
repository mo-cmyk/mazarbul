package dispatcher

import (
	"context"
	"database/sql"
	"fmt"
)

// ResolveRedirectForTest exposes resolveRedirect for convergence tests: given a
// UID it follows the entity_redirects chain to fixpoint within a fresh
// read-only query against the dispatcher's writer connection. This mirrors the
// read path an applier uses and lets tests assert canonical survivor UIDs
// without standing up a Navigator.
func ResolveRedirectForTest(ctx context.Context, d *Dispatcher, uid string) (string, error) {
	db := d.sqlite.Writer()
	// Open a throw-away transaction so resolveRedirect can use its tx interface.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx for resolve: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return resolveRedirect(ctx, tx, uid)
}

// EntityExistsForTest reports whether an entities row with the given uid is
// present in the dispatcher's SQLite store.
func EntityExistsForTest(ctx context.Context, d *Dispatcher, uid string) (bool, error) {
	var n int
	err := d.sqlite.Writer().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE uid = ?`, uid,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("count entity %q: %w", uid, err)
	}
	return n > 0, nil
}

// FactSubjectForTest returns the current subject_uid of the fact row with the
// given uid, or "" when no such row exists.
func FactSubjectForTest(ctx context.Context, d *Dispatcher, factUID string) (string, error) {
	var subj string
	err := d.sqlite.Writer().QueryRowContext(ctx,
		`SELECT subject_uid FROM facts WHERE uid = ?`, factUID,
	).Scan(&subj)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("fact subject %q: %w", factUID, err)
	}
	return subj, nil
}

// CloseInnerGraphForTest closes the dispatcher-owned LadybugDB connection
// directly, bypassing the OpenGraph non-closing wrapper. It exists only
// so the cross-store rollback integration test can provoke a graph
// failure (next MERGE returns ladybug.ErrClosed) without leaking the
// inner connection through the public surface.
//
// Build tag-less so it compiles alongside dispatcher.go in every test
// build; the underscore-test filename keeps it out of production
// binaries.
func CloseInnerGraphForTest(d *Dispatcher) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.graph == nil {
		return nil
	}
	return d.graph.Close()
}

// MarshalAttributesForTest is a test-only handle on the unexported
// marshalAttributes helper. The same JSON shape lands in the SQLite
// relations.attributes column and the LadybugDB r.attributes edge
// property; testing the marshaller in isolation pins the column
// contract without standing up the full dispatcher.
func MarshalAttributesForTest(attrs map[string]string) string {
	return marshalAttributes(attrs)
}

// RelationCloseForTest reads the materialized close columns (valid_until,
// superseded_by_uid) for one relation off the dispatcher's own SQLite store, so
// the convergent-retraction tests can assert the deterministic fold result
// without standing up a Navigator. Empty strings denote SQL NULL.
func RelationCloseForTest(ctx context.Context, d *Dispatcher, uid string) (validUntil, supersededBy string, err error) {
	row := d.sqlite.Writer().QueryRowContext(ctx,
		`SELECT COALESCE(valid_until, ''), COALESCE(superseded_by_uid, '') FROM relations WHERE uid = ?`,
		uid)
	if scanErr := row.Scan(&validUntil, &supersededBy); scanErr != nil {
		return "", "", fmt.Errorf("scan relation close: %w", scanErr)
	}
	return validUntil, supersededBy, nil
}

// RelationStatusForTest reads the materialized trust columns (status,
// asserted_by_principal) for one relation off the dispatcher's own SQLite
// store, so the promotion tests can assert the proposed→asserted flip and the
// promoter stamp without standing up a Navigator. Empty asserter denotes SQL
// NULL.
func RelationStatusForTest(ctx context.Context, d *Dispatcher, uid string) (status, assertedBy string, err error) {
	row := d.sqlite.Writer().QueryRowContext(ctx,
		`SELECT status, COALESCE(asserted_by_principal, '') FROM relations WHERE uid = ?`,
		uid)
	if scanErr := row.Scan(&status, &assertedBy); scanErr != nil {
		return "", "", fmt.Errorf("scan relation status: %w", scanErr)
	}
	return status, assertedBy, nil
}

// RelationAssertedByNullForTest reports whether a relation's
// asserted_by_principal column IS SQL NULL (as opposed to an empty string), so
// the promotion tests can pin that an unsigned/legacy-author promotion binds
// NULL — the COALESCE that preserves a prior asserter relies on the distinction.
func RelationAssertedByNullForTest(ctx context.Context, d *Dispatcher, uid string) (bool, error) {
	var assertedBy sql.NullString
	row := d.sqlite.Writer().QueryRowContext(ctx,
		`SELECT asserted_by_principal FROM relations WHERE uid = ?`, uid)
	if scanErr := row.Scan(&assertedBy); scanErr != nil {
		return false, fmt.Errorf("scan asserted_by_principal: %w", scanErr)
	}
	return !assertedBy.Valid, nil
}

// FactRowForTest is one facts-table row in supersede-chain order, projected
// for assertions: the value held, whether the row is closed (EventEnd != ""),
// and which Fact closed it (SupersededBy).
type FactRowForTest struct {
	UID          string
	Value        string
	EventEnd     string
	SupersededBy string
	Scope        string
	RetractedAt  string
}

// FactRowsForTest returns every Fact occupying (subjectUID, stateKey) in
// HLC-then-uid order — matching readFactSlot's canonical order — reading off
// the dispatcher's own SQLite store. It lets the facts-writer tests assert the
// validity window + supersede pointers without standing up a Navigator.
func FactRowsForTest(ctx context.Context, d *Dispatcher, subjectUID, stateKey string) ([]FactRowForTest, error) {
	rows, err := d.sqlite.Writer().QueryContext(ctx,
		`SELECT uid, value, COALESCE(event_end, ''), COALESCE(superseded_by_uid, ''), scope,
		        COALESCE(retracted_at, '')
		 FROM facts
		 WHERE subject_uid = ? AND state_key = ?
		 ORDER BY COALESCE(hlc, printf('%012x%04x',
		              CAST(ROUND((julianday(create_time) - 2440587.5) * 86400000.0) AS INTEGER), 0)) ASC,
		          uid ASC`,
		subjectUID, stateKey)
	if err != nil {
		return nil, fmt.Errorf("query facts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FactRowForTest
	for rows.Next() {
		var r FactRowForTest
		if serr := rows.Scan(&r.UID, &r.Value, &r.EventEnd, &r.SupersededBy, &r.Scope, &r.RetractedAt); serr != nil {
			return nil, fmt.Errorf("scan fact row: %w", serr)
		}
		out = append(out, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate facts: %w", rerr)
	}
	return out, nil
}
