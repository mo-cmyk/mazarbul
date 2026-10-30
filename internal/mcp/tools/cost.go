package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

const costBreakdownInputSchema = `{
  "type": "object",
  "properties": {
    "window": {
      "type": "string",
      "enum": ["today", "last7Days", "last30Days", "lifetime"],
      "description": "Aggregation window. Defaults to \"today\" when omitted. \"monthToDate\" is not yet supported — use \"last30Days\" as an approximation (matches the budget surface)."
    }
  },
  "additionalProperties": false
}`

const costBreakdownOutputSchema = `{
  "type": "object",
  "properties": {
    "window":   {"type": "string"},
    "totalUsd": {"type": "number", "minimum": 0},
    "byModel": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "provider":       {"type": "string"},
          "model":          {"type": "string"},
          "inputTokens":    {"type": "integer", "minimum": 0},
          "outputTokens":   {"type": "integer", "minimum": 0},
          "cacheReadTokens":   {"type": "integer", "minimum": 0},
          "cacheCreateTokens": {"type": "integer", "minimum": 0},
          "embedTokens":    {"type": "integer", "minimum": 0},
          "usd":            {"type": "number", "minimum": 0}
        },
        "required": ["provider", "model", "usd"],
        "additionalProperties": false
      }
    },
    "byOperation": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "op":  {"type": "string"},
          "usd": {"type": "number", "minimum": 0}
        },
        "required": ["op", "usd"],
        "additionalProperties": false
      }
    },
    "cacheAnalytics": {
      "type": "object",
      "properties": {
        "anthropicCacheHitRate": {"type": "number", "minimum": 0, "maximum": 1},
        "embedCacheHitRate":     {"type": "number", "minimum": 0, "maximum": 1}
      },
      "additionalProperties": false
    }
  },
  "required": ["window", "totalUsd"],
  "additionalProperties": false
}`

type costInput struct {
	Window string `json:"window,omitempty"`
}

type costOutput struct {
	Window         string        `json:"window"`
	TotalUSD       float64       `json:"totalUsd"`
	ByModel        []modelOut    `json:"byModel,omitempty"`
	ByOperation    []opOut       `json:"byOperation,omitempty"`
	CacheAnalytics cacheAnalyOut `json:"cacheAnalytics"`
}

type modelOut struct {
	Provider          string  `json:"provider"`
	Model             string  `json:"model"`
	InputTokens       int64   `json:"inputTokens"`
	OutputTokens      int64   `json:"outputTokens"`
	CacheReadTokens   int64   `json:"cacheReadTokens"`
	CacheCreateTokens int64   `json:"cacheCreateTokens"`
	EmbedTokens       int64   `json:"embedTokens"`
	USD               float64 `json:"usd"`
}

type opOut struct {
	Op  string  `json:"op"`
	USD float64 `json:"usd"`
}

type cacheAnalyOut struct {
	AnthropicCacheHitRate float64 `json:"anthropicCacheHitRate"`
	EmbedCacheHitRate     float64 `json:"embedCacheHitRate"`
}

// GetCostBreakdown is the MCP tool returning the per-window cost
// breakdown: total USD, per-model, per-operation, and cache analytics.
type GetCostBreakdown struct {
	store stats.Store
	table []stats.Price
}

// NewGetCostBreakdown constructs the tool. A nil store is tolerated
// and surfaces as an Internal error on invoke.
func NewGetCostBreakdown(s stats.Store, table []stats.Price) *GetCostBreakdown {
	if len(table) == 0 {
		table = stats.DefaultPrices()
	}
	return &GetCostBreakdown{store: s, table: table}
}

// Name implements mcp.Tool.
func (*GetCostBreakdown) Name() string { return "getCostBreakdown" }

// Description implements mcp.Tool.
func (*GetCostBreakdown) Description() string {
	return "Return the per-window token-spend breakdown: total USD, per-(provider,model) totals, per-operation totals, and cache analytics."
}

// InputSchema implements mcp.Tool.
func (*GetCostBreakdown) InputSchema() json.RawMessage {
	return json.RawMessage(costBreakdownInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*GetCostBreakdown) OutputSchema() json.RawMessage {
	return json.RawMessage(costBreakdownOutputSchema)
}

// Invoke implements mcp.Tool.
func (g *GetCostBreakdown) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	if g.store == nil {
		return nil, mcp.NewInternalError(errors.New("stats store not wired"))
	}
	var in costInput
	if len(req) > 0 {
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("decode arguments: %w", err))
		}
	}
	w, label := parseWindow(in.Window)
	snap, err := g.store.Snapshot(ctx, w)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("snapshot: %w", err))
	}
	cb := stats.ComputeCost(snap, g.table, label)

	out := costOutput{
		Window:   cb.Window,
		TotalUSD: cb.TotalUSD,
		CacheAnalytics: cacheAnalyOut{
			AnthropicCacheHitRate: cb.CacheAnalyt.AnthropicCacheHitRate,
			EmbedCacheHitRate:     cb.CacheAnalyt.EmbedCacheHitRate,
		},
	}
	for _, m := range cb.ByModel {
		out.ByModel = append(out.ByModel, modelOut{
			Provider:          m.Provider,
			Model:             m.Model,
			InputTokens:       m.InputTok,
			OutputTokens:      m.OutputTok,
			CacheReadTokens:   m.CacheReadTok,
			CacheCreateTokens: m.CacheCreateTok,
			EmbedTokens:       m.EmbedTok,
			USD:               m.USD,
		})
	}
	for _, o := range cb.ByOperation {
		out.ByOperation = append(out.ByOperation, opOut{Op: o.Op, USD: o.USD})
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}
