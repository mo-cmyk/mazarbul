package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// BenchmarkServer_Invocation measures end-to-end in-process dispatch
// latency through the mcp-go tool adapter. It excludes transport
// (no socket, no JSON-RPC framing) so the result captures the cost of
// argument marshaling, Tool.Invoke, and result wrapping. This is the
// AGENTS.md Rule 10 hot-path baseline for the MCP request handler.
//
// Run with:
//
//	go test -bench=BenchmarkServer_Invocation -benchmem ./internal/mcp/
func BenchmarkServer_Invocation(b *testing.B) {
	b.ReportAllocs()

	tool := echoTool{name: "echo"}
	_, handler := adaptTool(tool, NoopSecretProvider{}, &schemaValidator{}, 0)
	req := callToolRequest("echo", map[string]any{"k": "v"})
	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		if _, err := handler(ctx, req); err != nil {
			b.Fatalf("handler: %v", err)
		}
	}
}

// BenchmarkServer_InvocationWithLogging measures the same dispatch path
// but with the logging hook wired in to capture the observability
// overhead introduced by tool-invocation logging. The numbers paired
// with BenchmarkServer_Invocation show the marginal cost of structured
// logging on the hot path.
func BenchmarkServer_InvocationWithLogging(b *testing.B) {
	b.ReportAllocs()

	srv, err := New(Options{
		Name:    "bench",
		Version: "0.0.0",
		Stdio:   true,
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		b.Fatalf("Register: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		b.Fatalf("Server is %T, want *mcpServer", srv)
	}
	_, handler := adaptTool(echoTool{name: "echo"}, impl.opts.Secrets, &schemaValidator{}, 0)
	req := callToolRequest("echo", map[string]any{"k": "v"})
	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		if _, err := handler(ctx, req); err != nil {
			b.Fatalf("handler: %v", err)
		}
	}
}

// BenchmarkServer_MarshalArguments isolates the argument-marshaling cost
// from the rest of the adapter so allocation pressure on the hot path
// can be regression-tracked independently of tool logic changes.
func BenchmarkServer_MarshalArguments(b *testing.B) {
	b.ReportAllocs()

	args := map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"x": 1, "y": "two"},
	}
	b.ResetTimer()
	for range b.N {
		if _, err := marshalArguments(args); err != nil {
			b.Fatalf("marshalArguments: %v", err)
		}
	}
}

// BenchmarkServer_WrapResult isolates the result-wrapping cost. Tools
// that return larger payloads will dominate dispatch time here, so
// regressions in the marshaling path show up first.
func BenchmarkServer_WrapResult(b *testing.B) {
	b.ReportAllocs()

	payload := json.RawMessage(`{"status":"ok","detail":"the quick brown fox jumps over the lazy dog"}`)
	b.ResetTimer()
	for range b.N {
		_ = wrapResult(payload)
	}
}
