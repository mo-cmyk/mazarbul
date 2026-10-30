package navigate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate/filtersql"
)

// AggregateSpec parameterises AggregateEntities: an optional kind / AIP-160
// filter prefilter, the field to group by (an entity column or a fact slot
// state_key), and a cap on the number of groups returned.
type AggregateSpec struct {
	Kind    string
	Filter  string
	GroupBy string
	Limit   int
}

// GroupCount is one bucket of an aggregation: the group value and its count.
type GroupCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// entityColumn maps a filter/group field name to its qualified entities-table
// column. The ok result is false for any other field, which the caller treats
// as a fact slot (state_key).
//
// "status" is a short alias for "resolutionStatus" so agents and humans can
// write `status = "confirmed"` or `status != "deleted"` without spelling out
// the full camelCase field name. Both spellings resolve to the same SQL column.
func entityColumn(field string) (col string, ok bool) {
	switch field {
	case "kind":
		return "e.kind", true
	case "displayName":
		return "e.display_name", true
	case "confidence":
		return "e.confidence", true
	case "status", "resolutionStatus":
		return "e.resolution_status", true
	case "createTime":
		return "e.create_time", true
	default:
		return "", false
	}
}

// buildEntityWhere assembles the WHERE conditions (over entities alias e) and
// their bound args from both the structured EntityFilter fields and a compiled
// AIP-160 Filter. The two are AND-combined. Used by ListEntities,
// CountEntities, and AggregateEntities so the filter semantics are identical
// across list, count, and group-by.
func buildEntityWhere(opts EntityFilter, scopes kernel.ScopeSet) (conds []string, args []any, err error) {
	// Scope predicate leads every entity read: the bare-entity listing path is
	// bounded to the caller's scope span (the backstop per #206 Decision 2; an
	// empty set fails closed). The AIP-160 filter's fact/relation EXISTS
	// subqueries are scope-bounded independently via the compiler below.
	scope, scopeArgs := scopes.Predicate("e")
	conds = append(conds, scope)
	args = append(args, scopeArgs...)
	if opts.Kind != "" {
		// Stored kinds are always lowercase (e.g. "person", "organization").
		// Accept a caller-supplied kind in any case by lowercasing before matching.
		conds = append(conds, "e.kind = ?")
		args = append(args, strings.ToLower(opts.Kind))
	}
	switch {
	case opts.NameExact != "":
		conds = append(conds, "e.display_name = ? COLLATE NOCASE")
		args = append(args, opts.NameExact)
	case opts.NameLike != "":
		conds = append(conds, "e.display_name LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(opts.NameLike)+"%")
	case opts.AliasExact != "":
		// Alias-exact: the entity must have at least one matching alias in
		// entity_aliases. Case-insensitive via COLLATE NOCASE, matching the
		// display-name lane's comparison convention.
		conds = append(conds, "EXISTS (SELECT 1 FROM entity_aliases a WHERE a.entity_uid = e.uid AND a.alias = ? COLLATE NOCASE)")
		args = append(args, opts.AliasExact)
	}
	if opts.MinConfidence > 0 {
		conds = append(conds, "e.confidence >= ?")
		args = append(args, opts.MinConfidence)
	}
	if opts.Filter != "" {
		cf, cerr := compileFilter(opts.Filter, scopes)
		if cerr != nil {
			return nil, nil, cerr
		}
		if cf.where != "" {
			conds = append(conds, "("+cf.where+")")
			args = append(args, cf.args...)
		}
	}
	return conds, args, nil
}

// CountEntities returns the number of entities matching opts (filter semantics
// identical to ListEntities; Limit/Offset are ignored).
func (n *Navigator) CountEntities(ctx context.Context, opts EntityFilter) (int64, error) {
	conds, args, err := buildEntityWhere(opts, n.scopes)
	if err != nil {
		return 0, err
	}
	q := `SELECT COUNT(*) FROM entities e`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	var count int64
	if serr := n.reader.QueryRowContext(ctx, q, args...).Scan(&count); serr != nil {
		return 0, fmt.Errorf("count entities: %w", serr)
	}
	return count, nil
}

// AggregateEntities groups the entities matching spec's prefilter by spec.GroupBy
// and counts each bucket, most-populous first. GroupBy may be an entity column
// (kind, resolutionStatus, …) or a fact slot state_key (status, …); in the
// latter case entities lacking an open fact for that slot bucket under the empty
// value. Limit caps the number of buckets (0 → default cap).
func (n *Navigator) AggregateEntities(ctx context.Context, spec AggregateSpec) ([]GroupCount, error) {
	if spec.GroupBy == "" {
		return nil, errors.New("aggregate: groupBy is required")
	}
	conds, whereArgs, err := buildEntityWhere(EntityFilter{Kind: spec.Kind, Filter: spec.Filter}, n.scopes)
	if err != nil {
		return nil, err
	}

	var groupExpr, join string
	var leadingArgs []any
	if col, ok := entityColumn(spec.GroupBy); ok {
		groupExpr = "COALESCE(" + col + ", '')"
	} else {
		// Fact slot: join the entity's open in-scope fact for this state_key. The
		// join's bound state_key + scope args precede the WHERE args, so they lead.
		fScope, fScopeArgs := n.scopes.Predicate("f")
		join = " LEFT JOIN facts f ON f.subject_uid = e.uid AND f.state_key = ? AND f.event_end IS NULL AND " + fScope
		leadingArgs = append(leadingArgs, spec.GroupBy)
		leadingArgs = append(leadingArgs, fScopeArgs...)
		groupExpr = "COALESCE(f.value, '')"
	}

	limit := spec.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	//nolint:gosec // G202: groupExpr/join are compile-time fragments; all values are bound ? params
	q := "SELECT " + groupExpr + " AS gv, COUNT(*) AS cnt FROM entities e" + join
	args := append(leadingArgs, whereArgs...)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " GROUP BY gv ORDER BY cnt DESC, gv ASC LIMIT ?"
	args = append(args, limit)

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate entities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]GroupCount, 0, limit)
	for rows.Next() {
		var g GroupCount
		if serr := rows.Scan(&g.Value, &g.Count); serr != nil {
			return nil, fmt.Errorf("scan group: %w", serr)
		}
		out = append(out, g)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate groups: %w", rerr)
	}
	return out, nil
}

// --- AIP-160 filter → SQL compiler ---

// compiledFilter is a SQL predicate (without the leading WHERE) over the
// entities table aliased e, plus its ordered bound arguments.
type compiledFilter struct {
	where string
	args  []any
}

// ErrInvalidFilter wraps every parse/compile failure of an AIP-160 Filter so
// callers (MCP/CLI) can map a bad filter to a validation error rather than an
// internal one — the filter is user input.
var ErrInvalidFilter = errors.New("invalid filter")

// compileFilter parses an AIP-160-subset filter and emits a SQL predicate over
// entities (alias e), with fact slots and related() lowering to EXISTS
// subqueries. An empty filter yields an empty compiledFilter.
func compileFilter(filter string, scopes kernel.ScopeSet) (compiledFilter, error) {
	expr, err := filtersql.Parse(filter)
	if err != nil {
		return compiledFilter{}, fmt.Errorf("%w: %w", ErrInvalidFilter, err)
	}
	if expr == nil {
		return compiledFilter{}, nil
	}
	c := &filterCompiler{now: time.Now().UTC().Format(time.RFC3339Nano), scopes: scopes}
	where, err := c.expr(expr)
	if err != nil {
		return compiledFilter{}, fmt.Errorf("%w: %w", ErrInvalidFilter, err)
	}
	return compiledFilter{where: where, args: c.args}, nil
}

type filterCompiler struct {
	args   []any
	now    string          // bound for relation validity comparisons
	scopes kernel.ScopeSet // bounds the fact/relation EXISTS subqueries to the read's scope span
}

func (c *filterCompiler) expr(e filtersql.Expr) (string, error) {
	switch n := e.(type) {
	case filtersql.And:
		return c.join(n.Operands, " AND ")
	case filtersql.Or:
		return c.join(n.Operands, " OR ")
	case filtersql.Not:
		inner, err := c.expr(n.Operand)
		if err != nil {
			return "", err
		}
		return "NOT (" + inner + ")", nil
	case filtersql.Comparison:
		return c.comparison(n)
	case filtersql.Call:
		return c.call(n)
	default:
		return "", fmt.Errorf("filter: unsupported expression %T", e)
	}
}

func (c *filterCompiler) join(ops []filtersql.Expr, sep string) (string, error) {
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		s, err := c.expr(op)
		if err != nil {
			return "", err
		}
		parts = append(parts, "("+s+")")
	}
	return strings.Join(parts, sep), nil
}

func (c *filterCompiler) comparison(n filtersql.Comparison) (string, error) {
	if col, ok := entityColumn(n.Field); ok {
		return c.columnComparison(col, n)
	}
	return c.factComparison(n)
}

// columnComparison emits a direct comparison against an entities column. The
// `:` (has) operator becomes a case-insensitive substring LIKE; the rest map to
// their SQL spelling.
//
// When the column is resolution_status (from the status / resolutionStatus
// field aliases), string literals are validated against the closed enum set:
// resolved, confirmed, tentative, split_candidate, deleted. An unknown value
// would silently return [] or the full set (depending on the operator), so we
// reject it with ErrInvalidFilter and name the legal values instead.
func (c *filterCompiler) columnComparison(col string, n filtersql.Comparison) (string, error) {
	// Validate status-enum literals before emitting SQL so agents get an
	// actionable error instead of a silently empty or full result.
	if col == "e.resolution_status" && n.Value.Kind == filtersql.ValueString {
		if !isValidResolutionStatus(n.Value.Str) {
			return "", fmt.Errorf("%w: %q is not a valid resolutionStatus value; "+
				"valid values are: resolved, confirmed, tentative, split_candidate, deleted",
				ErrInvalidFilter, n.Value.Str)
		}
	}
	if n.Op == ":" {
		c.args = append(c.args, "%"+escapeLike(valueString(n.Value))+"%")
		return col + " LIKE ? ESCAPE '\\'", nil
	}
	sqlOp, err := sqlComparator(n.Op)
	if err != nil {
		return "", err
	}
	c.args = append(c.args, valueArg(n.Value))
	return fmt.Sprintf("%s %s ?", col, sqlOp), nil
}

// factComparison lowers a comparison on an unknown field to an EXISTS over the
// entity's open fact for that state_key. `!=` becomes NOT EXISTS of the
// equality (no open fact holds the value); `:` is a substring match on the
// value.
func (c *filterCompiler) factComparison(n filtersql.Comparison) (string, error) {
	c.args = append(c.args, n.Field) // state_key (first ? in the subquery)
	// The fact EXISTS is bounded to the read's scope span: an entity surfaced in
	// this scope must not be filtered by an out-of-scope fact value (#206).
	scopeClause, scopeArgs := c.scopes.Predicate("f")
	var valClause string
	switch n.Op {
	case "=":
		valClause = "f.value = ?"
		c.args = append(c.args, valueArg(n.Value))
	case "!=":
		valClause = "f.value = ?"
		c.args = append(c.args, valueArg(n.Value))
		c.args = append(c.args, scopeArgs...)
		return "NOT " + factExists(valClause, scopeClause), nil
	case ":":
		valClause = "f.value LIKE ? ESCAPE '\\'"
		c.args = append(c.args, "%"+escapeLike(valueString(n.Value))+"%")
	case "<", ">", "<=", ">=":
		// Fact values are stored as TEXT, so a range operator would compare
		// lexically (e.g. "10" < "9") rather than numerically — silently wrong.
		// Reject it with an actionable error; typed numeric attributes are a
		// later phase that will enable real range comparisons on fact slots.
		return "", fmt.Errorf("range comparison %q is not supported on attribute %q: "+
			"fact values are stored as text, so only =, != and : apply "+
			"(typed numeric attributes will enable ranges later)", n.Op, n.Field)
	default:
		return "", fmt.Errorf("filter: unsupported operator %q", n.Op)
	}
	c.args = append(c.args, scopeArgs...)
	return factExists(valClause, scopeClause), nil
}

// factExists wraps a value clause in the open-fact EXISTS subquery, bounded to
// the read's scope span. The caller has already bound the state_key arg, then
// any value arg in valClause order, then the scopeClause args.
func factExists(valClause, scopeClause string) string {
	return "EXISTS (SELECT 1 FROM facts f WHERE f.subject_uid = e.uid AND f.state_key = ? AND " +
		valClause + " AND f.event_end IS NULL AND " + scopeClause + ")"
}

func (c *filterCompiler) call(n filtersql.Call) (string, error) {
	switch n.Name {
	case "related":
		if len(n.Args) != 2 {
			return "", fmt.Errorf("filter: related(predicate, target) takes 2 arguments, got %d", len(n.Args))
		}
		pred := valueString(n.Args[0])
		target := valueString(n.Args[1])
		// Bound to the read's scope span: a related() match must traverse only
		// in-scope edges (#206). scopeClause args follow the validity args.
		scopeClause, scopeArgs := c.scopes.Predicate("r")
		// The object entity is matched by name (o.display_name) as well as by
		// UID. Matching by name reads the entity's own name, so scope o WITHIN
		// the name branch: a team-scoped "Mithril" must not satisfy a default-scope
		// related("works_at", "Mithril"). The UID branch rides the already-scoped
		// edge r, so it is left unrestricted (the caller named an exact object).
		oScopeClause, oScopeArgs := c.scopes.Predicate("o")
		c.args = append(c.args, pred, target, target)
		c.args = append(c.args, oScopeArgs...)
		c.args = append(c.args, c.now, c.now)
		c.args = append(c.args, scopeArgs...)
		return `EXISTS (SELECT 1 FROM relations r LEFT JOIN entities o ON o.uid = r.object_uid
			WHERE r.subject_uid = e.uid AND r.predicate = ?
			  AND (r.object_uid = ? OR (o.display_name = ? COLLATE NOCASE AND ` + oScopeClause + `))
			  AND (r.valid_from IS NULL OR r.valid_from = '' OR julianday(r.valid_from) <= julianday(?))
			  AND (r.valid_until IS NULL OR r.valid_until = '' OR julianday(?) < julianday(r.valid_until))
			  AND (r.superseded_by_uid IS NULL OR r.superseded_by_uid = '')
			  AND ` + scopeClause + `)`, nil
	default:
		return "", fmt.Errorf("filter: unknown function %q", n.Name)
	}
}

// sqlComparator maps a parsed operator to its SQL spelling for a direct column
// comparison. The `:` operator is handled by the caller (substring LIKE).
func sqlComparator(op string) (string, error) {
	switch op {
	case "=", "<", ">", "<=", ">=":
		return op, nil
	case "!=":
		return "!=", nil
	default:
		return "", fmt.Errorf("filter: unsupported operator %q", op)
	}
}

// valueArg returns the bound argument for a value: the float for a number, the
// string otherwise.
func valueArg(v filtersql.Value) any {
	if v.Kind == filtersql.ValueNumber {
		return v.Num
	}
	return v.Str
}

// valueString renders a value as a string (numbers via %v) for LIKE patterns
// and relation targets.
func valueString(v filtersql.Value) string {
	if v.Kind == filtersql.ValueNumber {
		return fmt.Sprintf("%v", v.Num)
	}
	return v.Str
}

// isValidResolutionStatus reports whether s is one of the legal
// resolution_status values stored in the entities table:
// resolved, confirmed, tentative, split_candidate, deleted.
// The check is case-sensitive (values are always stored lowercase).
func isValidResolutionStatus(s string) bool {
	switch s {
	case "resolved", "confirmed", "tentative", "split_candidate", "deleted":
		return true
	default:
		return false
	}
}
