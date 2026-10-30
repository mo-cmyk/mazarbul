package navigate

import (
	"context"
	"encoding/json"
	"fmt"
)

// ListInsights returns insights in the given state, most-confident first - the
// "list every insight in this state" reader behind the Insight Inbox
// (`maz insights`). It complements the two existing insight reads without
// duplicating either: GetInsight fetches one row by uid, InsightsForSeeds
// returns the rows about a seed-entity set (the `maz self` mirror), and this
// returns the whole state-filtered set so the reviewer surface can page it.
//
// The state argument filters the insights.state column ('hot', 'demoted', or
// 'dismissed'); an empty state returns every state. The read is scope-isolated:
// the navigator's scope predicate is ANDed in so an empty scope set fails closed
// (#206), exactly like GetInsight and InsightsForSeeds. A non-positive limit
// applies defaultLimit.
func (n *Navigator) ListInsights(ctx context.Context, state string, limit int) ([]Insight, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	scope, scopeArgs := n.scopeCond("")
	// Build the WHERE clause: an optional state equality followed by the scope
	// predicate. Args are ordered to match: [state?] + scopeArgs + limit.
	stateCond := ""
	args := make([]any, 0, len(scopeArgs)+2)
	if state != "" {
		stateCond = "state = ? AND "
		args = append(args, state)
	}
	args = append(args, scopeArgs...)
	args = append(args, limit)
	//nolint:gosec // G202: stateCond is a constant fragment + bound ? param; scope is constant fragments + bound ? params; limit is a bound ?; every value is parameterized
	q := `SELECT uid, statement, insight_type, source_uids, seed_entity_uid,
	             synthesized_confidence, current_confidence, state, source_event_uid,
	             synthesized_at, create_time, scope
	      FROM insights
	      WHERE ` + stateCond + scope + `
	      ORDER BY current_confidence DESC, synthesized_at DESC, uid ASC
	      LIMIT ?`

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list insights: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Insight, 0, limit)
	for rows.Next() {
		var (
			ins       Insight
			sourceRaw string
		)
		if serr := rows.Scan(&ins.UID, &ins.Statement, &ins.InsightType, &sourceRaw,
			&ins.SeedEntityUID, &ins.SynthesizedConfidence, &ins.CurrentConfidence,
			&ins.State, &ins.SourceEventUID, &ins.SynthesizedAt, &ins.CreateTime, &ins.Scope); serr != nil {
			return nil, fmt.Errorf("scan insight: %w", serr)
		}
		if uerr := json.Unmarshal([]byte(sourceRaw), &ins.SourceUIDs); uerr != nil {
			// Non-fatal: return the row without citations rather than failing a read.
			ins.SourceUIDs = nil
		}
		ins.Name = "insights/" + ins.UID
		out = append(out, ins)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate insights: %w", rerr)
	}
	return out, nil
}
