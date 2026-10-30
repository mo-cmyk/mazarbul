package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// Query escape-hatch limits. DefaultLimit/maxLimit bound the rows materialized
// (a fetch cap, not a SQL rewrite, so the clamp composes with any LIMIT the
// caller already wrote). QueryTimeout bounds wall-clock per query; SQLite
// honors the context deadline via interrupt.
const (
	queryDefaultLimit = 200
	queryMaxLimit     = 1000
	queryTimeout      = 5 * time.Second
)

// bannedSQLKeywords are the mutating / administrative keywords rejected by the
// statement validator. The read-only connection (mode=ro + PRAGMA query_only)
// already makes writes physically impossible; this list turns them into a clean
// E_VALIDATION up front and closes the `WITH … DELETE` form (a statement that
// starts with the allowed WITH but mutates).
//
//nolint:gochecknoglobals // immutable lookup table, package-private
var bannedSQLKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true,
	"ATTACH": true, "DETACH": true, "PRAGMA": true, "VACUUM": true,
	"REINDEX": true, "ANALYZE": true, "TRIGGER": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "SAVEPOINT": true, "RELEASE": true,
}

// allowedQuerySources are the curated views a query may read from. Every
// FROM/JOIN source must resolve to one of these (or a CTE the query itself
// defines, or a derived-table subquery). They are the documented stable
// contract; physical/internal tables (entities, relations, facts, episodes, …)
// stay off-limits even though the connection is read-only.
//
//nolint:gochecknoglobals // immutable lookup table, package-private
var allowedQuerySources = map[string]bool{
	"V_ENTITIES":  true,
	"V_RELATIONS": true,
	"V_FACTS":     true,
}

const queryInputSchema = `{
  "type": "object",
  "properties": {
    "sql":    {"type": "string", "description": "A single read-only SELECT (or WITH … SELECT) statement. Query the curated views: v_entities, v_relations, v_facts (run introspect for their columns). Writes, multiple statements, PRAGMA/ATTACH and DDL are rejected; the connection is read-only regardless."},
    "limit":  {"type": "integer", "minimum": 1, "maximum": 1000, "description": "Maximum rows to return. Defaults to 200, hard-capped at 1000. Rows beyond the cap set truncated=true."},
    "scopes": {"type": "array", "items": {"type": "string"}, "description": "Scope filter: which data partitions to include. Allowed values: project, personal, team. Defaults to [project, personal] when omitted — team rows are hidden unless you pass scopes:[\"team\"]. The curated views are pre-filtered to the requested scopes before your SQL runs, so no WHERE scope = … clause is needed."}
  },
  "required": ["sql"],
  "additionalProperties": false
}`

const queryOutputSchema = `{
  "type": "object",
  "properties": {
    "columns":   {"type": "array", "items": {"type": "string"}, "description": "Result column names in order."},
    "rows":      {"type": "array", "items": {"type": "object", "additionalProperties": true}, "description": "Result rows as column→value objects."},
    "rowCount":  {"type": "integer", "minimum": 0, "description": "Number of rows returned (after the limit cap)."},
    "truncated": {"type": "boolean", "description": "True when more rows matched than the limit returned."}
  },
  "required": ["columns", "rows", "rowCount", "truncated"],
  "additionalProperties": false
}`

// Query is the read-only SQL escape hatch: the long-tail analytical surface for
// questions the structured filter/count tools cannot express. It runs a single
// SELECT/WITH statement against the curated views (v_entities / v_relations /
// v_facts) on a hard read-only connection, bounding rows and wall-clock. It is
// MCP-listed: agents have full read autonomy, never write power.
type Query struct {
	reader *sql.DB
}

// NewQuery constructs the query tool over the read-only reader pool.
func NewQuery(reader *sql.DB) *Query { return &Query{reader: reader} }

// Name implements mcp.Tool.
func (*Query) Name() string { return "query" }

// Description implements mcp.Tool.
func (*Query) Description() string {
	return "Run a read-only SQL SELECT over the curated views (v_entities, v_relations, v_facts) — the analytical escape hatch for questions the structured entities/count filters can't express. Single statement, SELECT/WITH only; writes are rejected and impossible. Run introspect for the view columns. Scope: results default to project+personal; pass scopes:[\"team\"] to include team rows. The views are pre-filtered to the requested scopes before your SQL runs — no WHERE scope = … clause needed."
}

// InputSchema implements mcp.Tool.
func (*Query) InputSchema() json.RawMessage { return json.RawMessage(queryInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Query) OutputSchema() json.RawMessage { return json.RawMessage(queryOutputSchema) }

type queryInput struct {
	SQL    string   `json:"sql"`
	Limit  *int     `json:"limit,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

type queryOutput struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	RowCount  int              `json:"rowCount"`
	Truncated bool             `json:"truncated"`
}

// Invoke implements mcp.Tool.
func (q *Query) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in queryInput
	if len(req) > 0 {
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
		}
	}
	if err := validateReadOnlySQL(in.SQL); err != nil {
		return nil, err
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}

	out, err := q.run(ctx, in.SQL, queryRowLimit(in.Limit), scopeSet)
	if err != nil {
		return nil, err
	}
	body, mErr := json.Marshal(out)
	if mErr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", mErr))
	}
	return body, nil
}

// queryRowLimit clamps the caller's limit to [1, queryMaxLimit], defaulting an
// absent or non-positive value to queryDefaultLimit.
func queryRowLimit(limit *int) int {
	if limit == nil || *limit < 1 {
		return queryDefaultLimit
	}
	if *limit > queryMaxLimit {
		return queryMaxLimit
	}
	return *limit
}

// run executes sqlText on a dedicated read-only connection, materializing up to
// limit rows. It pulls one connection so PRAGMA query_only binds to the same
// connection the query runs on, and bounds wall-clock with a context deadline.
//
// Scope enforcement uses temporary SQLite views (temp schema shadows main) that
// are created before PRAGMA query_only = ON. The temp views filter each curated
// view (v_entities / v_relations / v_facts) to scopes, so the user's SQL sees
// only the rows in the requested scope set without any extra WHERE clause.
func (q *Query) run(ctx context.Context, sqlText string, limit int, scopes kernel.ScopeSet) (queryOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	conn, err := q.reader.Conn(ctx)
	if err != nil {
		// A contended on-disk lock surfaces here as a transient open/busy error;
		// route it to a retryable E_UNAVAILABLE rather than E_INTERNAL (issue #5).
		return queryOutput{}, readErr("acquire read connection", err)
	}
	defer func() { _ = conn.Close() }()

	// Pool-reused connections may carry query_only=ON from a previous run (the
	// PRAGMA persists on the physical SQLite connection even after sql.Conn.Close
	// returns it to the pool). Resetting to OFF here is safe: PRAGMA query_only
	// changes in-memory connection state, not the database file, so it succeeds
	// even when mode=ro is in effect. SQLite explicitly permits this — see the
	// query_only pragma documentation: "You can still use PRAGMA query_only = 0
	// to disable this PRAGMA." We reset first so createScopeViews can write to
	// the temp schema without hitting SQLITE_READONLY.
	if _, err = conn.ExecContext(ctx, "PRAGMA query_only = OFF"); err != nil {
		return queryOutput{}, mcp.NewInternalError(fmt.Errorf("reset query_only: %w", err))
	}

	// Create scope-filtered temp views BEFORE setting query_only: the temp
	// schema is writable even on a mode=ro connection (temp lives in memory,
	// not the main database file). Temp views shadow the main-schema views so
	// the user's SQL automatically sees only the scope-requested rows.
	if err = createScopeViews(ctx, conn, scopes); err != nil {
		return queryOutput{}, mcp.NewInternalError(fmt.Errorf("create scope views: %w", err))
	}

	// Defense in depth on top of the mode=ro reader pool: query_only binds to
	// this connection, so even a write that slipped past the validator fails.
	if _, err = conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return queryOutput{}, mcp.NewInternalError(fmt.Errorf("set query_only: %w", err))
	}

	rows, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return queryOutput{}, queryExecError(err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return queryOutput{}, mcp.NewInternalError(fmt.Errorf("read columns: %w", err))
	}
	if dup := firstDuplicate(cols); dup != "" {
		return queryOutput{}, mcp.NewValidationError(
			fmt.Sprintf("duplicate result column %q; alias columns with AS to disambiguate", dup), nil)
	}

	out := queryOutput{Columns: cols, Rows: make([]map[string]any, 0, limit)}
	for rows.Next() {
		if len(out.Rows) == limit {
			out.Truncated = true
			break
		}
		row, serr := scanQueryRow(rows, cols)
		if serr != nil {
			return queryOutput{}, mcp.NewInternalError(fmt.Errorf("scan row: %w", serr))
		}
		out.Rows = append(out.Rows, row)
	}
	if rerr := rows.Err(); rerr != nil {
		return queryOutput{}, queryExecError(rerr)
	}
	out.RowCount = len(out.Rows)
	return out, nil
}

// scanQueryRow scans the current row into a column→value map, normalizing the
// driver's []byte (TEXT/BLOB) to string so the JSON response is readable rather
// than base64.
func scanQueryRow(rows *sql.Rows, cols []string) (map[string]any, error) {
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("scan values: %w", err)
	}
	row := make(map[string]any, len(cols))
	for i, c := range cols {
		if b, ok := vals[i].([]byte); ok {
			row[c] = string(b)
			continue
		}
		row[c] = vals[i]
	}
	return row, nil
}

// queryExecError maps a query execution failure to a ToolError. A timeout and a
// SQL error are both the caller's concern (narrow the query / fix the SQL), so
// both surface as E_VALIDATION carrying an actionable message rather than an
// opaque internal error.
func queryExecError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return mcp.NewValidationError(
			fmt.Sprintf("query exceeded the %s time limit; add filters or a tighter LIMIT", queryTimeout), nil)
	}
	return mcp.NewValidationError(fmt.Sprintf("query failed: %v", err), nil)
}

// firstDuplicate returns the first column name that appears more than once, or
// "" when all are unique. A column→value map cannot represent collisions, so the
// tool rejects them with a pointed message instead of silently dropping a column.
func firstDuplicate(cols []string) string {
	seen := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		if _, dup := seen[c]; dup {
			return c
		}
		seen[c] = struct{}{}
	}
	return ""
}

// validateReadOnlySQL enforces the read-only contract before execution: a
// single statement, starting with SELECT or WITH, with no mutating/admin
// keyword anywhere. It scans the SQL once (skipping string/identifier literals
// and comments) so a banned word inside a literal — WHERE name = 'delete' — is
// not mistaken for the keyword.
func validateReadOnlySQL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return mcp.NewValidationError("sql is required", nil)
	}
	toks, multiStatement := scanSQLTokens(raw)
	if multiStatement {
		return mcp.NewValidationError("only a single statement is allowed; remove the ';'-chained statement", nil)
	}
	words := bareWords(toks)
	if len(words) == 0 {
		return mcp.NewValidationError("no SQL keyword found; expected a SELECT or WITH query", nil)
	}
	switch words[0] {
	case "SELECT", "WITH":
	default:
		return mcp.NewValidationError(
			fmt.Sprintf("only read-only SELECT / WITH queries are allowed; got %q", strings.ToLower(words[0])), nil)
	}
	for _, w := range words {
		if bannedSQLKeywords[w] {
			return mcp.NewValidationError(
				fmt.Sprintf("the %s keyword is not allowed in a read-only query", w), nil)
		}
	}
	return validateQuerySources(toks)
}

// bareWords pulls the uppercased identifier/keyword tokens (dropping the '('
// and ')' structure markers) so the read-only keyword checks read a flat word
// stream, exactly as before the scanner grew structure.
func bareWords(toks []sqlToken) []string {
	words := make([]string, 0, len(toks))
	for _, t := range toks {
		if t.word != "" {
			words = append(words, t.word)
		}
	}
	return words
}

// clauseEndKeywords terminate a FROM clause's source scan at the clause depth: a
// comma after one of these belongs to a different clause (ORDER BY a, b), and no
// further table source follows in this FROM clause.
//
//nolint:gochecknoglobals // immutable lookup table, package-private
var clauseEndKeywords = map[string]bool{
	"WHERE": true, "GROUP": true, "ORDER": true, "HAVING": true,
	"LIMIT": true, "WINDOW": true,
	"UNION": true, "INTERSECT": true, "EXCEPT": true,
}

// joinKeywords introduce another table source within the same FROM clause. After
// any of them (JOIN and its INNER/LEFT/RIGHT/FULL/CROSS/NATURAL qualifiers) the
// next factor is a source to validate, so the scan keeps going past a JOIN rather
// than treating it as the end of the clause.
//
//nolint:gochecknoglobals // immutable lookup table, package-private
var joinKeywords = map[string]bool{
	"JOIN": true, "INNER": true, "LEFT": true, "RIGHT": true,
	"FULL": true, "CROSS": true, "NATURAL": true,
}

// validateQuerySources enforces the curated-view boundary: every table source in
// a FROM clause — the leading factor, each comma-separated factor, and each
// JOINed factor — must be an allowed view, a CTE in scope, or a derived-table
// subquery. It works off the same single-pass token stream as the read-only
// checks — no full SQL parser. A banned table name that surfaces only inside a
// string literal never reaches here (the scanner emits it as an inert literal),
// so it stays inert.
func validateQuerySources(toks []sqlToken) error {
	scopes := collectCTEScopes(toks)
	for i, t := range toks {
		if t.word != "FROM" {
			continue
		}
		if err := checkFromClause(toks, i, t.depth, scopes); err != nil {
			return err
		}
	}
	return nil
}

// checkFromClause validates every table source of the FROM clause opened at index
// i, at paren depth clauseDepth: the leading factor, each comma-separated factor,
// and each JOINed factor, until a clause-end keyword, a drop below clauseDepth, or
// end of input. ON/USING join conditions are skipped (their commas and names are
// not sources). Tokens deeper than clauseDepth belong to a subquery / function /
// condition and are not scanned here (a nested FROM is reached by the outer loop;
// a parenthesized factor recurses via checkParenSource).
func checkFromClause(toks []sqlToken, i, clauseDepth int, scopes []cteScope) error {
	st := fromScanState{expectSource: true}
	for j := i + 1; j < len(toks); j++ {
		tok := toks[j]
		if tok.depth < clauseDepth {
			return nil // the enclosing group closed
		}
		if tok.depth > clauseDepth {
			continue // inside a subquery / function arg / condition
		}
		done, err := st.step(toks, j, scopes)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return nil
}

// fromScanState tracks a FROM-clause scan at the clause depth: expectSource is
// true at a position where a table factor may begin (clause start, after a comma,
// after a JOIN keyword); inCondition is true while skipping an ON/USING join
// condition, where commas and names are not sources.
type fromScanState struct {
	expectSource bool
	inCondition  bool
}

// step consumes one clause-depth token, validating it when it is a source and
// updating the scan state. It returns done=true at a clause-end keyword. ON/USING
// conditions are routed to stepCondition; everything else to stepSource.
func (st *fromScanState) step(toks []sqlToken, j int, scopes []cteScope) (done bool, err error) {
	tok := toks[j]
	switch {
	case tok.word == "ON" || tok.word == "USING":
		st.inCondition, st.expectSource = true, false
		return false, nil
	case st.inCondition:
		return st.stepCondition(tok), nil
	default:
		return st.stepSource(toks, j, scopes)
	}
}

// stepCondition advances through an ON/USING join condition. The condition runs
// until the next comma or JOIN at this depth (which reopens the source list); a
// clause-end keyword still ends the whole clause.
func (st *fromScanState) stepCondition(tok sqlToken) (done bool) {
	switch {
	case tok.isComma || joinKeywords[tok.word]:
		st.inCondition, st.expectSource = false, true
	case clauseEndKeywords[tok.word]:
		return true
	}
	return false
}

// stepSource handles a token outside any join condition: comma / JOIN keyword
// reopen the source list, a clause-end keyword stops the scan, and a token in a
// source position (bareword, string literal, or parenthesized factor) is
// validated against the allowlist.
func (st *fromScanState) stepSource(toks []sqlToken, j int, scopes []cteScope) (done bool, err error) {
	tok := toks[j]
	switch {
	case tok.isComma, joinKeywords[tok.word]:
		st.expectSource = true
	case clauseEndKeywords[tok.word]:
		return true, nil
	case tok.isOpenParen:
		if st.expectSource {
			err = checkParenSource(toks, j, scopes)
		}
		st.expectSource = false
	case tok.strLit:
		if st.expectSource {
			err = rejectIfDisallowed(tok.litText, j, scopes)
			st.expectSource = false
		}
	case tok.word == "":
		// close-paren marker at this depth; ignore
	case st.expectSource:
		err = rejectIfDisallowed(tok.word, j, scopes)
		st.expectSource = false
	}
	return false, err
}

// rejectIfDisallowed returns an E_VALIDATION error when the source name (already
// uppercased) at token index srcIdx is neither a curated view nor a CTE in scope
// at that position. An empty name (e.g. FROM ”) cannot be a real table and is
// left to the exec parse.
func rejectIfDisallowed(name string, srcIdx int, scopes []cteScope) error {
	if name == "" || sourceAllowed(name, srcIdx, scopes) {
		return nil
	}
	return mcp.NewValidationError(
		fmt.Sprintf("table %q is not queryable; read from the curated views (v_entities, v_relations, v_facts) or a CTE you define",
			strings.ToLower(name)), nil)
}

// checkParenSource classifies the '(' at openIdx that appears where a FROM
// source is expected. SQLite reads a parenthesized non-SELECT as a table factor or
// a join group, not just a single name: FROM (episodes), FROM ('episodes'),
// FROM ((episodes)), FROM (v_entities, episodes), and FROM (a JOIN b) all reach
// their tables. Only a leading SELECT/WITH is a derived-table subquery (left for
// the in-stream FROM scan). Otherwise the parenthesized contents are a nested FROM
// clause at the inner depth, so every comma-separated and JOINed factor in the
// group is validated by recursing through checkFromClause (a nested '(' recurses
// back here).
func checkParenSource(toks []sqlToken, openIdx int, scopes []cteScope) error {
	inner := nextSignificant(toks, openIdx)
	if inner == nil {
		return nil // empty parens — the exec parse reports the syntax error
	}
	if inner.word == "SELECT" || inner.word == "WITH" {
		return nil // derived-table subquery; its inner FROM is checked in-stream
	}
	return checkFromClause(toks, openIdx, toks[openIdx].depth+1, scopes)
}

// collectCTENames returns the set of CTE names the query defines at any nesting
// level. A token T is a CTE name when the next significant token is AS and the
// token after AS is '('. That `<name> AS (` shape is an unambiguous CTE-definition
// signal at every depth: a column alias is `expr AS alias` (AS then an
// identifier, not '('), and CAST(x AS type) / window OVER (…) / FILTER (…) never
// place '(' immediately after AS. Collecting at all depths means a CTE declared
// inside a derived-table subquery — FROM (WITH c AS (…) SELECT … FROM c) — is
// still recognized as an allowed source.
func collectCTEScopes(toks []sqlToken) []cteScope {
	scopes := make([]cteScope, 0, 4) // most queries define few or no CTEs
	for i, t := range toks {
		if t.word == "" {
			continue
		}
		asTok := nextSignificant(toks, i)
		if asTok == nil || asTok.word != "AS" {
			continue
		}
		openTok := nextSignificant(toks, asTok.index)
		if openTok == nil || !openTok.isOpenParen {
			continue
		}
		scopes = append(scopes, cteScope{name: t.word, start: i, end: blockEnd(toks, i, t.depth)})
	}
	return scopes
}

// cteScope is a CTE definition and the half-open token range [start, end) over
// which its name is a visible source. The name is in scope from its own token to
// the end of the query block that introduced it; a CTE declared inside a
// derived-table subquery cannot authorize a same-named physical table outside it.
type cteScope struct {
	name       string
	start, end int
}

// blockEnd returns the index that closes the query block a CTE at index ci (paren
// depth d) belongs to: the first later token whose depth drops below d (the
// enclosing paren group closing), or len(toks) for a top-level CTE. The CTE's name
// is visible to siblings, the block's main SELECT, and nested subqueries — all of
// which sit at depth >= d before that close — but not to anything outside it.
func blockEnd(toks []sqlToken, ci, d int) int {
	for j := ci + 1; j < len(toks); j++ {
		if toks[j].depth < d {
			return j
		}
	}
	return len(toks)
}

// sourceAllowed reports whether the uppercased source name at token index srcIdx
// is permitted: a curated view, or a CTE whose scope range covers srcIdx.
func sourceAllowed(name string, srcIdx int, scopes []cteScope) bool {
	if allowedQuerySources[name] {
		return true
	}
	for _, s := range scopes {
		if s.name == name && srcIdx > s.start && srcIdx < s.end {
			return true
		}
	}
	return false
}

// nextSignificant returns the first token strictly after index i that carries a
// word, is an open paren, or is a string literal — the markers the source/CTE
// checks key off — or nil if none remain. Comma and close-paren markers are
// skipped so they never stand in for a source name.
func nextSignificant(toks []sqlToken, i int) *sqlToken {
	for j := i + 1; j < len(toks); j++ {
		if toks[j].word != "" || toks[j].isOpenParen || toks[j].strLit {
			return &toks[j]
		}
	}
	return nil
}

// sqlToken is one significant element of a scanned statement: a bareword (word
// set, uppercased), a paren marker (isOpenParen set for '('), a comma marker
// (isComma set), or a single-quoted string literal (strLit set, litText holding
// the inner text). The depth field is the paren nesting at the token — a top-level
// FROM and a CTE name both sit at depth 0, while a derived-table or CAST argument
// sits deeper. Commas carry depth too, so the source check can tell a FROM-list
// separator (same depth as its FROM) from a function-argument comma (deeper).
// The index field is the token's own position, so a follow-on lookup can resume
// from it.
//
// A string literal keeps word empty so it never reaches the keyword / banned-word
// scan (WHERE name = 'delete' stays inert), but SQLite accepts a single-quoted
// string in a table-identifier position, so the source check inspects litText
// when a literal lands where a source is expected.
type sqlToken struct {
	word        string
	litText     string
	isOpenParen bool
	isComma     bool
	strLit      bool
	depth       int
	index       int
}

// scanSQLTokens walks raw SQL once and returns its significant tokens —
// uppercased barewords plus '(' / ')' markers, each tagged with paren depth and
// position — along with whether a statement separator ';' occurs before the
// final non-space byte (i.e. the input is more than one statement). String
// literals ('…', "…", `…`), bracket identifiers ([…]), and comments (-- …,
// /* … */) are skipped so their contents never reach the keyword/source checks.
func scanSQLTokens(raw string) (toks []sqlToken, multiStatement bool) {
	lastNonSpace := strings.LastIndexFunc(raw, func(r rune) bool { return !unicode.IsSpace(r) })
	depth := 0
	var word []byte
	flush := func() {
		if len(word) > 0 {
			toks = append(toks, sqlToken{word: strings.ToUpper(string(word)), depth: depth, index: len(toks)})
			word = word[:0]
		}
	}
	for i := 0; i < len(raw); {
		if next, skipped := skipNonTokenSpan(raw, i); skipped {
			flush()
			i = next
			continue
		}
		if inner, next, ok := quotedIdentSpan(raw, i); ok {
			flush()
			// A quoted identifier is a single, complete name token (it never
			// contributes to an adjacent bareword), so emit it directly.
			toks = append(toks, sqlToken{word: strings.ToUpper(inner), depth: depth, index: len(toks)})
			i = next
			continue
		}
		if inner, next, ok := stringLitSpan(raw, i); ok {
			flush()
			// A string literal stays out of the keyword scan (no word), but carries
			// its text so the source check can reject FROM 'episodes'.
			toks = append(toks, sqlToken{strLit: true, litText: strings.ToUpper(inner), depth: depth, index: len(toks)})
			i = next
			continue
		}
		c := raw[i]
		switch {
		case c == '(':
			flush()
			toks = append(toks, sqlToken{isOpenParen: true, depth: depth, index: len(toks)})
			depth++
			i++
		case c == ')':
			flush()
			if depth > 0 {
				depth--
			}
			toks = append(toks, sqlToken{depth: depth, index: len(toks)})
			i++
		case c == ',':
			flush()
			toks = append(toks, sqlToken{isComma: true, depth: depth, index: len(toks)})
			i++
		case c == ';':
			flush()
			if i < lastNonSpace {
				multiStatement = true
			}
			i++
		case isIdentByte(c):
			word = append(word, c)
			i++
		default:
			flush()
			i++
		}
	}
	flush()
	return toks, multiStatement
}

// skipNonTokenSpan recognizes the comment spans whose contents must never reach
// the keyword/source checks — line and block comments — and returns the index
// just past the span plus true. When raw[i] opens neither it returns (i, false)
// so the caller tokenizes the byte normally. String literals ('…') and quoted
// identifiers ("…", `…`, […]) are handled separately (stringLitSpan /
// quotedIdentSpan) because both can name a source in SQLite and must reach the
// allowlist. Centralizing the comment openers keeps scanSQLTokens' switch flat.
func skipNonTokenSpan(raw string, i int) (next int, skipped bool) {
	switch {
	case isLineCommentStart(raw, i):
		return skipLineComment(raw, i), true
	case isBlockCommentStart(raw, i):
		return skipBlockComment(raw, i), true
	default:
		return i, false
	}
}

// stringLitSpan reads a single-quoted string literal opened at index i (closing
// quote may be doubled to escape it) and returns its inner text plus the index
// just past the closing quote. The ok result is false when raw[i] is not a single
// quote. The inner text is surfaced (not skipped) because SQLite accepts a
// single-quoted string in a table-identifier position — FROM 'episodes' reads the
// episodes table — so the source check must see it; in every other position the
// literal stays inert (its token carries no word).
func stringLitSpan(raw string, i int) (inner string, next int, ok bool) {
	if raw[i] != '\'' {
		return "", i, false
	}
	var b []byte
	for j := i + 1; j < len(raw); j++ {
		if raw[j] != '\'' {
			b = append(b, raw[j])
			continue
		}
		if j+1 < len(raw) && raw[j+1] == '\'' {
			b = append(b, '\'') // doubled quote: an escaped literal quote
			j++
			continue
		}
		return string(b), j + 1, true
	}
	return string(b), len(raw), true // unterminated; exec parse reports it
}

// quotedIdentSpan reads a quoted identifier opened at index i — "…" or `…`
// (closing quote may be doubled to escape it) or […] — and returns its inner
// text plus the index just past the closing quote. The ok result is false when
// raw[i] does not open a quoted identifier. The inner text is the source name the
// allowlist must check, so `FROM "episodes"` can no longer slip a physical table
// through.
func quotedIdentSpan(raw string, i int) (inner string, next int, ok bool) {
	switch raw[i] {
	case '"', '`':
		q := raw[i]
		var b []byte
		for j := i + 1; j < len(raw); j++ {
			if raw[j] != q {
				b = append(b, raw[j])
				continue
			}
			if j+1 < len(raw) && raw[j+1] == q {
				b = append(b, q) // doubled close-quote: an escaped literal quote
				j++
				continue
			}
			return string(b), j + 1, true
		}
		return string(b), len(raw), true // unterminated; exec parse reports it
	case '[':
		for j := i + 1; j < len(raw); j++ {
			if raw[j] == ']' {
				return raw[i+1 : j], j + 1, true
			}
		}
		return raw[i+1:], len(raw), true // unterminated
	default:
		return "", i, false
	}
}

// isLineCommentStart / isBlockCommentStart detect the two SQL comment openers at
// index i, with the bounds check kept here so scanSQLTokens' switch stays flat.
func isLineCommentStart(raw string, i int) bool {
	return raw[i] == '-' && i+1 < len(raw) && raw[i+1] == '-'
}

func isBlockCommentStart(raw string, i int) bool {
	return raw[i] == '/' && i+1 < len(raw) && raw[i+1] == '*'
}

// skipLineComment returns the index of the newline (or end) ending the -- comment
// opened at i.
func skipLineComment(raw string, i int) int {
	for i < len(raw) && raw[i] != '\n' {
		i++
	}
	return i
}

// skipBlockComment returns the index just past the closing */ of the block
// comment opened at i (or len(raw) if unterminated).
func skipBlockComment(raw string, i int) int {
	for i += 2; i < len(raw); i++ {
		if raw[i] == '*' && i+1 < len(raw) && raw[i+1] == '/' {
			return i + 2
		}
	}
	return i
}

// isIdentByte reports whether c can be part of a SQL bareword (keyword or
// identifier): ASCII letters, digits, and underscore.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// createScopeViews creates scope-filtered TEMPORARY views that shadow the three
// curated main-schema views (v_entities, v_relations, v_facts). SQLite searches
// the temp schema before the main schema, so any subsequent SELECT from
// v_entities / v_relations / v_facts transparently reads only the rows whose
// scope column is in the requested set — no WHERE clause needed in the user's
// query.
//
// Using main.v_entities in the body (explicit schema qualifier) references the
// base migration view rather than the temp shadow, avoiding SQLite's circular
// reference error that fires when a CTE or view body references a same-named
// view. The temp schema is writable even on a mode=ro reader connection because
// temp objects live in memory (not the main database file).
//
// DROP VIEW IF EXISTS before each CREATE ensures pool-reused connections do not
// carry stale views from a previous request (a different scope set).
//
// Scope string interpolation is safe: kernel.Scope is a validated enum whose
// values are ASCII-safe identifiers (personal, project, team); they never
// originate from user input after passing through kernel.ParseScopeSet.
func createScopeViews(ctx context.Context, conn *sql.Conn, scopes kernel.ScopeSet) error {
	var inList string
	if scopes.IsEmpty() {
		inList = "NULL" // fail-closed: no rows visible when scope set is empty
	} else {
		parts := make([]string, 0, len(scopes.Scopes()))
		for _, s := range scopes.Scopes() {
			parts = append(parts, "'"+string(s)+"'")
		}
		inList = strings.Join(parts, ", ")
	}
	stmts := []string{
		"DROP VIEW IF EXISTS temp.v_entities",
		"CREATE TEMPORARY VIEW v_entities AS SELECT * FROM main.v_entities WHERE scope IN (" + inList + ")",
		"DROP VIEW IF EXISTS temp.v_relations",
		"CREATE TEMPORARY VIEW v_relations AS SELECT * FROM main.v_relations WHERE scope IN (" + inList + ")",
		"DROP VIEW IF EXISTS temp.v_facts",
		"CREATE TEMPORARY VIEW v_facts AS SELECT * FROM main.v_facts WHERE scope IN (" + inList + ")",
	}
	for i, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("scope view stmt %d: %w", i, err)
		}
	}
	return nil
}
