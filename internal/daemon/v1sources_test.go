package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestV1HandlerListSources(t *testing.T) {
	t.Run("nil reader yields 503 while the daemon is starting", func(t *testing.T) {
		h := &v1Handler{logger: zap.NewNop()}
		rec := httptest.NewRecorder()
		h.listSources(rec, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		var resp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Error.Code != "UNAVAILABLE" {
			t.Errorf("error code = %q, want UNAVAILABLE", resp.Error.Code)
		}
	})
}
