package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/daemonclient"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// daemonLockHeldError signals a Mazarbul daemon process holds the
// exclusive on-disk locks for the data dir. CLI wrappers detect this
// to route through the daemon's MCP transport (or print an actionable
// message) instead of the cryptic ladybug "status 1" error.
type daemonLockHeldError struct {
	PID      int
	PIDPath  string
	StopHint string
}

func (e *daemonLockHeldError) Error() string {
	pidPart := "(pid unknown)"
	if e.PID > 0 {
		pidPart = fmt.Sprintf("(pid %d)", e.PID)
	}
	return fmt.Sprintf("the Mazarbul daemon %s is running and holds the database lock.\n\n"+
		"  • Stop the daemon to run this command in-process:\n"+
		"      %s\n\n"+
		"  • Or let this command reach the daemon over its Unix socket\n"+
		"    (mazarbul.sock in the data dir) — the default transport.",
		pidPart, e.StopHint)
}

// toolError signals that the daemon was reached and ran the tool, but the tool
// itself reported a failure (the MCP result had IsError set) — e.g. a plain
// E_NOT_FOUND for a UID that does not exist, or E_VALIDATION for a bad
// argument. This is categorically different from a transport/dial failure: the
// daemon is alive and answering, so callers MUST surface this error verbatim
// rather than the "daemon is running and holds the database lock" hint, which
// would tell the user to stop a perfectly healthy daemon to debug a simple
// not-found.
type toolError struct {
	Msg string
}

func (e *toolError) Error() string { return e.Msg }

// probeDaemonListening reports whether a daemon is accepting connections
// on the Unix socket for dataDir (mcp.DefaultUnixSocket(dataDir)). Used as
// a fallback when the PID file lookup misses — e.g. when MAZ_PIDFILE points
// elsewhere, or the calling process disagrees with the daemon on the PID
// path. The socket lives in the data dir (not $TMPDIR), so it is a stable
// liveness signal. A short connect timeout keeps this cheap on the common
// no-daemon path.
//
//nolint:gochecknoglobals // test seam for the loopback socket probe
var probeDaemonListening = func(dataDir string) bool {
	conn, err := net.DialTimeout("unix", mcp.DefaultUnixSocket(dataDir), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close() //nolint:errcheck // best-effort probe close
	return true
}

// preflightDaemon returns daemonLockHeldError when a live Mazarbul
// daemon owns the on-disk locks; returns nil when no daemon is
// running (so callers can fall through to in-process OpenWiring).
//
// Detection has two layers. Layer 1 reads the PID file at pidfilePath
// (empty string defaults to daemon.DefaultPath()) and confirms
// ownership via the token handshake. Layer 2 is the loopback TCP
// probe — required because daemon.DefaultPath() depends on $TMPDIR,
// which the calling process does not necessarily share with the
// daemon (Claude Desktop, cron, a different shell, etc.).
//
//nolint:gochecknoglobals // test seam: tests stub this to inject a fake pidfile state without writing real files
var preflightDaemon = func(pidfilePath, dataDir string) error {
	if pidfilePath == "" {
		pidfilePath = daemon.DataDirPIDPath(dataDir)
	}
	stopCmd := "maz daemon stop"
	if exe, eerr := os.Executable(); eerr == nil {
		stopCmd = exe + " daemon stop"
	}
	if info, err := daemon.ReadPID(pidfilePath); err == nil &&
		info.Running && daemon.IsMazDaemon(info.PID, info.Token) {
		// A live daemon owns the PID file. Only claim the lock when it
		// actually serves the requested data dir; otherwise --data-dir
		// points at a different store the daemon does not hold, so the
		// caller must run in-process against that dir (the F12 fix: a
		// daemon serving dir B must not answer reads for dir A).
		if daemonServesDataDir(info.DataDir, dataDir) {
			return &daemonLockHeldError{PID: info.PID, PIDPath: pidfilePath, StopHint: stopCmd}
		}
		return nil
	}
	// PID file unreadable/absent (e.g. a $TMPDIR mismatch between this
	// process and the daemon — Claude Desktop, cron, a different shell).
	// The loopback probe confirms a daemon is up but not which dir it
	// serves, so only claim when the requested dir is the default store a
	// daemon serves by default; an explicit non-default --data-dir runs
	// in-process rather than silently hitting the daemon's dir.
	if probeDaemonListening(dataDir) && isDefaultDataDir(dataDir) {
		return &daemonLockHeldError{PID: 0, PIDPath: pidfilePath, StopHint: stopCmd}
	}
	return nil
}

// openWiringFn is the in-process wiring constructor. A package-level var so
// tests can simulate open outcomes (success, a held-lock failure, a genuine
// non-lock failure) without touching disk.
//
//nolint:gochecknoglobals // test seam for in-process wiring open outcomes
var openWiringFn = daemon.OpenWiring

// invokeToolViaDaemonFn dials the running daemon and calls a tool. A
// package-level var so daemon-transport routing stays hermetic in tests.
//
//nolint:gochecknoglobals // test seam for the daemon MCP transport
var invokeToolViaDaemonFn = invokeToolViaDaemon

// daemonServesDataDir reports whether a daemon whose PID file recorded
// servedDir is serving the store at requestedDir. Both are normalised
// (abs + clean) before comparison. A daemon written before the data dir
// was recorded (servedDir == "") cannot be matched precisely, so it
// degrades to the default-dir heuristic: the store a daemon serves when
// started without an explicit --data-dir.
func daemonServesDataDir(servedDir, requestedDir string) bool {
	if servedDir == "" {
		return isDefaultDataDir(requestedDir)
	}
	return sameDir(servedDir, requestedDir)
}

// daemonServedDir reports the data dir the running daemon serves, read from its
// co-located PID file (DataDirPIDPath), and whether it could be determined. It
// lets dispatchViaDaemon decide whether the "--data-dir is ignored" warning is
// truthful: a daemon serving exactly the requested dir means the flag is
// effectively honored, so the warning would be a false alarm. Unreadable or a
// legacy PID file without a recorded dir yields ("", false) — caller warns.
func daemonServedDir(dataDir string) (string, bool) {
	info, err := daemon.ReadPID(daemon.DataDirPIDPath(dataDir))
	if err != nil || info.DataDir == "" {
		return "", false
	}
	return info.DataDir, true
}

// isDefaultDataDir reports whether dataDir is the convention default store —
// the dir a daemon serves when started with no explicit selection. It gates the
// loopback-probe fallback, which can confirm a daemon is up but not which dir it
// serves, so it must only ever claim that convention default.
//
// Because config.Defaults().DataDir folds in MAZ_DATA_DIR, a set env var makes
// "default" mean "the dir this process was told to use" — which a :7842 daemon
// may not serve at all. An env-selected store (eval harnesses, per-agent stores)
// is therefore treated as non-default so the probe never claims it; the PID-file
// dir match still routes the genuine shared-store case.
func isDefaultDataDir(dataDir string) bool {
	if os.Getenv("MAZ_DATA_DIR") != "" {
		return false
	}
	return sameDir(dataDir, config.Defaults().DataDir)
}

// sameDir compares two filesystem paths for equality after resolving each
// to an absolute, cleaned form. Falls back to a cleaned lexical compare
// when Abs fails (e.g. an unreadable working directory).
func sameDir(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return absA == absB
}

// invokeNamedTool looks up the tool by name on the wired tool slice and
// calls Invoke. Returns (nil, nil) when the tool is not registered, so
// callers can render their own "this tool is not available" message.
func invokeNamedTool(ctx context.Context, wired *daemon.Wired, name string, req json.RawMessage) (json.RawMessage, error) {
	for _, t := range wired.Tools {
		if t.Name() == name {
			return t.Invoke(ctx, req)
		}
	}
	return nil, nil
}

// inProcessToolFn is the existing in-process invocation signature shared
// by remember/recall/forget/ask: it opens a wired daemon, finds the
// named tool, calls Invoke. The wrapper picks the right path: route via
// the daemon's MCP transport when one is running, fall back to
// in-process otherwise.
type inProcessToolFn func(ctx context.Context, cmd *cobra.Command, wired *daemon.Wired, reqBody json.RawMessage) (json.RawMessage, error)

// runToolViaDaemonOrInProcess is the single entry every read-write CLI
// command should use to dispatch into either:
//
//	(a) the running daemon's streamable HTTP MCP transport, OR
//	(b) an in-process OpenWiring + tool.Invoke path.
//
// (a) wins whenever a live Mazarbul daemon is detected via the
// platform pidfile. This avoids the cryptic "ladybug status 1" failure
// the user used to see when issuing `maz remember` while their hub or
// `maz daemon start` was already holding the LadybugDB lock.
func runToolViaDaemonOrInProcess(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	toolName string,
	reqBody json.RawMessage,
	inProc inProcessToolFn,
) (json.RawMessage, error) {
	// Data-dir-aware (stack #181): preflightDaemon only claims the lock when a
	// live daemon actually serves dataDir; an explicit non-default --data-dir
	// otherwise falls through to in-process. When it does claim, route through
	// dispatchViaDaemon (#178) so a tool-level failure surfaces verbatim instead
	// of the misleading daemon-lock hint.
	if perr := preflightDaemon("", dataDir); perr != nil {
		return dispatchViaDaemon(ctx, cmd, dataDir, logger, toolName, reqBody, perr)
	}

	wired, err := openWiringFn(ctx, daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		// preflightDaemon reported no daemon, yet the open failed because the
		// database lock is held AND a daemon is listening right now: preflight
		// raced the loopback probe (its pidfile lives under a different $TMPDIR —
		// Claude Desktop, cron, a foreign shell). A held lock is stronger evidence
		// of a live daemon than the probe, so route through the daemon transport
		// instead of surfacing the cryptic "open database with status 1" the user
		// used to hit intermittently on `ask`. See UX-5.
		if isDatabaseLockHeld(err) && probeDaemonListening(dataDir) {
			// Benign: the daemon's socket lives at <dataDir>/mazarbul.sock, so the
			// daemon that holds the lock owns THIS data dir — routing to it returns
			// the requested store's results. Debug, not Warn: nothing failed.
			logger.Debug("in-process open yielded to the Mazarbul daemon that owns this data dir; routing via its MCP transport (results unaffected)",
				zap.String("tool", toolName), zap.Error(err))
			return dispatchViaDaemon(ctx, cmd, dataDir, logger, toolName, reqBody, newDaemonLockHint())
		}
		// The lock is held but nothing is listening on the data dir's socket —
		// e.g. a `maz mcp serve` over stdio (Claude Code) owns the LadybugDB lock
		// without binding a socket. There is no transport to reroute to, so name
		// the actual holder and the fix instead of leaking the opaque C "status 1"
		// (and instead of misdirecting the user to `maz daemon stop`, which cannot
		// release a stdio serve's lock).
		if isDatabaseLockHeld(err) {
			return nil, fmt.Errorf("%s: %w", dataDirLockedHint(dataDir), err)
		}
		return nil, fmt.Errorf("open wiring: %w", err)
	}
	defer func() {
		// Drain async extraction + resolution before tearing down the
		// dispatcher pools. WaitForBackgroundWork covers the FileEventLog's
		// bgWork WaitGroup (extraction/resolution goroutines spawned by
		// Append); drainBackgroundVec covers the dispatcher's bgVec
		// WaitGroup (entity/mention/episode embedding goroutines). Without
		// this drain a one-shot `maz remember` exits before extraction
		// completes, the entity embed goroutines outlive the dispatcher
		// close, and the vec upserts fail with "dispatcher: closed".
		drainAllBackground(ctx, wired, logger, toolName)
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("close wiring",
				zap.String("tool", toolName), zap.Error(cerr))
		}
	}()
	return inProc(ctx, cmd, wired, reqBody)
}

// dispatchViaDaemon routes a tool call through the running daemon's streamable
// HTTP MCP transport. The hint argument is the actionable daemon-lock message
// surfaced only if the transport itself is unreachable (a genuine failure); a
// tool-level error is surfaced verbatim by resolveDaemonDispatchError.
func dispatchViaDaemon(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	toolName string,
	reqBody json.RawMessage,
	hint error,
) (json.RawMessage, error) {
	// A running daemon serves this command from ITS data dir, so an explicit
	// --data-dir is silently ignored on the daemon path. Warn (to stderr, never
	// the machine-readable stdout) so an agent that set --data-dir expecting an
	// isolated store learns it hit the daemon instead. BUT preflightDaemon only
	// routes here after matching the daemon's PID-file data dir to --data-dir, so
	// in the common case the daemon serves EXACTLY this dir and the flag is
	// effectively honored — a "--data-dir is ignored" warning there is a false
	// alarm that erodes trust (every usability tester flagged it). Suppress it on
	// a confirmed match; warn only when the served dir is unknown or differs, and
	// name the actual served dir so the message informs rather than alarms.
	if cmd.Root().PersistentFlags().Changed("data-dir") {
		served, known := daemonServedDir(dataDir)
		if !known || !sameDir(served, dataDir) {
			servedDesc := "the daemon's data dir"
			if known {
				servedDesc = fmt.Sprintf("the daemon's data dir (%s)", served)
			}
			//nolint:errcheck // best-effort advisory on stderr
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: --data-dir is ignored for %q while a Mazarbul daemon is running; "+
					"the command is served from %s. Stop the daemon (maz daemon stop) "+
					"to run in-process against --data-dir.\n", toolName, servedDesc)
		}
	}
	out, derr := invokeToolViaDaemonFn(ctx, dataDir, toolName, reqBody)
	if derr == nil {
		return out, nil
	}
	return nil, resolveDaemonDispatchError(derr, hint, logger, toolName)
}

// suppressCmdStderr redirects cmd's stderr to io.Discard and returns a function
// that restores the previous writer. Wrap an interactive Bubble Tea program
// with it: the daemon-dispatch advisory (and any other stderr write) fires on
// every tool call — including each debounced per-keystroke re-query — and a
// single byte written to the TTY while the alt-screen owns it corrupts the
// frame (wraps rows, evicts content, punches through borders). The advisory is
// only useful to scripted callers, so dropping it for the TUI's lifetime is
// correct; restore stderr before reporting any post-TUI error.
func suppressCmdStderr(cmd *cobra.Command) func() {
	prev := cmd.ErrOrStderr()
	cmd.SetErr(io.Discard)
	return func() { cmd.SetErr(prev) }
}

// isDatabaseLockHeld reports whether err is the LadybugDB "lock already held"
// open failure — the signature emitted when another process (almost always the
// Mazarbul daemon) owns the graph database file.
func isDatabaseLockHeld(err error) bool {
	return err != nil && strings.Contains(err.Error(), "failed to open database with status 1")
}

// dataDirLockedHint builds an accurate, actionable message for a held data-dir
// lock. When a daemon is serving the socket, `maz daemon stop` is the fix.
// Otherwise the holder is named — typically a standalone `maz mcp serve` an
// editor or Claude Code spawned, which `maz daemon stop` will NOT release — so
// the user is steered to the real culprit instead of being told to stop a
// daemon that isn't the problem (the exact dead end the user hit).
func dataDirLockedHint(dataDir string) string {
	if probeDaemonListening(dataDir) {
		return fmt.Sprintf(
			"data dir %s is in use by the running Mazarbul daemon; stop it with `maz daemon stop` (or run against a different --data-dir)",
			dataDir)
	}
	if holder := lockHolderDescriber(dataDir); holder != "" {
		return fmt.Sprintf(
			"data dir %s is already open in another maz process: %s. "+
				"That is most often the `maz mcp serve` an editor or coding agent auto-starts, but it can also be another `maz` command of yours still finishing its async reconcile, or a stray serve. "+
				"Check the named PID/command above: if it is an editor/agent serve, quit it (or `kill` the PID); if it is a transient command, wait a moment and retry. `maz daemon stop` only helps if the holder is the background daemon",
			dataDir, holder)
	}
	return fmt.Sprintf(
		"data dir %s is already open in another maz process (no daemon socket is responding) — most likely a `maz mcp serve` an editor/agent auto-started, or a previous `maz` command of yours still finishing. "+
			"Wait a moment and retry; if it persists, find and quit that process. `maz daemon stop` only helps if the holder is the background daemon",
		dataDir)
}

// lockHolderDescriber is the seam tests stub so they never shell out to lsof.
// Production code must not reassign it.
var lockHolderDescriber = describeDataDirLockHolder //nolint:gochecknoglobals

// describeDataDirLockHolder makes a best-effort, dependency-free attempt to name
// the process holding dataDir's LadybugDB lock. The single-opener lock is most
// often grabbed by a standalone `maz mcp serve`, so naming the PID + command
// line turns the opaque "status 1" into something the user can act on. Returns
// "" when lsof is unavailable or finds nothing, so callers fall back to a
// generic hint. Best-effort and bounded: it runs on an error path only.
func describeDataDirLockHolder(dataDir string) string {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	target := filepath.Join(dataDir, "graph.ladybug")
	// +D recurses when target is a directory (LadybugDB's on-disk form); for a
	// plain file lsof still reports the holder. -t prints bare PIDs.
	pids := lsofPIDs(ctx, lsof, "-t", "+D", target)
	if len(pids) == 0 {
		pids = lsofPIDs(ctx, lsof, "-t", "--", target)
	}
	seen := map[string]bool{strconv.Itoa(os.Getpid()): true}
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		if seen[pid] {
			continue
		}
		seen[pid] = true
		parts = append(parts, "PID "+pid+lockHolderCommand(ctx, pid))
	}
	return strings.Join(parts, ", ")
}

// lsofPIDs runs lsof with args and returns the bare PIDs it printed (empty on
// any error, including "no holder found", which lsof reports via exit code 1).
func lsofPIDs(ctx context.Context, lsof string, args ...string) []string {
	out, err := exec.CommandContext(ctx, lsof, args...).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// lockHolderCommand returns " (<command>)" for pid, or "" when ps can't read it.
func lockHolderCommand(ctx context.Context, pid string) string {
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", pid).Output()
	if err != nil {
		return ""
	}
	cmdline := strings.TrimSpace(string(out))
	if cmdline == "" {
		return ""
	}
	const maxLen = 72
	if len(cmdline) > maxLen {
		cmdline = cmdline[:maxLen-1] + "…"
	}
	return " (" + cmdline + ")"
}

// newDaemonLockHint builds the actionable "a daemon holds the lock" error used
// when an in-process open loses the lock race but the daemon transport is then
// found unreachable. The PID is unknown here (preflight's pidfile lookup
// missed), so it renders as "(pid unknown)".
func newDaemonLockHint() *daemonLockHeldError {
	stopCmd := "maz daemon stop"
	if exe, eerr := os.Executable(); eerr == nil {
		stopCmd = exe + " daemon stop"
	}
	return &daemonLockHeldError{PID: 0, StopHint: stopCmd}
}

// resolveDaemonDispatchError decides which error a daemon-routed CLI command
// surfaces when a daemon was detected (perr, the daemon-lock hint) but the tool
// call returned derr.
//
//   - A tool-level failure (*toolError, e.g. E_NOT_FOUND / E_VALIDATION) means
//     the daemon answered and the tool said no — surface it verbatim. The lock
//     hint would be actively misleading: it tells the user to stop a healthy
//     daemon to debug what is really a missing record or a bad argument.
//   - Anything else is a genuine transport failure (a daemon was detected via
//     pidfile or loopback probe, but the HTTP MCP endpoint is unreachable) —
//     surface the actionable preflight hint after logging the underlying cause.
func resolveDaemonDispatchError(derr, perr error, logger *zap.Logger, toolName string) error {
	var te *toolError
	if errors.As(derr, &te) {
		return derr
	}
	logger.Warn("daemon MCP transport unreachable; surfacing preflight hint",
		zap.String("tool", toolName), zap.Error(derr))
	return perr
}

// openMaterializingLog opens the dispatcher-backed wiring whose appends are
// applied into the SQLite + graph projection synchronously, so a daemon-less
// CLI authoring command (schema kinds add / predicates add / alias / install)
// is queryable the instant it returns — no `maz rebuild` needed. This is the
// same path the daemon and `maz mcp serve` use, in contrast to
// kernelboot.OpenLog, which appends a durable but un-materialised event.
//
// Append the event through the returned wiring's Orchestrator.Log, then confirm
// the row landed with confirmMaterialised before reporting success:
// FileEventLog.Append keeps the JSONL durable but warn-logs and swallows
// storage.Apply failures, so a write whose materialisation failed would
// otherwise exit 0 with the row still unqueryable — the very silent-until-rebuild
// mode this path closes.
//
// It refuses with an actionable message when a daemon already serves dataDir:
// the daemon holds the exclusive store lock, so an in-process open would race
// it; the write must go through the daemon (stop it, or use the MCP authoring
// tool). The opName argument prefixes that message (e.g. "schema kinds add").
// The returned closeFn must be called to release the store lock.
func openMaterializingLog(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	opName string,
) (*daemon.Wired, func() error, error) {
	if perr := preflightDaemon("", dataDir); perr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", opName, perr)
		return nil, nil, silentError{code: 1, err: perr}
	}
	wired, err := daemon.OpenWiring(ctx, daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: open wiring: %v\n", opName, err)
		return nil, nil, silentError{code: 2, err: err}
	}
	return wired, func() error {
		drainAllBackground(ctx, wired, logger, opName)
		return wired.Close(ctx)
	}, nil
}

// drainAllBackground waits for both async extraction/resolution (FileEventLog
// bgWork WaitGroup) and async vector embedding (dispatcher bgVec WaitGroup)
// to complete before the wiring is torn down. This is the correct drain for
// any one-shot CLI write command (e.g. maz remember) that schedules LLM
// extraction after Append returns.
//
// Without this drain:
//   - Extraction goroutines may still be running when the dispatcher closes,
//     causing entity events to be written but their vector embeddings to fail
//     with "dispatcher: closed".
//   - The user sees a spurious WARN line on an otherwise-successful command.
//
// Both waits are bounded independently (90 s extraction, 30 s vec) so a
// stalled LLM cannot hang the command indefinitely. Written rows are durable
// in the event log regardless; a rebuild regenerates the vectors.
//
// The opName argument is used only for debug logging.
func drainAllBackground(ctx context.Context, wired *daemon.Wired, logger *zap.Logger, opName string) {
	if wired == nil || wired.Orchestrator == nil {
		return
	}
	// Phase 1: wait for async extraction + resolution. These goroutines are
	// tracked by FileEventLog.bgWork; they write derived Entity/Relation
	// events back into the log which in turn schedules bgVec goroutines.
	// Must complete before we drain bgVec, otherwise new vec work can arrive
	// after the vec drain finishes but before Close.
	drainExtrCtx, cancelExtraction := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancelExtraction()
	if err := wired.Orchestrator.Log.WaitForBackgroundWork(drainExtrCtx); err != nil && logger != nil {
		logger.Debug("cli: background extraction drain timed out before close",
			zap.String("op", opName), zap.Error(err))
	}
	// Phase 2: wait for async vector embedding goroutines (entity/mention/
	// episode embed + upsert into sqlite-vec).
	drainVecCtx, cancelVec := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelVec()
	disp, ok := wired.Orchestrator.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return
	}
	if err := disp.WaitForBackgroundVec(drainVecCtx); err != nil && logger != nil {
		logger.Debug("cli: background vector drain timed out before close",
			zap.String("op", opName), zap.Error(err))
	}
}

// drainBackgroundVec waits for the dispatcher's in-flight background embedding
// goroutines (the entity / mention / episode vector upserts that Apply
// schedules asynchronously) to finish before the wiring is torn down. A
// one-shot CLI write would otherwise close the SQLite + vector pool out from
// under those workers, which both drops the embedding — the entity stays
// unsearchable by the vector lane until the next `maz rebuild` — and logs a
// spurious "dispatcher: closed" warning on an otherwise successful command.
//
// The wait is bounded so a stalled embedder (e.g. Ollama unreachable) cannot
// hang the command: the written row is already durable in the event log and a
// rebuild regenerates the vectors. The drain is a cheap no-op when no async
// work is in flight (the WaitGroup counter is zero), so ontology-only writes
// that schedule no embeddings pay nothing.
func drainBackgroundVec(ctx context.Context, wired *daemon.Wired, logger *zap.Logger) {
	if wired == nil || wired.Orchestrator == nil {
		return
	}
	disp, ok := wired.Orchestrator.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return
	}
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := disp.WaitForBackgroundVec(drainCtx); err != nil && logger != nil {
		logger.Debug("cli: background vector drain did not finish before close", zap.Error(err))
	}
}

// confirmMaterialised converts a swallowed storage.Apply failure into an
// actionable non-zero exit. FileEventLog.Append keeps the JSONL durable but
// warn-logs and drops Apply failures, so without this an authoring command
// could exit 0 with its row absent from the query projection. Each check runs a
// SELECT COUNT query against the just-written wiring's SQLite reader (the same
// locked connection, so no second store lock is taken); a zero count means the
// append did not materialise.
func confirmMaterialised(ctx context.Context, cmd *cobra.Command, wired *daemon.Wired, opName string, checks ...matCheck) error {
	db, err := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: verify materialisation: %v\n", opName, err)
		return silentError{code: 2, err: err}
	}
	for _, c := range checks {
		var n int
		if qerr := db.QueryRowContext(ctx, c.query, c.args...).Scan(&n); qerr != nil {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: verify materialisation: %v\n", opName, qerr)
			return silentError{code: 2, err: qerr}
		}
		if n == 0 {
			verr := fmt.Errorf("%q committed to the durable log but did not materialise into the query projection; run `maz rebuild --yes` to reconcile", c.label)
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", opName, verr)
			return silentError{code: 2, err: verr}
		}
	}
	return nil
}

// matCheck is one read-back probe for confirmMaterialised: a count query whose
// args bind the just-written row's key, plus a human label for the error.
type matCheck struct {
	query string
	args  []any
	label string
	// confirm controls what confirmMaterialised / the admin endpoint asserts.
	// "present" (or empty) requires count > 0; "absent" requires count == 0.
	// Use "absent" for tombstone checks (e.g. preset uninstall).
	confirm string
}

// confirmAbsent is the inverse of confirmMaterialised: it fails when a row that
// should have been DELETED is still present in the query projection. A
// delete-then-verify command (e.g. `maz preset uninstall`) uses it to catch a
// tombstone event that committed to the durable log but did not materialise the
// deletion. A non-zero count means the projection still holds the row.
func confirmAbsent(ctx context.Context, cmd *cobra.Command, wired *daemon.Wired, opName string, checks ...matCheck) error {
	db, err := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: verify removal: %v\n", opName, err)
		return silentError{code: 2, err: err}
	}
	for _, c := range checks {
		var n int
		if qerr := db.QueryRowContext(ctx, c.query, c.args...).Scan(&n); qerr != nil {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: verify removal: %v\n", opName, qerr)
			return silentError{code: 2, err: qerr}
		}
		if n != 0 {
			verr := fmt.Errorf("%q committed to the durable log but its row did not clear from the query projection; run `maz rebuild --yes` to reconcile", c.label)
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", opName, verr)
			return silentError{code: 2, err: verr}
		}
	}
	return nil
}

// invokeToolViaDaemon dials the running daemon's streamable HTTP MCP
// endpoint and calls the named tool with the given JSON input. Returns
// the raw tool result (structuredContent if present, else the first
// text block) as json.RawMessage so callers can reuse their existing
// in-process renderers.
func invokeToolViaDaemon(ctx context.Context, dataDir, toolName string, input json.RawMessage) (json.RawMessage, error) {
	c, err := dialDaemonMCP(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { c.Close() }() //nolint:errcheck // best-effort close

	args, err := decodeToolArgs(input)
	if err != nil {
		return nil, err
	}

	// 5-minute ceiling. The `ask` tool is bounded by the daemon's own
	// composer/LLM timeout (~2 min today), but cold-start can push it
	// past a 60s budget — qwen2.5:7b's first eval after Ollama wake
	// regularly costs 30-50s of model-load on top of the actual
	// compose. Keep this generous; the daemon side has its own caps.
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := c.CallTool(callCtx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: toolName, Arguments: args},
	})
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", toolName, err)
	}
	return extractToolResult(res)
}

func dialDaemonMCP(ctx context.Context, dataDir string) (*mcpclient.Client, error) {
	// Reach the daemon over its Unix socket — the default transport. The
	// URL host is a placeholder; the socket dialer ignores it. The socket's
	// 0600 permissions are the credential, so no bearer token is needed.
	socket := mcp.DefaultUnixSocket(dataDir)
	c, err := mcpclient.NewStreamableHttpClient(
		"http://unix/mcp",
		transport.WithHTTPBasicClient(mcp.UnixHTTPClient(socket, 0)),
	)
	if err != nil {
		return nil, fmt.Errorf("mcp unix client: %w", err)
	}
	startCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Start(startCtx); err != nil {
		c.Close() //nolint:errcheck // closing on the error path; the inner error is more interesting
		return nil, fmt.Errorf("mcp http start: %w", err)
	}
	initCtx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	if _, err := c.Initialize(initCtx, mcpgo.InitializeRequest{
		Params: mcpgo.InitializeParams{
			ProtocolVersion: mcpgo.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcpgo.Implementation{Name: "maz-cli", Version: "dev"},
		},
	}); err != nil {
		c.Close() //nolint:errcheck // closing on the error path; the inner error is more interesting
		return nil, fmt.Errorf("mcp http initialize: %w", err)
	}
	return c, nil
}

func decodeToolArgs(input json.RawMessage) (map[string]any, error) {
	if len(input) == 0 {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("decode tool input: %w", err)
	}
	return args, nil
}

func extractToolResult(res *mcpgo.CallToolResult) (json.RawMessage, error) {
	if res.IsError {
		var sb strings.Builder
		for _, b := range res.Content {
			if tb, ok := b.(mcpgo.TextContent); ok {
				sb.WriteString(tb.Text)
			}
		}
		msg := sb.String()
		if msg == "" {
			msg = "tool returned IsError with no message"
		}
		// A tool-level failure (the daemon answered, the tool said no). Typed
		// so the dispatch wrapper surfaces it verbatim instead of the
		// daemon-lock hint — see runToolViaDaemonOrInProcess.
		return nil, &toolError{Msg: msg}
	}
	if res.StructuredContent != nil {
		out, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("marshal structuredContent: %w", err)
		}
		return out, nil
	}
	for _, b := range res.Content {
		if tb, ok := b.(mcpgo.TextContent); ok {
			return json.RawMessage(tb.Text), nil
		}
	}
	return json.RawMessage(`{}`), nil
}

// appendBatchViaDaemonOrInProcess is the single dispatch point for CLI
// authoring commands that build a batch of events and need to append them
// durably. It routes the write through the live daemon's privileged admin
// endpoint when one is running, and falls back to the in-process
// openMaterializingLog path otherwise — so the same batch and check logic
// drives both paths without divergence.
//
// The trust gate (pack resolution, checksum verification, human confirmation)
// MUST run in the caller before this function is invoked. The daemon only
// receives a pre-approved batch.
//
// OpName is used in error messages (e.g. "preset install", "schema alias").
// Precheck entries are evaluated atomically BEFORE the append (idempotency
// guard); checks entries are evaluated AFTER to confirm materialization.
func appendBatchViaDaemonOrInProcess(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	opName string,
	batch []kernel.Event,
	precheck []matCheck,
	checks []matCheck,
) error {
	if perr := preflightDaemon("", dataDir); perr != nil {
		return appendViaDaemon(ctx, cmd, dataDir, logger, opName, perr, batch, precheck, checks)
	}
	return appendInProcess(ctx, cmd, dataDir, logger, opName, batch, checks)
}

// appendViaDaemon routes a batch through the running daemon's admin endpoint.
// The perr argument is the non-nil result of preflightDaemon — used for the
// lock hint message when the admin transport is unreachable.
func appendViaDaemon(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	opName string,
	perr error,
	batch []kernel.Event,
	precheck []matCheck,
	checks []matCheck,
) error {
	c, cerr := daemonclient.New(dataDir)
	if cerr != nil {
		logger.Warn("appendBatchViaDaemonOrInProcess: build admin client failed",
			zap.String("op", opName), zap.Error(cerr))
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", opName, perr)
		return silentError{code: 1, err: perr}
	}

	req := &daemon.AppendEventsRequest{
		Events:   batch,
		Precheck: matChecksToWire(precheck),
		Checks:   matChecksToWire(checks),
	}
	resp, aerr := c.AppendEvents(ctx, req)
	if aerr == nil {
		if resp.AlreadyPresent {
			logger.Debug("appendBatchViaDaemonOrInProcess: batch already present; no-op",
				zap.String("op", opName))
		}
		return nil
	}

	return mapDaemonAppendError(cmd, opName, perr, aerr)
}

// mapDaemonAppendError translates a daemon AppendEvents error to a CLI exit
// code. The perr argument is the preflightDaemon result used for the lock hint.
func mapDaemonAppendError(cmd *cobra.Command, opName string, perr, aerr error) error {
	var httpErr *daemonclient.HTTPError
	if errors.As(aerr, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusBadRequest:
			// 400 INVALID_ARGUMENT: the batch or a security guard rejected it.
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", opName, aerr)
			return silentError{code: 1, err: aerr}
		case http.StatusUnprocessableEntity:
			// 422 FAILED_PRECONDITION: appended but did not materialise.
			hint := fmt.Errorf("%s: committed but did not materialise; run `maz rebuild --yes`", opName)
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "%v\n", hint)
			return silentError{code: 2, err: hint}
		}
	}
	// Transport failure: daemon detected but admin path unreachable.
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", opName, perr)
	return silentError{code: 1, err: perr}
}

// appendInProcess opens the in-process materialising log and appends batch.
func appendInProcess(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	opName string,
	batch []kernel.Event,
	checks []matCheck,
) error {
	wired, closeFn, oerr := openMaterializingLog(ctx, cmd, dataDir, logger, opName)
	if oerr != nil {
		return oerr
	}
	defer func() {
		drainAllBackground(ctx, wired, logger, opName)
		if cerr := closeFn(); cerr != nil {
			logger.Warn(opName+": close wiring", zap.Error(cerr))
		}
	}()

	if _, aerr := wired.Orchestrator.Log.Append(ctx, batch); aerr != nil {
		if errors.Is(aerr, kernel.ErrInvalidEvent) {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: invalid event in batch: %v\n", opName, aerr)
			return silentError{code: 1, err: aerr}
		}
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: append batch: %v\n", opName, aerr)
		return silentError{code: 2, err: aerr}
	}

	return runInProcessChecks(ctx, cmd, wired, opName, checks)
}

// runInProcessChecks splits post-append checks by confirm type and runs each
// group through the appropriate helper.
func runInProcessChecks(ctx context.Context, cmd *cobra.Command, wired *daemon.Wired, opName string, checks []matCheck) error {
	var presentChecks, absentChecks []matCheck
	for _, c := range checks {
		if c.confirm == "absent" {
			absentChecks = append(absentChecks, c)
		} else {
			presentChecks = append(presentChecks, c)
		}
	}
	if len(presentChecks) > 0 {
		if mcerr := confirmMaterialised(ctx, cmd, wired, opName, presentChecks...); mcerr != nil {
			return mcerr
		}
	}
	if len(absentChecks) > 0 {
		if mcerr := confirmAbsent(ctx, cmd, wired, opName, absentChecks...); mcerr != nil {
			return mcerr
		}
	}
	return nil
}

// matChecksToWire converts the CLI-internal []matCheck slice to the
// []daemon.MatCheckWire wire type used by the admin endpoint. The confirm
// field defaults to "present" when empty so existing callers that only use
// confirmMaterialised (not confirmAbsent) work without change.
func matChecksToWire(checks []matCheck) []daemon.MatCheckWire {
	if len(checks) == 0 {
		return nil
	}
	out := make([]daemon.MatCheckWire, len(checks))
	for i, c := range checks {
		confirm := c.confirm
		if confirm == "" {
			confirm = "present"
		}
		out[i] = daemon.MatCheckWire{
			Query:   c.query,
			Args:    c.args,
			Label:   c.label,
			Confirm: confirm,
		}
	}
	return out
}
