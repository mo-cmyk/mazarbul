package cli

// hook.go — `maz hook <event>`: lifecycle-hook entry points a client (Claude
// Code) invokes on session events. Two subcommands:
//
//   - `stop`          passive capture. Reads Claude Code's Stop-hook JSON on
//                     stdin, distills the new transcript turns since the last
//                     fire (per-session cursor), and writes them as a signed
//                     `remember` episode (actor=assistant). Stop fires every
//                     turn, so capture is a delta, not the whole transcript.
//   - `session-start` warm-start injection. With --warm-start, emits a
//                     scope-filtered context brief (reusing getContext) as
//                     Claude Code's hookSpecificOutput.additionalContext.
//
// Both are strictly FAIL-OPEN: every invocation exits 0 and never blocks the
// session, even when the daemon is down, stdin is malformed, or the transcript
// is missing. A hook that returned non-zero (exit 2) would block the Stop or
// surface an error banner — unacceptable for an ambient, always-on capture.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

const (
	// Capture-dispatch deadline: a slow in-process open (daemon down) must not
	// stall the session past a few seconds. The common daemon-up path is a
	// sub-second HTTP call.
	stopHookDeadline = 5 * time.Second
	// Warm-start brief-fetch deadline. The getContext read is cheap in-process,
	// so this only caps a pathological open.
	sessionStartHookDeadline = 5 * time.Second
)

// newHookCmd is the `maz hook` group. Bare `maz hook` shows help.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Lifecycle-hook entry points for passive capture and warm-start",
		Long: `Entry points a client's lifecycle hooks invoke on session events.

These commands read a hook-event JSON payload on stdin and are FAIL-OPEN: they
always exit 0 and never block the session, even when the Mazarbul daemon is
down. Install them with 'maz connect claude-code --capture --warm-start'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newHookStopCmd())
	cmd.AddCommand(newHookSessionStartCmd())
	cmd.AddCommand(newHookStatusCmd())
	return cmd
}

// newHookStopCmd is `maz hook stop` — the Claude Code Stop-hook capture entry.
func newHookStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Capture new session turns into signed memory (Claude Code Stop hook)",
		Long: `Read a Claude Code Stop-hook payload on stdin and capture the transcript
turns added since the last fire as a signed assistant episode
(source=hook:claude-code/stop). Fail-open: always exits 0.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runHookStop(cmd)
			return nil // fail-open: a Stop hook must never block the session.
		},
	}
	return cmd
}

// newHookSessionStartCmd is `maz hook session-start` — the warm-start entry.
func newHookSessionStartCmd() *cobra.Command {
	var (
		warmStart bool
		scopes    []string
	)
	cmd := &cobra.Command{
		Use:   "session-start",
		Short: "Inject a scope-filtered context brief at session start (Claude Code SessionStart hook)",
		Long: `Read a Claude Code SessionStart payload on stdin and, when --warm-start is
set, emit a scope-filtered context brief as hookSpecificOutput.additionalContext.
Injection is opt-in; without --warm-start this is a no-op. Fail-open: always
exits 0 and emits nothing when the brief is unavailable.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runHookSessionStart(cmd, warmStart, scopes)
			return nil // fail-open
		},
	}
	cmd.Flags().BoolVar(&warmStart, "warm-start", false, "emit the context brief (injection is opt-in)")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	return cmd
}

// runHookStop performs the delta capture. Every failure is logged to stderr
// (captured by the client's hook debug log) and swallowed; the function never
// returns an error.
func runHookStop(cmd *cobra.Command) {
	logger := logging.FromContext(cmd.Context())
	// Belt-and-suspenders fail-open: a panic anywhere below (a malformed
	// transcript record, a wiring bug) must not propagate out of RunE and exit
	// the process non-zero, which would surface an error in the session.
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("hook stop: recovered from panic", zap.Any("panic", r))
		}
	}()

	payload, err := decodeStopPayload(cmd.InOrStdin())
	if err != nil {
		logger.Warn("hook stop: decode payload", zap.Error(err))
		return
	}
	if payload.StopHookActive {
		// Re-entrancy: a prior Stop hook already blocked this turn. We never
		// block, so there is nothing to do.
		return
	}
	if payload.TranscriptPath == "" {
		return
	}
	if payload.SessionID == "" {
		// Without a session id we cannot persist a per-session cursor, so every
		// fire would re-dispatch the full transcript tail (duplicate captures).
		// Claude Code always sends one; a payload without it is malformed —
		// skip rather than capture unbounded duplicates.
		logger.Warn("hook stop: payload has no session_id; skipping capture")
		return
	}

	dataDir := resolveHookDataDir(cmd)
	text, newOffset, err := captureDelta(dataDir, payload.SessionID, payload.TranscriptPath)
	if err != nil {
		logger.Warn("hook stop: read transcript delta", zap.Error(err))
		return
	}
	if strings.TrimSpace(text) == "" {
		// No conversational turns in the delta (only tool results, or an
		// unterminated trailing line still being written). Write the cursor at
		// newOffset: for tool-result-only deltas this advances past them so they
		// are not re-scanned; for an unterminated record newOffset equals the
		// prior cursor, so the write is a no-op and the record is retried next
		// fire once it completes.
		if cerr := writeCursor(dataDir, payload.SessionID, newOffset); cerr != nil {
			logger.Warn("hook stop: advance cursor", zap.Error(cerr))
		}
		return
	}

	body, err := buildHookRememberRequest(text)
	if err != nil {
		logger.Warn("hook stop: build request", zap.Error(err))
		return
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), stopHookDeadline)
	defer cancel()
	raw, derr := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger, "remember", body, invokeRememberQuiet)
	if derr != nil || raw == nil {
		// Leave the cursor unadvanced so the delta is retried on the next Stop
		// (e.g. once the daemon is back up / memory is initialised). Bounded by
		// the read cap. raw==nil means the remember tool is not registered
		// (minimal boot) — also a retry, not a drop.
		if derr != nil {
			logger.Warn("hook stop: capture failed; will retry next turn", zap.Error(derr))
		}
		return
	}
	if cerr := writeCursor(dataDir, payload.SessionID, newOffset); cerr != nil {
		logger.Warn("hook stop: advance cursor", zap.Error(cerr))
	}
}

// invokeRememberQuiet locates the remember tool and invokes it without the
// stderr diagnostics the interactive `maz remember` path emits — a Stop hook
// runs every turn, so a missing/erroring tool must not spam the session log.
// Returns (nil, nil) when the tool is not registered (minimal boot).
func invokeRememberQuiet(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, reqBody json.RawMessage) (json.RawMessage, error) {
	return invokeNamedTool(ctx, wired, "remember", reqBody)
}

// runHookSessionStart fetches and emits the warm-start brief. Fail-open: any
// error results in no stdout and exit 0.
func runHookSessionStart(cmd *cobra.Command, warmStart bool, scopes []string) {
	logger := logging.FromContext(cmd.Context())
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("hook session-start: recovered from panic", zap.Any("panic", r))
		}
	}()
	if !warmStart {
		return // injection is opt-in
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), sessionStartHookDeadline)
	defer cancel()
	md, err := fetchWarmStartBrief(ctx, cmd, scopes)
	if err != nil {
		logger.Warn("hook session-start: brief unavailable", zap.Error(err))
		return
	}
	if strings.TrimSpace(md) == "" {
		return // nothing to inject
	}
	if eerr := emitSessionStartContext(cmd.OutOrStdout(), md); eerr != nil {
		logger.Warn("hook session-start: emit context", zap.Error(eerr))
	}
}

// fetchWarmStartBrief invokes getContext (daemon-or-in-process) and returns the
// assembled markdown. Reuses the same contextbrief assembly behind getContext —
// no second assembly path. Returns "" with nil error when the tool is not
// registered (minimal boot).
func fetchWarmStartBrief(ctx context.Context, cmd *cobra.Command, scopes []string) (string, error) {
	logger := logging.FromContext(ctx)
	dataDir := resolveHookDataDir(cmd)

	in := map[string]any{}
	if len(scopes) > 0 {
		if _, err := kernel.ParseScopeSet(scopes); err != nil {
			return "", fmt.Errorf("parse scopes: %w", err)
		}
		in["scopes"] = scopes
	}
	body, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	raw, err := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger, "getContext", body,
		func(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, req json.RawMessage) (json.RawMessage, error) {
			return invokeNamedTool(ctx, wired, "getContext", req)
		})
	if err != nil {
		return "", err
	}
	if raw == nil {
		return "", nil // getContext not registered (minimal boot)
	}
	var resp struct {
		Markdown string `json:"markdown"`
	}
	if uErr := json.Unmarshal(raw, &resp); uErr != nil {
		return "", fmt.Errorf("parse getContext response: %w", uErr)
	}
	return resp.Markdown, nil
}

// sessionStartHookOutput is Claude Code's SessionStart context-injection shape:
// {"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"…"}}.
type sessionStartHookOutput struct {
	HookSpecificOutput sessionStartHookSpecific `json:"hookSpecificOutput"`
}

type sessionStartHookSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// emitSessionStartContext writes the SessionStart injection JSON to w.
func emitSessionStartContext(w io.Writer, md string) error {
	out := sessionStartHookOutput{HookSpecificOutput: sessionStartHookSpecific{
		HookEventName:     "SessionStart",
		AdditionalContext: md,
	}}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal hook output: %w", err)
	}
	if _, werr := fmt.Fprintln(w, string(data)); werr != nil {
		return werr
	}
	return nil
}

// resolveHookDataDir resolves the data dir from --data-dir / MAZ_DATA_DIR,
// falling back to the config default — the same recipe as the other CLI write
// commands.
func resolveHookDataDir(cmd *cobra.Command) string {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	return dataDir
}
