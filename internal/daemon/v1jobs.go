package daemon

import (
	"net/http"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
)

// v1JobsResponse is the GET /v1/jobs payload. Jobs are the registry's
// tracked runs (ingest connectors + rebuild). Queues carries the
// background-enrichment depths that never register as tracked jobs: the
// daemon runs no extraction worker pool, so background extraction +
// resolution spill onto detached goroutines past the JobStarter and are
// observable only through the eventlog/dispatcher depth counters. The
// watch dashboard surfaces those as the "background drain" gauge.
type v1JobsResponse struct {
	Jobs   []jobs.Job    `json:"jobs"`
	Queues v1QueueDepths `json:"queues"`
}

// v1QueueDepths groups the two background drains the dashboard tracks.
type v1QueueDepths struct {
	// Extraction is the episode→entity extraction + resolution drain.
	Extraction v1Depth `json:"extraction"`
	// Embedding is the dispatcher's dense-vector upsert drain.
	Embedding v1Depth `json:"embedding"`
}

// v1Depth is a point-in-time queue snapshot. Queued is always <= InFlight
// (queued work is a subset of in-flight); it is zero whenever no bounded
// worker pool is configured, which is the daemon default.
type v1Depth struct {
	InFlight int `json:"inFlight"`
	Queued   int `json:"queued"`
}

// listJobs implements GET /v1/jobs. It is read-only and fail-soft: a nil
// registry yields an empty job list, and depth counters degrade to zero
// when the storage layer is not the production FileEventLog/Dispatcher
// (e.g. in tests) rather than erroring.
func (h *v1Handler) listJobs(w http.ResponseWriter, _ *http.Request) {
	resp := v1JobsResponse{Jobs: []jobs.Job{}}
	if h.jobs != nil {
		resp.Jobs = h.jobs.List()
	}
	if fl, ok := h.eventLog.(*kernel.FileEventLog); ok {
		resp.Queues.Extraction.InFlight = int(fl.BackgroundWorkDepth())
		resp.Queues.Extraction.Queued = fl.ExtractionQueueDepth()
	}
	if h.vec != nil {
		resp.Queues.Embedding.InFlight = int(h.vec.BackgroundVecDepth())
	}
	h.writeJSON(w, resp)
}
