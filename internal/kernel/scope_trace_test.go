package kernel_test

// Scope-isolation enforcement tests (#206). This is the acceptance-criteria
// centerpiece: a build-time guarantee that NO read-spine query against a shared
// derived table executes without a scope predicate. SQLite has no row-level
// security, so we emulate it with two layers that this file exercises:
//
//   1. A capability reader you cannot construct without a ScopeSet
//      (kernel.ScopedReader / Navigator.WithScopes) — fail-closed by TYPE.
//   2. The tracing driver below, which records every SQL statement the read
//      spine submits and asserts each shared-table SELECT carries a `scope`
//      predicate — fail-closed by TEST.
//
// The driver wraps mattn/go-sqlite3. Because the production sqlite.Open call in
// each test already invokes sqlite_vec.Auto() (a process-global C-level
// auto-extension), connections opened through this driver inherit the vec
// extension for free; we never call Auto() a second time (that would
// double-register the vec0 module).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"regexp"
	"strings"
	"sync"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	sqlitestore "github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// ---- tracing driver ---------------------------------------------------------

const traceDriverName = "sqlite3_scopetrace"

// sqlRecorder captures SQL statements submitted through the tracing driver. It
// is process-global because database/sql drivers are registered per process;
// only connections opened via traceDriverName feed it, so it never sees an
// unrelated test's queries. Recording is gated by `on` so the leak / fail-closed
// tests (which share the driver but only assert behavior) do not accumulate.
type sqlRecorder struct {
	mu    sync.Mutex
	on    bool
	stmts []string
}

func (r *sqlRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = true
	r.stmts = nil
}

func (r *sqlRecorder) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = false
}

func (r *sqlRecorder) record(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on {
		r.stmts = append(r.stmts, q)
	}
}

func (r *sqlRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.stmts))
	copy(out, r.stmts)
	return out
}

var (
	traceRecorder     = &sqlRecorder{}
	registerTraceOnce sync.Once
)

type traceDriver struct{ inner driver.Driver }

func (d traceDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	return &traceConn{Conn: c}, nil
}

// traceConn records the SQL of every statement database/sql routes through the
// connection. mattn's *SQLiteConn implements QueryerContext / ExecerContext /
// ConnPrepareContext, so database/sql prefers those over Prepare; we implement
// the same optional interfaces, record, then delegate.
type traceConn struct{ driver.Conn }

func (c *traceConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	traceRecorder.record(query)
	if qc, ok := c.Conn.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *traceConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	traceRecorder.record(query)
	if ec, ok := c.Conn.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *traceConn) Prepare(query string) (driver.Stmt, error) {
	traceRecorder.record(query)
	return c.Conn.Prepare(query)
}

func (c *traceConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	traceRecorder.record(query)
	if pc, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func registerTraceDriver() {
	registerTraceOnce.Do(func() {
		sql.Register(traceDriverName, traceDriver{inner: &sqlite3.SQLiteDriver{}})
	})
}

// ---- fixture ----------------------------------------------------------------

const traceTS = "2026-01-01T00:00:00Z"

// scopeFixture creates a migrated store, seeds a personal + project row in every
// shared derived table, and returns a tracing reader over the SAME file plus the
// owning store (kept open as a concurrent WAL reader; closed by cleanup).
func scopeFixture(t *testing.T) (*sql.DB, *sqlitestore.Store) {
	t.Helper()
	registerTraceDriver()

	path := t.TempDir() + "/scope.db"
	store, err := sqlitestore.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	seedScopeRows(t, store.Writer())

	tracedDB, err := sql.Open(traceDriverName, "file:"+path+"?_busy_timeout=5000&mode=ro")
	if err != nil {
		t.Fatalf("open traced reader: %v", err)
	}
	tracedDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = tracedDB.Close() })
	if perr := tracedDB.PingContext(t.Context()); perr != nil {
		t.Fatalf("ping traced reader: %v", perr)
	}
	return tracedDB, store
}

func seedScopeRows(t *testing.T, w *sql.DB) {
	t.Helper()
	ctx := t.Context()
	exec := func(q string, args ...any) {
		if _, err := w.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	// episodes (FK target for mentions).
	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source, class, scope)
	      VALUES (?, ?, 'remember', '{}', ?, 'test', 'episodic', ?)`,
		"ep_personal", traceTS, "personal episode text", "personal")
	exec(`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source, class, scope)
	      VALUES (?, ?, 'remember', '{}', ?, 'test', 'episodic', ?)`,
		"ep_project", traceTS, "project episode text", "project")

	// entities.
	exec(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope)
	      VALUES (?, 'person', ?, 1.0, 'resolved', ?, ?)`,
		"ent_personal", "Personal Person", traceTS, "personal")
	exec(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope)
	      VALUES (?, 'person', ?, 1.0, 'resolved', ?, ?)`,
		"ent_project", "Project Person", traceTS, "project")

	// mentions (link episode -> entity).
	exec(`INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid, scope)
	      VALUES (?, 'person', ?, ?, ?, ?, ?)`,
		"m_personal", "Personal Person", "ep_personal", traceTS, "ent_personal", "personal")
	exec(`INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid, scope)
	      VALUES (?, 'person', ?, ?, ?, ?, ?)`,
		"m_project", "Project Person", "ep_project", traceTS, "ent_project", "project")

	// relations.
	exec(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, scope)
	      VALUES (?, ?, 'related_to', ?, 1.0, ?, ?, ?)`,
		"rel_personal", "ent_personal", "ent_project", "ep_personal", traceTS, "personal")

	// facts.
	exec(`INSERT INTO facts (uid, subject_uid, state_key, value, create_time, source_event_uid, scope)
	      VALUES (?, ?, 'state.status', 'active', ?, ?, ?)`,
		"fact_personal", "ent_personal", traceTS, "ep_personal", "personal")

	// documents (so the recall doc lane produces a hit and attachDocumentEntities runs).
	exec(`INSERT INTO documents (document_uid, path, slug, title, body, content_hash, revision_uid, create_time, update_time, scope)
	      VALUES (?, ?, 'p', 'P', ?, 'hash1', 'rev1', ?, ?, ?)`,
		"doc_personal", "notes/p.md", "personal document body", traceTS, traceTS, "personal")
}

// ---- stubs for the HybridQuery lanes ----------------------------------------

type traceEmbedder struct{}

func (traceEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// traceVector returns a document-chunk hit for the document lane (Kinds carries
// "documentRevision") and episode hits otherwise, so both the episode and
// document hydration paths execute their SQL.
type traceVector struct{}

func (traceVector) Search(_ context.Context, _ []float32, _ int, f kernel.VectorFilter) ([]kernel.VectorHit, error) {
	for _, k := range f.Kinds {
		if string(k) == "documentRevision" {
			return []kernel.VectorHit{{ID: "doc_personal#0"}}, nil
		}
	}
	return []kernel.VectorHit{{ID: "ep_personal"}, {ID: "ep_project"}}, nil
}
func (traceVector) Upsert(context.Context, []kernel.VectorItem) error { return nil }
func (traceVector) Delete(context.Context, []string) error            { return nil }
func (traceVector) Close() error                                      { return nil }

type traceFTS struct{}

func (traceFTS) Search(context.Context, string, int) ([]kernel.FTSHit, error) {
	return []kernel.FTSHit{{ID: "ep_project"}}, nil
}
func (traceFTS) Index(context.Context, []kernel.FTSItem) error { return nil }
func (traceFTS) Delete(context.Context, []string) error        { return nil }
func (traceFTS) Close() error                                  { return nil }

// ---- enforcement test -------------------------------------------------------

// sharedDerivedTables are the projection tables that carry a scope column
// (migration 0027). Every read-spine SELECT touching one of these must filter on
// scope. Word-boundary matching deliberately excludes the vec/fts sidecars
// (documents_vec, documents_fts, entities_vec, *_fts) and the curated views
// (v_entities) — an underscore is a word character, so `\bentities\b` matches
// "FROM entities" but not "v_entities" or "entities_vec".
var sharedDerivedTables = []string{"entities", "relations", "facts", "episodes", "mentions", "documents"}

func TestScopeTrace_NoSharedTableReadRunsWithoutScopePredicate(t *testing.T) {
	tracedDB, _ := scopeFixture(t)

	nav, err := navigate.New(tracedDB)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	// Bind the request scope, exactly as the MCP/CLI boundary does.
	nav = nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))

	hybrid, err := kernel.NewHybridQuery(traceEmbedder{}, traceVector{}, traceFTS{}, tracedDB)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	traceRecorder.start()
	driveReadSpine(t, nav, hybrid)
	traceRecorder.stop()

	// contextbrief is intentionally NOT driven here: its reads delegate to the
	// Navigator (ListDocuments / ListEntities) and the HybridQuery recall lane,
	// both covered above. It introduces no novel shared-table SQL, so covering
	// it would only re-exercise the same statements. (No silent gap: stated.)
	t.Log("coverage note: contextbrief.Build emits no shared-table SQL beyond Navigator + HybridQuery (covered). The spatial Near lane (geo.NearbyEntities) is not driven through this in-process tracing harness (the geo scan runs on its own *sql.DB), but it now REQUIRES a non-empty scope predicate by signature and is covered by TestNearbyEntities_OutOfScopeExcluded in the dispatcher package (team entity in radius hidden from a default-scope caller, fail-closed on empty). The doc-lint Navigator methods (OrphanDocuments / DocumentContradictions / DanglingDocumentEdges / NearDuplicateDocuments) are maintenance reads invoked with AllScopes (deliberate full read); each now carries a scope predicate on EVERY joined alias (edge + both endpoint documents), so a narrowed scope would isolate correctly. Under AllScopes the predicate matches all rows, so lint behaviour is unchanged. They are not driven in this per-request spine assertion.")

	stmts := traceRecorder.snapshot()
	if len(stmts) == 0 {
		t.Fatal("recorder captured zero statements — the tracing driver is not wired")
	}

	var (
		checked  int
		exempted int
	)
	seen := make(map[string]bool)
	for _, raw := range stmts {
		stmt := strings.Join(strings.Fields(raw), " ") // normalize whitespace
		if seen[stmt] {
			continue
		}
		seen[stmt] = true

		tbl, refs := referencesSharedTable(stmt)
		if !refs {
			continue
		}
		if reason, ok := scopeExempt(stmt); ok {
			exempted++
			t.Logf("ALLOWLIST (%s): %s", reason, truncate(stmt, 120))
			continue
		}
		checked++
		if !hasScopePredicate(stmt) {
			t.Errorf("shared table %q read WITHOUT any scope predicate:\n  %s", tbl, stmt)
			continue
		}
		// Stricter: every gating shared-table alias (FROM / INNER JOIN /
		// subquery source) must be scoped, not just one of them. A multi-table
		// read that scopes only the driving table still leaks through the
		// unscoped join — the class Greptile kept finding on #206.
		if unscoped := unscopedGatingAliases(stmt); len(unscoped) > 0 {
			t.Errorf("shared-table read scopes some but not all gating aliases (unscoped: %v):\n  %s", unscoped, stmt)
		}
	}

	if checked == 0 {
		t.Fatal("no shared-table statements were checked — the spine driver exercised nothing")
	}
	t.Logf("scope-trace: %d shared-table statements checked, %d allowlisted", checked, exempted)
}

// driveReadSpine exercises every read-spine entry point so the tracing driver
// captures its SQL. Errors are tolerated (missing UIDs return ErrNotFound) — the
// goal is to make the query EXECUTE so its statement is recorded, not to assert
// results.
func driveReadSpine(t *testing.T, nav *navigate.Navigator, hybrid *kernel.HybridQuery) {
	t.Helper()
	ctx := t.Context()

	// Navigator — entities.
	_, _ = nav.ListEntities(ctx, navigate.EntityFilter{})
	_, _ = nav.GetEntity(ctx, "ent_personal")
	_, _ = nav.CountEntities(ctx, navigate.EntityFilter{})
	_, _ = nav.EntityKinds(ctx)
	// AIP-160 filter lowering: the related() call compiles an EXISTS over
	// relations + a LEFT JOIN entities o whose display_name is matched in the
	// WHERE — exercises the outer-join-used-in-predicate gating path.
	_, _ = nav.ListEntities(ctx, navigate.EntityFilter{Filter: `related("works_at", "Mithril")`})
	// Navigator — relations / traversal.
	_, _ = nav.EntityRelations(ctx, "ent_personal", navigate.RelationFilter{})
	_, _ = nav.GetRelation(ctx, "rel_personal")
	_, _ = nav.Neighbors(ctx, "ent_personal", 1, navigate.RelationFilter{})
	// Navigator — facts.
	_, _ = nav.GetFact(ctx, "fact_personal")
	_, _ = nav.FactsForSubject(ctx, "ent_personal", navigate.FactFilter{IncludeInactive: true})
	// Navigator — episodes / provenance / identifiers.
	_, _ = nav.GetEpisode(ctx, "ep_personal")
	_, _ = nav.EpisodesCitingEntity(ctx, "ent_personal", 10)
	_, _ = nav.EntityProvenance(ctx, "ent_personal")
	// EntityIdentifiers joins entities (gating on entity scope), so it must carry
	// a scope predicate; EventProvenance touches only the unscoped provenance
	// lineage table (reached only via a scoped parent read) — driving both keeps
	// the captured-SQL set honest about what the spine actually executes.
	_, _ = nav.EntityIdentifiers(ctx, "ent_personal")
	_, _ = nav.EventProvenance(ctx, "ep_personal")
	// Navigator — documents.
	_, _ = nav.ListDocuments(ctx, navigate.DocumentFilter{})
	_, _ = nav.GetDocumentByUID(ctx, "doc_personal")

	// HybridQuery — recall (episode + document hydration lanes, plus
	// markSuperseded / attachEntities / attachDocumentEntities).
	for _, err := range hybrid.Recall(ctx, kernel.RecallRequest{Query: "person", Scopes: []string{"personal"}}) {
		if err != nil {
			t.Logf("recall lane error (tolerated): %v", err)
			break
		}
	}
}

func referencesSharedTable(stmt string) (string, bool) {
	low := strings.ToLower(stmt)
	for _, tbl := range sharedDerivedTables {
		if regexp.MustCompile(`\b` + tbl + `\b`).MatchString(low) {
			return tbl, true
		}
	}
	return "", false
}

// scopeExempt returns the documented reason a shared-table-referencing statement
// is allowed to omit a scope predicate. The only legitimate case is a schema
// probe that names a table as a string literal in sqlite_master rather than
// reading its rows.
func scopeExempt(stmt string) (string, bool) {
	low := strings.ToLower(stmt)
	if strings.Contains(low, "sqlite_master") || strings.Contains(low, "pragma_table_info") {
		return "schema probe (sqlite_master / pragma) — names tables as literals, reads no rows", true
	}
	return "", false
}

func hasScopePredicate(stmt string) bool {
	// ScopeSet.Predicate always emits "<alias.>scope IN (...)"; the empty
	// (fail-closed) set emits "scope IN (NULL)". Both contain "scope in (".
	return strings.Contains(strings.ToLower(stmt), "scope in (")
}

// unscopedGatingAliases returns the shared-derived-table bindings in stmt that
// GATE which rows are returned (a FROM, INNER JOIN, or subquery FROM) yet carry
// no matching scope predicate. This is stricter than hasScopePredicate: a
// statement that joins two shared tables but scopes only one passes the coarse
// check (a scope predicate is "present") while still leaking through the
// unscoped alias — the exact class Greptile kept surfacing on #206.
//
// INNER joins and FROM/subquery sources always gate inclusion and must be
// scoped. A LEFT / RIGHT / FULL OUTER join is exempt ONLY when it is pure
// hydration — its alias appears solely in the SELECT list and its own ON clause,
// projecting a column (e.g. a display_name) for a UID already proven in-scope by
// the driving statement. The moment an outer-joined alias is referenced in a
// WHERE predicate it gates row inclusion (e.g. related()'s
// `o.display_name = ?`), so it must be scoped too.
func unscopedGatingAliases(stmt string) []string {
	low := strings.ToLower(stmt)
	shared := map[string]bool{}
	for _, t := range sharedDerivedTables {
		shared[t] = true
	}
	// Tokens that can sit where an alias would, but are not aliases.
	stop := map[string]bool{
		"where": true, "on": true, "group": true, "order": true, "limit": true,
		"having": true, "union": true, "join": true, "left": true, "right": true,
		"inner": true, "cross": true, "full": true, "outer": true, "using": true,
		"and": true, "or": true, "as": true, "natural": true, "set": true,
	}
	// keyword table (optional `as`) optional-alias. Longest join variants first
	// so "left join" binds before the bare "join" alternative.
	bindingRE := regexp.MustCompile(
		`\b(left\s+outer\s+join|left\s+join|right\s+outer\s+join|right\s+join|` +
			`full\s+outer\s+join|full\s+join|inner\s+join|cross\s+join|join|from)\s+` +
			`([a-z_][a-z0-9_]*)\b(?:\s+(?:as\s+)?([a-z_][a-z0-9_]*))?`)
	var unscoped []string
	for _, m := range bindingRE.FindAllStringSubmatch(low, -1) {
		kw, table, alias := m[1], m[2], m[3]
		if !shared[table] {
			continue
		}
		if stop[alias] {
			alias = "" // the captured token is a clause keyword, not an alias
		}
		isOuter := strings.Contains(kw, "left") || strings.Contains(kw, "right") || strings.Contains(kw, "full")
		if isOuter && (alias == "" || !aliasUsedInPredicate(low, alias)) {
			continue // outer join used only for hydration (SELECT / ON) — exempt
		}
		var ok bool
		if alias != "" {
			ok = regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\.scope\s+in\s*\(`).MatchString(low)
		} else {
			// Unaliased single source: a bare (unqualified) "scope IN (" covers it.
			ok = regexp.MustCompile(`(^|[^\w.])scope\s+in\s*\(`).MatchString(low)
		}
		if !ok {
			label := table
			if alias != "" {
				label = table + " " + alias
			}
			unscoped = append(unscoped, label)
		}
	}
	return unscoped
}

// aliasUsedInPredicate reports whether alias is referenced in the statement's
// WHERE predicate (everything after the first " where "). ON-clause join keys
// and SELECT-list projections sit before the first WHERE, so an outer-joined
// alias that appears only there is hydration; one that appears after is a
// filter that gates row inclusion and therefore must be scoped.
func aliasUsedInPredicate(low, alias string) bool {
	wi := strings.Index(low, " where ")
	if wi < 0 {
		return false
	}
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\.`).MatchString(low[wi:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestUnscopedGatingAliases pins the detector that backs the per-alias trace
// gate, using the exact query shapes Greptile flagged on #206 (and their fixes).
// It proves the gate has teeth — a multi-table read that scopes only the driving
// table is caught — independent of which recall lanes a given fixture drives.
func TestUnscopedGatingAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		stmt string
		want []string
	}{
		{
			name: "single unaliased source with bare scope predicate is clean",
			stmt: `SELECT DISTINCT entity_uid FROM mentions WHERE entity_uid IN (?) AND scope IN (?, ?)`,
			want: nil,
		},
		{
			name: "mention join scoping both aliases is clean",
			stmt: `SELECT m.source_event_uid, e.uid FROM mentions m JOIN entities e ON e.uid = m.entity_uid ` +
				`WHERE m.source_event_uid IN (?) AND m.scope IN (?, ?) AND e.scope IN (?, ?)`,
			want: nil,
		},
		{
			name: "mention join scoping only the entity leaks the mention",
			stmt: `SELECT m.source_event_uid, e.uid FROM mentions m JOIN entities e ON e.uid = m.entity_uid ` +
				`WHERE m.source_event_uid IN (?) AND e.scope IN (?, ?)`,
			want: []string{"mentions m"},
		},
		{
			name: "left join hydration is exempt when the driving statement is scoped",
			stmt: `SELECT r.uid, s.display_name, o.display_name FROM relations r ` +
				`LEFT JOIN entities s ON s.uid = r.subject_uid ` +
				`LEFT JOIN entities o ON o.uid = r.object_uid ` +
				`WHERE r.uid = ? AND r.scope IN (?, ?)`,
			want: nil,
		},
		{
			name: "scoped mention subquery is clean",
			stmt: `SELECT e.uid FROM episodes e WHERE e.scope IN (?, ?) AND e.uid IN (` +
				`SELECT DISTINCT m.source_event_uid FROM mentions m WHERE m.entity_uid = ? AND m.scope IN (?, ?))`,
			want: nil,
		},
		{
			name: "unscoped mention subquery leaks even when the outer episode is scoped",
			stmt: `SELECT e.uid FROM episodes e WHERE e.scope IN (?, ?) AND e.uid IN (` +
				`SELECT DISTINCT m.source_event_uid FROM mentions m WHERE m.entity_uid = ?)`,
			want: []string{"mentions m"},
		},
		{
			name: "document graph lane scoping only the edge leaks the document",
			stmt: `SELECT r.subject_uid FROM relations r JOIN documents d ON d.document_uid = r.subject_uid ` +
				`WHERE r.predicate IN (?) AND r.scope IN (?, ?)`,
			want: []string{"documents d"},
		},
		{
			name: "outer join matched by name in the where leaks when unscoped",
			stmt: `SELECT 1 FROM relations r LEFT JOIN entities o ON o.uid = r.object_uid ` +
				`WHERE r.subject_uid = ? AND (r.object_uid = ? OR o.display_name = ?) AND r.scope IN (?, ?)`,
			want: []string{"entities o"},
		},
		{
			name: "outer join matched by name in the where is clean when scoped",
			stmt: `SELECT 1 FROM relations r LEFT JOIN entities o ON o.uid = r.object_uid ` +
				`WHERE r.subject_uid = ? AND (r.object_uid = ? OR (o.display_name = ? AND o.scope IN (?, ?))) ` +
				`AND r.scope IN (?, ?)`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unscopedGatingAliases(strings.ToLower(strings.Join(strings.Fields(tc.stmt), " ")))
			if len(got) != len(tc.want) {
				t.Fatalf("unscopedGatingAliases = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("unscopedGatingAliases = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// ---- fail-closed + leak regression ------------------------------------------

func TestScopeIsolation_EmptyScopeSetFailsClosed(t *testing.T) {
	tracedDB, _ := scopeFixture(t)
	nav, err := navigate.New(tracedDB)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	// An explicitly empty ScopeSet must return nothing over a populated store —
	// fail-closed, never fail-open to everything.
	empty := nav.WithScopes(kernel.NewScopeSet())
	ents, err := empty.ListEntities(t.Context(), navigate.EntityFilter{})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("empty ScopeSet returned %d entities, want 0 (fail-closed)", len(ents))
	}
}

func TestScopeIsolation_PersonalReadHidesOtherScopes(t *testing.T) {
	tracedDB, _ := scopeFixture(t)
	nav, err := navigate.New(tracedDB)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := t.Context()

	personal, err := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal)).ListEntities(ctx, navigate.EntityFilter{})
	if err != nil {
		t.Fatalf("personal ListEntities: %v", err)
	}
	if !containsEntity(personal, "ent_personal") {
		t.Errorf("personal read missing ent_personal: %v", entityUIDs(personal))
	}
	if containsEntity(personal, "ent_project") {
		t.Errorf("LEAK: personal read surfaced project entity: %v", entityUIDs(personal))
	}

	project, err := nav.WithScopes(kernel.NewScopeSet(kernel.ScopeProject)).ListEntities(ctx, navigate.EntityFilter{})
	if err != nil {
		t.Fatalf("project ListEntities: %v", err)
	}
	if !containsEntity(project, "ent_project") {
		t.Errorf("explicit {project} read missing ent_project: %v", entityUIDs(project))
	}

	def, err := nav.WithScopes(kernel.DefaultReadSet()).ListEntities(ctx, navigate.EntityFilter{})
	if err != nil {
		t.Fatalf("default ListEntities: %v", err)
	}
	// DefaultReadSet = {project, personal} → both visible, team would not be.
	if !containsEntity(def, "ent_personal") || !containsEntity(def, "ent_project") {
		t.Errorf("default read should span project+personal, got %v", entityUIDs(def))
	}
}

// TestScopeIsolation_OutOfScopeEdgeHiddenFromInScopeEntity proves the headline
// guarantee that a row's own scope column alone does not: an entity that is
// itself in-scope must NOT have its out-of-scope edges (or the entities reached
// only through them) surface under a narrowed read. Both entities here are
// personal; the ONLY relation between them is project-scoped, so a personal read
// must not traverse it.
func TestScopeIsolation_OutOfScopeEdgeHiddenFromInScopeEntity(t *testing.T) {
	tracedDB, store := scopeFixture(t)
	ctx := t.Context()
	w := store.Writer()
	seed := func(q string, args ...any) {
		if _, err := w.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	// Two in-scope (personal) entities, reachable from each other ONLY via a
	// project-scoped relation.
	seed(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope)
	      VALUES ('ent_p_a', 'person', 'Merry', 1.0, 'resolved', ?, 'personal')`, traceTS)
	seed(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope)
	      VALUES ('ent_p_b', 'person', 'Sam', 1.0, 'resolved', ?, 'personal')`, traceTS)
	seed(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, scope)
	      VALUES ('rel_proj_ab', 'ent_p_a', 'related_to', 'ent_p_b', 1.0, 'ep_project', ?, 'project')`, traceTS)

	nav, err := navigate.New(tracedDB)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	personal := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))

	// EntityRelations under personal must not surface the project-scope edge.
	rels, err := personal.EntityRelations(ctx, "ent_p_a", navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	if len(rels) != 0 {
		t.Errorf("LEAK: personal read of ent_p_a surfaced %d project-scope relation(s): %+v", len(rels), rels)
	}

	// Neighbors walks via EntityRelations, so b must be unreachable under personal.
	nb, err := personal.Neighbors(ctx, "ent_p_a", 2, navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	for _, n := range nb {
		if n.Entity.UID == "ent_p_b" {
			t.Errorf("LEAK: personal Neighbors(ent_p_a) reached ent_p_b through a project-scope edge")
		}
	}

	// Sanity: including project scope makes the edge (and b) reachable — proving
	// the hiding above is the scope filter, not a seeding mistake.
	both := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal, kernel.ScopeProject))
	rels2, err := both.EntityRelations(ctx, "ent_p_a", navigate.RelationFilter{})
	if err != nil {
		t.Fatalf("EntityRelations (both scopes): %v", err)
	}
	if len(rels2) != 1 || rels2[0].ObjectUID != "ent_p_b" {
		t.Errorf("with {personal,project} the edge should be visible, got %+v", rels2)
	}
}

// TestScopeDraft_NeverSurfacesThroughDefaultOrAllScopes is the security
// regression guard for the draft scope: a row written with scope='draft'
// must NEVER appear under DefaultReadSet() or AllScopes() (which intentionally
// exclude ScopeDraft). It must only be visible when the caller explicitly
// constructs ScopeDraftReadSet().
func TestScopeDraft_NeverSurfacesThroughDefaultOrAllScopes(t *testing.T) {
	tracedDB, store := scopeFixture(t)
	ctx := t.Context()
	w := store.Writer()
	seed := func(q string, args ...any) {
		if _, err := w.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	// Seed one entity and one episode in the draft scope.
	seed(`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source, class, scope)
	      VALUES ('ep_draft', ?, 'remember', '{}', 'draft episode', 'test', 'episodic', 'draft')`, traceTS)
	seed(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope)
	      VALUES ('ent_draft', 'person', 'Draft Person', 1.0, 'resolved', ?, 'draft')`, traceTS)

	nav, err := navigate.New(tracedDB)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	// DefaultReadSet must not surface the draft entity.
	defEnts, err := nav.WithScopes(kernel.DefaultReadSet()).ListEntities(ctx, navigate.EntityFilter{})
	if err != nil {
		t.Fatalf("DefaultReadSet ListEntities: %v", err)
	}
	if containsEntity(defEnts, "ent_draft") {
		t.Errorf("LEAK: DefaultReadSet surfaced draft entity 'ent_draft'")
	}

	// AllScopes is personal+project+team — it must also exclude draft.
	allEnts, err := nav.WithScopes(kernel.AllScopes()).ListEntities(ctx, navigate.EntityFilter{})
	if err != nil {
		t.Fatalf("AllScopes ListEntities: %v", err)
	}
	if containsEntity(allEnts, "ent_draft") {
		t.Errorf("LEAK: AllScopes surfaced draft entity 'ent_draft'")
	}

	// ScopeDraftReadSet is the ONLY way to observe the draft row.
	draftEnts, err := nav.WithScopes(kernel.ScopeDraftReadSet()).ListEntities(ctx, navigate.EntityFilter{})
	if err != nil {
		t.Fatalf("ScopeDraftReadSet ListEntities: %v", err)
	}
	if !containsEntity(draftEnts, "ent_draft") {
		t.Errorf("ScopeDraftReadSet missing 'ent_draft' — draft row not visible even through explicit draft read set")
	}
	// Sanity: the existing personal entity must NOT bleed into the draft read set.
	if containsEntity(draftEnts, "ent_personal") {
		t.Errorf("LEAK: ScopeDraftReadSet surfaced 'ent_personal' (wrong scope)")
	}
}

func containsEntity(ents []navigate.Entity, uid string) bool {
	for _, e := range ents {
		if e.UID == uid {
			return true
		}
	}
	return false
}

func entityUIDs(ents []navigate.Entity) []string {
	out := make([]string, len(ents))
	for i, e := range ents {
		out[i] = e.UID
	}
	return out
}
