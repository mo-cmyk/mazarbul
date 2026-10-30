package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// daemonReadyTimeout bounds how long the install path waits for the
// launchd-spawned daemon's HTTP transport to come up before reporting
// back. Generous: a cold boot loads sqlite-vec, opens four storage
// layers, and probes Ollama. Exceeding it is a soft "still starting",
// not a failure.
const daemonReadyTimeout = 15 * time.Second

func newDaemonInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the daemon launchd agent (start now + relaunch on every login)",
		Long: `Hand the Mazarbul daemon's lifecycle to macOS launchd.

Writes ~/Library/LaunchAgents/io.mazarbul.daemon.plist and loads it, which
starts the daemon now (RunAtLoad) and relaunches it on every login and
after a crash (KeepAlive). Use 'maz daemon start' instead for a one-shot
daemon that does not survive logout, or 'maz daemon uninstall' to remove
the agent.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != darwinGOOS {
				return fmt.Errorf("maz daemon install is macOS-only (current: %s); use `maz daemon start` for a one-shot daemon", runtime.GOOS)
			}
			dataDir, err := resolveDataDir(cmd)
			if err != nil {
				return err
			}
			ready, ierr := installDaemonLaunchAgent(cmd.Context(), dataDir)
			if ierr != nil {
				return ierr
			}
			plistPath, perr := daemon.LaunchAgentPlistPath()
			if perr != nil {
				return perr
			}
			return renderDaemonInstall(cmd, cmd.OutOrStdout(), plistPath, dataDir, ready)
		},
	}
}

func newDaemonUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the daemon launchd agent (stop + disable login relaunch)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != darwinGOOS {
				return fmt.Errorf("maz daemon uninstall is macOS-only (current: %s)", runtime.GOOS)
			}
			if err := daemon.UninstallLaunchAgent(cmd.Context()); err != nil {
				return err
			}
			return renderDaemonUninstall(cmd, cmd.OutOrStdout())
		},
	}
}

// installDaemonLaunchAgent installs + loads the daemon LaunchAgent for
// dataDir and waits for the HTTP transport to come up. MacOS-only; the
// caller guards GOOS. Shared by `maz daemon install` and the turnkey
// `maz init` auto path so both report readiness the same way.
func installDaemonLaunchAgent(ctx context.Context, dataDir string) (bool, error) {
	if err := daemon.InstallLaunchAgent(ctx, &daemon.LaunchAgentOptions{DataDir: dataDir}); err != nil {
		return false, err
	}
	return waitForDaemonReady(ctx, dataDir, daemonReadyTimeout), nil
}

// waitForDaemonReady polls until the launchd child from THIS install is
// genuinely serving on the loopback port, or the deadline elapses.
// Launchctl bootstrap returns as soon as the job is loaded — before the
// daemon finishes its boot sequence — so this gives callers a real
// readiness signal. A false return is a soft "not up yet", not a hard error.
//
// Readiness binds two facts to a SINGLE process: the daemon's own /healthz
// endpoint (served over its Unix socket) reports the PID answering, and
// that PID must equal the PID launchd reports for our job. So a foreign
// squatter (non-maz response), a missing socket (dial error), AND an
// *older* maz daemon still answering while our child fails to bind (PID
// mismatch) all read as not-ready — only the process launchd actually
// spawned counts.
func waitForDaemonReady(ctx context.Context, dataDir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if servingPID, ok := healthzPID(ctx, dataDir); ok && servesThisInstall(ctx, servingPID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// healthzPID fetches the daemon's /healthz endpoint over its Unix socket
// and returns the PID it self-reports, plus whether a healthy maz daemon
// answered at all. Asking the listener to identify itself (rather than a
// bare dial) rules out a foreign process: a non-maz responder yields
// 404/garbage and a missing socket yields a dial error, both → (0, false).
func healthzPID(ctx context.Context, dataDir string) (int, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://unix"+mcp.HealthPath, nil)
	if err != nil {
		return 0, false
	}
	resp, err := mcp.UnixHTTPClient(mcp.DefaultUnixSocket(dataDir), 500*time.Millisecond).Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort readiness probe close
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var h mcp.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return 0, false
	}
	return h.PID, h.Status == "ok"
}

// servesThisInstall confirms the maz daemon answering :7842 is the launchd
// child this install just loaded: its self-reported PID must match the PID
// launchd holds for our job. This closes the gap where an older one-shot or
// stale managed daemon answers /healthz while the freshly bootstrapped
// child fails to start (or opens a different data dir) — the launchd job
// PID would then differ from the responder's, so we keep waiting rather
// than report the wrong process as ready. On a platform with no launchd job
// (non-macOS, where waitForDaemonReady is not used today), a healthy
// /healthz is the strongest available signal.
func servesThisInstall(ctx context.Context, servingPID int) bool {
	jobPID, loaded, err := daemon.LaunchAgentJobPID(ctx)
	if err != nil {
		return false // launchctl probe failed; treat as not-ready, retry
	}
	if !loaded {
		return true // no launchd job to correlate against; trust the health proof
	}
	return jobPID != 0 && jobPID == servingPID
}

func renderDaemonInstall(cmd *cobra.Command, w io.Writer, plistPath, dataDir string, ready bool) error {
	wr := output.From(cmd)
	// The launchd child binds only the Unix socket (its plist enables no
	// TCP transport), so report the socket path, not a TCP address.
	socket := mcp.DefaultUnixSocket(dataDir)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"action":    "installed",
			"plistPath": plistPath,
			"ready":     ready,
			"socket":    socket,
		})
	}
	state := "running"
	if !ready {
		state = "loaded (still starting)"
	}
	_, err := fmt.Fprintf(w, "ok daemon launchd agent installed; %s on socket %s\n", state, socket)
	return err
}

func renderDaemonUninstall(cmd *cobra.Command, w io.Writer) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{"action": "uninstalled"})
	}
	_, err := fmt.Fprintln(w, "ok daemon launchd agent uninstalled")
	return err
}
