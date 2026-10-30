package daemonclient

import "context"

// SourceWatermark mirrors the daemon's ingest.WatermarkStatus wire shape
// (GET /v1/sources). Defined locally so the client stays decoupled from the
// ingest package, matching the package's convention for Job / Entity /
// Episode. AdvanceTime is the RFC3339Nano instant the watermark last
// advanced, empty when it has never advanced.
type SourceWatermark struct {
	Connector   string `json:"connector"`
	Stream      string `json:"stream"`
	Sequence    uint64 `json:"sequence"`
	AdvanceTime string `json:"advanceTime,omitempty"`
}

// SourcesResponse is the decoded GET /v1/sources payload: the registered
// connectors plus the per-(connector, stream) resume watermarks.
type SourcesResponse struct {
	Connectors []string          `json:"connectors"`
	Watermarks []SourceWatermark `json:"watermarks"`
}

// ListSources calls GET /v1/sources and returns the registered connectors
// plus each connected source's resume watermark. The watch dashboard polls
// this to render the sources pane against a live daemon.
func (c *Client) ListSources(ctx context.Context) (SourcesResponse, error) {
	raw, err := c.Get(ctx, "/v1/sources")
	if err != nil {
		return SourcesResponse{}, err
	}
	var resp SourcesResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return SourcesResponse{}, err
	}
	return resp, nil
}
