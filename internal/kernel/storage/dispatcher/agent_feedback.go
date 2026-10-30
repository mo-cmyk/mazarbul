package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// errEmptyFeedbackSummary rejects an agentFeedbackRecorded event with no
// summary; the summary is the human-readable identity of a feedback row, so an
// empty one is a hard decode-time error rather than a blank row.
var errEmptyFeedbackSummary = errors.New("agentFeedbackRecorded: summary is required")

// errEmptyReviewCluster rejects an agentFeedbackReviewed event with no
// clusterKey; the review row is keyed by cluster, so a blank key has no target.
var errEmptyReviewCluster = errors.New("agentFeedbackReviewed: clusterKey is required")

// applyAgentFeedbackRecorded runs the single-layer (SQLite) write for one
// agentFeedbackRecorded event. Like a Fact or SourceRef, feedback is a scalar
// record rather than a graph edge, so there is no LadybugDB / vector / FTS
// fan-out — only the relational agent_feedback table.
//
// Each capture is a DISTINCT occurrence: the row is keyed by the carrying event
// uid (ev.UID), and replay re-applies identical values (ON CONFLICT(uid) upsert
// is a no-op on identity), so the projection converges under Rebuild. Counting
// recurrence per cluster_key is what drives auto-promotion downstream.
func (d *Dispatcher) applyAgentFeedbackRecorded(ctx context.Context, ev kernel.Event) (retErr error) {
	var fb ontology.AgentFeedbackRecordedEvent
	if err := json.Unmarshal(ev.Payload, &fb); err != nil {
		return fmt.Errorf("decode agentFeedbackRecorded payload: %w", err)
	}
	if strings.TrimSpace(fb.Summary) == "" {
		return errEmptyFeedbackSummary
	}

	clusterKey := fb.ClusterKey
	if clusterKey == "" {
		// Deterministic embedder-free fallback so rebuild converges. Prefer the
		// distilled Rule as the salient text (a lesson clusters on its rule);
		// fall back to the summary for friction reports.
		salient := fb.Rule
		if strings.TrimSpace(salient) == "" {
			salient = fb.Summary
		}
		clusterKey = ontology.LexicalClusterKey(fb.Category, fb.Surface, salient)
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	createTime := ev.CreateTime.UTC().Format(time.RFC3339Nano)
	keywords := strings.Join(fb.Keywords, " ")
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_feedback
			(uid, cluster_key, category, severity, surface, summary, detail, rule,
			 keywords, version, active_preset, source_event_uid, create_time, scope)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(uid) DO UPDATE SET
			cluster_key   = excluded.cluster_key,
			category      = excluded.category,
			severity      = excluded.severity,
			surface       = excluded.surface,
			summary       = excluded.summary,
			detail        = excluded.detail,
			rule          = excluded.rule,
			keywords      = excluded.keywords,
			version       = excluded.version,
			active_preset = excluded.active_preset,
			create_time   = excluded.create_time,
			scope         = excluded.scope`,
		ev.UID, clusterKey, fb.Category, fb.Severity, fb.Surface, fb.Summary,
		fb.Detail, fb.Rule, keywords, fb.Version, fb.ActivePreset, ev.UID,
		createTime, scopeValue(ev),
	); err != nil {
		return fmt.Errorf("exec insert agent feedback: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyReviewAction folds one review verb into the prior (pinned, vetoed,
// editedRule) state and returns the new state. Kept separate from the tx body
// so the projection stays under the cyclomatic-complexity gate and the action
// semantics are unit-testable in isolation. An unknown action errors.
func applyReviewAction(rv ontology.AgentFeedbackReviewedEvent, pinned, vetoed int, editedRule string) (int, int, string, error) {
	switch rv.Action {
	case "pin":
		pinned = 1
	case "unpin":
		pinned = 0
	case "veto":
		vetoed = 1
	case "unveto":
		vetoed = 0
	case "edit":
		editedRule = rv.EditedRule
	default:
		return pinned, vetoed, editedRule, fmt.Errorf("agentFeedbackReviewed: unknown action %q", rv.Action)
	}
	return pinned, vetoed, editedRule, nil
}

// applyAgentFeedbackPromoted records that a cluster crossed its recurrence
// threshold. The agentFeedbackPromoted event is appended exactly once (by the
// agentFeedback MCP tool when it detects threshold crossing); this projection
// inserts into agent_feedback_promotions on first application and is a
// do-nothing no-op on replay (ON CONFLICT DO NOTHING).
func (d *Dispatcher) applyAgentFeedbackPromoted(ctx context.Context, ev kernel.Event) (retErr error) {
	var p ontology.AgentFeedbackPromotedEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode agentFeedbackPromoted payload: %w", err)
	}
	if strings.TrimSpace(p.ClusterKey) == "" {
		return errors.New("agentFeedbackPromoted: clusterKey is required")
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	promoteTime := ev.CreateTime.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_feedback_promotions
			(cluster_key, rule, recurrence, severity, source_event_uid, promote_time)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cluster_key) DO NOTHING`,
		p.ClusterKey, p.Rule, p.Recurrence, p.Severity, ev.UID, promoteTime,
	); err != nil {
		return fmt.Errorf("exec insert agent feedback promotion: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyAgentFeedbackReviewed folds one review action into the per-cluster review
// state. It is read-modify-write inside a tx: the action mutates only its own
// dimension (pin → pinned, veto → vetoed, edit → edited_rule), leaving the
// others untouched, then the resolved state is upserted. Because the event log
// applies events in order, replaying converges to the same final state.
func (d *Dispatcher) applyAgentFeedbackReviewed(ctx context.Context, ev kernel.Event) (retErr error) {
	var rv ontology.AgentFeedbackReviewedEvent
	if err := json.Unmarshal(ev.Payload, &rv); err != nil {
		return fmt.Errorf("decode agentFeedbackReviewed payload: %w", err)
	}
	if strings.TrimSpace(rv.ClusterKey) == "" {
		return errEmptyReviewCluster
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Current state (defaults if the cluster has never been reviewed).
	var pinned, vetoed int
	var editedRule string
	row := tx.QueryRowContext(ctx,
		`SELECT pinned, vetoed, edited_rule FROM agent_feedback_reviews WHERE cluster_key = ?`,
		rv.ClusterKey)
	switch err := row.Scan(&pinned, &vetoed, &editedRule); {
	case errors.Is(err, sql.ErrNoRows):
		// leave zero/empty defaults
	case err != nil:
		return fmt.Errorf("read review state: %w", err)
	}

	pinned, vetoed, editedRule, err = applyReviewAction(rv, pinned, vetoed, editedRule)
	if err != nil {
		return err
	}

	reviewTime := ev.CreateTime.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_feedback_reviews
			(cluster_key, pinned, vetoed, edited_rule, last_action, source_event_uid, review_time)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cluster_key) DO UPDATE SET
			pinned           = excluded.pinned,
			vetoed           = excluded.vetoed,
			edited_rule      = excluded.edited_rule,
			last_action      = excluded.last_action,
			source_event_uid = excluded.source_event_uid,
			review_time      = excluded.review_time`,
		rv.ClusterKey, pinned, vetoed, editedRule, rv.Action, ev.UID, reviewTime,
	); err != nil {
		return fmt.Errorf("exec upsert agent feedback review: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
