package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// TestPreflightDaemon_DetectsViaHTTPProbeWhenPidfileMissing reproduces
// the production bug where a Mazarbul daemon was running (holding the
// LadybugDB exclusive lock and listening on 127.0.0.1:7842) but
// `maz mcp serve` and `maz remember` both fell through to the
// standalone in-process path and crashed with "ladybug status 1".
//
// Root cause: daemon.DefaultPath() resolves "$TMPDIR/mazarbul.pid",
// and the shell that ran `maz daemon start` had a different TMPDIR
// than the Claude-Desktop child / a later shell. The PID file is
// effectively invisible to those other processes, so the previous
// detection logic returned nil and the caller raced the daemon for
// the on-disk lock.
//
// Fix: when the PID file lookup misses, fall back to a loopback TCP
// probe against the deterministic MCP HTTP endpoint
// (mcp.DefaultHTTPAddr). Any process listening there is, by Mazarbul's
// own convention, the running daemon.
func TestPreflightDaemon_DetectsViaHTTPProbeWhenPidfileMissing(t *testing.T) {
	// No t.Parallel: this test stubs the package-level
	// probeDaemonListening seam and clears a process-wide env var.
	// Clear MAZ_DATA_DIR so the genuine convention-default path is
	// exercised: CI sets MAZ_DATA_DIR suite-wide, which would make
	// isDefaultDataDir() return false and the probe decline to claim the
	// (now env-selected) store — the F12 behaviour pinned elsewhere.
	t.Setenv("MAZ_DATA_DIR", "")
	prev := probeDaemonListening
	probeDaemonListening = func(string) bool { return true }
	t.Cleanup(func() { probeDaemonListening = prev })

	// Default data dir: the HTTP-probe fallback can confirm a daemon is up
	// but not which dir it serves, so it only claims the default store.
	err := preflightDaemon(filepath.Join(t.TempDir(), "definitely-missing.pid"), config.Defaults().DataDir)

	var dlhe *daemonLockHeldError
	if !errors.As(err, &dlhe) {
		t.Fatalf("missing pidfile but HTTP probe positive → want daemonLockHeldError, got %v", err)
	}
}

// TestPreflightDaemon_HTTPProbeIgnoresNonDefaultDataDir pins the F12 fix
// for the TMPDIR-fallback path: a daemon is detected via the loopback
// probe, but the caller asked for an explicit non-default --data-dir the
// probe cannot confirm the daemon serves, so the CLI must run in-process
// (nil) rather than silently dispatching to the daemon's own store.
func TestPreflightDaemon_HTTPProbeIgnoresNonDefaultDataDir(t *testing.T) {
	prev := probeDaemonListening
	probeDaemonListening = func(string) bool { return true }
	t.Cleanup(func() { probeDaemonListening = prev })

	if err := preflightDaemon(filepath.Join(t.TempDir(), "definitely-missing.pid"), t.TempDir()); err != nil {
		t.Fatalf("explicit non-default --data-dir with unconfirmable daemon dir → want nil (in-process), got %v", err)
	}
}

// TestConfirmMaterialised_SurfacesUnmaterialisedRow pins the Greptile P1 on
// #181: FileEventLog.Append keeps the JSONL durable but warn-logs and swallows
// storage.Apply failures, so a write command must read the row back and fail
// loudly when it is absent rather than exit 0 with the silent-until-rebuild
// mode intact. A seeded core kind ("person") reads back; a never-written name
// returns silentError(2).
func TestConfirmMaterialised_SurfacesUnmaterialisedRow(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a real wiring (exclusive store lock) on a temp dir.
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}
	t.Cleanup(func() { _ = wired.Close(t.Context()) })

	cmd := &cobra.Command{}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	// A genesis-seeded core kind is materialised → no error.
	if cerr := confirmMaterialised(t.Context(), cmd, wired, "test",
		matCheck{query: "SELECT COUNT(1) FROM entity_kinds WHERE name = ?", args: []any{"person"}, label: "person"}); cerr != nil {
		t.Fatalf("seeded core kind person should read back: %v", cerr)
	}

	// A name that was never written → silentError(2) + actionable stderr.
	cerr := confirmMaterialised(t.Context(), cmd, wired, "test",
		matCheck{query: "SELECT COUNT(1) FROM entity_kinds WHERE name = ?", args: []any{"user:never_written"}, label: "user:never_written"})
	var se silentError
	if !errors.As(cerr, &se) {
		t.Fatalf("absent row → want silentError, got %v", cerr)
	}
	if se.code != 2 {
		t.Errorf("absent row exit code = %d, want 2", se.code)
	}
	if !bytes.Contains(errBuf.Bytes(), []byte("rebuild")) {
		t.Errorf("stderr should point at `maz rebuild`, got %q", errBuf.String())
	}
}

// TestPreflightDaemon_HTTPProbeIgnoresEnvSelectedDataDir pins the F12
// follow-up bug: config.Defaults().DataDir folds in MAZ_DATA_DIR, so a process
// that points the env at an isolated store (every eval harness and unbiased
// agent run does exactly this) made isDefaultDataDir() return true — and with a
// daemon on :7842 serving a *different* dir, the loopback-probe fallback then
// claimed the lock and refused the write / mis-routed the read. The CLI must run
// in-process against the env-selected store instead.
func TestPreflightDaemon_HTTPProbeIgnoresEnvSelectedDataDir(t *testing.T) {
	// No t.Parallel: stubs the probe seam and sets a process-wide env var.
	dir := t.TempDir()
	t.Setenv("MAZ_DATA_DIR", dir) // config.Defaults().DataDir now resolves to dir
	prev := probeDaemonListening
	probeDaemonListening = func(string) bool { return true }
	t.Cleanup(func() { probeDaemonListening = prev })

	if err := preflightDaemon(filepath.Join(t.TempDir(), "missing.pid"), dir); err != nil {
		t.Fatalf("MAZ_DATA_DIR-selected store + daemon on :7842 serving another dir → want nil (in-process), got %v", err)
	}
}

// TestPreflightDaemon_RoutesWhenDaemonServesRequestedDir confirms that a
// live daemon whose PID file records the requested data dir claims the
// lock (so the caller routes to the daemon's MCP transport).
func TestPreflightDaemon_RoutesWhenDaemonServesRequestedDir(t *testing.T) {
	restore := daemon.SetIsMazDaemonForTest(func(int, string) bool { return true })
	t.Cleanup(func() { daemon.SetIsMazDaemonForTest(restore) })

	served := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "maz.pid")
	if err := daemon.WritePID(pidPath, "tok", served); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	err := preflightDaemon(pidPath, served)
	var dlhe *daemonLockHeldError
	if !errors.As(err, &dlhe) {
		t.Fatalf("daemon serves the requested dir → want daemonLockHeldError, got %v", err)
	}
}

// TestPreflightDaemon_InProcessWhenDaemonServesOtherDir is the core F12
// regression: a live daemon serving dir B must NOT answer for an explicit
// --data-dir A. preflight returns nil so the caller opens A in-process.
func TestPreflightDaemon_InProcessWhenDaemonServesOtherDir(t *testing.T) {
	restore := daemon.SetIsMazDaemonForTest(func(int, string) bool { return true })
	t.Cleanup(func() { daemon.SetIsMazDaemonForTest(restore) })
	// Probe stub guards against a real daemon on :7842 flipping the
	// PID-file-miss fallback; here the PID file is present and matches.
	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	served := t.TempDir()
	requested := t.TempDir() // a different store
	pidPath := filepath.Join(t.TempDir(), "maz.pid")
	if err := daemon.WritePID(pidPath, "tok", served); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	if err := preflightDaemon(pidPath, requested); err != nil {
		t.Fatalf("daemon serves a different dir → want nil (in-process), got %v", err)
	}
}

// TestPreflightDaemon_StandaloneWhenNoPidAndNoEndpoint pins the
// negative path: with no PID file AND nothing listening on the
// loopback MCP endpoint, the CLI must take the in-process path.
func TestPreflightDaemon_StandaloneWhenNoPidAndNoEndpoint(t *testing.T) {
	// No t.Parallel: this test stubs the package-level
	// probeDaemonListening seam.
	prev := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prev })

	if err := preflightDaemon(filepath.Join(t.TempDir(), "definitely-missing.pid"), config.Defaults().DataDir); err != nil {
		t.Fatalf("no daemon visible → want nil, got %v", err)
	}
}

// dispatchDataDirCmd builds a root command carrying a *changed* --data-dir
// persistent flag and a captured stderr buffer, the shape dispatchViaDaemon
// inspects to decide whether to emit the "--data-dir is ignored" warning.
func dispatchDataDirCmd(t *testing.T, dataDir string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{Use: "maz"}
	cmd.PersistentFlags().String("data-dir", "", "")
	if err := cmd.PersistentFlags().Set("data-dir", dataDir); err != nil {
		t.Fatalf("set data-dir: %v", err)
	}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	return cmd, &errBuf
}

// TestDispatchViaDaemon_SuppressesWarningWhenServedDirMatches pins the usability
// fix: when the running daemon serves EXACTLY the requested --data-dir, the flag
// is effectively honored, so the "--data-dir is ignored" warning is a false
// alarm and must be suppressed (all four sparse-context usability agents flagged
// the warning firing even when the daemon's dir equalled the dir they passed).
func TestDispatchViaDaemon_SuppressesWarningWhenServedDirMatches(t *testing.T) {
	// No t.Parallel: stubs the invokeToolViaDaemonFn seam.
	dir := t.TempDir()
	// The daemon's PID file (co-located with the dir it serves) records THIS dir.
	if err := daemon.WritePID(daemon.DataDirPIDPath(dir), "tok", dir); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	prevInvoke := invokeToolViaDaemonFn
	invokeToolViaDaemonFn = func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	t.Cleanup(func() { invokeToolViaDaemonFn = prevInvoke })

	cmd, errBuf := dispatchDataDirCmd(t, dir)
	if _, err := dispatchViaDaemon(t.Context(), cmd, dir, zap.NewNop(), "get", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("dispatchViaDaemon: %v", err)
	}
	if bytes.Contains(errBuf.Bytes(), []byte("is ignored")) {
		t.Errorf("daemon serves exactly --data-dir → warning must be suppressed; got stderr: %q", errBuf.String())
	}
}

// TestDispatchViaDaemon_WarnsAndNamesServedDirWhenDifferent pins the other half:
// when the daemon serves a DIFFERENT dir than requested, the warning is truthful
// and must fire — and it should name the actual served dir so it informs rather
// than alarms.
func TestDispatchViaDaemon_WarnsAndNamesServedDirWhenDifferent(t *testing.T) {
	// No t.Parallel: stubs the invokeToolViaDaemonFn seam.
	requested := t.TempDir()
	served := t.TempDir()
	// PID file co-located with the requested dir, but recording a different served dir.
	if err := daemon.WritePID(daemon.DataDirPIDPath(requested), "tok", served); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	prevInvoke := invokeToolViaDaemonFn
	invokeToolViaDaemonFn = func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	t.Cleanup(func() { invokeToolViaDaemonFn = prevInvoke })

	cmd, errBuf := dispatchDataDirCmd(t, requested)
	if _, err := dispatchViaDaemon(t.Context(), cmd, requested, zap.NewNop(), "get", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("dispatchViaDaemon: %v", err)
	}
	if !bytes.Contains(errBuf.Bytes(), []byte("is ignored")) {
		t.Errorf("daemon serves a different dir → warning expected; got %q", errBuf.String())
	}
	if !bytes.Contains(errBuf.Bytes(), []byte(served)) {
		t.Errorf("warning should name the actual served dir %q; got %q", served, errBuf.String())
	}
}

// TestResolveDaemonDispatchError_ToolErrorSurfacedVerbatim is the regression
// for the worst CLI UX bug found in verification: `maz get <missing-uid>`
// against a healthy daemon printed "the daemon is running and holds the
// database lock — stop the daemon", burying the real [E_NOT_FOUND] in a warn
// log. A tool-level failure means the daemon ANSWERED, so the dispatcher must
// surface that error verbatim, never the lock hint.
func TestResolveDaemonDispatchError_ToolErrorSurfacedVerbatim(t *testing.T) {
	t.Parallel()

	toolErr := &toolError{Msg: "[E_NOT_FOUND] entities/01ZZ not found"}
	perr := &daemonLockHeldError{PID: 4242, StopHint: "maz daemon stop"}

	got := resolveDaemonDispatchError(toolErr, perr, zap.NewNop(), "get")

	var te *toolError
	if !errors.As(got, &te) {
		t.Fatalf("want the tool error surfaced verbatim, got %v", got)
	}
	if te.Msg != toolErr.Msg {
		t.Errorf("tool error message = %q, want %q", te.Msg, toolErr.Msg)
	}
	var dlhe *daemonLockHeldError
	if errors.As(got, &dlhe) {
		t.Error("surfaced the daemon-lock hint for a tool-level failure; that hint is misleading when the daemon is healthy")
	}
}

// TestResolveDaemonDispatchError_TransportErrorSurfacesHint pins the other
// half: when the failure is a genuine transport error (a daemon was detected
// but its HTTP endpoint is unreachable), the actionable daemon-lock hint IS the
// right thing to surface.
func TestResolveDaemonDispatchError_TransportErrorSurfacesHint(t *testing.T) {
	t.Parallel()

	transportErr := fmt.Errorf("mcp http start: %w", errors.New("connection refused"))
	perr := &daemonLockHeldError{PID: 4242, StopHint: "maz daemon stop"}

	got := resolveDaemonDispatchError(transportErr, perr, zap.NewNop(), "get")

	var dlhe *daemonLockHeldError
	if !errors.As(got, &dlhe) {
		t.Fatalf("transport failure → want the daemon-lock hint, got %v", got)
	}
}

// TestExtractToolResult_IsErrorReturnsTypedToolError proves the typing that the
// dispatcher relies on: an MCP result with IsError set yields a *toolError
// carrying the tool's message, so errors.As can distinguish it from a transport
// failure upstream.
func TestExtractToolResult_IsErrorReturnsTypedToolError(t *testing.T) {
	t.Parallel()

	res := &mcpgo.CallToolResult{
		IsError: true,
		Content: []mcpgo.Content{mcpgo.TextContent{Type: "text", Text: "[E_VALIDATION] bad arg"}},
	}

	_, err := extractToolResult(res)

	var te *toolError
	if !errors.As(err, &te) {
		t.Fatalf("IsError result → want *toolError, got %T: %v", err, err)
	}
	if te.Msg != "[E_VALIDATION] bad arg" {
		t.Errorf("toolError.Msg = %q, want %q", te.Msg, "[E_VALIDATION] bad arg")
	}
}

// TestRunToolViaDaemonOrInProcess_LockHeldReroutesToDaemon reproduces UX-5: a
// read tool (e.g. `ask`) intermittently died with "failed to open database with
// status 1" while a healthy daemon held the LadybugDB lock. The cause is a race
// in daemon detection — preflightDaemon's pidfile lookup misses (foreign
// $TMPDIR) and its loopback probe loses the race, so the CLI takes the
// in-process branch and collides on the lock. This test pins the fix: when the
// in-process open fails on a held lock AND a daemon is in fact listening, the
// call must reroute through the daemon transport instead of surfacing the
// cryptic open error.
func TestRunToolViaDaemonOrInProcess_LockHeldReroutesToDaemon(t *testing.T) {
	// No t.Parallel: stubs package-level seams.
	prevPre := preflightDaemon
	preflightDaemon = func(string, string) error { return nil } // preflight raced the probe and missed
	t.Cleanup(func() { preflightDaemon = prevPre })

	prevOpen := openWiringFn
	openWiringFn = func(context.Context, daemon.WiringConfig) (*daemon.Wired, error) {
		return nil, errors.New("open ladybug: open database at /x/graph.ladybug: " +
			"failed to open database with status 1")
	}
	t.Cleanup(func() { openWiringFn = prevOpen })

	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return true } // a daemon IS listening
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	prevInvoke := invokeToolViaDaemonFn
	var routed bool
	const wantDataDir = "/tmp/isolated"
	invokeToolViaDaemonFn = func(_ context.Context, gotDataDir, name string, _ json.RawMessage) (json.RawMessage, error) {
		routed = true
		// Guard the dataDir/toolName argument order: a future refactor that
		// swaps the two strings would otherwise pass silently.
		if gotDataDir != wantDataDir {
			t.Errorf("routed dataDir = %q, want %q", gotDataDir, wantDataDir)
		}
		if name != "ask" {
			t.Errorf("routed tool = %q, want ask", name)
		}
		return json.RawMessage(`{"answer":"ok"}`), nil
	}
	t.Cleanup(func() { invokeToolViaDaemonFn = prevInvoke })

	inProc := func(context.Context, *cobra.Command, *daemon.Wired, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("in-process path must not run after a lock-held open failure")
		return nil, nil
	}

	out, err := runToolViaDaemonOrInProcess(context.Background(), &cobra.Command{},
		"/tmp/isolated", zap.NewNop(), "ask", nil, inProc)
	if err != nil {
		t.Fatalf("runToolViaDaemonOrInProcess: unexpected error %v", err)
	}
	if !routed {
		t.Error("a lock-held open failure with a live daemon must reroute via the daemon transport")
	}
	if string(out) != `{"answer":"ok"}` {
		t.Errorf("out = %s, want the daemon answer", out)
	}
}

// TestRunToolViaDaemonOrInProcess_LockHeldNoListenerIsActionable covers the
// case the user hit: a `maz mcp serve` over stdio (Claude Code) holds the
// LadybugDB lock but binds NO socket, so probeDaemonListening is false. The
// in-process open then fails with the opaque "status 1"; the surfaced error
// must name the lock + the fix, not leak the raw C status as "open wiring: …".
func TestRunToolViaDaemonOrInProcess_LockHeldNoListenerIsActionable(t *testing.T) {
	prevPre := preflightDaemon
	preflightDaemon = func(string, string) error { return nil }
	t.Cleanup(func() { preflightDaemon = prevPre })

	prevOpen := openWiringFn
	openWiringFn = func(context.Context, daemon.WiringConfig) (*daemon.Wired, error) {
		return nil, errors.New("daemon: open orchestrator: open ladybug: " +
			"failed to open database with status 1")
	}
	t.Cleanup(func() { openWiringFn = prevOpen })

	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return false } // stdio serve: no socket
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	inProc := func(context.Context, *cobra.Command, *daemon.Wired, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("in-process tool must not run after a lock-held open failure")
		return nil, nil
	}

	_, err := runToolViaDaemonOrInProcess(context.Background(), &cobra.Command{},
		"/tmp/isolated", zap.NewNop(), "remember", nil, inProc)
	if err == nil {
		t.Fatal("expected an error when the data dir lock is held")
	}
	msg := err.Error()
	for _, want := range []string{"already open in another maz process", "maz daemon stop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing actionable hint %q", msg, want)
		}
	}
	if strings.HasPrefix(msg, "open wiring:") {
		t.Errorf("error should be actionable, not the raw %q", msg)
	}
}

// TestRunToolViaDaemonOrInProcess_NonLockOpenErrorSurfacedRaw guards the
// boundary: a genuine (non-lock) in-process open failure must NOT be masked by a
// daemon reroute, even when a daemon happens to be listening — otherwise a real
// corruption/migration error would be hidden behind a misleading lock hint.
func TestRunToolViaDaemonOrInProcess_NonLockOpenErrorSurfacedRaw(t *testing.T) {
	prevPre := preflightDaemon
	preflightDaemon = func(string, string) error { return nil }
	t.Cleanup(func() { preflightDaemon = prevPre })

	prevOpen := openWiringFn
	openWiringFn = func(context.Context, daemon.WiringConfig) (*daemon.Wired, error) {
		return nil, errors.New("migrate: schema version mismatch")
	}
	t.Cleanup(func() { openWiringFn = prevOpen })

	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return true } // a daemon is up, but the error is not a lock
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	prevInvoke := invokeToolViaDaemonFn
	invokeToolViaDaemonFn = func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("a non-lock open error must not reroute to the daemon")
		return nil, nil
	}
	t.Cleanup(func() { invokeToolViaDaemonFn = prevInvoke })

	_, err := runToolViaDaemonOrInProcess(context.Background(), &cobra.Command{},
		"/tmp/x", zap.NewNop(), "ask", nil,
		func(context.Context, *cobra.Command, *daemon.Wired, json.RawMessage) (json.RawMessage, error) {
			return nil, nil
		})
	if err == nil || !strings.Contains(err.Error(), "schema version mismatch") {
		t.Fatalf("want the raw open-wiring error surfaced, got %v", err)
	}
}

// TestDrainBackgroundVec_NilSafe pins that the drain helper is a no-op on the
// degenerate inputs a caller may pass when wiring failed to open, so it never
// panics on the close path.
func TestDrainBackgroundVec_NilSafe(t *testing.T) {
	t.Parallel()
	drainBackgroundVec(t.Context(), nil, nil)             // nil wired
	drainBackgroundVec(t.Context(), &daemon.Wired{}, nil) // wired with nil orchestrator
}

// TestDrainBackgroundVec_LiveWiringReturnsClean pins that draining a live
// wiring returns promptly (no in-flight embedding work under embed=none) and
// that Close still succeeds afterwards — the close path the materialising CLI
// write commands rely on. The behavioural proof that the drain *waits* for a
// real embedding before the vector pool tears down lives in the end-to-end
// smoke against a binary with Ollama; here we pin the wiring + no-op contract.
func TestDrainBackgroundVec_LiveWiringReturnsClean(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a real wiring (exclusive store lock) on a temp dir.
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}

	drainBackgroundVec(t.Context(), wired, nil) // must not hang or panic

	if cerr := wired.Close(t.Context()); cerr != nil {
		t.Fatalf("Close after drain: %v", cerr)
	}
}

// TestDrainAllBackground_NoHangOnIdleWiring verifies that drainAllBackground
// returns promptly when there is no in-flight background work — the happy
// path for a one-shot CLI write command that did not schedule extraction.
func TestDrainAllBackground_NoHangOnIdleWiring(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a real wiring (exclusive store lock) on a temp dir.
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}

	drainAllBackground(t.Context(), wired, nil, "test") // must not hang or panic

	if cerr := wired.Close(t.Context()); cerr != nil {
		t.Fatalf("Close after drain: %v", cerr)
	}
}

// TestDrainAllBackground_NilWiringIsNoop verifies that drainAllBackground does
// not panic when called with a nil *daemon.Wired (e.g. when OpenWiring failed
// on a partial-boot path).
func TestDrainAllBackground_NilWiringIsNoop(t *testing.T) {
	t.Parallel()
	// Must not panic.
	drainAllBackground(t.Context(), nil, nil, "test")
}

// TestSuppressCmdStderr_silencesDaemonAdvisory is the C1 regression: the
// --data-dir daemon-dispatch advisory must reach stderr for scripted callers
// but be silenced while a TUI (alt-screen) owns the terminal, then restored.
func TestSuppressCmdStderr_silencesDaemonAdvisory(t *testing.T) {
	prevInvoke := invokeToolViaDaemonFn
	invokeToolViaDaemonFn = func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	t.Cleanup(func() { invokeToolViaDaemonFn = prevInvoke })

	cmd := &cobra.Command{Use: "recall"}
	cmd.PersistentFlags().String("data-dir", "", "")
	if err := cmd.PersistentFlags().Set("data-dir", "/tmp/iso"); err != nil {
		t.Fatalf("set data-dir: %v", err)
	}
	var buf bytes.Buffer
	cmd.SetErr(&buf)

	ctx := context.Background()

	// Scripted path: the advisory IS written to stderr.
	if _, err := dispatchViaDaemon(ctx, cmd, "/tmp/iso", zap.NewNop(), "recall", nil, nil); err != nil {
		t.Fatalf("dispatchViaDaemon: %v", err)
	}
	if !strings.Contains(buf.String(), "--data-dir is ignored") {
		t.Fatalf("scripted path should warn; got %q", buf.String())
	}

	// TUI path: suppressed for the program's lifetime.
	buf.Reset()
	restore := suppressCmdStderr(cmd)
	if _, err := dispatchViaDaemon(ctx, cmd, "/tmp/iso", zap.NewNop(), "recall", nil, nil); err != nil {
		t.Fatalf("dispatchViaDaemon (suppressed): %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("advisory leaked into the alt-screen: %q", buf.String())
	}

	// After restore, stderr is wired back and the advisory warns again.
	restore()
	if _, err := dispatchViaDaemon(ctx, cmd, "/tmp/iso", zap.NewNop(), "recall", nil, nil); err != nil {
		t.Fatalf("dispatchViaDaemon (restored): %v", err)
	}
	if !strings.Contains(buf.String(), "--data-dir is ignored") {
		t.Errorf("restore() should re-wire stderr; got %q", buf.String())
	}
}
