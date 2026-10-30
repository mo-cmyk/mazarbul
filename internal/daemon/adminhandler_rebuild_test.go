package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestAdminTriggerRebuildAccepted: POST /admin/v1/rebuild returns 202 with a
// jobUid when a coordinator is wired. (The spawned job fails fast against the
// empty Wired; the 202 is written before that, which is what we assert.)
func TestAdminTriggerRebuildAccepted(t *testing.T) {
	t.Parallel()
	coord := newRebuildCoordinator(context.Background(), zap.NewNop())
	h, err := NewAdminHandler(&Wired{}, coord, t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatalf("NewAdminHandler: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/v1/rebuild", strings.NewReader(`{"strict":false}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202; body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["jobUid"] == "" {
		t.Errorf("missing jobUid in 202 body: %s", rr.Body.String())
	}
	if resp["kind"] != "rebuild" {
		t.Errorf("kind = %q; want rebuild", resp["kind"])
	}
}

// TestAdminTriggerRebuildConflict: a second trigger while one runs returns 409.
func TestAdminTriggerRebuildConflict(t *testing.T) {
	t.Parallel()
	coord := newRebuildCoordinator(context.Background(), zap.NewNop())
	coord.running = true // simulate an in-flight rebuild
	h, err := NewAdminHandler(&Wired{}, coord, t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatalf("NewAdminHandler: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/v1/reindex", strings.NewReader(`{}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409", rr.Code)
	}
}

// TestAdminRebuildStatus: GET /admin/v1/rebuild returns the coordinator status.
func TestAdminRebuildStatus(t *testing.T) {
	t.Parallel()
	coord := newRebuildCoordinator(context.Background(), zap.NewNop())
	coord.status = testStatus("job-x", rebuildKindRebuild)
	h, err := NewAdminHandler(&Wired{}, coord, t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatalf("NewAdminHandler: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/v1/rebuild", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var st rebuildStatusView
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.JobUID != "job-x" || st.State != string(rebuildStateBuilding) {
		t.Fatalf("status = %+v; want job-x/building", st)
	}
}

// TestAdminRebuildRoutesAbsentWithoutCoordinator: the append-only constructor
// (no coordinator) does NOT register the rebuild routes — they 404.
func TestAdminRebuildRoutesAbsentWithoutCoordinator(t *testing.T) {
	t.Parallel()
	h, err := NewAdminHandlerDirect(nil, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("NewAdminHandlerDirect: %v", err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/v1/rebuild", strings.NewReader(`{}`)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (rebuild route absent without a coordinator)", rr.Code)
	}
}
