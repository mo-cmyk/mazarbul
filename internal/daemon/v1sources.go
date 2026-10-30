package daemon

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/ingest"
)

// v1SourcesResponse is the GET /v1/sources payload: the registered
// connectors plus the per-(connector, stream) resume watermarks. It
// mirrors the listSources MCP tool's output shape (internal/mcp/tools/
// list_sources.go) so the watch dashboard's sources pane renders the
// same data whether it reads the daemon REST surface or the MCP tool.
type v1SourcesResponse struct {
	Connectors []string                 `json:"connectors"`
	Watermarks []ingest.WatermarkStatus `json:"watermarks"`
}

// listSources implements GET /v1/sources. It is read-only: it reports the
// registered connectors (the static registry, always available) plus every
// persisted connector watermark from the SQLite projection. A nil reader
// yields 503 (daemon still starting), matching the other data endpoints.
func (h *v1Handler) listSources(w http.ResponseWriter, r *http.Request) {
	if !h.requireReader(w) {
		return
	}
	wms, err := ingest.NewSQLReader(h.reader).ListWatermarks(r.Context())
	if err != nil {
		h.logger.Warn("v1: list watermarks", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "query failed")
		return
	}
	if wms == nil {
		wms = []ingest.WatermarkStatus{}
	}
	h.writeJSON(w, v1SourcesResponse{
		Connectors: ingest.Connectors(),
		Watermarks: wms,
	})
}
