package navigate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ReinforcedEntity is an entity ranked by passive-reinforcement strength: an
// entities row joined to its memory_strength accumulator (#231). AccessCount is
// the folded memoryAccessed set cardinality; LastAccessedAt is the MAX access
// instant (the same value the query-time recall decay anchors on). It is the
// read behind `maz self`'s "Reinforced lately" section.
type ReinforcedEntity struct {
	Entity
	AccessCount    int    `json:"accessCount"`
	LastAccessedAt string `json:"lastAccessedAt,omitempty"`
}

// ReinforcedEntities returns entities ordered by descending memory_strength
// access count - the entities the reader keeps coming back to. It inner-joins
// memory_strength to entities on target_uid, so only reinforced ENTITIES
// surface (episode / insight reinforcement targets fall out of the join), and
// threads the navigator's scope predicate on the entities side so an
// out-of-scope node never leaks (#206). Rows with a zero access count are
// omitted. The decay term is intentionally NOT applied here: this is the raw
// reinforcement ledger, not the recall ranking.
func (n *Navigator) ReinforcedEntities(ctx context.Context, limit int) ([]ReinforcedEntity, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	scope, scopeArgs := n.scopeCond("e")
	//nolint:gosec // G202: scope is constant fragments + bound ? params; limit is a bound ?; all values parameterized
	q := `SELECT e.uid, e.kind, e.display_name, e.confidence, e.resolution_status, e.create_time, e.properties,
	             ms.access_count, COALESCE(ms.last_accessed_at, '')
	      FROM memory_strength ms
	      JOIN entities e ON e.uid = ms.target_uid
	      WHERE ms.access_count > 0 AND ` + scope + `
	      ORDER BY ms.access_count DESC, ms.last_accessed_at DESC, e.uid ASC
	      LIMIT ?`
	args := append([]any{}, scopeArgs...)
	args = append(args, limit)

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("reinforced entities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ReinforcedEntity, 0, limit)
	for rows.Next() {
		var (
			re                    ReinforcedEntity
			createTime, propsJSON sql.NullString
		)
		if serr := rows.Scan(&re.UID, &re.Kind, &re.DisplayName, &re.Confidence,
			&re.ResolutionStatus, &createTime, &propsJSON,
			&re.AccessCount, &re.LastAccessedAt); serr != nil {
			return nil, fmt.Errorf("scan reinforced entity: %w", serr)
		}
		re.CreateTime = createTime.String
		if propsJSON.Valid && json.Valid([]byte(propsJSON.String)) {
			re.Properties = json.RawMessage(propsJSON.String)
		}
		out = append(out, re)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate reinforced entities: %w", rerr)
	}
	return out, nil
}

// InsightsForSeeds returns the insights synthesized about any of seedUIDs (their
// seed_entity_uid), most-confident first. Unlike recall - which surfaces only
// hot insights - it returns every state, mirroring GetInsight: the `maz self`
// mirror shows what was synthesized about the reader regardless of the decay
// sweep, and labels each row's state. The read is scope-isolated (#206); an
// empty seed set returns no rows (it never widens to every insight).
func (n *Navigator) InsightsForSeeds(ctx context.Context, seedUIDs []string, limit int) ([]Insight, error) {
	if len(seedUIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	scope, scopeArgs := n.scopeCond("")
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(seedUIDs)), ",")
	//nolint:gosec // G202: placeholders are "?" only; scope is constant fragments + bound ? params; all values parameterized
	q := `SELECT uid, statement, insight_type, source_uids, seed_entity_uid,
	             synthesized_confidence, current_confidence, state, source_event_uid,
	             synthesized_at, create_time, scope
	      FROM insights
	      WHERE seed_entity_uid IN (` + placeholders + `) AND ` + scope + `
	      ORDER BY current_confidence DESC, synthesized_at DESC, uid ASC
	      LIMIT ?`
	args := make([]any, 0, len(seedUIDs)+len(scopeArgs)+1)
	for _, s := range seedUIDs {
		args = append(args, s)
	}
	args = append(args, scopeArgs...)
	args = append(args, limit)

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("insights for seeds: %w", err)
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
