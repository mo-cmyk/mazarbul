//go:build integration

// Package mcptest provides helpers for integration tests that drive a
// Mazarbul MCP server through in-memory io.Pipe pairs over the stdio
// transport. The helpers are extracted from
// internal/mcp/tools/recall_integration_test.go per the plan in issue #45
// so they do not need to be duplicated across every tools integration test.
//
// Usage: call StartStdioServer to boot a server with a single tool, or
// StartStdioServerMulti for multiple tools. Then call Handshake to
// perform the MCP initialize handshake, then drive individual tool calls
// with JSONRPCToolsCall + WriteJSONLine + ReadJSONResponse.
package mcptest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// StartStdioServer wires tool onto a fresh MCP server backed by an
// in-memory io.Pipe pair and starts it in a goroutine. The returned
// stdinW and stdoutR are the write and read ends of the server's stdio
// transport. The cleanup function closes the pipes and shuts the server
// down; callers must invoke it (typically via defer) after all sends and
// reads complete.
//
// The function fails the test immediately on any wiring error, so callers
// can assume the server is running when it returns.
func StartStdioServer(t *testing.T, ctx context.Context, tool mcp.Tool) (stdinW io.Writer, stdoutR io.Reader, cleanup func()) {
	t.Helper()
	return StartStdioServerMulti(t, ctx, tool)
}

// StartStdioServerMulti is like StartStdioServer but registers multiple
// tools on the same server instance.
func StartStdioServerMulti(t *testing.T, ctx context.Context, tools ...mcp.Tool) (stdinW io.Writer, stdoutR io.Reader, cleanup func()) {
	t.Helper()
	stdinR, stdinWPipe := io.Pipe()
	stdoutRPipe, stdoutW := io.Pipe()

	srv, err := mcp.New(mcp.Options{
		Name:            "mcptest",
		Version:         "0.0.0",
		Logger:          zap.NewNop(),
		Stdio:           true,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("mcptest: mcp.New: %v", err)
	}
	cfg, ok := srv.(mcp.StdioConfigurer)
	if !ok {
		t.Fatalf("mcptest: Server %T does not satisfy mcp.StdioConfigurer", srv)
	}
	cfg.SetStdio(stdinR, stdoutW)

	for _, tool := range tools {
		if err := srv.Register(tool); err != nil {
			t.Fatalf("mcptest: Register %q: %v", tool.Name(), err)
		}
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()

	// baseCtx strips the test context's cancellation so the shutdown
	// sequence gets its own 2-second grace period even when ctx is already
	// done. Captured here so contextcheck can trace context flow into the
	// cleanup closure.
	baseCtx := context.WithoutCancel(ctx)
	cleanup = func() {
		if err := stdinWPipe.Close(); err != nil {
			t.Logf("mcptest: close stdin: %v", err)
		}
		shutdownCtx, cancel := context.WithTimeout(baseCtx, 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("mcptest: shutdown: %v", err)
		}
		if err := stdoutW.Close(); err != nil {
			t.Logf("mcptest: close stdout: %v", err)
		}
		<-startErrCh
	}
	return stdinWPipe, stdoutRPipe, cleanup
}

// WriteJSONLine marshals msg and writes it as a single newline-terminated
// JSON line to w. This mirrors the wire encoding the mcp-go stdio
// transport expects.
func WriteJSONLine(w io.Writer, msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// ReadJSONResponse blocks until the next line on scanner is available,
// then decodes it as a JSON-RPC response. A scanner failure or
// non-JSON payload fails the test immediately.
func ReadJSONResponse(t *testing.T, scanner *bufio.Scanner) map[string]any {
	t.Helper()
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			t.Fatalf("mcptest: scanner.Scan: %v", err)
		}
		t.Fatal("mcptest: scanner.Scan returned false; pipe closed")
	}
	var resp map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("mcptest: unmarshal %q: %v", scanner.Bytes(), err)
	}
	return resp
}

// JSONRPCInitialize builds the canonical MCP initialize request body with
// the given message id.
func JSONRPCInitialize(id int) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"clientInfo":      map[string]any{"name": "mcp-test-client", "version": "0.0.0"},
			"capabilities":    map[string]any{},
		},
	}
}

// JSONRPCToolsCall builds a tools/call request body. Args may be nil when
// calling a tool that requires no arguments.
func JSONRPCToolsCall(id int, name string, args any) map[string]any {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  params,
	}
}

// Handshake performs the MCP initialize handshake over the given pipe
// pair using scanner as the read side. The test fails immediately if the
// server returns an error. Call once per server instance before making
// tool calls.
func Handshake(t *testing.T, w io.Writer, scanner *bufio.Scanner) {
	t.Helper()
	if err := WriteJSONLine(w, JSONRPCInitialize(1)); err != nil {
		t.Fatalf("mcptest: write initialize: %v", err)
	}
	if resp := ReadJSONResponse(t, scanner); resp["error"] != nil {
		t.Fatalf("mcptest: initialize error: %v", resp["error"])
	}
}

// NewScanner returns a bufio.Scanner pre-configured for MCP line-framing:
// a 64 KiB initial buffer and a 1 MiB max line size, matching the sizes
// used in the existing integration tests.
func NewScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return s
}
