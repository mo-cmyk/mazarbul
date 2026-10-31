package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the Mazarbul daemon is running",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			if dataDir == "" {
				dataDir = config.Defaults().DataDir
			}
			return runDaemonStatus(cmd, cmd.OutOrStdout(), dataDir)
		},
	}
}

func runDaemonStatus(cmd *cobra.Command, w io.Writer, dataDir string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background() // cobra returns nil for a command never run via Execute
	}
	pidPath := daemon.DataDirPIDPath(dataDir)

	info, err := daemon.ReadPID(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return renderStatusAbsent(cmd, w, launchdCodesignBlocked(ctx))
		}
		return fmt.Errorf("daemon: read pid: %w", err)
	}
	// A live PID belonging to an unrelated process (PID recycled
	// after a crashed daemon) is functionally stale from the user's
	// perspective: there is no daemon. Treat it as stale so the user
	// is not misled into thinking the daemon is running.
	if !info.Running || !daemon.IsMazDaemon(info.PID, info.Token) {
		return renderStatusStale(cmd, w, info.PID, launchdCodesignBlocked(ctx))
	}

	// A live daemon PID without a socket at the dataDir the user asked
	// about is classified by socketMissingStatus: stale (it exited
	// meanwhile), another book (the PID file names a different data dir),
	// or not yet / no longer serving this book.
	socket := daemonSocketPath(dataDir)
	if _, sterr := os.Stat(socket); errors.Is(sterr, os.ErrNotExist) {
		switch socketMissingStatus(info, dataDir) {
		case daemonStatusStale:
			return renderStatusStale(cmd, w, info.PID, launchdCodesignBlocked(ctx))
		case daemonStatusNotServing:
			return renderStatusNotServing(cmd, w, info.PID, socket)
		default:
			return renderStatusOtherBook(cmd, w, info.PID, socket)
		}
	}

	startedAt, terr := pidFileStartTime(pidPath)
	if terr != nil {
		// Falling back to "now" yields uptime 0s, which is honest
		// about the unknown but does not block the status report.
		startedAt = time.Now()
	}
	uptime := time.Since(startedAt).Round(time.Second)
	// Best-effort: surface whether launchd owns this daemon so the user
	// knows `maz daemon stop` will boot it out (vs a one-shot SIGTERM).
	launchd, _ := launchAgentLoadedFn(ctx) //nolint:errcheck // best-effort launchd ownership hint; absence is reported as not-managed
	// Whether TCP is live is a property of the *running* daemon, not of
	// this status command's shell. Sourcing it from MAZ_HTTP_ADDR would
	// report the caller's env, which can disagree with how the daemon was
	// started. The daemon writes its HTTP bearer-token file iff it enabled
	// the (opt-in) TCP transport, so the file's presence is the authoritative
	// on/off signal.
	httpEnabled := daemonHTTPEnabled(dataDir)
	return renderStatusRunning(cmd, w, info.PID, uptime, httpEnabled, socket, config.DaemonLogFile(), launchd)
}

// readLaunchAgentStateFn is the launchd state probe behind the
// codesign-blocked hint. Tests stub it; production code must not
// reassign it.
//
//nolint:gochecknoglobals // test seam for the launchd state probe
var readLaunchAgentStateFn = daemon.ReadLaunchAgentState

// launchdCodesignBlocked reports whether the daemon's launch agent is
// installed but launchd refuses to spawn it because the maz binary was
// replaced after registration (see daemon.LaunchAgentState). Read-only
// and best-effort: a probe error reads as not blocked.
func launchdCodesignBlocked(ctx context.Context) bool {
	st, err := readLaunchAgentStateFn(ctx)
	return err == nil && st.CodesignBlocked()
}

// launchdCodesignHint is the fix shown when launchdCodesignBlocked.
const launchdCodesignHint = "launchd cannot start the daemon because the maz binary was replaced after the agent was installed; run `maz daemon install` (or `maz doctor fix`) to re-register it"

// socketMissingStatus classifies a live, ownership-verified daemon PID
// whose Unix socket is absent for dataDir:
//
//   - daemonStatusStale when the process is gone by now. On shutdown the
//     daemon removes its socket before its PID file, so a daemon that is
//     exiting (e.g. right after `maz daemon uninstall`) passes the first
//     liveness check and dies before the socket stat.
//   - daemonStatusOtherDataDir when the PID file records a different
//     data dir (a shared MAZ_PIDFILE) or none at all (legacy file).
//   - daemonStatusNotServing when the PID file names this very data dir:
//     the daemon is starting, stopping, or mid-rebuild, and it is not
//     "another book".
func socketMissingStatus(info daemon.PIDInfo, dataDir string) string {
	if !daemon.IsMazDaemon(info.PID, info.Token) {
		return daemonStatusStale
	}
	if info.DataDir == "" || !sameDir(info.DataDir, dataDir) {
		return daemonStatusOtherDataDir
	}
	return daemonStatusNotServing
}

func renderStatusNotServing(cmd *cobra.Command, w io.Writer, pid int, expectedSocket string) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status":         daemonStatusNotServing,
			"pid":            pid,
			"expectedSocket": expectedSocket,
			"hint":           "the daemon (pid " + strconv.Itoa(pid) + ") is starting, stopping, or mid-rebuild; retry in a few seconds",
		})
	}
	_, err := fmt.Fprintf(w, "not serving    daemon pid %d is alive but has no socket at %s yet (starting, stopping, or mid-rebuild); retry in a few seconds\n", pid, expectedSocket)
	return err
}

// daemonHTTPEnabled reports whether the running daemon enabled the opt-in
// TCP transport, inferred from the bearer-token file it writes only in that
// case. A stat error other than not-exist is treated as "not enabled": the
// token file is the daemon's own artifact, so an unreadable one means the
// transport is not serving for this caller.
func daemonHTTPEnabled(dataDir string) bool {
	_, err := os.Stat(daemon.HTTPTokenPath(dataDir))
	return err == nil
}

func renderStatusOtherBook(cmd *cobra.Command, w io.Writer, pid int, expectedSocket string) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status":         "running_other_data_dir",
			"pid":            pid,
			"expectedSocket": expectedSocket,
			"hint":           "this dataDir has no socket; the running daemon (pid " + strconv.Itoa(pid) + ") serves a different book",
		})
	}
	_, err := fmt.Fprintf(w, "daemon pid %d is running, but no socket at %s — running daemon serves a different data-dir\n", pid, expectedSocket)
	return err
}

// pidFileStartTime returns the PID file's modification time, which the
// supervisor writes exactly once at start (O_CREATE|O_EXCL), so it
// equals the daemon's start instant. A stat failure falls back to "now"
// which yields uptime 0s; that is honest about the unknown.
func pidFileStartTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Now(), fmt.Errorf("daemon: stat pid file: %w", err)
	}
	return fi.ModTime(), nil
}

func renderStatusRunning(cmd *cobra.Command, w io.Writer, pid int, uptime time.Duration, httpEnabled bool, socket, logFile string, launchd bool) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status":      "running",
			"pid":         pid,
			"uptime":      uptime.String(),
			"httpEnabled": httpEnabled,
			"socket":      socket,
			"logFile":     logFile,
			"launchd":     launchd,
		})
	}
	managed := ""
	if launchd {
		managed = "    (launchd-managed)"
	}
	// The Unix socket is the default transport; the opt-in TCP transport is
	// noted only when the daemon enabled it. The address is the operator's
	// own --http-addr / MAZ_HTTP_ADDR choice and is not echoed here, since
	// the daemon does not persist it for the status command to read back.
	transports := "socket " + socket
	if httpEnabled {
		transports += " + http (enabled)"
	}
	_, err := fmt.Fprintf(w, "running    pid %d    uptime %s    mcp listening on %s%s\n",
		pid, humanDuration(uptime), transports, managed)
	return err
}

func renderStatusStale(cmd *cobra.Command, w io.Writer, pid int, codesignBlocked bool) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		body := map[string]any{
			"status": "stale",
			"pid":    pid,
		}
		addCodesignHint(body, codesignBlocked)
		return json.NewEncoder(w).Encode(body)
	}
	if _, err := fmt.Fprintf(w, "not running (stale pid %d)\n", pid); err != nil {
		return err
	}
	return printCodesignHint(w, codesignBlocked)
}

func renderStatusAbsent(cmd *cobra.Command, w io.Writer, codesignBlocked bool) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		body := map[string]any{"status": "not_running"}
		addCodesignHint(body, codesignBlocked)
		return json.NewEncoder(w).Encode(body)
	}
	if _, err := fmt.Fprintln(w, "not running"); err != nil {
		return err
	}
	return printCodesignHint(w, codesignBlocked)
}

// addCodesignHint adds the launchd codesign fields to a not-running JSON
// body when the agent is blocked.
func addCodesignHint(body map[string]any, codesignBlocked bool) {
	if codesignBlocked {
		body["launchd"] = "codesign_blocked"
		body["hint"] = launchdCodesignHint
	}
}

// printCodesignHint prints the launchd codesign fix line when blocked.
func printCodesignHint(w io.Writer, codesignBlocked bool) error {
	if !codesignBlocked {
		return nil
	}
	_, err := fmt.Fprintln(w, "  "+launchdCodesignHint)
	return err
}

// Daemon status values returned in daemonReadState.Status. Defined as
// constants so the goconst linter does not flag repeated literals across the
// cli package and so callers can switch exhaustively without magic strings.
const (
	daemonStatusRunning      = "running"
	daemonStatusNotRunning   = "not_running"
	daemonStatusStale        = "stale"
	daemonStatusOtherDataDir = "other_data_dir"
	daemonStatusNotServing   = "not_serving"
)

// daemonReadState is the flat daemon summary produced by [readDaemonState].
// It is consumed by both [runDaemonStatus] and [runStatus] so the two
// commands present a consistent, cheap-local-read daemon picture.
type daemonReadState struct {
	// Status is one of the daemonStatus* constants.
	Status      string
	PID         int
	Uptime      time.Duration
	Socket      string
	LogFile     string
	Launchd     bool
	HTTPEnabled bool
}

// readDaemonState reads the daemon PID file for dataDir and returns a
// summary. All reads are local-only: no network calls, no socket dials.
// Both [runDaemonStatus] and [runStatus] call this so the two commands
// stay consistent without duplicating the PID-file parsing logic.
func readDaemonState(ctx context.Context, dataDir string) daemonReadState {
	logFile := config.DaemonLogFile()
	pidPath := daemon.DataDirPIDPath(dataDir)

	info, err := daemon.ReadPID(pidPath)
	if err != nil {
		return daemonReadState{Status: daemonStatusNotRunning, LogFile: logFile}
	}
	if !info.Running || !daemon.IsMazDaemon(info.PID, info.Token) {
		return daemonReadState{Status: daemonStatusStale, PID: info.PID, LogFile: logFile}
	}
	socket := daemonSocketPath(dataDir)
	if _, sterr := os.Stat(socket); errors.Is(sterr, os.ErrNotExist) {
		return daemonReadState{Status: socketMissingStatus(info, dataDir), PID: info.PID, Socket: socket, LogFile: logFile}
	}
	// pidFileStartTime may fail (e.g. the file was just rotated away); falling
	// back to time.Now() yields uptime 0s, which is honest about the unknown
	// without blocking the rest of the status report.
	startedAt, terr := pidFileStartTime(pidPath)
	if terr != nil {
		startedAt = time.Now()
	}
	uptime := time.Since(startedAt).Round(time.Second)
	launchd, _ := launchAgentLoadedFn(ctx) //nolint:errcheck // best-effort ownership hint; absence is not-managed
	httpEnabled := daemonHTTPEnabled(dataDir)
	return daemonReadState{
		Status:      daemonStatusRunning,
		PID:         info.PID,
		Uptime:      uptime,
		Socket:      socket,
		LogFile:     logFile,
		Launchd:     launchd,
		HTTPEnabled: httpEnabled,
	}
}

// humanDuration renders d as the shortest readable form: "12s",
// "3m04s", "1h12m". Time.Duration.String emits "1h12m4.123s" which is
// loud for a status line.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%dh%02dm", h, m)
}
