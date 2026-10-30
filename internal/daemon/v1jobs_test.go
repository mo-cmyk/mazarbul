package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
)

func TestV1HandlerListJobs(t *testing.T) {
	t.Run("running registry jobs surface and queues default to zero", func(t *testing.T) {
		reg := jobs.NewRegistry()
		// Begin leaves the job in the running state (no tracker.Done()).
		_, _ = reg.Begin(context.Background(), "ingest", "ingest granola")

		h := &v1Handler{jobs: reg, logger: zap.NewNop()}
		rec := httptest.NewRecorder()
		h.listJobs(rec, httptest.NewRequest(http.MethodGet, "/v1/jobs", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp v1JobsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Jobs) != 1 {
			t.Fatalf("jobs len = %d, want 1: %+v", len(resp.Jobs), resp.Jobs)
		}
		if resp.Jobs[0].Kind != "ingest" || resp.Jobs[0].State != jobs.StateRunning {
			t.Errorf("unexpected job: %+v", resp.Jobs[0])
		}
		// No FileEventLog / Dispatcher wired ⇒ depths degrade to zero, not error.
		if resp.Queues.Extraction.InFlight != 0 || resp.Queues.Embedding.InFlight != 0 {
			t.Errorf("queues should be zero without storage: %+v", resp.Queues)
		}
	})

	t.Run("nil registry yields an empty job list not null", func(t *testing.T) {
		h := &v1Handler{logger: zap.NewNop()}
		rec := httptest.NewRecorder()
		h.listJobs(rec, httptest.NewRequest(http.MethodGet, "/v1/jobs", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp v1JobsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Jobs == nil {
			t.Error("Jobs must serialize as [] not null")
		}
		if len(resp.Jobs) != 0 {
			t.Errorf("want empty jobs, got %+v", resp.Jobs)
		}
	})
}
