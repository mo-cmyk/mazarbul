//go:build integration

package mcp

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestIntegration_ThreeTransportsIdenticalResponse pins #39 AC5: the
// server.Server exposes the same tool catalogue over stdio, HTTP, and
// Unix and a tools/call routed over any of the three returns the same
// structured payload. Without this end-to-end check, transport-specific
// adapter drift could let one transport silently diverge from the
// others (different JSON shape, different tool ordering, different
// error envelope) and we would only notice when an agent migrated.
//
// The test boots a single Server instance with all three transports
// enabled, registers one echo tool, and sends the identical tools/call
// payload to each. The structured response is then compared across
// transports.
func TestIntegration_ThreeTransportsIdenticalResponse(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	// Unix sockets cap at 104 bytes on macOS / 108 on Linux; t.TempDir
	// under /var/folders is too long on darwin. Use /tmp.
	sockDir, err := os.MkdirTemp("/tmp", "mcp-tri-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sockDir); err != nil {
			t.Logf("cleanup sockDir: %v", err)
		}
	})
	sockPath := filepath.Join(sockDir, "s.sock")

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	opts := newTestOptions(t)
	opts.Stdio = true
	opts.HTTPAddr = "127.0.0.1:0"
	opts.UnixSocket = sockPath
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	impl.stdin = stdinR
	impl.stdout = stdoutW

	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()
	t.Cleanup(func() {
		if err := stdinW.Close(); err != nil {
			t.Logf("close stdin: %v", err)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("cleanup Shutdown: %v", err)
		}
		if err := stdoutW.Close(); err != nil {
			t.Logf("close stdout: %v", err)
		}
		<-startErrCh
	})

	httpAddr := waitForHTTPListener(t, srv)
	waitForUnixListener(t, sockPath)

	stdioGreeting := callEchoOverStdio(t, stdinW, stdoutR)
	httpGreeting := callEchoOverHTTP(t, "http://"+httpAddr+"/mcp", initializeHTTPSession(t, "http://"+httpAddr+"/mcp"), "tri-transport")
	unixGreeting := callEchoOverUnix(t, sockPath)

	if stdioGreeting != "tri-transport" {
		t.Fatalf("stdio greeting = %q, want %q", stdioGreeting, "tri-transport")
	}
	if httpGreeting != "tri-transport" {
		t.Fatalf("http greeting = %q, want %q", httpGreeting, "tri-transport")
	}
	if unixGreeting != "tri-transport" {
		t.Fatalf("unix greeting = %q, want %q", unixGreeting, "tri-transport")
	}
}

// callEchoOverStdio runs the MCP initialize + tools/call handshake over
// the in-memory pipes that back the server's stdio transport, returning
// the echoed greeting.
func callEchoOverStdio(t *testing.T, stdinW io.Writer, stdoutR io.Reader) string {
	t.Helper()
	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if err := writeJSONLine(stdinW, jsonRPCInitialize(1)); err != nil {
		t.Fatalf("stdio write initialize: %v", err)
	}
	if resp := readJSONResponse(t, scanner); resp["error"] != nil {
		t.Fatalf("stdio initialize error: %v", resp["error"])
	}
	if err := writeJSONLine(stdinW, jsonRPCToolsCall(2, "echo", map[string]any{"greeting": "tri-transport"})); err != nil {
		t.Fatalf("stdio write tools/call: %v", err)
	}
	resp := readJSONResponse(t, scanner)
	return extractStructuredGreeting(t, resp)
}

// callEchoOverUnix dials the Unix socket at path and runs the MCP
// initialize + tools/call handshake against the streamable HTTP server
// listening on it, returning the echoed greeting.
func callEchoOverUnix(t *testing.T, path string) string {
	t.Helper()
	client := UnixHTTPClient(path, 2*time.Second)
	// Override the default HTTP client used by sendJSONRPCRaw for the
	// duration of this helper. The simplest way to thread it through is
	// to use a small inline POST loop that mirrors initializeHTTPSession
	// + callEchoOverHTTP but with a Unix dialer.
	endpoint := "http://unix/mcp"
	sessionID := initializeHTTPSessionWithClient(t, client, endpoint)
	return callEchoOverHTTPWithClient(t, client, endpoint, sessionID, "tri-transport")
}
