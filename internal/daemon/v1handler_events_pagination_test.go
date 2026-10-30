package daemon

// v1handler_events_pagination_test.go pins the contract that the
// /v1/events handler never emits the same UID twice across pages.
// Greptile P1 on PR #144: the page token encoded only CreateTime, and
// eventLog.Read treats Since as an INCLUSIVE lower bound, so the next
// request reads from the same instant and returns the cursor event a
// second time. With UID-tiebreak encoded in the cursor and a
// "strictly greater than" skip in the handler, pagination becomes
// monotonic again.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// pagingFakeOntology accepts any payload so the test can append plain
// `{"text":...}` events without registering a schema.
type pagingFakeOntology struct{}

func (pagingFakeOntology) Schema(_ context.Context, _ string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}

func (pagingFakeOntology) Register(_ context.Context, _ kernel.Schema) error {
	return kernel.ErrNotImplemented
}

func (pagingFakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}

func (pagingFakeOntology) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}

// TestListEvents_PaginationDoesNotRepeatUIDs asserts that paging
// through three events with pageSize=1 yields three DISTINCT UIDs.
// Before the fix, the second page would re-emit the first event
// because the page token encoded only CreateTime and Since is
// inclusive.
func TestListEvents_PaginationDoesNotRepeatUIDs(t *testing.T) {
	dataDir := t.TempDir()

	// Pin clock so all three events share a CreateTime — the
	// worst-case for tiebreaker logic. With a fixed clock the only
	// thing distinguishing the three events is their ULID, which is
	// exactly the cursor field Greptile said was missing.
	clock := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	log := kernel.NewFileEventLog(dataDir, pagingFakeOntology{}, kernel.WithClock(func() time.Time { return clock }))

	ctx := context.Background()
	events := []kernel.Event{
		{Kind: "remember", Source: "test", Confidence: 1, Payload: json.RawMessage(`{"text":"one"}`)},
		{Kind: "remember", Source: "test", Confidence: 1, Payload: json.RawMessage(`{"text":"two"}`)},
		{Kind: "remember", Source: "test", Confidence: 1, Payload: json.RawMessage(`{"text":"three"}`)},
	}
	if _, err := log.Append(ctx, events); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	h := &v1Handler{eventLog: log, logger: zap.NewNop()}

	// Walk three pages of size 1; collect UIDs.
	seen := make([]string, 0, 3)
	token := ""
	for page := range 4 { // one guard iteration past the last expected page
		req := httptest.NewRequest(http.MethodGet, "/v1/events?pageSize=1", nil)
		if token != "" {
			req = httptest.NewRequest(http.MethodGet, "/v1/events?pageSize=1&pageToken="+token, nil)
		}
		rr := httptest.NewRecorder()
		h.listEvents(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", page, rr.Code, rr.Body.String())
		}
		var resp struct {
			Items []struct {
				UID string `json:"uid"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d: decode: %v\nbody: %s", page, err, rr.Body.String())
		}
		if len(resp.Items) == 0 {
			break
		}
		for _, it := range resp.Items {
			seen = append(seen, it.UID)
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}

	if len(seen) != 3 {
		t.Fatalf("paginated UID count = %d, want 3; got %v", len(seen), seen)
	}
	uniq := map[string]struct{}{}
	for _, u := range seen {
		uniq[u] = struct{}{}
	}
	if len(uniq) != 3 {
		t.Errorf("pagination repeated UIDs: %v (uniques=%d)", seen, len(uniq))
	}
}
