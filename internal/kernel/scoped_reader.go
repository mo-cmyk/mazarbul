package kernel

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// ScopeSet is an immutable set of scopes a read may span. It is the unit the
// read spine carries instead of an implicit "all scopes" so that scope
// isolation is a structural property, not a per-call-site afterthought.
//
// The zero value is the empty set, which FAILS CLOSED: Predicate emits a
// never-match guard so a reader that reached the SQL layer without an explicit
// scope returns nothing rather than everything. A forgotten scope is therefore
// a loud visibility miss, never a silent cross-scope leak (#206).
type ScopeSet struct {
	// scopes is sorted + deduplicated. nil/empty == fail-closed. Stored by
	// value and never mutated after construction, so ScopeSet copies freely.
	scopes []Scope
}

// NewScopeSet builds a ScopeSet from the given scopes, deduplicating and
// sorting them for deterministic Predicate output. Empty / unrecognised scopes
// are dropped; pass validated scopes (use ParseScopeSet for untrusted input).
func NewScopeSet(scopes ...Scope) ScopeSet {
	out := make([]Scope, 0, len(scopes))
	for _, s := range scopes {
		if s.Valid() && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return ScopeSet{scopes: out}
}

// DefaultReadSet is the scope span applied when a caller does not name scopes:
// {project, personal}. This is the deliberate single-user default — recall and
// getContext no longer span every scope; `team` (and any future scope) is
// explicit opt-in. Distinct from the empty set, which fails closed.
func DefaultReadSet() ScopeSet { return NewScopeSet(ScopeProject, ScopePersonal) }

// AllScopes is every navigable user scope: {project, personal, team}. Reserved
// for deliberate full-projection reads (rebuild verification, doctor,
// maintenance) and graph navigation; naming it makes a full-scope read an
// explicit choice, never an accident. ScopeDraft is intentionally NOT included:
// draft is device-local staging that must stay invisible to both default recall
// and navigation, so it is reachable only via an explicit NewScopeSet(ScopeDraft)
// at the dedicated draft call sites — never by widening to "all".
func AllScopes() ScopeSet { return NewScopeSet(ScopeProject, ScopePersonal, ScopeTeam) }

// ScopeDraftReadSet is the scope span used to read draft-only rows. It is the
// ONLY way to see draft-scoped data; draft is excluded from DefaultReadSet and
// AllScopes so it never leaks into recall or navigation by accident.
func ScopeDraftReadSet() ScopeSet { return NewScopeSet(ScopeDraft) }

// ParseScopeSet converts untrusted scope strings (an MCP `scopes` array, a CLI
// --scope flag) into a ScopeSet, rejecting any token that is not a recognised
// scope. An empty input yields the empty set; callers decide whether absent
// scopes mean "fail closed" or "apply DefaultReadSet". Unknown scopes return an
// error the tool layer maps to E_VALIDATION.
func ParseScopeSet(raw []string) (ScopeSet, error) {
	parsed := make([]Scope, 0, len(raw))
	for _, r := range raw {
		s := Scope(r)
		if !s.Valid() {
			return ScopeSet{}, fmt.Errorf("scope %q is not in {project|personal|team|draft}", r)
		}
		parsed = append(parsed, s)
	}
	return NewScopeSet(parsed...), nil
}

// IsEmpty reports whether the set spans no scopes (the fail-closed state).
func (s ScopeSet) IsEmpty() bool { return len(s.scopes) == 0 }

// Scopes returns a copy of the set's scopes in sorted order.
func (s ScopeSet) Scopes() []Scope { return slices.Clone(s.scopes) }

// Contains reports whether sc is in the set.
func (s ScopeSet) Contains(sc Scope) bool { return slices.Contains(s.scopes, sc) }

// Equal reports whether two sets span exactly the same scopes.
func (s ScopeSet) Equal(other ScopeSet) bool { return slices.Equal(s.scopes, other.scopes) }

// String renders the set for logs and errors, e.g. "{personal,project}" or
// "{}" for the empty (fail-closed) set.
func (s ScopeSet) String() string {
	parts := make([]string, len(s.scopes))
	for i, sc := range s.scopes {
		parts[i] = string(sc)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Predicate returns a SQL boolean expression that admits only rows whose scope
// is in the set, plus its bound arguments. Alias is the table alias the `scope`
// column belongs to ("" for an unqualified column, "e" for `e.scope`).
//
//   - empty set  -> ("<col> IN (NULL)", nil): NULL never satisfies IN, so the
//     guard reliably admits zero rows AND still references the scope column, so
//     the SQL-trace enforcement test recognises it as scope-predicated.
//   - non-empty  -> ("<col> IN (?, ?)", ["personal","project"]): args are the
//     sorted scope strings, so callers can rely on stable ordering in tests.
//
// Callers AND this expression into every shared-table SELECT's WHERE clause.
func (s ScopeSet) Predicate(alias string) (string, []any) {
	col := "scope"
	if alias != "" {
		col = alias + ".scope"
	}
	if s.IsEmpty() {
		return col + " IN (NULL)", nil
	}
	placeholders := strings.Repeat("?, ", len(s.scopes))
	placeholders = placeholders[:len(placeholders)-2] // trim trailing ", "
	args := make([]any, len(s.scopes))
	for i, sc := range s.scopes {
		args[i] = string(sc)
	}
	return fmt.Sprintf("%s IN (%s)", col, placeholders), args
}

// ScopedReader wraps the read-only SQLite pool with a ScopeSet. It is the
// capability the read spine is constructed over: a component cannot obtain one
// without a ScopeSet, and the zero ScopeSet fails closed. Query builders read
// the scope predicate from Predicate(alias) and AND it into every shared-table
// SELECT; the SQL-trace test proves no shared-table query escapes without it.
//
// The predicate is applied by the builders, not auto-injected here: SQLite has
// no row-level security, and the read-spine SQL has joins and subqueries that
// defy reliable rewriting. The wrapper's guarantee is that the scope is always
// present and always explicit; the trace test is what makes it non-bypassable.
type ScopedReader struct {
	db     *sql.DB
	scopes ScopeSet
}

// NewScopedReader wraps db with the given scope span. A nil db panics on first
// use; pass the reader pool from sqlite.Store.Reader / Dispatcher.SQLiteReader.
func NewScopedReader(db *sql.DB, scopes ScopeSet) *ScopedReader {
	return &ScopedReader{db: db, scopes: scopes}
}

// WithScopes returns a sibling reader over the same pool narrowed (or widened)
// to s. Used to derive a per-request reader from a long-lived one without
// threading scope through every call site.
func (r *ScopedReader) WithScopes(s ScopeSet) *ScopedReader {
	return &ScopedReader{db: r.db, scopes: s}
}

// Scopes returns the reader's scope span.
func (r *ScopedReader) Scopes() ScopeSet { return r.scopes }

// Predicate is shorthand for the reader's ScopeSet.Predicate.
func (r *ScopedReader) Predicate(alias string) (string, []any) { return r.scopes.Predicate(alias) }

// DB exposes the underlying pool for the rare call that hands a *sql.DB to an
// external helper (e.g. a geo scan). Callers MUST still apply the scope
// predicate themselves; reaching for DB does not exempt a query from isolation.
func (r *ScopedReader) DB() *sql.DB { return r.db }

// QueryContext delegates to the underlying pool so ScopedReader is a drop-in
// for *sql.DB at read-spine call sites.
func (r *ScopedReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	//nolint:wrapcheck // transparent delegation: ScopedReader mirrors the *sql.DB contract, so the raw sql error is the contract callers expect.
	return r.db.QueryContext(ctx, query, args...)
}

// QueryRowContext delegates to the underlying pool.
func (r *ScopedReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.db.QueryRowContext(ctx, query, args...)
}
