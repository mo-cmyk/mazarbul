package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// WatermarkLister reads the persisted connector watermarks. *ingest.SQLReader
// satisfies it; tests pass a fake.
type WatermarkLister interface {
	ListWatermarks(ctx context.Context) ([]ingest.WatermarkStatus, error)
}

const listSourcesOutputSchema = `{
  "type": "object",
  "properties": {
    "connectors": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Registered connector names available to connectSource/ingestSource."
    },
    "watermarks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "connector":   {"type": "string"},
          "stream":      {"type": "string"},
          "sequence":    {"type": "integer", "description": "Items ingested through this stream so far."},
          "advanceTime": {"type": "string", "description": "When the watermark last advanced (RFC 3339)."},
          "status":      {"type": "string", "enum": ["live","stale","down"], "description": "Freshness: live (advanced within a day), stale (over a day), down (over a week, or never advanced)."},
          "ageSeconds":  {"type": "integer", "description": "Seconds since the watermark last advanced; -1 when it never advanced."}
        }
      },
      "description": "Per-(connector, stream) resume state for sources connected so far, with a derived freshness status."
    }
  },
  "required": ["connectors", "watermarks"],
  "additionalProperties": false
}`

// ListSources reports the registered connectors and the resume watermark of each
// connected (connector, stream). Read-only.
type ListSources struct {
	lister WatermarkLister
}

// NewListSources builds the listSources tool over a watermark lister.
func NewListSources(lister WatermarkLister) *ListSources { return &ListSources{lister: lister} }

// Name implements mcp.Tool.
func (*ListSources) Name() string { return "listSources" }

// Description implements mcp.Tool.
func (*ListSources) Description() string {
	return "List the external source connectors available to ingest from (e.g. local-git) and the resume watermark of each source connected so far (connector, stream, items ingested, last advance time). Read-only."
}

// InputSchema implements mcp.Tool. ListSources takes no arguments.
func (*ListSources) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// OutputSchema implements mcp.Tool.
func (*ListSources) OutputSchema() json.RawMessage { return json.RawMessage(listSourcesOutputSchema) }

// watermarkStatusWire is a watermark row augmented with its derived freshness
// (computed at read time from the advance time), so scripts and agents get the
// stale/down classification without re-deriving the thresholds.
type watermarkStatusWire struct {
	ingest.WatermarkStatus
	Status     string `json:"status"`
	AgeSeconds int64  `json:"ageSeconds"`
}

type listSourcesOutput struct {
	Connectors []string              `json:"connectors"`
	Watermarks []watermarkStatusWire `json:"watermarks"`
}

// Invoke implements mcp.Tool.
func (l *ListSources) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	wms, err := l.lister.ListWatermarks(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("list watermarks: %w", err))
	}
	now := time.Now()
	rows := make([]watermarkStatusWire, 0, len(wms))
	for _, wm := range wms {
		status, age := ingest.ClassifyWatermark(wm.AdvanceTime, now)
		rows = append(rows, watermarkStatusWire{WatermarkStatus: wm, Status: status, AgeSeconds: age})
	}
	body, err := json.Marshal(listSourcesOutput{
		Connectors: ingest.Connectors(),
		Watermarks: rows,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}
