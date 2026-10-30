package tools

// Usage / capacity / top-entities MCP tools. These are read-only
// surfaces that mirror the data the Usage hub panel renders, so that
// MCP clients (Claude Code, Cursor, …) can ask Mazarbul "how big is my
// knowledge base?" without launching the TUI.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel/capacity"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

const (
	windowToday      = "today"
	windowLast7Days  = "last7Days"
	windowLast30Days = "last30Days"
	windowLifetime   = "lifetime"
)

// ---------- getUsageSnapshot ----------

const usageSnapshotInputSchema = `{
  "type": "object",
  "properties": {
    "window": {
      "type": "string",
      "enum": ["today", "last7Days", "last30Days", "lifetime"],
      "description": "Aggregation window. Defaults to \"today\" when omitted."
    }
  },
  "additionalProperties": false
}`

const usageSnapshotOutputSchema = `{
  "type": "object",
  "properties": {
    "window":              {"type": "string"},
    "events":              {"type": "integer", "minimum": 0},
    "bytesIngested":       {"type": "integer", "minimum": 0},
    "recalls":             {"type": "integer", "minimum": 0},
    "recallLatencyP50Ms":  {"type": "number", "minimum": 0},
    "recallLatencyP95Ms":  {"type": "number", "minimum": 0},
    "recallLatencyP99Ms":  {"type": "number", "minimum": 0},
    "embedCacheHits":      {"type": "integer", "minimum": 0},
    "embedCacheMisses":    {"type": "integer", "minimum": 0},
    "embedCacheHitRate":   {"type": "number", "minimum": 0, "maximum": 1}
  },
  "required": ["window", "events", "recalls"],
  "additionalProperties": false
}`

type usageSnapshotInput struct {
	Window string `json:"window,omitempty"`
}

type usageSnapshotOutput struct {
	Window             string  `json:"window"`
	Events             int64   `json:"events"`
	BytesIngested      int64   `json:"bytesIngested"`
	Recalls            int64   `json:"recalls"`
	RecallLatencyP50Ms float64 `json:"recallLatencyP50Ms"`
	RecallLatencyP95Ms float64 `json:"recallLatencyP95Ms"`
	RecallLatencyP99Ms float64 `json:"recallLatencyP99Ms"`
	EmbedCacheHits     int64   `json:"embedCacheHits"`
	EmbedCacheMisses   int64   `json:"embedCacheMisses"`
	EmbedCacheHitRate  float64 `json:"embedCacheHitRate"`
}

// GetUsageSnapshot is the MCP tool returning today's (or the requested
// window's) counters from the in-process stats store.
type GetUsageSnapshot struct{ store stats.Store }

// NewGetUsageSnapshot constructs the tool. A nil store is tolerated and
// surfaces as an Internal error on invoke — the daemon wires a real
// store in `internal/daemon/wiring.go`.
func NewGetUsageSnapshot(s stats.Store) *GetUsageSnapshot { return &GetUsageSnapshot{store: s} }

// Name implements mcp.Tool.
func (*GetUsageSnapshot) Name() string { return "getUsageSnapshot" }

// Description implements mcp.Tool.
func (*GetUsageSnapshot) Description() string {
	return "Return the current usage snapshot: events written, recalls performed, embed-cache hit rate, recall latency percentiles."
}

// InputSchema implements mcp.Tool.
func (*GetUsageSnapshot) InputSchema() json.RawMessage {
	return json.RawMessage(usageSnapshotInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*GetUsageSnapshot) OutputSchema() json.RawMessage {
	return json.RawMessage(usageSnapshotOutputSchema)
}

// Invoke implements mcp.Tool.
func (g *GetUsageSnapshot) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	if g.store == nil {
		return nil, mcp.NewInternalError(errors.New("stats store not wired"))
	}
	var in usageSnapshotInput
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
	out := usageSnapshotOutput{
		Window:             label,
		Events:             snap.Events,
		BytesIngested:      snap.BytesIngested,
		Recalls:            snap.Recalls,
		RecallLatencyP50Ms: snap.RecallLatencyP50Ms,
		RecallLatencyP95Ms: snap.RecallLatencyP95Ms,
		RecallLatencyP99Ms: snap.RecallLatencyP99Ms,
		EmbedCacheHits:     snap.EmbedCacheHits,
		EmbedCacheMisses:   snap.EmbedCacheMisses,
		EmbedCacheHitRate:  snap.EmbedCacheHitRate,
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}

// ---------- getCapacity ----------

const capacityInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const capacityOutputSchema = `{
  "type": "object",
  "properties": {
    "onDisk": {
      "type": "object",
      "properties": {
        "eventLog":   {"type": "integer", "minimum": 0},
        "sqliteFts":  {"type": "integer", "minimum": 0},
        "graph":      {"type": "integer", "minimum": 0},
        "vector":     {"type": "integer", "minimum": 0},
        "embedCache": {"type": "integer", "minimum": 0},
        "total":      {"type": "integer", "minimum": 0},
        "freeDisk":   {"type": "integer", "minimum": 0}
      },
      "required": ["total"],
      "additionalProperties": false
    }
  },
  "required": ["onDisk"],
  "additionalProperties": false
}`

type capacityOutput struct {
	OnDisk capacityOnDiskOut `json:"onDisk"`
}

type capacityOnDiskOut struct {
	EventLog   int64 `json:"eventLog"`
	SQLiteFTS  int64 `json:"sqliteFts"`
	Graph      int64 `json:"graph"`
	Vector     int64 `json:"vector"`
	EmbedCache int64 `json:"embedCache"`
	Total      int64 `json:"total"`
	FreeDisk   int64 `json:"freeDisk"`
}

// GetCapacity is the MCP tool returning on-disk byte totals per
// storage layer.
type GetCapacity struct{ rep capacity.Reporter }

// NewGetCapacity constructs the tool.
func NewGetCapacity(r capacity.Reporter) *GetCapacity { return &GetCapacity{rep: r} }

// Name implements mcp.Tool.
func (*GetCapacity) Name() string { return "getCapacity" }

// Description implements mcp.Tool.
func (*GetCapacity) Description() string {
	return "Return per-storage-layer on-disk byte totals: event log, sqlite+fts, graph, vector index, embedding cache, plus free disk."
}

// InputSchema implements mcp.Tool.
func (*GetCapacity) InputSchema() json.RawMessage { return json.RawMessage(capacityInputSchema) }

// OutputSchema implements mcp.Tool.
func (*GetCapacity) OutputSchema() json.RawMessage { return json.RawMessage(capacityOutputSchema) }

// Invoke implements mcp.Tool.
func (g *GetCapacity) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if g.rep == nil {
		return nil, mcp.NewInternalError(errors.New("capacity reporter not wired"))
	}
	rep, err := g.rep.Report(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("report: %w", err))
	}
	out := capacityOutput{OnDisk: capacityOnDiskOut{
		EventLog:   rep.OnDisk.EventLog,
		SQLiteFTS:  rep.OnDisk.SQLiteFTS,
		Graph:      rep.OnDisk.Graph,
		Vector:     rep.OnDisk.Vector,
		EmbedCache: rep.OnDisk.EmbedCache,
		Total:      rep.OnDisk.Total,
		FreeDisk:   rep.OnDisk.FreeDisk,
	}}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}

// parseWindow maps the wire-level window string to a stats.Window enum
// + a canonical label echoed back in the response.
func parseWindow(s string) (stats.Window, string) {
	switch s {
	case windowLast7Days:
		return stats.Last7Days, windowLast7Days
	case windowLast30Days:
		return stats.Last30Days, windowLast30Days
	case windowLifetime:
		return stats.Lifetime, windowLifetime
	default:
		return stats.Today, windowToday
	}
}
