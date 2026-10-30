package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// DupCandidateQuerier is the read surface needed by ListDupCandidates.
// Satisfied by *sql.DB and the dispatcher's read-only pool.
type DupCandidateQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const listDupCandidatesOutputSchema = `{
  "type": "object",
  "properties": {
    "candidates": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "entityUidA":  {"type": "string", "description": "Lexicographically smaller (older) entity UID."},
          "entityUidB":  {"type": "string", "description": "Lexicographically larger (newer) entity UID."},
          "reason":      {"type": "string", "enum": ["exact_name", "name_emb"], "description": "Detection lane: exact_name = same kind + case-insensitive display name; name_emb = embedding cosine similarity above advisory threshold."},
          "score":       {"type": "number", "description": "Similarity score (1.0 for exact_name, cosine similarity for name_emb)."},
          "detectedAt":  {"type": "string", "description": "RFC 3339 timestamp when the sweep recorded this pair."}
        },
        "required": ["entityUidA", "entityUidB", "reason", "score", "detectedAt"],
        "additionalProperties": false
      },
      "description": "Entity pairs flagged by the post-consume dedup sweep after cross-device sync. Auto-merged strong-identifier pairs (email, username) do not appear here — they are already consolidated."
    },
    "total": {"type": "integer", "description": "Total candidate count (may exceed the 200-row page)."}
  },
  "required": ["candidates", "total"],
  "additionalProperties": false
}`

// dupCandidateItem is one row in the listDupCandidates response.
type dupCandidateItem struct {
	EntityUIDA string  `json:"entityUidA"`
	EntityUIDB string  `json:"entityUidB"`
	Reason     string  `json:"reason"`
	Score      float64 `json:"score"`
	DetectedAt string  `json:"detectedAt"`
}

type listDupCandidatesOutput struct {
	Candidates []dupCandidateItem `json:"candidates"`
	Total      int                `json:"total"`
}

// ListDupCandidates is a read-only MCP tool that surfaces the
// entity_dup_candidates table populated by the post-consume dedup sweep.
// Agents use it to discover cross-device duplicate entities and then call
// mergeEntities to consolidate them; strong-identifier pairs are already
// auto-merged by the sweep and do not appear here.
type ListDupCandidates struct {
	q DupCandidateQuerier
}

// NewListDupCandidates builds the tool bound to querier q.
func NewListDupCandidates(q DupCandidateQuerier) *ListDupCandidates {
	return &ListDupCandidates{q: q}
}

// Name implements mcp.Tool.
func (*ListDupCandidates) Name() string { return "listDupCandidates" }

// Description implements mcp.Tool.
func (*ListDupCandidates) Description() string {
	return "List entity pairs the post-consume dedup sweep flagged as likely cross-device duplicates. Auto-merged strong-identifier pairs (same email / username) are not included — only advisory candidates that need human review. Call mergeEntities on a pair to consolidate; use maz review dups for the interactive CLI view."
}

// InputSchema implements mcp.Tool. No arguments accepted.
func (*ListDupCandidates) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// OutputSchema implements mcp.Tool.
func (*ListDupCandidates) OutputSchema() json.RawMessage {
	return json.RawMessage(listDupCandidatesOutputSchema)
}

// Invoke queries entity_dup_candidates and returns the result. It degrades
// gracefully when the table does not yet exist (the sweep has not run) by
// returning an empty candidate list rather than an error.
func (t *ListDupCandidates) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	rows, err := t.q.QueryContext(ctx,
		`SELECT entity_uid_a, entity_uid_b, reason, score, detected_at
		   FROM entity_dup_candidates
		  ORDER BY score DESC, detected_at DESC
		  LIMIT 200`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return marshalListDupOutput(listDupCandidatesOutput{
				Candidates: []dupCandidateItem{},
				Total:      0,
			})
		}
		return nil, mcp.NewInternalError(fmt.Errorf("query dup candidates: %w", err))
	}
	defer func() { _ = rows.Close() }()

	var items []dupCandidateItem
	for rows.Next() {
		var r dupCandidateItem
		if serr := rows.Scan(&r.EntityUIDA, &r.EntityUIDB, &r.Reason, &r.Score, &r.DetectedAt); serr != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("scan dup candidate: %w", serr))
		}
		items = append(items, r)
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("iterate dup candidates: %w", iterErr))
	}

	if items == nil {
		items = []dupCandidateItem{}
	}
	return marshalListDupOutput(listDupCandidatesOutput{
		Candidates: items,
		Total:      len(items),
	})
}

func marshalListDupOutput(out listDupCandidatesOutput) (json.RawMessage, error) {
	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal dup candidates: %w", err))
	}
	return body, nil
}
