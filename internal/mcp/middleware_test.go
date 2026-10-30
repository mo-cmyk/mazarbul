package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// hardenedTestHandler mirrors the production wrapping in startHTTP:
// securityHeaders(hardenTCP(...)). The inner handler returns 200 so a
// request that survives the gate is distinguishable from a rejection.
func hardenedTestHandler(token string) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return securityHeaders(hardenTCP(inner, token, zap.NewNop()))
}

func TestHardenTCP(t *testing.T) {
	t.Parallel()
	const tok = "correct-horse-battery-staple"
	bearer := "Bearer " + tok
	cases := []struct {
		name       string
		path       string
		host       string
		origin     string
		auth       string
		wantStatus int
	}{
		{"mcp without token is rejected", "/mcp", "127.0.0.1:7842", "", "", http.StatusUnauthorized},
		{"mcp with wrong token is rejected", "/mcp", "127.0.0.1:7842", "", "Bearer wrong", http.StatusUnauthorized},
		{"mcp with empty bearer is rejected", "/mcp", "127.0.0.1:7842", "", "Bearer ", http.StatusUnauthorized},
		{"mcp with valid token is allowed", "/mcp", "127.0.0.1:7842", "", bearer, http.StatusOK},
		{"bearer scheme is case insensitive", "/mcp", "127.0.0.1:7842", "", "bearer " + tok, http.StatusOK},
		{"non loopback host is rejected", "/mcp", "evil.example.com", "", bearer, http.StatusForbidden},
		{"non loopback host rejected on healthz too", "/healthz", "evil.example.com", "", "", http.StatusForbidden},
		{"disallowed origin is rejected", "/mcp", "127.0.0.1:7842", "https://evil.example.com", bearer, http.StatusForbidden},
		{"loopback origin is allowed", "/mcp", "127.0.0.1:7842", "http://localhost:7842", bearer, http.StatusOK},
		{"healthz is exempt from the token", "/healthz", "127.0.0.1:7842", "", "", http.StatusOK},
		{"ipv6 loopback host is allowed", "/mcp", "[::1]:7842", "", bearer, http.StatusOK},
		{"localhost host is allowed", "/mcp", "localhost:7842", "", bearer, http.StatusOK},
	}
	h := hardenedTestHandler(tok)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "http://placeholder"+tc.path, nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			// Security headers are set unconditionally, even on rejection.
			if got := rec.Header().Get("Content-Security-Policy"); got != cspValue {
				t.Errorf("CSP = %q, want %q", got, cspValue)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != contentTypeNosniff {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, contentTypeNosniff)
			}
		})
	}
}

func TestBearerTokenValid(t *testing.T) {
	t.Parallel()
	want := []byte("tokenvalue")
	cases := []struct {
		name   string
		header string
		ok     bool
	}{
		{"exact match", "Bearer tokenvalue", true},
		{"lowercase scheme", "bearer tokenvalue", true},
		{"wrong token", "Bearer nope", false},
		{"prefix of token", "Bearer token", false},
		{"token plus suffix", "Bearer tokenvalueX", false},
		{"missing scheme", "tokenvalue", false},
		{"wrong scheme", "Basic tokenvalue", false},
		{"empty header", "", false},
		{"scheme only", "Bearer", false},
		{"scheme and space only", "Bearer ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bearerTokenValid(tc.header, want); got != tc.ok {
				t.Fatalf("bearerTokenValid(%q) = %v, want %v", tc.header, got, tc.ok)
			}
		})
	}
}

// TestBearerTokenValid_EmptyWantRejects pins the fail-closed guard: an empty
// expected token must reject every credential — including a syntactically
// valid empty bearer — rather than let ConstantTimeCompare("", "") match.
func TestBearerTokenValid_EmptyWantRejects(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"Bearer ", "Bearer", "Bearer anything", ""} {
		if bearerTokenValid(header, []byte("")) {
			t.Errorf("bearerTokenValid(%q, empty want) = true, want false (fail closed)", header)
		}
	}
}

func TestIsLoopbackHostport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in string
		ok bool
	}{
		{"127.0.0.1:7842", true},
		{"127.0.0.1", true},
		{"127.0.0.2:1", true},
		{"localhost:7842", true},
		{"localhost", true},
		{"[::1]:7842", true},
		{"::1", true},
		{"evil.example.com", false},
		{"evil.example.com:7842", false},
		{"10.0.0.1:7842", false},
		{"0.0.0.0:7842", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackHostport(tc.in); got != tc.ok {
				t.Fatalf("isLoopbackHostport(%q) = %v, want %v", tc.in, got, tc.ok)
			}
		})
	}
}

// TestOptionsValidate_HTTPRequiresToken pins the fail-closed rule: the TCP
// transport must never be mounted without a bearer credential.
func TestOptionsValidate_HTTPRequiresToken(t *testing.T) {
	t.Parallel()
	_, err := New(Options{Name: "x", Version: "1", HTTPAddr: "127.0.0.1:0"})
	if err == nil {
		t.Fatal("New with HTTPAddr and no Token = nil error, want fail-closed error")
	}
	if !strings.Contains(err.Error(), "HTTPAddr requires a non-empty Token") {
		t.Fatalf("error %q does not mention the token requirement", err.Error())
	}
}

// TestHTTPTransport_RejectsUnauthenticatedAndRebinding drives the real
// bound TCP listener: the MCP surface demands a token, /healthz is exempt
// but still loopback-gated, and a spoofed Host (DNS-rebinding) is rejected.
func TestHTTPTransport_RejectsUnauthenticatedAndRebinding(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(testutil.TestContext(t)) }()
	addr := waitForHTTPListener(t, srv)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			t.Errorf("Shutdown: %v", serr)
		}
		<-startErrCh
	})

	// hostOverride sets req.Host directly: net/http's client derives the
	// wire Host header from req.Host (or the URL), never from a "Host" entry
	// in req.Header — so a DNS-rebinding spoof must go through req.Host.
	do := func(method, path, hostOverride string) int {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		req, rerr := http.NewRequestWithContext(ctx, method, "http://"+addr+path, bytes.NewReader([]byte("{}")))
		if rerr != nil {
			t.Fatalf("NewRequest: %v", rerr)
		}
		if hostOverride != "" {
			req.Host = hostOverride
		}
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("Do %s %s: %v", method, path, derr)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := do(http.MethodPost, "/mcp", ""); code != http.StatusUnauthorized {
		t.Errorf("POST /mcp without token = %d, want 401", code)
	}
	if code := do(http.MethodGet, HealthPath, ""); code != http.StatusOK {
		t.Errorf("GET /healthz without token = %d, want 200 (token-exempt)", code)
	}
	if code := do(http.MethodGet, HealthPath, "attacker.example.com"); code != http.StatusForbidden {
		t.Errorf("GET /healthz with spoofed Host = %d, want 403 (DNS-rebinding defense)", code)
	}
}

// TestUnixTransport_NoTokenRequired proves the Unix socket is the trusted
// channel: its 0600 permissions are the credential, so the protected MCP
// surface answers without a bearer token (a request that would be 401 over
// TCP must not be 401 over the socket).
func TestUnixTransport_NoTokenRequired(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(t.TempDir(), "s.sock")
	opts := newTestOptions(t)
	opts.UnixSocket = socket
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(testutil.TestContext(t)) }()
	waitForUnixListener(t, socket)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			t.Errorf("Shutdown: %v", serr)
		}
		<-startErrCh
	})

	client := UnixHTTPClient(socket, 2*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// No Authorization header, and an MCP initialize body the streamable
	// handler accepts. The point is that the socket does not 401.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("unix /mcp without token = %d, want it served (the socket is not token-gated)", resp.StatusCode)
	}
}

// TestUnixTransport_ServesHealthz is the regression guard for the
// socket-default posture: with TCP off by default, first-party liveness
// probes (doctor's mcp.healthz check, the daemon-install readiness wait)
// reach the daemon over the Unix socket. The socket handler must therefore
// answer GET /healthz with the canonical JSON body. If /healthz is not
// registered on the socket the request falls through to the MCP JSON-RPC
// catch-all, which opens a streaming response and hangs the probe until its
// timeout — the bug this test exists to prevent.
func TestUnixTransport_ServesHealthz(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(t.TempDir(), "s.sock")
	opts := newTestOptions(t)
	opts.UnixSocket = socket
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(testutil.TestContext(t)) }()
	waitForUnixListener(t, socket)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			t.Errorf("Shutdown: %v", serr)
		}
		<-startErrCh
	})

	client := UnixHTTPClient(socket, 2*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+HealthPath, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// A hang here (body never completes) trips the client timeout and Do
	// returns a deadline error — the precise failure the missing route caused.
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz over socket: %v (a timeout here means /healthz is not served on the socket)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz over socket = %d, want 200", resp.StatusCode)
	}
	var body HealthResponse
	if derr := json.NewDecoder(resp.Body).Decode(&body); derr != nil {
		t.Fatalf("decode healthz body: %v", derr)
	}
	if body.Status != "ok" {
		t.Fatalf("healthz status = %q, want ok", body.Status)
	}
}
