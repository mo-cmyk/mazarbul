package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestComposeServerInstructions_BaseOnly(t *testing.T) {
	t.Parallel()
	base := ComposeServerInstructions("", "")

	// Load-bearing routing guidance must lead, inside the early window a
	// truncating client preserves (#214 AC #5).
	head := base
	if len(head) > 512 {
		head = head[:512]
	}
	if !strings.Contains(head, "ALWAYS use this MCP") {
		t.Errorf("routing guidance missing from first 512 chars:\n%s", head)
	}
	for _, want := range []string{"recall", "remember", "getContext", "agentFeedback"} {
		if !strings.Contains(base, want) {
			t.Errorf("base instructions missing %q", want)
		}
	}
	if strings.Contains(base, operatingInstructionsHeader) {
		t.Errorf("base-only instructions must not carry the Operating instructions header:\n%s", base)
	}
}

func TestComposeServerInstructions_WithPreset(t *testing.T) {
	t.Parallel()
	section := "**From preset: crm (v0.1.0)** — external data\n> File deals under crm:Account."
	composed := ComposeServerInstructions(section, "")

	if composed == ComposeServerInstructions("", "") {
		t.Fatal("composed-with-preset must differ from base")
	}
	if !strings.Contains(composed, operatingInstructionsHeader) {
		t.Errorf("missing Operating instructions header:\n%s", composed)
	}
	// Preset section LAST: bootstrap pointer precedes the header, which precedes
	// the untrusted body.
	iBootstrap := strings.Index(composed, "getContext")
	iHeader := strings.Index(composed, operatingInstructionsHeader)
	iBody := strings.Index(composed, "File deals under crm:Account.")
	if iBootstrap >= iHeader || iHeader >= iBody {
		t.Errorf("expected bootstrap(%d) < header(%d) < body(%d):\n%s", iBootstrap, iHeader, iBody, composed)
	}
	if !strings.HasSuffix(strings.TrimSpace(composed), "File deals under crm:Account.") {
		t.Errorf("preset section must be last:\n%s", composed)
	}
}

func TestComposeServerInstructions_GotchasComeLast(t *testing.T) {
	t.Parallel()
	preset := "**From preset: crm** — File deals under crm:Account."
	gotchas := "## Known gotchas & lessons\n- prefer get over recall for exact names (seen 4×)"
	composed := ComposeServerInstructions(preset, gotchas)

	iHeader := strings.Index(composed, operatingInstructionsHeader)
	iPresetBody := strings.Index(composed, "File deals under crm:Account.")
	iGotchas := strings.Index(composed, "## Known gotchas & lessons")
	if iHeader < 0 || iPresetBody < 0 || iGotchas < 0 {
		t.Fatalf("missing a section:\n%s", composed)
	}
	// Order: preset header < preset body < gotchas (gotchas are the lowest-trust
	// window, placed after everything).
	if iHeader >= iPresetBody || iPresetBody >= iGotchas {
		t.Errorf("expected header(%d) < presetBody(%d) < gotchas(%d):\n%s", iHeader, iPresetBody, iGotchas, composed)
	}
	if !strings.HasSuffix(strings.TrimSpace(composed), "(seen 4×)") {
		t.Errorf("gotchas must be the very last content:\n%s", composed)
	}
	// With no preset, gotchas still append after the bootstrap pointer.
	noPreset := ComposeServerInstructions("", gotchas)
	if !strings.Contains(noPreset, "## Known gotchas & lessons") {
		t.Errorf("gotchas must appear even with no preset:\n%s", noPreset)
	}
}

// TestAfterInitializeInstructions_OverwritesAndDegrades exercises the hook logic
// in isolation: a successful composer overwrites result.Instructions; an error
// or empty result leaves the static base untouched (degrade clean, #214 AC #3).
func TestAfterInitializeInstructions_OverwritesAndDegrades(t *testing.T) {
	t.Parallel()
	base := ComposeServerInstructions("", "")

	cases := []struct {
		name     string
		composer func(context.Context) (string, error)
		want     string
	}{
		{"success overwrites", func(context.Context) (string, error) { return "COMPOSED", nil }, "COMPOSED"},
		{"error degrades to base", func(context.Context) (string, error) { return "x", errors.New("boom") }, base},
		{"empty degrades to base", func(context.Context) (string, error) { return "   ", nil }, base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := afterInitializeInstructions(zap.NewNop(), tc.composer)
			result := &mcpgo.InitializeResult{Instructions: base}
			hook(context.Background(), nil, nil, result)
			if result.Instructions != tc.want {
				t.Errorf("instructions = %q, want %q", result.Instructions, tc.want)
			}
		})
	}
}

// TestAfterInitializeInstructions_ErrorLogsSessionID asserts the composer-error
// warn log carries a session_id field so concurrent clients failing the composer
// are distinguishable (matches the tool-log correlation in invocationKey).
func TestAfterInitializeInstructions_ErrorLogsSessionID(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zapcore.WarnLevel)
	hook := afterInitializeInstructions(zap.New(core), func(context.Context) (string, error) {
		return "", errors.New("boom")
	})

	hook(context.Background(), nil, nil, &mcpgo.InitializeResult{})

	entries := logs.FilterMessageSnippet("compose server instructions").All()
	if len(entries) != 1 {
		t.Fatalf("expected one composer-error warn, got %d", len(entries))
	}
	if _, ok := entries[0].ContextMap()["session_id"]; !ok {
		t.Errorf("composer-error log must carry a session_id field; fields: %v", entries[0].ContextMap())
	}
}

// TestAfterInitializeInstructions_NilResultIsNoOp asserts the success path is
// nil-safe: a non-empty composed value with a nil result pointer must not panic
// (mcp-go passes non-nil in production, but the guard keeps all three paths
// uniformly safe for tests / future callers).
func TestAfterInitializeInstructions_NilResultIsNoOp(t *testing.T) {
	t.Parallel()
	hook := afterInitializeInstructions(zap.NewNop(), func(context.Context) (string, error) {
		return "COMPOSED", nil
	})
	// Must not panic on a nil *InitializeResult.
	hook(context.Background(), nil, nil, nil)
}

// TestAfterInitializeInstructions_RereadsPerInvocation proves the composer is
// re-invoked on every initialize, so a new connection reflects current state
// (#214 AC #2): mutate the backing value between hook calls and observe the
// change.
func TestAfterInitializeInstructions_RereadsPerInvocation(t *testing.T) {
	t.Parallel()
	var state atomic.Value
	state.Store("A")
	hook := afterInitializeInstructions(zap.NewNop(), func(context.Context) (string, error) {
		return state.Load().(string), nil
	})

	r1 := &mcpgo.InitializeResult{}
	hook(context.Background(), nil, nil, r1)
	if r1.Instructions != "A" {
		t.Fatalf("first session = %q, want A", r1.Instructions)
	}
	state.Store("B")
	r2 := &mcpgo.InitializeResult{}
	hook(context.Background(), nil, nil, r2)
	if r2.Instructions != "B" {
		t.Fatalf("second session = %q, want B (per-session re-read failed)", r2.Instructions)
	}
}

// TestServer_InitializeComposesInstructionsOverStdio asserts the composed value
// reaches the wire: the OnAfterInitialize hook is registered and WINS over the
// static WithInstructions base (#214 AC #1).
func TestServer_InitializeComposesInstructionsOverStdio(t *testing.T) {
	t.Parallel()
	const sentinel = "COMPOSED-PER-SESSION-INSTRUCTIONS"

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	opts := newTestOptions(t)
	opts.Stdio = true
	opts.InstructionComposer = func(context.Context) (string, error) { return sentinel, nil }
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("New returned %T, want *mcpServer", srv)
	}
	impl.stdin = stdinR
	impl.stdout = stdoutW

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(context.Background()) }()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if err := writeJSONLine(stdinW, jsonRPCInitialize(1)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	resp := readJSONResponse(t, scanner)
	if resp["error"] != nil {
		t.Fatalf("initialize returned error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result is %T, want object", resp["result"])
	}
	if got, _ := result["instructions"].(string); got != sentinel {
		t.Errorf("initialize instructions = %q, want composed %q", got, sentinel)
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := <-startErrCh; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}
