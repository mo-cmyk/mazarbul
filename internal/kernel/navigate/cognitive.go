package navigate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Insight is a synthesized reflection row read off the insights table. It is a
// derived fact produced by the cognitive reflection pass and cites the entity
// source records it was synthesized from.
type Insight struct {
	UID                   string   `json:"uid"`
	Name                  string   `json:"name"` // insights/{uid}
	Statement             string   `json:"statement"`
	InsightType           string   `json:"insightType"`
	SourceUIDs            []string `json:"sourceUids"`
	SeedEntityUID         string   `json:"seedEntityUid"`
	SynthesizedConfidence float64  `json:"synthesizedConfidence"`
	CurrentConfidence     float64  `json:"currentConfidence"`
	State                 string   `json:"state"` // hot | demoted
	SourceEventUID        string   `json:"sourceEventUid"`
	SynthesizedAt         string   `json:"synthesizedAt"`
	CreateTime            string   `json:"createTime"`
	Scope                 string   `json:"scope"`
}

// ConsolidatedMemory is one consolidated-memory row read off the
// consolidated_memories table. It is a derived fact produced by the cognitive
// consolidation pass and cites the episode source records it was compressed from.
type ConsolidatedMemory struct {
	UID            string   `json:"uid"`
	Name           string   `json:"name"` // consolidatedMemories/{uid}
	Summary        string   `json:"summary"`
	MemoryType     string   `json:"memoryType"`
	SourceUIDs     []string `json:"sourceUids"`
	CitedEntityUID string   `json:"citedEntityUid"`
	Confidence     float64  `json:"confidence"`
	State          string   `json:"state"` // hot | demoted
	SourceEventUID string   `json:"sourceEventUid"`
	ConsolidatedAt string   `json:"consolidatedAt"`
	CreateTime     string   `json:"createTime"`
	Scope          string   `json:"scope"`
}

// GetInsight returns the Insight with uid, or ErrNotFound. The read is
// scope-isolated: the navigator's scope predicate is ANDed into the query so an
// empty scope set fails closed (#206), exactly like GetFact and GetSourceRef.
// A demoted insight (state='demoted') is still returned — get is not the hot-
// recall index, so every fetchable row is accessible regardless of state.
func (n *Navigator) GetInsight(ctx context.Context, uid string) (Insight, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT uid, statement, insight_type, source_uids,
	             seed_entity_uid, synthesized_confidence, current_confidence,
	             state, source_event_uid, synthesized_at, create_time, scope
	      FROM insights WHERE uid = ? AND ` + scope
	var (
		ins       Insight
		sourceRaw string
	)
	err := n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...).Scan(
		&ins.UID, &ins.Statement, &ins.InsightType, &sourceRaw,
		&ins.SeedEntityUID, &ins.SynthesizedConfidence, &ins.CurrentConfidence,
		&ins.State, &ins.SourceEventUID, &ins.SynthesizedAt, &ins.CreateTime, &ins.Scope,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Insight{}, fmt.Errorf("insight %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return Insight{}, fmt.Errorf("get insight %q: %w", uid, err)
	}
	if uerr := json.Unmarshal([]byte(sourceRaw), &ins.SourceUIDs); uerr != nil {
		// Non-fatal: return the row without citations rather than failing a read.
		ins.SourceUIDs = nil
	}
	ins.Name = "insights/" + ins.UID
	return ins, nil
}

// HasHotInsightForSeed reports whether the insights table already holds at least
// one row with seed_entity_uid == seedUID and state == 'hot'. The check is
// scope-isolated: the navigator's scope predicate is ANDed in, so an empty
// scope set fails closed (#206) and returns false (no hot insight visible).
//
// A false return means "proceed with synthesis"; a true return means "skip this
// seed — it is already covered by a live insight". On a real query failure the
// caller should log and proceed (treat as false) so the reflection pass degrades
// safely rather than stopping.
func (n *Navigator) HasHotInsightForSeed(ctx context.Context, seedUID string) (bool, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT 1 FROM insights WHERE seed_entity_uid = ? AND state = 'hot' AND ` + scope + ` LIMIT 1`
	var dummy int
	err := n.reader.QueryRowContext(ctx, q, append([]any{seedUID}, scopeArgs...)...).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("has hot insight for seed %q: %w", seedUID, err)
	}
	return true, nil
}

// GetConsolidatedMemory returns the ConsolidatedMemory with uid, or ErrNotFound.
// The read is scope-isolated: the navigator's scope predicate is ANDed into the
// query so an empty scope set fails closed (#206), exactly like GetInsight.
func (n *Navigator) GetConsolidatedMemory(ctx context.Context, uid string) (ConsolidatedMemory, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT uid, summary, memory_type, source_uids,
	             cited_entity_uid, confidence, state, source_event_uid,
	             consolidated_at, create_time, scope
	      FROM consolidated_memories WHERE uid = ? AND ` + scope
	var (
		cm        ConsolidatedMemory
		sourceRaw string
	)
	err := n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...).Scan(
		&cm.UID, &cm.Summary, &cm.MemoryType, &sourceRaw,
		&cm.CitedEntityUID, &cm.Confidence, &cm.State, &cm.SourceEventUID,
		&cm.ConsolidatedAt, &cm.CreateTime, &cm.Scope,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsolidatedMemory{}, fmt.Errorf("consolidated memory %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return ConsolidatedMemory{}, fmt.Errorf("get consolidated memory %q: %w", uid, err)
	}
	if uerr := json.Unmarshal([]byte(sourceRaw), &cm.SourceUIDs); uerr != nil {
		cm.SourceUIDs = nil
	}
	cm.Name = "consolidatedMemories/" + cm.UID
	return cm, nil
}
