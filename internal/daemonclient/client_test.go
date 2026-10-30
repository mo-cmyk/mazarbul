package daemonclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/daemonclient"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// newUnixTestServer creates an httptest-style server that listens on a Unix
// socket at socketPath. The caller must close the returned *httptest.Server
// when done. This helper exists because httptest.NewServer only binds TCP.
func newUnixTestServer(t *testing.T, socketPath string, h http.Handler) *httptest.Server {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", socketPath, err)
	}
	srv := &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: h},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// newTestClient builds a Client pointed at srv using plain TCP (no Unix socket).
func newTestClient(srv *httptest.Server) *daemonclient.Client {
	return daemonclient.NewWithHTTP(strings.TrimPrefix(srv.URL, "http://"))
}

func TestListEntities(t *testing.T) {
	t.Parallel()
	want := daemonclient.ListEntitiesResponse{
		Items: []daemonclient.Entity{
			{UID: "01ABC", Kind: "person", DisplayName: "Merry"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/entities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(want); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListEntities(context.Background(), daemonclient.ListEntitiesRequest{})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].UID != "01ABC" {
		t.Errorf("got %+v, want items with UID=01ABC", got)
	}
}

func TestListEntities_KindFilter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		if kind != "organization" {
			http.Error(w, "unexpected kind", http.StatusBadRequest)
			return
		}
		resp := daemonclient.ListEntitiesResponse{
			Items: []daemonclient.Entity{{UID: "01ORG", Kind: "organization", DisplayName: "Mithril"}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListEntities(context.Background(), daemonclient.ListEntitiesRequest{Kind: "organization"})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Kind != "organization" {
		t.Errorf("got %+v, want organization", got)
	}
}

func TestGetEntity(t *testing.T) {
	t.Parallel()
	want := daemonclient.Entity{UID: "01XYZ", Kind: "person", DisplayName: "Sam"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/entities/01XYZ" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.GetEntity(context.Background(), "01XYZ")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.DisplayName != "Sam" {
		t.Errorf("got %+v, want DisplayName=Sam", got)
	}
}

func TestListRelations(t *testing.T) {
	t.Parallel()
	want := daemonclient.ListRelationsResponse{
		Items: []daemonclient.Relation{
			{UID: "01REL", SubjectUID: "01A", Predicate: "KNOWS", ObjectUID: "01B"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/relations" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListRelations(context.Background(), daemonclient.ListRelationsRequest{})
	if err != nil {
		t.Fatalf("ListRelations: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Predicate != "KNOWS" {
		t.Errorf("got %+v, want KNOWS relation", got)
	}
}

func TestListEpisodes(t *testing.T) {
	t.Parallel()
	want := daemonclient.ListEpisodesResponse{
		Items: []daemonclient.Episode{
			{UID: "01EP", Source: "user", Text: "hello world"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/episodes" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListEpisodes(context.Background(), daemonclient.ListEpisodesRequest{})
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Text != "hello world" {
		t.Errorf("got %+v", got)
	}
}

func TestListEvents(t *testing.T) {
	t.Parallel()
	want := daemonclient.ListEventsResponse{
		Items: []daemonclient.EventRecord{
			{UID: "01EV", Kind: "episode", Source: "user", CreateTime: time.Now().UTC()},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListEvents(context.Background(), daemonclient.ListEventsRequest{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].UID != "01EV" {
		t.Errorf("got %+v", got)
	}
}

func TestStreamEvents(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events:stream" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not implement Flusher")
			return
		}
		ev := daemonclient.EventRecord{UID: "01STREAM", Kind: "episode"}
		data, _ := json.Marshal(ev)
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		flusher.Flush()
	}))
	defer srv.Close()

	c := newTestClient(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, cancelStream, err := c.StreamEvents(ctx, daemonclient.StreamEventsFilter{})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	defer cancelStream()

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before receiving event")
		}
		if ev.UID != "01STREAM" {
			t.Errorf("got UID=%q, want 01STREAM", ev.UID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"NOT_FOUND"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetEntity(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var httpErr *daemonclient.HTTPError
	if ok := isHTTPError(err, &httpErr); !ok {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusNotFound {
		t.Errorf("got status %d, want 404", httpErr.StatusCode)
	}
}

// isHTTPError is a helper that uses errors.As to unwrap daemonclient.HTTPError.
func isHTTPError(err error, target **daemonclient.HTTPError) bool {
	return errors.As(err, target)
}

func TestAppendEvents_RoundTrip(t *testing.T) {
	t.Parallel()
	want := daemon.AppendEventsResponse{
		Receipts: []daemon.ReceiptWire{{UID: "01ABC"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/v1/events:append" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	// NewWithHTTP disables Unix socket; AppendEvents must refuse (unix-only).
	c := newTestClient(srv)
	_, err := c.AppendEvents(context.Background(), &daemon.AppendEventsRequest{
		Events: []kernel.Event{
			{Kind: "episodeObserved", Source: "load"},
		},
	})
	// TCP-only client cannot reach the admin surface.
	if err == nil {
		t.Fatal("AppendEvents via TCP-only client should fail, got nil error")
	}
	if !strings.Contains(err.Error(), "Unix socket") && !strings.Contains(err.Error(), "unix") {
		t.Errorf("error should mention unix socket, got: %v", err)
	}
}

func TestAppendEvents_422_TypedError(t *testing.T) {
	t.Parallel()
	// Build a client that dials a real Unix socket so doUnixOnly can be
	// exercised. We point it at a temp socket served by httptest.
	socketPath := t.TempDir() + "/a.sock"
	srv := newUnixTestServer(t, socketPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "FAILED_PRECONDITION", "message": "committed but did not materialise"},
		})
	}))
	defer srv.Close()

	c, err := daemonclient.NewWithSocket(socketPath)
	if err != nil {
		t.Fatalf("NewWithSocket: %v", err)
	}
	_, aerr := c.AppendEvents(context.Background(), &daemon.AppendEventsRequest{
		Events: []kernel.Event{{Kind: "x", Source: "load"}},
	})
	if aerr == nil {
		t.Fatal("expected error from 422 response, got nil")
	}
	var httpErr *daemonclient.HTTPError
	if !errors.As(aerr, &httpErr) {
		t.Fatalf("error is %T, want *daemonclient.HTTPError", aerr)
	}
	if httpErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode = %d, want 422", httpErr.StatusCode)
	}
}

func TestAppendEvents_NoTCPFallback(t *testing.T) {
	t.Parallel()
	// A Client built with a non-existent socket path must NOT fall back to TCP.
	// We verify this by pointing the TCP fallback at a server that would return
	// 200 — if we see 200 back, fallback happened.
	tcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(daemon.AppendEventsResponse{
			Receipts: []daemon.ReceiptWire{{UID: "FALLBACK"}},
		})
	}))
	defer tcpSrv.Close()

	addr := strings.TrimPrefix(tcpSrv.URL, "http://")
	c, err := daemonclient.NewWithSocketAndHTTP("/nonexistent/socket.sock", addr)
	if err != nil {
		t.Fatalf("NewWithSocketAndHTTP: %v", err)
	}
	_, aerr := c.AppendEvents(context.Background(), &daemon.AppendEventsRequest{
		Events: []kernel.Event{{Kind: "episodeObserved", Source: "load"}},
	})
	if aerr == nil {
		t.Fatal("AppendEvents should fail on socket dial error (no TCP fallback), got nil")
	}
	// Success would mean the TCP fallback fired — that is the bug we're guarding.
	if strings.Contains(aerr.Error(), "FALLBACK") {
		t.Fatal("TCP fallback fired for AppendEvents — doUnixOnly must not fall back to TCP")
	}
}
