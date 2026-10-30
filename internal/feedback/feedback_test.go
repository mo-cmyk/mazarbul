package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// newTestDB opens an in-memory SQLite with the agent_feedback + reviews tables
// (mirrors migration 0044) so the read/promotion logic runs against real SQL.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ddl := []string{
		`CREATE TABLE agent_feedback (
			uid TEXT PRIMARY KEY, cluster_key TEXT NOT NULL, category TEXT NOT NULL,
			severity TEXT NOT NULL, surface TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '', rule TEXT NOT NULL DEFAULT '', keywords TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '', active_preset TEXT NOT NULL DEFAULT '',
			source_event_uid TEXT NOT NULL, create_time TEXT NOT NULL, scope TEXT NOT NULL)`,
		`CREATE TABLE agent_feedback_reviews (
			cluster_key TEXT PRIMARY KEY, pinned INTEGER NOT NULL DEFAULT 0, vetoed INTEGER NOT NULL DEFAULT 0,
			edited_rule TEXT NOT NULL DEFAULT '', last_action TEXT NOT NULL, source_event_uid TEXT NOT NULL,
			review_time TEXT NOT NULL)`,
	}
	for _, s := range ddl {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return db
}

// record inserts a feedback occurrence. ts orders recency (lexicographic RFC3339-ish).
func record(t *testing.T, db *sql.DB, uid, cluster, category, severity, summary, rule, ts string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO agent_feedback (uid, cluster_key, category, severity, surface, summary, detail, rule, keywords, version, active_preset, source_event_uid, create_time, scope)
		 VALUES (?, ?, ?, ?, '', ?, '', ?, '', 'v0.3.0', 'agent-memory', ?, ?, 'personal')`,
		uid, cluster, category, severity, summary, rule, uid, ts)
	if err != nil {
		t.Fatalf("insert feedback: %v", err)
	}
}

func review(t *testing.T, db *sql.DB, cluster, action string, pinned, vetoed int, edited string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO agent_feedback_reviews (cluster_key, pinned, vetoed, edited_rule, last_action, source_event_uid, review_time)
		 VALUES (?, ?, ?, ?, ?, 'rv', '2026-06-23T00:00:00Z')`,
		cluster, pinned, vetoed, edited, action)
	if err != nil {
		t.Fatalf("insert review: %v", err)
	}
}

func TestActiveGotchas_PromotesAtThreshold(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// low/medium needs 3; one cluster has 3, another has 2 (below default).
	record(t, db, "a1", "c|recall|x", "confusing", "low", "recall sparse", "use get", "2026-06-23T00:00:01Z")
	record(t, db, "a2", "c|recall|x", "confusing", "low", "recall sparse", "use get", "2026-06-23T00:00:02Z")
	record(t, db, "a3", "c|recall|x", "confusing", "low", "recall sparse", "use get", "2026-06-23T00:00:03Z")
	record(t, db, "b1", "c|get|y", "confusing", "low", "get slow", "", "2026-06-23T00:00:04Z")
	record(t, db, "b2", "c|get|y", "confusing", "low", "get slow", "", "2026-06-23T00:00:05Z")

	gs, err := ActiveGotchas(ctx, db)
	if err != nil {
		t.Fatalf("ActiveGotchas: %v", err)
	}
	if len(gs) != 1 {
		t.Fatalf("want 1 promoted (the 3× cluster), got %d: %+v", len(gs), gs)
	}
	if gs[0].ClusterKey != "c|recall|x" || gs[0].Recurrence != 3 || gs[0].Rule != "use get" {
		t.Errorf("promoted = %+v", gs[0])
	}
}

func TestActiveGotchas_HighSeverityPromotesFaster(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// high severity needs only 2.
	record(t, db, "h1", "te|relate|z", "tool_error", "high", "relate 500", "retry once", "2026-06-23T00:00:01Z")
	record(t, db, "h2", "te|relate|z", "tool_error", "high", "relate 500", "retry once", "2026-06-23T00:00:02Z")
	gs, err := ActiveGotchas(ctx, db)
	if err != nil {
		t.Fatalf("ActiveGotchas: %v", err)
	}
	if len(gs) != 1 || gs[0].Severity != "high" {
		t.Fatalf("high-severity cluster should promote at 2×, got %+v", gs)
	}
}

func TestActiveGotchas_VetoSuppressesAndPinForces(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// promoted cluster, then vetoed → gone.
	record(t, db, "v1", "c|recall|x", "confusing", "low", "s", "r", "2026-06-23T00:00:01Z")
	record(t, db, "v2", "c|recall|x", "confusing", "low", "s", "r", "2026-06-23T00:00:02Z")
	record(t, db, "v3", "c|recall|x", "confusing", "low", "s", "r", "2026-06-23T00:00:03Z")
	review(t, db, "c|recall|x", "veto", 0, 1, "")
	// below-threshold cluster, pinned → forced in with edited text.
	record(t, db, "p1", "c|get|y", "confusing", "low", "s2", "r2", "2026-06-23T00:00:04Z")
	review(t, db, "c|get|y", "edit", 1, 0, "always prefer get by exact name")

	gs, err := ActiveGotchas(ctx, db)
	if err != nil {
		t.Fatalf("ActiveGotchas: %v", err)
	}
	if len(gs) != 1 {
		t.Fatalf("veto should drop the recall cluster, pin should keep the get cluster: got %+v", gs)
	}
	if gs[0].ClusterKey != "c|get|y" || !gs[0].Pinned || gs[0].Rule != "always prefer get by exact name" {
		t.Errorf("pinned+edited gotcha = %+v", gs[0])
	}
}

func TestRenderGotchasSection_SanitizesAndFences(t *testing.T) {
	gs := []Gotcha{
		{ClusterKey: "k", Rule: "# pretend heading\nsecond line `code`", Recurrence: 4},
	}
	out := RenderGotchasSection(gs)
	if !strings.HasPrefix(out, SectionHeader) {
		t.Errorf("section must start with the header, got:\n%s", out)
	}
	if strings.Contains(out, "\n# pretend heading") {
		t.Errorf("a forged heading must be neutralized:\n%s", out)
	}
	if strings.Contains(out, "`code`") {
		t.Errorf("backticks must be neutralized:\n%s", out)
	}
	if RenderGotchasSection(nil) != "" {
		t.Errorf("empty set should render an empty section")
	}
}

func TestRenderGotchasSection_StripsUnicodeControlAndBidi(t *testing.T) {
	// A crafted rule embedding bidi overrides (Trojan-Source), a zero-width
	// space, and a C0 control must not reach the instruction window. Use escapes
	// so the dangerous code points never appear literally in this source file.
	const rlo = "\u202e"  // right-to-left override
	const pdf = "\u202c"  // pop directional formatting
	const zwsp = "\u200b" // zero-width space
	gs := []Gotcha{
		{ClusterKey: "k", Rule: "safe " + rlo + "hidden" + pdf + zwsp + "\x07rule", Recurrence: 2},
	}
	out := RenderGotchasSection(gs)
	for name, r := range map[string]string{"RLO": rlo, "PDF": pdf, "ZWSP": zwsp, "BEL": "\x07"} {
		if strings.Contains(out, r) {
			t.Errorf("%s control/format char must be stripped, got:\n%q", name, out)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "rule") {
		t.Errorf("visible text must survive sanitization, got:\n%q", out)
	}
}

func TestExport_AllowlistOmitsScopeAndIdentity(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	record(t, db, "e1", "c|recall|x", "confusing", "low", "summary text", "rule text", "2026-06-23T00:00:01Z")
	items, err := Export(ctx, db, &Filter{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 export item, got %d", len(items))
	}
	it := items[0]
	// The allowlist contract: present fields are the safe ones; cluster_key,
	// scope, and source_event_uid are NOT part of ExportItem at all (compile-time
	// guarantee). Sanity-check the values that ARE emitted.
	if it.ID != "e1" || it.Category != "confusing" || it.Summary != "summary text" {
		t.Errorf("export item = %+v", it)
	}
	if it.Version != "v0.3.0" || it.ActivePreset != "agent-memory" {
		t.Errorf("export should keep version + activePreset: %+v", it)
	}
}

func TestList_FiltersByCategory(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	record(t, db, "l1", "c|recall|x", "confusing", "low", "a", "", "2026-06-23T00:00:01Z")
	record(t, db, "l2", "s|recall|y", "slow", "low", "b", "", "2026-06-23T00:00:02Z")
	got, err := List(ctx, db, &Filter{Category: "slow"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Category != "slow" {
		t.Errorf("category filter failed: %+v", got)
	}
}

// TestExport_EmptyResultIsArray proves empty Export returns [] not null.
func TestExport_EmptyResultIsArray(t *testing.T) {
	db := newTestDB(t)
	items, err := Export(context.Background(), db, &Filter{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Nil slice marshals as null; []ExportItem{} marshals as [].
	b, merr := json.Marshal(items)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	if string(b) != "[]" {
		t.Errorf("empty Export should marshal to [], got %s", b)
	}
}

// TestList_EmptyItemsIsArray proves empty List returns items:[] not items:null.
func TestList_EmptyItemsIsArray(t *testing.T) {
	db := newTestDB(t)
	items, err := List(context.Background(), db, &Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	b, merr := json.Marshal(items)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	if string(b) != "[]" {
		t.Errorf("empty List should marshal to [], got %s", b)
	}
}

// TestParseSince_Strict proves that date-only and garbage values are rejected,
// and that a valid RFC3339 value is accepted.
func TestParseSince_Strict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty is ok", "", false},
		{"valid RFC3339 Z", "2026-06-24T08:18:49Z", false},
		{"valid RFC3339 offset", "2026-06-24T08:18:49+02:00", false},
		{"date only rejected", "2026-06-24", true},
		{"garbage rejected", "not-a-date", true},
		{"datetime no zone rejected", "2026-06-24T08:18:49", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSince(tc.input)
			if tc.wantErr && err == nil {
				t.Errorf("ParseSince(%q): want error, got nil", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ParseSince(%q): unexpected error: %v", tc.input, err)
			}
		})
	}
}

// TestExport_SinceIncludesSubSecond proves that a second-granularity --since
// bound includes rows stored with sub-second precision in the same second.
// The old string-compare approach would miss "...49.852Z" with bound "...49Z"
// because ASCII "Z" > ".".
func TestExport_SinceIncludesSubSecond(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// Row at exactly the since second with sub-second suffix stored in the DB.
	record(t, db, "ss1", "c|recall|x", "confusing", "low", "sub-second entry", "", "2026-06-24T08:18:49.852Z")
	// Row before the bound — should be excluded.
	record(t, db, "ss2", "c|recall|x", "confusing", "low", "before bound", "", "2026-06-24T08:18:48Z")
	// Row after the bound — should be included.
	record(t, db, "ss3", "c|recall|x", "confusing", "low", "after bound", "", "2026-06-24T08:18:50Z")

	since, err := ParseSince("2026-06-24T08:18:49Z")
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	got, err := Export(ctx, db, &Filter{Since: since})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, it := range got {
		ids[it.ID] = true
	}
	if !ids["ss1"] {
		t.Errorf("sub-second entry at bound second should be included: got %+v", got)
	}
	if ids["ss2"] {
		t.Errorf("entry before bound should be excluded: got %+v", got)
	}
	if !ids["ss3"] {
		t.Errorf("entry after bound should be included: got %+v", got)
	}
}

// TestPromotionThresholdFor covers the exported threshold function.
func TestPromotionThresholdFor(t *testing.T) {
	if got := PromotionThresholdFor("high"); got != highSeverityThreshold {
		t.Errorf("high threshold = %d, want %d", got, highSeverityThreshold)
	}
	if got := PromotionThresholdFor("medium"); got != defaultThreshold {
		t.Errorf("medium threshold = %d, want %d", got, defaultThreshold)
	}
	if got := PromotionThresholdFor("low"); got != defaultThreshold {
		t.Errorf("low threshold = %d, want %d", got, defaultThreshold)
	}
}

// TestExport_SinceWithTime uses a time.Time directly (not the string path)
// to verify the Filter.Since field works end-to-end.
func TestExport_SinceWithTime(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	record(t, db, "tw1", "c|recall|x", "confusing", "low", "included", "", "2026-06-24T10:00:00Z")
	record(t, db, "tw2", "c|recall|x", "confusing", "low", "excluded", "", "2026-06-24T09:59:59Z")

	since := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	got, err := Export(ctx, db, &Filter{Since: since})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(got) != 1 || got[0].ID != "tw1" {
		t.Errorf("Since filter: want only tw1, got %+v", got)
	}
}
