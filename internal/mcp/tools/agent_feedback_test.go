package tools_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestAgentFeedback_RecordsAndReturnsCluster(t *testing.T) {
	t.Parallel()
	app := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAgentFeedback(app, nil)
	body := `{"category":"confusing","severity":"medium","surface":"recall","summary":"recall missed an exact name","rule":"prefer get when you hold the exact name"}`
	out, err := tool.Invoke(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var resp struct {
		Status     string `json:"status"`
		ClusterKey string `json:"clusterKey"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.Status != "recorded" || resp.ClusterKey == "" {
		t.Errorf("resp = %+v", resp)
	}
	if !app.called || len(app.gotEvents) != 1 {
		t.Fatalf("want 1 appended event, got %d (called=%v)", len(app.gotEvents), app.called)
	}
	if app.gotEvents[0].Kind != kernel.EventKind(ontology.KindAgentFeedbackRecorded) {
		t.Errorf("event kind = %q", app.gotEvents[0].Kind)
	}
}

func TestAgentFeedback_ValidationErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing summary":  `{"category":"slow","severity":"low"}`,
		"missing category": `{"severity":"low","summary":"x"}`,
		"bad category":     `{"category":"vibes","severity":"low","summary":"x"}`,
		"bad severity":     `{"category":"slow","severity":"critical","summary":"x"}`,
		"bad scope":        `{"category":"slow","severity":"low","summary":"x","scope":"bogus"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := &fakeAppender{}
			tool := tools.NewAgentFeedback(app, nil)
			_, err := tool.Invoke(context.Background(), json.RawMessage(body))
			wantRelateCode(t, err, mcp.CodeValidation)
			if app.called {
				t.Errorf("appended on a validation failure")
			}
		})
	}
}

func TestFeedbackReview_AppendsAndValidates(t *testing.T) {
	t.Parallel()
	app := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewFeedbackReview(app)
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"clusterKey":"c|recall|x","action":"pin"}`)); err != nil {
		t.Fatalf("valid pin: %v", err)
	}
	if app.gotEvents[0].Kind != kernel.EventKind(ontology.KindAgentFeedbackReviewed) {
		t.Errorf("event kind = %q", app.gotEvents[0].Kind)
	}

	bad := map[string]string{
		"missing cluster":   `{"action":"pin"}`,
		"bad action":        `{"clusterKey":"c","action":"delete"}`,
		"edit without rule": `{"clusterKey":"c","action":"edit"}`,
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := tools.NewFeedbackReview(&fakeAppender{}).Invoke(context.Background(), json.RawMessage(b))
			wantRelateCode(t, err, mcp.CodeValidation)
		})
	}
}

func TestFeedbackReview_IsUnlisted(t *testing.T) {
	t.Parallel()
	if !tools.NewFeedbackReview(&fakeAppender{}).Unlisted() {
		t.Errorf("feedbackReview must be unlisted (human override, not an agent action)")
	}
}

func TestFeedbackQuery_GotchasOnEmptyDBAndBadView(t *testing.T) {
	t.Parallel()
	db := newFeedbackQueryDB(t)
	tool := tools.NewFeedbackQuery(db)

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"view":"gotchas"}`))
	if err != nil {
		t.Fatalf("gotchas view: %v", err)
	}
	var resp struct {
		View    string `json:"view"`
		Gotchas []any  `json:"gotchas"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.View != "gotchas" {
		t.Errorf("view = %q", resp.View)
	}

	_, err = tool.Invoke(context.Background(), json.RawMessage(`{"view":"nonsense"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

// newFeedbackQueryDB creates an in-memory DB with all feedback tables (mirrors
// migrations 0044 + 0045) so the read/write tools can run their real SQL.
func newFeedbackQueryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range []string{
		`CREATE TABLE agent_feedback (uid TEXT PRIMARY KEY, cluster_key TEXT NOT NULL, category TEXT NOT NULL, severity TEXT NOT NULL, surface TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', rule TEXT NOT NULL DEFAULT '', keywords TEXT NOT NULL DEFAULT '', version TEXT NOT NULL DEFAULT '', active_preset TEXT NOT NULL DEFAULT '', source_event_uid TEXT NOT NULL, create_time TEXT NOT NULL, scope TEXT NOT NULL)`,
		`CREATE TABLE agent_feedback_reviews (cluster_key TEXT PRIMARY KEY, pinned INTEGER NOT NULL DEFAULT 0, vetoed INTEGER NOT NULL DEFAULT 0, edited_rule TEXT NOT NULL DEFAULT '', last_action TEXT NOT NULL, source_event_uid TEXT NOT NULL, review_time TEXT NOT NULL)`,
		`CREATE TABLE agent_feedback_promotions (cluster_key TEXT PRIMARY KEY, rule TEXT NOT NULL DEFAULT '', recurrence INTEGER NOT NULL, severity TEXT NOT NULL, source_event_uid TEXT NOT NULL, promote_time TEXT NOT NULL)`,
	} {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return db
}

// insertFeedbackRow seeds one agent_feedback row directly for tool tests.
func insertFeedbackRow(t *testing.T, db *sql.DB, uid, clusterKey, category, severity string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO agent_feedback (uid, cluster_key, category, severity, surface, summary, detail, rule, keywords, version, active_preset, source_event_uid, create_time, scope)
		 VALUES (?, ?, ?, ?, '', 'summary', '', '', '', 'v0.3.0', '', ?, '2026-06-24T00:00:00Z', 'personal')`,
		uid, clusterKey, category, severity, uid)
	if err != nil {
		t.Fatalf("insert feedback row: %v", err)
	}
}

// TestFeedbackReview_PhantomKeyReturnsNotFound proves that reviewing a
// clusterKey that has no matching feedback rows is rejected with E_NOT_FOUND.
func TestFeedbackReview_PhantomKeyReturnsNotFound(t *testing.T) {
	t.Parallel()
	db := newFeedbackQueryDB(t)
	app := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewFeedbackReview(app, db)

	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"clusterKey":"does|not|exist","action":"pin"}`))
	wantRelateCode(t, err, mcp.CodeNotFound)
	if app.called {
		t.Errorf("must not append when cluster does not exist")
	}
}

// TestFeedbackReview_ExistingKeySucceeds proves that a review on a real cluster
// is accepted (exit 0 path).
func TestFeedbackReview_ExistingKeySucceeds(t *testing.T) {
	t.Parallel()
	db := newFeedbackQueryDB(t)
	insertFeedbackRow(t, db, "fb1", "confusing|recall|x", "confusing", "low")
	app := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewFeedbackReview(app, db)

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"clusterKey":"confusing|recall|x","action":"pin"}`))
	if err != nil {
		t.Fatalf("review on existing cluster: %v", err)
	}
	var resp struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.Status != "reviewed" {
		t.Errorf("status = %q, want reviewed", resp.Status)
	}
}

// TestAgentFeedback_PromoEventOnThresholdCrossing proves that when a cluster
// reaches the promotion threshold, maybeAppendPromotion appends exactly one
// agentFeedbackPromoted event, and that a second call (idempotency) does not
// append a second one.
func TestAgentFeedback_PromoEventOnThresholdCrossing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newFeedbackQueryDB(t)

	// LexicalClusterKey("tool_error","relate","retry once") = "tool_error|relate|retry once".
	// high severity threshold = 2; seed 1 existing row so the cluster count is
	// already 1, then pre-insert the row the fakeAppender cannot write (it has no
	// DB handle) so the promotion query sees count=2 and fires.
	const ck = "tool_error|relate|retry once"
	insertFeedbackRow(t, db, "h1", ck, "tool_error", "high")
	insertFeedbackRow(t, db, "h2", ck, "tool_error", "high") // simulates the just-appended row

	// fakeAppender always returns exactly 1 receipt; Invoke checks len==1.
	app := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool := tools.NewAgentFeedback(app, db)

	// Invoke appends the recorded event (receipt ok); then maybeAppendPromotion
	// sees count=2 >= threshold=2 and appends the promoted event.
	_, err := tool.Invoke(ctx, json.RawMessage(`{"category":"tool_error","severity":"high","summary":"relate 500","surface":"relate","rule":"retry once"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// The second appended event must be the promotion event.
	if len(app.gotEvents) < 2 {
		t.Fatalf("want 2 events (recorded + promoted), got %d", len(app.gotEvents))
	}
	promoEv := app.gotEvents[1]
	if string(promoEv.Kind) != "agentFeedbackPromoted" {
		t.Errorf("second event kind = %q, want agentFeedbackPromoted", promoEv.Kind)
	}

	// Simulate the dispatcher writing to agent_feedback_promotions (idempotency
	// guard) so the second invocation sees it as already promoted.
	_, dbErr := db.ExecContext(ctx,
		`INSERT INTO agent_feedback_promotions (cluster_key, rule, recurrence, severity, source_event_uid, promote_time)
		 VALUES ('tool_error|relate|retry once', 'retry once', 2, 'high', 'ev', '2026-06-24T00:00:00Z')`)
	if dbErr != nil {
		t.Fatalf("seed promotions: %v", dbErr)
	}

	// Reset appender; a second record call must NOT emit another promo event.
	app2 := &fakeAppender{receipts: []kernel.Receipt{{}}}
	tool2 := tools.NewAgentFeedback(app2, db)
	_, err = tool2.Invoke(ctx, json.RawMessage(`{"category":"tool_error","severity":"high","summary":"relate 500 again","surface":"relate","rule":"retry once"}`))
	if err != nil {
		t.Fatalf("second Invoke: %v", err)
	}
	for _, ev := range app2.gotEvents {
		if string(ev.Kind) == "agentFeedbackPromoted" {
			t.Errorf("second invocation must not emit another promotion event (idempotent)")
		}
	}
}
