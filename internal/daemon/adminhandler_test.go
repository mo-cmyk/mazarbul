package daemon_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// fakeEventLog is a minimal kernel.EventLog stub for handler tests.
// It records Append calls and returns configurable receipts or errors.
type fakeEventLog struct {
	appended  []kernel.Event
	receipts  []kernel.Receipt
	appendErr error
}

func (f *fakeEventLog) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	f.appended = append(f.appended, events...)
	if len(f.receipts) > 0 {
		return f.receipts, nil
	}
	out := make([]kernel.Receipt, len(events))
	for i := range events {
		uid := events[i].UID
		if uid == "" {
			uid = fmt.Sprintf("FAKE%02d", i)
		}
		out[i] = kernel.Receipt{UID: uid, CreateTime: time.Now().UTC()}
	}
	return out, nil
}

//nolint:gocritic // hugeParam: ReadOpts is by-value to satisfy kernel.EventLog interface.
func (f *fakeEventLog) Read(_ context.Context, _ kernel.ReadOpts) iter.Seq2[kernel.Event, error] {
	return func(yield func(kernel.Event, error) bool) {}
}

func (f *fakeEventLog) At(_ context.Context, _ string) (kernel.Event, error) {
	return kernel.Event{}, kernel.ErrNotFound
}

func (f *fakeEventLog) Rebuild(_ context.Context, _ kernel.Storage, _ ...kernel.RebuildOption) ([]kernel.ApplyError, error) {
	return nil, nil
}

func (f *fakeEventLog) Provenance(_ context.Context, _ string) ([]kernel.ProvenanceRecord, error) {
	return nil, nil
}

func (f *fakeEventLog) WaitForBackgroundWork(_ context.Context) error { return nil }

// newHandler builds an admin handler with fake deps. db may be nil for tests
// that do not exercise the check paths.
func newHandler(t *testing.T, log kernel.EventLog, db *sql.DB) http.Handler {
	t.Helper()
	h, err := daemon.NewAdminHandlerDirect(log, db, zap.NewNop())
	if err != nil {
		t.Fatalf("NewAdminHandlerDirect: %v", err)
	}
	return h
}

// newInMemDB creates a throw-away in-memory SQLite database with a minimal
// schema so materialization check queries can run.
func newInMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	stmts := []string{
		`CREATE TABLE entity_kinds (name TEXT PRIMARY KEY, display_name TEXT, same_as TEXT)`,
		`CREATE TABLE predicates  (predicate TEXT PRIMARY KEY)`,
		`CREATE TABLE presets     (name TEXT PRIMARY KEY)`,
	}
	ctx := context.Background()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup schema: %v", err)
		}
	}
	return db
}

// post sends a POST request to path on h with body marshalled as JSON.
func post(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAdminHandler_AppendEvents_HappyPath(t *testing.T) {
	t.Parallel()
	log := &fakeEventLog{}
	h := newHandler(t, log, nil)

	rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
		Events: []kernel.Event{
			{Kind: "episodeObserved", Source: "load", Scope: kernel.ScopePersonal, Confidence: 1.0},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp daemon.AppendEventsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Receipts) != 1 {
		t.Errorf("receipts = %d, want 1", len(resp.Receipts))
	}
	if resp.AlreadyPresent {
		t.Error("alreadyPresent should be false for a new append")
	}
	if len(log.appended) != 1 {
		t.Errorf("log.appended = %d, want 1", len(log.appended))
	}
}

func TestAdminHandler_AppendEvents_EmptyBatch(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeEventLog{}, nil)

	rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{Events: nil})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "empty") {
		t.Errorf("body should mention 'empty', got: %s", rr.Body.String())
	}
}

func TestAdminHandler_AppendEvents_NonPersonalScope(t *testing.T) {
	t.Parallel()
	for _, scope := range []kernel.Scope{kernel.ScopeTeam, kernel.ScopeProject, kernel.ScopeDraft} {
		t.Run(string(scope), func(t *testing.T) {
			t.Parallel()
			h := newHandler(t, &fakeEventLog{}, nil)
			rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
				Events: []kernel.Event{
					{Kind: "episodeObserved", Source: "load", Scope: scope, Confidence: 1.0},
				},
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("scope %q: status = %d, want 400", scope, rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "scope") {
				t.Errorf("body should mention 'scope', got: %s", rr.Body.String())
			}
		})
	}
}

// TestAdminHandler_AppendEvents_ArbitrarySourcePreserved pins the decision to
// NOT allowlist Source: the in-scope operations carry legitimate caller-set
// sources (preset install → "schemapack:<pack>"; load → a backup's original,
// arbitrary connector sources), so the handler must pass them through unchanged
// for parity with the in-process Append path. EventAuthor (re-signed by the
// kernel) is the trustworthy attribution; Source is informational.
func TestAdminHandler_AppendEvents_ArbitrarySourcePreserved(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"schemapack:notes-organizer", "granola", "claude.ai", "arbitrary:source", ""} {
		t.Run("source "+src, func(t *testing.T) {
			t.Parallel()
			h := newHandler(t, &fakeEventLog{}, nil)
			rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
				Events: []kernel.Event{
					{Kind: "episodeObserved", Source: src, Scope: kernel.ScopePersonal, Confidence: 1.0},
				},
			})
			if rr.Code != http.StatusOK {
				t.Errorf("source %q: status = %d, want 200 (sources are not allowlisted); body: %s", src, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAdminHandler_AppendEvents_OversizeBody(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeEventLog{}, nil)

	// Build a body larger than 512 KiB.
	large := make([]byte, 600*1024)
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/events:append", bytes.NewReader(large))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}

func TestAdminHandler_AppendEvents_ErrInvalidEvent(t *testing.T) {
	t.Parallel()
	log := &fakeEventLog{appendErr: fmt.Errorf("bad: %w", kernel.ErrInvalidEvent)}
	h := newHandler(t, log, nil)

	rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
		Events: []kernel.Event{
			{Kind: "episodeObserved", Source: "load", Scope: kernel.ScopePersonal, Confidence: 1.0},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAdminHandler_AppendEvents_NilLog(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil, nil)

	rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
		Events: []kernel.Event{
			{Kind: "episodeObserved", Source: "load", Scope: kernel.ScopePersonal, Confidence: 1.0},
		},
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestAdminHandler_AppendEvents_PrecheckAbsent_AlreadyPresent(t *testing.T) {
	t.Parallel()
	db := newInMemDB(t)
	// Pre-populate the row so the "absent" precheck fails (row IS present).
	if _, err := db.ExecContext(context.Background(), `INSERT INTO presets (name) VALUES ('my-preset')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	log := &fakeEventLog{}
	h := newHandler(t, log, db)

	// Precheck says "row should be absent before we append" — but it IS present,
	// so the handler returns alreadyPresent:true and skips the append.
	rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
		Events: []kernel.Event{
			{Kind: "presetInstalled", Source: "preset/install", Scope: kernel.ScopePersonal, Confidence: 1.0},
		},
		Precheck: []daemon.MatCheckWire{{
			Query:   "SELECT COUNT(1) FROM presets WHERE name = ?",
			Args:    []any{"my-preset"},
			Label:   "my-preset",
			Confirm: "absent",
		}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp daemon.AppendEventsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.AlreadyPresent {
		t.Error("expected alreadyPresent=true; precheck absent-check failed so append should be skipped")
	}
	if len(log.appended) != 0 {
		t.Errorf("expected 0 appended events, got %d", len(log.appended))
	}
}

func TestAdminHandler_AppendEvents_PrecheckAbsent_PassThrough(t *testing.T) {
	t.Parallel()
	db := newInMemDB(t)
	// Table is empty — the "absent" precheck passes, append proceeds.
	log := &fakeEventLog{}
	h := newHandler(t, log, db)

	rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
		Events: []kernel.Event{
			{Kind: "presetInstalled", Source: "preset/install", Scope: kernel.ScopePersonal, Confidence: 1.0},
		},
		Precheck: []daemon.MatCheckWire{{
			Query:   "SELECT COUNT(1) FROM presets WHERE name = ?",
			Args:    []any{"new-preset"},
			Label:   "new-preset",
			Confirm: "absent",
		}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp daemon.AppendEventsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AlreadyPresent {
		t.Error("expected alreadyPresent=false; row was absent so append should proceed")
	}
	if len(log.appended) != 1 {
		t.Errorf("expected 1 appended event, got %d", len(log.appended))
	}
}

func TestAdminHandler_AppendEvents_PostCheckFails_422(t *testing.T) {
	t.Parallel()
	db := newInMemDB(t)
	// The fake log appends the event to the JSONL but does NOT materialise it
	// into SQLite (fakeEventLog has no dispatcher). The post-check queries
	// SQLite and finds the row absent — simulating a materialisation failure.
	log := &fakeEventLog{}
	h := newHandler(t, log, db)

	rr := post(t, h, "/admin/v1/events:append", daemon.AppendEventsRequest{
		Events: []kernel.Event{
			{Kind: "presetInstalled", Source: "preset/install", Scope: kernel.ScopePersonal, Confidence: 1.0},
		},
		Checks: []daemon.MatCheckWire{{
			Query:   "SELECT COUNT(1) FROM presets WHERE name = ?",
			Args:    []any{"missing-preset"},
			Label:   "missing-preset",
			Confirm: "present",
		}},
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rebuild") {
		t.Errorf("body should mention rebuild hint, got: %s", rr.Body.String())
	}
}
