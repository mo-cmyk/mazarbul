package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

func TestLaunchdAgentStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		st         daemon.LaunchAgentState
		wantStatus Status
		wantDetail string
	}{
		{name: "codesign blocked", st: daemon.LaunchAgentState{Installed: true, Loaded: true, State: "spawn scheduled", LastExitReason: "OS_REASON_CODESIGNING"}, wantStatus: StatusFail, wantDetail: "maz doctor fix"},
		{name: "not installed", wantStatus: StatusOK, wantDetail: "not installed"},
		{name: "running", st: daemon.LaunchAgentState{Installed: true, Loaded: true, PID: 42, State: "running"}, wantStatus: StatusOK, wantDetail: "pid 42"},
		{name: "stopped via daemon stop", st: daemon.LaunchAgentState{Installed: true}, wantStatus: StatusOK, wantDetail: "not loaded"},
		{name: "loaded without plist", st: daemon.LaunchAgentState{Loaded: true, PID: 7}, wantStatus: StatusWarn, wantDetail: "without a plist"},
		{name: "loaded but crashed", st: daemon.LaunchAgentState{Installed: true, Loaded: true, State: "not running", LastExitReason: "OS_REASON_EXIT"}, wantStatus: StatusWarn, wantDetail: "OS_REASON_EXIT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, detail := launchdAgentStatus(tc.st)
			if st != tc.wantStatus || !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("got (%s, %q), want (%s, contains %q)", st, detail, tc.wantStatus, tc.wantDetail)
			}
		})
	}
}

// TestDaemonLaunchdRemediation asserts the fix re-registers only while
// the agent is codesign-blocked (idempotent no-op otherwise) and is
// offered as safe.
func TestDaemonLaunchdRemediation(t *testing.T) {
	blocked := daemon.LaunchAgentState{Installed: true, Loaded: true, LastExitReason: "OS_REASON_CODESIGNING"}
	healthy := daemon.LaunchAgentState{Installed: true, Loaded: true, PID: 9}
	tests := []struct {
		name      string
		st        daemon.LaunchAgentState
		stateErr  error
		wantCalls int
		wantErr   bool
	}{
		{name: "blocked re-registers", st: blocked, wantCalls: 1},
		{name: "healthy is a no-op", st: healthy},
		{name: "probe error surfaces", stateErr: errors.New("launchctl wedged"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prevRead, prevRereg := readLaunchAgentState, reregisterLaunchAgent
			t.Cleanup(func() { readLaunchAgentState, reregisterLaunchAgent = prevRead, prevRereg })
			calls := 0
			readLaunchAgentState = func(context.Context) (daemon.LaunchAgentState, error) { return tc.st, tc.stateErr }
			reregisterLaunchAgent = func(context.Context) error { calls++; return nil }

			rem := RemediationOf(newDaemonLaunchdCheck())
			if rem == nil || !rem.Safe {
				t.Fatalf("remediation = %+v, want a safe remediation", rem)
			}
			err := rem.Apply(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Apply err = %v, wantErr %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Fatalf("re-register calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
