package daemonclient

import (
	"context"
	"time"
)

// JobStateRunning is the kernel/jobs.State for an in-flight job. Defined
// here so consumers filter on a name rather than a bare literal; the
// watch dashboard shows only running jobs (terminal history is omitted).
const JobStateRunning = "running"

// Job kinds the watch dashboard distinguishes, mirroring the kernel +
// ingest producers (extraction worker, ingest orchestrator, rebuild).
const (
	JobKindExtraction = "extraction"
	JobKindIngest     = "ingest"
	JobKindRebuild    = "rebuild"
)

// Job mirrors the daemon's jobs.Job wire shape (GET /v1/jobs). Defined
// locally so the client stays decoupled from the kernel job registry,
// matching the package's convention for Entity / Episode / Relation.
type Job struct {
	UID       string    `json:"uid"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Progress  float64   `json:"progress"`
	Detail    string    `json:"detail,omitempty"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime,omitempty"`
	ErrMsg    string    `json:"errMsg,omitempty"`
}

// QueueDepth is a point-in-time background-drain snapshot.
type QueueDepth struct {
	InFlight int `json:"inFlight"`
	Queued   int `json:"queued"`
}

// QueueDepths groups the extraction and embedding background drains.
type QueueDepths struct {
	Extraction QueueDepth `json:"extraction"`
	Embedding  QueueDepth `json:"embedding"`
}

// JobsResponse is the decoded GET /v1/jobs payload.
type JobsResponse struct {
	Jobs   []Job       `json:"jobs"`
	Queues QueueDepths `json:"queues"`
}

// ListJobs calls GET /v1/jobs and returns the tracked job list plus the
// background-enrichment queue depths. The watch dashboard polls this to
// render the extraction-jobs and embedding-queue panes.
func (c *Client) ListJobs(ctx context.Context) (JobsResponse, error) {
	raw, err := c.Get(ctx, "/v1/jobs")
	if err != nil {
		return JobsResponse{}, err
	}
	var resp JobsResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return JobsResponse{}, err
	}
	return resp, nil
}
