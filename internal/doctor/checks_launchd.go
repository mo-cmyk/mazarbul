package doctor

import (
	"context"
	"fmt"
	"runtime"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// launchd seams for the daemon.launchd check. Tests stub them; production
// code must not reassign them.
//
//nolint:gochecknoglobals // test seams for the launchd state probe and re-register
var (
	readLaunchAgentState  = daemon.ReadLaunchAgentState
	reregisterLaunchAgent = daemon.ReregisterLaunchAgent
)

// newDaemonLaunchdCheck surfaces the daemon's launchd agent on macOS. Its
// main job is the failure mode an in-place binary replacement causes
// (`brew upgrade`, the install script): launchd pinned the old binary's
// code signature at registration and now refuses to spawn the new one
// (OS_REASON_CODESIGNING), so the daemon silently stays down. The
// remediation re-registers the agent, which pins the current binary.
//
// The remediation is Safe: it only rewrites the agent from its own plist
// (same binary path, data dir, env) and touches no user data, and Apply
// is a no-op unless the agent is still codesign-blocked.
func newDaemonLaunchdCheck() CheckFunc {
	return CheckFunc{
		N:   "daemon.launchd",
		Sub: subsystemMCP,
		Fn: func(ctx context.Context) (Status, string, error) {
			if runtime.GOOS != "darwin" {
				return StatusOK, "not applicable (launchd is macOS-only)", nil
			}
			st, err := readLaunchAgentState(ctx)
			if err != nil {
				return StatusWarn, fmt.Sprintf("could not read launchd state: %v", err), nil
			}
			status, detail := launchdAgentStatus(st)
			return status, detail, nil
		},
		Rem: &Remediation{
			Description: "re-register the daemon launchd agent so launchd accepts the current maz binary",
			Safe:        true,
			Apply: func(ctx context.Context) error {
				st, err := readLaunchAgentState(ctx)
				if err != nil {
					return fmt.Errorf("read launchd state: %w", err)
				}
				if !st.CodesignBlocked() {
					return nil // already healthy: idempotent no-op
				}
				return reregisterLaunchAgent(ctx)
			},
		},
	}
}

// launchdAgentStatus maps the launchd view of the daemon agent to a
// doctor status. Only the codesign block is a FAIL: it is actionable and
// never self-heals. An installed-but-unloaded agent is the normal state
// after `maz daemon stop`, so it is OK.
func launchdAgentStatus(st daemon.LaunchAgentState) (Status, string) {
	switch {
	case st.CodesignBlocked():
		return StatusFail, "launchd refuses to start the daemon (" + st.LastExitReason +
			"): the maz binary was replaced after the agent was installed; run `maz doctor fix` or `maz daemon install`"
	case !st.Installed && !st.Loaded:
		return StatusOK, "launchd agent not installed (`maz daemon install` enables start at login)"
	case !st.Installed:
		return StatusWarn, "launchd job loaded without a plist on disk; run `maz daemon uninstall` then `maz daemon install`"
	case !st.Loaded:
		return StatusOK, "launchd agent installed but not loaded (starts at next login; `maz daemon install` starts it now)"
	case st.PID > 0:
		return StatusOK, fmt.Sprintf("running under launchd (pid %d)", st.PID)
	default:
		detail := "launchd job loaded but daemon not running (state " + st.State
		if st.LastExitReason != "" {
			detail += ", last exit reason " + st.LastExitReason
		}
		return StatusWarn, detail + "); see `maz daemon logs`"
	}
}
