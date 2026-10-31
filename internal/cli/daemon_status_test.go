package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// deadPID returns the PID of a process that has already exited and been
// reaped, so processAlive reports it dead.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.CommandContext(context.Background(), "true")
	if err := c.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return c.Process.Pid
}

// stubLaunchAgentState swaps the launchd state probe for the test.
func stubLaunchAgentState(t *testing.T, st daemon.LaunchAgentState) {
	t.Helper()
	prev := readLaunchAgentStateFn
	readLaunchAgentStateFn = func(context.Context) (daemon.LaunchAgentState, error) { return st, nil }
	t.Cleanup(func() { readLaunchAgentStateFn = prev })
}

// TestSocketMissingStatus covers the stale-pid regression: a PID whose
// process is gone must never read as "running in another data dir", and
// a live daemon whose PID file names THIS data dir is not another book.
func TestSocketMissingStatus(t *testing.T) {
	restore := daemon.SetIsMazDaemonForTest(func(int, string) bool { return true })
	t.Cleanup(func() { daemon.SetIsMazDaemonForTest(restore) })

	dataDir := t.TempDir()
	live, dead := os.Getpid(), deadPID(t)
	tests := []struct {
		name string
		info daemon.PIDInfo
		want string
	}{
		{name: "pid died before socket check", info: daemon.PIDInfo{PID: dead, Token: "t", DataDir: dataDir}, want: daemonStatusStale},
		{name: "dead pid recorded for another dir", info: daemon.PIDInfo{PID: dead, Token: "t", DataDir: "/elsewhere"}, want: daemonStatusStale},
		{name: "live daemon of this dir without socket", info: daemon.PIDInfo{PID: live, Token: "t", DataDir: dataDir}, want: daemonStatusNotServing},
		{name: "live daemon of another dir", info: daemon.PIDInfo{PID: live, Token: "t", DataDir: "/elsewhere"}, want: daemonStatusOtherDataDir},
		{name: "live legacy pid file without dir", info: daemon.PIDInfo{PID: live, Token: "t"}, want: daemonStatusOtherDataDir},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := socketMissingStatus(tc.info, dataDir); got != tc.want {
				t.Fatalf("socketMissingStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunDaemonStatus_StaleAndCodesignHint drives `maz daemon status`
// end to end against a temp PID file: a dead PID reports "stale" (never
// running_other_data_dir), and a codesign-blocked launch agent adds the
// re-register hint to the not-running answers.
func TestRunDaemonStatus_StaleAndCodesignHint(t *testing.T) {
	blocked := daemon.LaunchAgentState{Installed: true, Loaded: true, LastExitReason: "OS_REASON_CODESIGNING"}
	tests := []struct {
		name       string
		writePID   bool
		launchd    daemon.LaunchAgentState
		wantStatus string
		wantHint   bool
	}{
		{name: "dead pid is stale", writePID: true, wantStatus: "stale"},
		{name: "dead pid with blocked agent hints", writePID: true, launchd: blocked, wantStatus: "stale", wantHint: true},
		{name: "no pid file with blocked agent hints", launchd: blocked, wantStatus: "not_running", wantHint: true},
		{name: "no pid file healthy agent", wantStatus: "not_running"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			pidPath := filepath.Join(dataDir, "maz.pid")
			t.Setenv("MAZ_PIDFILE", pidPath)
			if tc.writePID {
				body := strconv.Itoa(deadPID(t)) + "\ntoken\n" + dataDir + "\n"
				if err := os.WriteFile(pidPath, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stubLaunchAgentState(t, tc.launchd)

			var buf bytes.Buffer
			cmd := newLogsShowTestCmd(&buf)
			if err := runDaemonStatus(cmd, &buf, dataDir); err != nil {
				t.Fatalf("runDaemonStatus: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", buf.String(), err)
			}
			if got["status"] != tc.wantStatus {
				t.Fatalf("status = %v, want %s", got["status"], tc.wantStatus)
			}
			if _, has := got["hint"]; has != tc.wantHint {
				t.Fatalf("hint present = %v, want %v (%v)", has, tc.wantHint, got)
			}
		})
	}
}
