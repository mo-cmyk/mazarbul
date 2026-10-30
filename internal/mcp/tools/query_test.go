package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// --- statement validator (pure, no DB) ---

func TestValidateReadOnlySQL_Accept(t *testing.T) {
	t.Parallel()
	ok := []string{
		`SELECT 1`,
		`select * from v_entities`,
		`SELECT kind, COUNT(*) FROM v_entities GROUP BY kind`,
		`WITH c AS (SELECT uid FROM v_entities) SELECT * FROM c`,
		`SELECT * FROM v_facts WHERE value = 'a;b'`,            // ';' inside a string literal
		`SELECT * FROM v_facts WHERE value = 'drop table x'`,   // banned word inside a literal
		`SELECT * FROM v_entities -- trailing comment`,         // line comment
		`SELECT * FROM v_entities /* block ; DROP */ LIMIT 10`, // comment hides ';' + DROP
		`SELECT 1;`, // single trailing ';'
		"  \n SELECT 1 \n ",
	}
	for _, q := range ok {
		if err := validateReadOnlySQL(q); err != nil {
			t.Errorf("validateReadOnlySQL(%q) = %v, want nil", q, err)
		}
	}
}

func TestValidateReadOnlySQL_Reject(t *testing.T) {
	t.Parallel()
	bad := []string{
		``,
		`   `,
		`UPDATE entities SET kind = 'x'`,
		`DELETE FROM facts`,
		`INSERT INTO facts (uid) VALUES ('x')`,
		`DROP TABLE entities`,
		`CREATE TABLE t (a INT)`,
		`ALTER TABLE entities ADD COLUMN x TEXT`,
		`PRAGMA table_info(entities)`,
		`ATTACH DATABASE 'evil.db' AS evil`,
		`VACUUM`,
		`SELECT 1; DROP TABLE entities`, // chained statement
		`SELECT 1; SELECT 2`,            // chained statement
		`WITH c AS (SELECT 1) DELETE FROM facts WHERE 1 = 1`, // WITH-prefixed write
		`EXPLAIN SELECT * FROM v_entities`,                   // not SELECT/WITH
	}
	for _, q := range bad {
		err := validateReadOnlySQL(q)
		if err == nil {
			t.Errorf("validateReadOnlySQL(%q) = nil, want a validation error", q)
			continue
		}
		var te *mcp.ToolError
		if !errors.As(err, &te) || te.Code != mcp.CodeValidation {
			t.Errorf("validateReadOnlySQL(%q) error = %v, want E_VALIDATION", q, err)
		}
	}
}

// TestValidateReadOnlySQL_SourceAllowlist pins the curated-view boundary: a
// query may read only the views (or CTEs/derived tables built atop them), never
// a physical/internal table — even though the connection is read-only.
func TestValidateReadOnlySQL_SourceAllowlist(t *testing.T) {
	t.Parallel()
	allow := []string{
		`SELECT * FROM v_entities`,
		`SELECT * FROM v_relations`,
		`SELECT * FROM v_facts`,
		// cross-view JOIN
		`SELECT e.uid FROM v_entities e JOIN v_relations r ON r.subject_uid = e.uid`,
		// a top-level comma source after a JOIN's ON / USING condition (still a view)
		`SELECT * FROM v_entities e JOIN v_relations r ON r.subject_uid = e.uid, v_facts`,
		`SELECT * FROM v_entities JOIN v_relations USING (uid), v_facts`,
		// case-insensitive source
		`select * from V_Entities`,
		// CTE the query defines, then read from it
		`WITH t AS (SELECT * FROM v_facts) SELECT * FROM t`,
		// multiple CTEs, one referencing a view, one referencing another CTE
		`WITH a AS (SELECT uid FROM v_entities), b AS (SELECT uid FROM a) SELECT * FROM b`,
		// a CTE defined inside a derived table is in scope for that block's own FROM
		`SELECT * FROM (WITH c AS (SELECT * FROM v_facts) SELECT * FROM c) y`,
		// a CTE named like a physical table is fine WITHIN its own scope
		`WITH episodes AS (SELECT * FROM v_entities) SELECT * FROM episodes`,
		// derived-table subquery
		`SELECT * FROM (SELECT * FROM v_relations) x`,
		// derived table without an alias keyword visible
		`SELECT n FROM (SELECT COUNT(*) AS n FROM v_entities)`,
		// banned table name only inside a string literal stays inert
		`SELECT * FROM v_facts WHERE value = 'episodes'`,
		`SELECT * FROM v_entities WHERE display_name = 'SELECT * FROM entities'`,
		// column alias `expr AS name` must not be misread as a CTE / source
		`SELECT kind AS entities FROM v_entities`,
		`SELECT (1 + 1) AS facts FROM v_relations`,
		// comma-separated (implicit cross join) over allowed views, with aliases
		`SELECT * FROM v_entities, v_relations`,
		`SELECT * FROM v_entities e, v_relations r WHERE e.uid = r.subject_uid`,
		`SELECT * FROM v_entities, v_relations, v_facts`,
		// comma after a derived table built on an allowed view
		`SELECT * FROM (SELECT * FROM v_facts) x, v_entities`,
		// a comma in the SELECT list / ORDER BY is not a source
		`SELECT uid, kind FROM v_entities ORDER BY kind, uid`,
		// a comma inside a function-arg list is not a source
		`SELECT COALESCE(kind, 'x') FROM v_entities`,
		// a quoted IDENTIFIER naming an allowed view is fine (case-folded)
		`SELECT * FROM "v_entities"`,
		"SELECT * FROM `v_relations`",
		`SELECT * FROM [v_facts]`,
		`SELECT * FROM "v_entities", [v_relations]`,
		// a single-quoted string naming an allowed view (SQLite source form) is fine
		`SELECT * FROM 'v_entities'`,
		// parenthesized table factor naming an allowed view (and nested parens)
		`SELECT * FROM (v_entities)`,
		`SELECT * FROM ((v_relations))`,
		`SELECT * FROM ('v_facts')`,
		`SELECT * FROM ("v_entities")`,
		// a real derived-table subquery in parens is still fine
		`SELECT * FROM (SELECT * FROM v_entities) x`,
		`SELECT * FROM (WITH c AS (SELECT * FROM v_facts) SELECT * FROM c) y`,
		// parenthesized table LIST / join group over only allowed views is fine
		`SELECT * FROM (v_entities, v_relations)`,
		`SELECT * FROM (v_entities e JOIN v_relations r ON r.subject_uid = e.uid)`,
		`SELECT * FROM (v_entities, (v_relations))`,
		// a banned word inside a single-quoted string LITERAL stays inert
		`SELECT * FROM v_facts WHERE state_key = 'select from episodes'`,
		`SELECT * FROM v_facts WHERE value = 'episodes' AND state_key = 'relations'`,
		`SELECT * FROM v_entities WHERE display_name = 'entities'`,
	}
	for _, q := range allow {
		if err := validateReadOnlySQL(q); err != nil {
			t.Errorf("validateReadOnlySQL(%q) = %v, want nil", q, err)
		}
	}

	reject := []string{
		`SELECT * FROM episodes`,
		`SELECT * FROM entities`,
		`SELECT * FROM relations`,
		`SELECT * FROM facts`,
		// physical table reached through a JOIN
		`SELECT * FROM v_entities e JOIN episodes ep ON ep.uid = e.uid`,
		// physical table inside an otherwise-allowed CTE body
		`WITH t AS (SELECT * FROM entities) SELECT * FROM t`,
		// physical table inside a derived table
		`SELECT * FROM (SELECT * FROM relations) x`,
		// physical table reached through a comma (implicit cross join) after a view
		`SELECT * FROM v_entities, episodes`,
		`SELECT * FROM v_entities e, episodes ep`,
		// physical table as a later comma source among several allowed views
		`SELECT * FROM v_entities, v_relations, facts`,
		// physical table comma-joined after a derived table on an allowed view
		`SELECT * FROM (SELECT * FROM v_facts) x, episodes`,
		// physical table reached through a quoted identifier (the three SQLite forms)
		`SELECT * FROM "episodes"`,
		"SELECT * FROM `entities`",
		`SELECT * FROM [relations]`,
		// quoted physical table as a comma source after an allowed view
		`SELECT * FROM v_entities, "episodes"`,
		// quoted physical table inside a CTE body and a derived table
		`WITH t AS (SELECT * FROM "episodes") SELECT * FROM t`,
		`SELECT * FROM (SELECT * FROM [facts]) x`,
		// quoted physical table reached through a JOIN
		`SELECT * FROM v_entities e JOIN "episodes" ep ON ep.uid = e.uid`,
		// single-quoted physical table (SQLite accepts a string in source position)
		`SELECT * FROM 'episodes'`,
		`SELECT * FROM v_entities, 'episodes'`,
		`SELECT * FROM v_entities e JOIN 'episodes' ep ON ep.uid = e.uid`,
		`WITH t AS (SELECT * FROM 'facts') SELECT * FROM t`,
		`SELECT * FROM (SELECT * FROM 'relations') x`,
		// parenthesized physical table factor (the bare and quoted forms SQLite reads)
		`SELECT * FROM (episodes)`,
		`SELECT * FROM ('episodes')`,
		"SELECT * FROM (`entities`)",
		`SELECT * FROM ([relations])`,
		// nested parens around a physical table
		`SELECT * FROM ((episodes))`,
		// parenthesized physical table as a comma source / via JOIN / in a CTE body
		`SELECT * FROM v_entities, (episodes)`,
		`SELECT * FROM v_entities e JOIN (episodes) ep ON ep.uid = e.uid`,
		`WITH t AS (SELECT * FROM (facts)) SELECT * FROM t`,
		// parenthesized table LIST / join group hiding a physical table after a view
		`SELECT * FROM (v_entities, episodes)`,
		`SELECT * FROM (v_entities e CROSS JOIN episodes)`,
		`SELECT * FROM (v_entities, 'episodes')`,
		`SELECT * FROM (v_entities, (episodes))`,
		// physical table as the FIRST factor of a parenthesized group
		`SELECT * FROM (episodes, v_entities)`,
		// CTE-name-collision bypass: a CTE named `episodes` inside a derived table
		// must NOT authorize the outer physical `episodes` source.
		`SELECT * FROM (WITH episodes AS (SELECT * FROM v_entities) SELECT 1 AS one) x, episodes`,
		// the inner CTE name is out of scope for a sibling subquery's source too
		`SELECT * FROM (WITH t AS (SELECT 1) SELECT * FROM t) a, (SELECT * FROM t) b`,
		// a physical table comma-joined after a JOIN's ON / USING condition
		`SELECT * FROM v_entities e JOIN v_relations r ON r.subject_uid = e.uid, episodes`,
		`SELECT * FROM v_entities JOIN v_relations USING (uid), episodes`,
		// physical table reached through a JOIN whose source follows a join qualifier
		`SELECT * FROM v_entities LEFT JOIN episodes ON 1 = 1`,
		// schema-qualified sources: the scope filter lives in temp-schema shadow
		// views, so main.v_entities would read the UNFILTERED base view and
		// bypass scope isolation entirely. The tokenizer splits at '.', so the
		// checked source name is the schema word — never in the allowlist.
		`SELECT * FROM main.v_entities`,
		`SELECT * FROM temp.v_entities`,
		`SELECT * FROM "main".v_entities`,
		`SELECT * FROM main."v_entities"`,
		`SELECT * FROM v_entities e JOIN main.v_relations r ON r.subject_uid = e.uid`,
		`WITH t AS (SELECT * FROM main.v_facts) SELECT * FROM t`,
		`SELECT * FROM (main.v_entities)`,
		`SELECT * FROM v_entities, main.v_relations`,
	}
	for _, q := range reject {
		err := validateReadOnlySQL(q)
		if err == nil {
			t.Errorf("validateReadOnlySQL(%q) = nil, want a validation error", q)
			continue
		}
		var te *mcp.ToolError
		if !errors.As(err, &te) || te.Code != mcp.CodeValidation {
			t.Errorf("validateReadOnlySQL(%q) error = %v, want E_VALIDATION", q, err)
		}
	}
}

// --- query execution over a real, fully-migrated store ---

// newQueryStore opens a migrated SQLite store and seeds a small graph for the
// query tool: two people (+ the migration-seeded "self"), one org, an open fact,
// and a relation. Returns the store so tests can also read it directly.
func newQueryStore(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "alice", "person", "Merry", 1.0, "confirmed", now)
	exec(ent, "bob", "person", "Sam", 0.9, "confirmed", now)
	exec(ent, "acme", "organization", "Mithril", 1.0, "confirmed", now)

	exec(`INSERT INTO facts (uid, subject_uid, state_key, value, event_start, event_end, create_time, source_event_uid, content_hash, superseded_by_uid, source_priority) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"f1", "alice", "state.task.status", "pending", nil, nil, now, "f1", "h1", nil, 1.0)
	exec(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"r1", "alice", "works_at", "acme", 1.0, "ep1", now, nil, nil, nil, `{"role":"CTO"}`)
	return store
}

// invokeQuery runs the query tool and decodes the response.
func invokeQuery(t *testing.T, q *Query, args string) queryOutput {
	t.Helper()
	body, err := q.Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Invoke(%s): %v", args, err)
	}
	var out queryOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("unmarshal query output: %v", uerr)
	}
	return out
}

func TestQuery_GroupByOverViews(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	q := NewQuery(store.Reader())
	out := invokeQuery(t, q, `{"sql":"SELECT kind, COUNT(*) AS n FROM v_entities GROUP BY kind ORDER BY kind"}`)

	counts := map[string]int64{}
	for _, row := range out.Rows {
		kind, _ := row["kind"].(string)
		n, _ := row["n"].(float64) // JSON numbers decode to float64
		counts[kind] = int64(n)
	}
	// organization is exactly Mithril; person is alice + bob + the seeded self.
	if counts["organization"] != 1 {
		t.Errorf("organization count = %d, want 1 (rows=%+v)", counts["organization"], out.Rows)
	}
	if counts["person"] < 2 {
		t.Errorf("person count = %d, want >= 2", counts["person"])
	}
}

func TestQuery_FactsViewShowsOpenFact(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	q := NewQuery(store.Reader())
	out := invokeQuery(t, q, `{"sql":"SELECT subject_name, state_key, value FROM v_facts WHERE subject_uid = 'alice'"}`)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %+v, want one open fact for alice", out.Rows)
	}
	if got := out.Rows[0]["value"]; got != "pending" {
		t.Errorf("value = %v, want pending", got)
	}
	if got := out.Rows[0]["subject_name"]; got != "Merry" {
		t.Errorf("subject_name = %v, want Merry (hydrated)", got)
	}
}

func TestQuery_Truncation(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	q := NewQuery(store.Reader())
	out := invokeQuery(t, q, `{"sql":"SELECT uid FROM v_entities","limit":1}`)
	if out.RowCount != 1 || !out.Truncated {
		t.Errorf("rowCount=%d truncated=%v, want 1 and true", out.RowCount, out.Truncated)
	}
}

// TestQuery_InjectionLiteralIsHarmless pins the safety contract: SQL crafted to
// drop a table inside a string literal validates (the ';' and DROP are literal),
// runs as an ordinary filtered SELECT, returns nothing, and leaves the data
// intact.
func TestQuery_InjectionLiteralIsHarmless(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	q := NewQuery(store.Reader())
	out := invokeQuery(t, q, `{"sql":"SELECT uid FROM v_entities WHERE display_name = '; DROP TABLE entities; --'"}`)
	if out.RowCount != 0 {
		t.Errorf("rowCount = %d, want 0 (the literal matches no one)", out.RowCount)
	}
	// The entities table must still be queryable and populated.
	after := invokeQuery(t, q, `{"sql":"SELECT COUNT(*) AS n FROM v_entities"}`)
	if n, _ := after.Rows[0]["n"].(float64); n < 3 {
		t.Errorf("entities count after injection attempt = %v, want >= 3 (table intact)", n)
	}
}

func TestQuery_RejectsWrite(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	q := NewQuery(store.Reader())
	_, err := q.Invoke(context.Background(), json.RawMessage(`{"sql":"UPDATE entities SET kind = 'x'"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestQuery_DuplicateColumns(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	q := NewQuery(store.Reader())
	_, err := q.Invoke(context.Background(), json.RawMessage(`{"sql":"SELECT uid, uid FROM v_entities"}`))
	wantCode(t, err, mcp.CodeValidation)
}

func TestQuery_BadSQL_Validation(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	q := NewQuery(store.Reader())
	_, err := q.Invoke(context.Background(), json.RawMessage(`{"sql":"SELECT * FROM no_such_view"}`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestVRelationsActiveHonorsValidFrom pins the live-now contract of
// v_relations.active against migration 0017: the flag mirrors the navigator's
// relation-validity gate (not superseded, valid_from already in effect, no
// closing valid_until reached). A future-dated edge is active=0 but stays
// SELECTable; the view never hides a row.
func TestVRelationsActiveHonorsValidFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC()
	past := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	future := now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "alice", "person", "Merry", 1.0, "confirmed", past)
	exec(ent, "acme", "organization", "Mithril", 1.0, "confirmed", past)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	// open (no validity bounds) -> active
	exec(rel, "r_open", "alice", "works_at", "acme", 1.0, "ep1", past, nil, nil, nil, `{}`)
	// valid_from in the past, no valid_until -> active
	exec(rel, "r_past", "alice", "knows", "acme", 1.0, "ep2", past, past, nil, nil, `{}`)
	// valid_from in the future -> NOT active (the bug this test guards)
	exec(rel, "r_future", "alice", "will_join", "acme", 1.0, "ep3", past, future, nil, nil, `{}`)
	// superseded -> NOT active
	exec(rel, "r_super", "alice", "worked_at", "acme", 1.0, "ep4", past, past, nil, "r_open", `{}`)
	// valid_until already passed -> NOT active
	exec(rel, "r_closed", "alice", "former", "acme", 1.0, "ep5", past, past, past, nil, `{}`)

	active := map[string]int64{}
	rows, qerr := store.Reader().QueryContext(ctx, "SELECT uid, active FROM v_relations")
	if qerr != nil {
		t.Fatalf("query v_relations: %v", qerr)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid string
		var act int64
		if serr := rows.Scan(&uid, &act); serr != nil {
			t.Fatalf("scan: %v", serr)
		}
		active[uid] = act
	}
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("rows.Err: %v", rerr)
	}

	want := map[string]int64{
		"r_open": 1, "r_past": 1,
		"r_future": 0, "r_super": 0, "r_closed": 0,
	}
	for uid, w := range want {
		got, ok := active[uid]
		if !ok {
			t.Errorf("relation %s missing from v_relations (a row must stay selectable, active=%d expected)", uid, w)
			continue
		}
		if got != w {
			t.Errorf("v_relations.active[%s] = %d, want %d", uid, got, w)
		}
	}
	// The future-dated edge is non-live but must still be visible.
	if _, ok := active["r_future"]; !ok {
		t.Errorf("future-dated relation r_future is not selectable from v_relations")
	}
}

// TestQueryViewCatalog_MatchesLiveViews validates the introspect view catalog
// against the live schema: every column it advertises must exist in the actual
// view, so the documented contract cannot silently drift from migration 0017.
func TestQueryViewCatalog_MatchesLiveViews(t *testing.T) {
	t.Parallel()
	store := newQueryStore(t)
	for _, v := range queryViews {
		cols := make([]string, 0, len(v.Columns))
		for _, c := range v.Columns {
			cols = append(cols, c.Name)
		}
		// LIMIT 0: we only need the statement to bind, not return rows. An unknown
		// column or view name fails to prepare.
		q := "SELECT " + strings.Join(cols, ", ") + " FROM " + v.Name + " LIMIT 0"
		if err := bindQuery(store, q); err != nil {
			t.Errorf("view %s: catalog column missing from live view: %v", v.Name, err)
		}
	}
}

// bindQuery runs q for its side effect of binding (preparing + executing) only,
// returning any error so the caller can assert the view/columns exist.
func bindQuery(store *sqlite.Store, q string) error {
	rows, err := store.Reader().QueryContext(context.Background(), q)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	return rows.Err()
}

// --- scope enforcement: temp-view shadowing isolates team rows ---

// TestQuery_TeamScopeHidden proves that a team-scoped entity is invisible to
// the default query (omitting scopes defaults to DefaultReadSet =
// {project, personal}). The test also verifies it surfaces when
// scopes:["team"] is explicitly requested and when all three scopes are passed.
func TestQuery_TeamScopeHidden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed: %v", eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, "scope-personal", "person", "Frodo", 1.0, "confirmed", now, "personal")
	exec(ent, "scope-team", "person", "Bilbo", 1.0, "confirmed", now, "team")

	q := NewQuery(store.Reader())
	sql := `{"sql":"SELECT uid FROM v_entities WHERE uid IN ('scope-personal','scope-team') ORDER BY uid"}`

	// Default (no scopes): only personal is visible.
	out := invokeQuery(t, q, sql)
	if len(out.Rows) != 1 {
		t.Fatalf("default scope: got %d rows, want 1 (team hidden); rows=%+v", len(out.Rows), out.Rows)
	}
	if uid, _ := out.Rows[0]["uid"].(string); uid != "scope-personal" {
		t.Errorf("default scope: uid=%q, want scope-personal", uid)
	}

	// scopes:["team"] — only the team entity surfaces.
	outTeam := invokeQuery(t, q, `{"sql":"SELECT uid FROM v_entities WHERE uid IN ('scope-personal','scope-team') ORDER BY uid","scopes":["team"]}`)
	if len(outTeam.Rows) != 1 {
		t.Fatalf("team scope: got %d rows, want 1; rows=%+v", len(outTeam.Rows), outTeam.Rows)
	}
	if uid, _ := outTeam.Rows[0]["uid"].(string); uid != "scope-team" {
		t.Errorf("team scope: uid=%q, want scope-team", uid)
	}

	// scopes:["project","personal","team"] — both entities surface.
	outAll := invokeQuery(t, q, `{"sql":"SELECT uid FROM v_entities WHERE uid IN ('scope-personal','scope-team') ORDER BY uid","scopes":["project","personal","team"]}`)
	if len(outAll.Rows) != 2 {
		t.Fatalf("all scopes: got %d rows, want 2; rows=%+v", len(outAll.Rows), outAll.Rows)
	}
}

// TestQuery_ScopeCTEWithUserCTE verifies that scope injection composes
// correctly with a user-defined CTE: the user CTE reads from our scope-filtered
// v_entities shadow, so only the requested-scope rows reach the final SELECT.
func TestQuery_ScopeCTEWithUserCTE(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed: %v", eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, "cte-personal", "wizard", "Gandalf", 1.0, "confirmed", now, "personal")
	exec(ent, "cte-team", "wizard", "Saruman", 1.0, "confirmed", now, "team")

	q := NewQuery(store.Reader())
	// A user-defined CTE that reads from v_entities — our injected CTE is first
	// in the WITH clause so the user's CTE sees only scope-filtered rows.
	userCTE := `{"sql":"WITH wizards AS (SELECT uid FROM v_entities WHERE kind = 'wizard' AND uid IN ('cte-personal','cte-team')) SELECT uid FROM wizards ORDER BY uid"}`

	// Default scope (personal+project): user CTE only sees cte-personal.
	out := invokeQuery(t, q, userCTE)
	if len(out.Rows) != 1 {
		t.Fatalf("default scope via user CTE: got %d rows, want 1; rows=%+v", len(out.Rows), out.Rows)
	}
	if uid, _ := out.Rows[0]["uid"].(string); uid != "cte-personal" {
		t.Errorf("default scope: uid=%q, want cte-personal", uid)
	}

	// scopes:["team"]: user CTE only sees cte-team.
	userCTETeam := `{"sql":"WITH wizards AS (SELECT uid FROM v_entities WHERE kind = 'wizard' AND uid IN ('cte-personal','cte-team')) SELECT uid FROM wizards ORDER BY uid","scopes":["team"]}`
	outTeam := invokeQuery(t, q, userCTETeam)
	if len(outTeam.Rows) != 1 {
		t.Fatalf("team scope via user CTE: got %d rows, want 1; rows=%+v", len(outTeam.Rows), outTeam.Rows)
	}
	if uid, _ := outTeam.Rows[0]["uid"].(string); uid != "cte-team" {
		t.Errorf("team scope: uid=%q, want cte-team", uid)
	}
}
