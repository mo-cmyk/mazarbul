package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
)

// TestResolveDaemonMode pins the empty-default policy: explicit flags win
// everywhere; otherwise JSON opts out, --yes opts in, and the interactive
// form defers (empty) to its confirm prompt.
func TestResolveDaemonMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		flagVal  string
		yes      bool
		jsonMode bool
		want     string
	}{
		{"explicit off wins over yes", daemonModeOff, true, false, daemonModeOff},
		{"explicit once wins over json", daemonModeOnce, false, true, daemonModeOnce},
		{"explicit auto in json", daemonModeAuto, false, true, daemonModeAuto},
		{"empty json -> off", "", false, true, daemonModeOff},
		{"empty yes -> auto", "", true, false, daemonModeAuto},
		{"empty yes wins over piped json", "", true, true, daemonModeAuto},
		{"empty interactive -> defer", "", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveDaemonMode(tc.flagVal, tc.yes, tc.jsonMode); got != tc.want {
				t.Errorf("resolveDaemonMode(%q,%v,%v) = %q, want %q", tc.flagVal, tc.yes, tc.jsonMode, got, tc.want)
			}
		})
	}
}

// TestValidateDaemonMode accepts the three modes plus empty and rejects
// anything else.
func TestValidateDaemonMode(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", daemonModeAuto, daemonModeOnce, daemonModeOff} {
		if err := validateDaemonMode(ok); err != nil {
			t.Errorf("validateDaemonMode(%q) = %v, want nil", ok, err)
		}
	}
	if err := validateDaemonMode("sometimes"); err == nil {
		t.Error("validateDaemonMode(\"sometimes\") = nil, want error")
	}
}

// initTestFlags is the minimal valid flag set for the CLI/JSON init paths
// (Anthropic chat + Ollama embeddings), matching the existing init tests.
func initTestFlags() *initFlags {
	return &initFlags{
		provider:      "anthropic",
		embedProvider: EmbedProviderOllama,
		embedModel:    ollama.DefaultEmbedModel,
	}
}

// stubInitDaemon swaps the daemon seam for the duration of a test so init
// never spawns a real daemon, recording the mode it was asked to start.
func stubInitDaemon(t *testing.T, outcome initDaemonOutcome, err error) *string {
	t.Helper()
	got := new(string)
	prev := startDaemonForInitFn
	startDaemonForInitFn = func(_ context.Context, _, mode string) (initDaemonOutcome, error) {
		*got = mode
		if outcome.Mode == "" {
			outcome.Mode = mode
		}
		return outcome, err
	}
	t.Cleanup(func() { startDaemonForInitFn = prev })
	return got
}

// TestRunInitCLI_StartsDaemonViaSeam asserts the CLI path invokes the
// daemon seam with the resolved mode and prints its summary line.
func TestRunInitCLI_StartsDaemonViaSeam(t *testing.T) {
	gotMode := stubInitDaemon(t, initDaemonOutcome{Running: true, Detail: "running (stub)"}, nil)
	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, initTestFlags(), true, daemonModeOnce, ""); err != nil {
		t.Fatalf("runInitCLI: %v", err)
	}
	if *gotMode != daemonModeOnce {
		t.Errorf("daemon seam mode = %q, want %q", *gotMode, daemonModeOnce)
	}
	if got := buf.String(); !strings.Contains(got, "daemon") || !strings.Contains(got, "running (stub)") {
		t.Errorf("missing daemon summary line:\n%s", got)
	}
}

// TestRunInitCLI_DaemonFailureIsSoft asserts a daemon-start failure does
// not abort init: it prints a warning and returns nil.
func TestRunInitCLI_DaemonFailureIsSoft(t *testing.T) {
	stubInitDaemon(t, initDaemonOutcome{}, errors.New("launchctl exploded"))
	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, initTestFlags(), true, daemonModeAuto, ""); err != nil {
		t.Fatalf("runInitCLI should swallow daemon failure, got %v", err)
	}
	got := buf.String()
	warnIdx := strings.Index(got, "warning: daemon not started")
	if warnIdx < 0 {
		t.Fatalf("missing soft-failure warning:\n%s", got)
	}
	// The daemon warning must precede the final "Ready." line: a turnkey
	// init that says "Ready." and only THEN admits the daemon did not
	// start reports the opposite of its real status.
	readyIdx := strings.Index(got, "Ready.")
	if readyIdx < 0 {
		t.Fatalf("missing ready line:\n%s", got)
	}
	if warnIdx > readyIdx {
		t.Errorf("daemon warning must come before Ready. (warn@%d ready@%d):\n%s", warnIdx, readyIdx, got)
	}
}

// TestSelectInitConnectClients_NotOffered asserts the form connect picker
// is a no-op (no detection, no form) when connect was not offered — the
// gate that keeps `--connect`-explicit and non-interactive paths off the
// interactive multi-select.
func TestSelectInitConnectClients_NotOffered(t *testing.T) {
	t.Parallel()
	ids, err := selectInitConnectClients(context.Background(), t.TempDir(), false)
	if err != nil {
		t.Fatalf("selectInitConnectClients(offer=false): %v", err)
	}
	if ids != nil {
		t.Errorf("offer=false must select nothing, got %v", ids)
	}
}

// TestRunInitCLI_DaemonOffSkipsSeam asserts that mode "off" never calls
// the seam (and prints no daemon line).
func TestRunInitCLI_DaemonOffSkipsSeam(t *testing.T) {
	gotMode := stubInitDaemon(t, initDaemonOutcome{}, nil)
	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, initTestFlags(), true, daemonModeOff, ""); err != nil {
		t.Fatalf("runInitCLI: %v", err)
	}
	if *gotMode != "" {
		t.Errorf("daemon seam was called with %q for mode off; want no call", *gotMode)
	}
}

// TestRunInitJSON_DaemonBlock asserts the JSON path emits a daemon block
// when a mode is given, carrying the seam outcome.
func TestRunInitJSON_DaemonBlock(t *testing.T) {
	stubInitDaemon(t, initDaemonOutcome{Mode: daemonModeOnce, Running: true, PID: 4242}, nil)
	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	if err := runInitJSON(initTestCmd(context.Background(), &buf), dir, initTestFlags(), daemonModeOnce, "", connectSpecNone); err != nil {
		t.Fatalf("runInitJSON: %v", err)
	}
	var out struct {
		Daemon struct {
			Mode    string `json:"mode"`
			Running bool   `json:"running"`
			PID     int    `json:"pid"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal init JSON: %v\n%s", err, buf.String())
	}
	if out.Daemon.Mode != daemonModeOnce || !out.Daemon.Running || out.Daemon.PID != 4242 {
		t.Errorf("daemon block = %+v; want once/running/4242", out.Daemon)
	}
}

// TestRunInitJSON_NoDaemonByDefault asserts the JSON path omits the daemon
// block when mode is off (the resolved default for --output=json).
func TestRunInitJSON_NoDaemonByDefault(t *testing.T) {
	stubInitDaemon(t, initDaemonOutcome{}, nil)
	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	if err := runInitJSON(initTestCmd(context.Background(), &buf), dir, initTestFlags(), daemonModeOff, "", connectSpecNone); err != nil {
		t.Fatalf("runInitJSON: %v", err)
	}
	if strings.Contains(buf.String(), "\"daemon\"") {
		t.Errorf("daemon block should be absent for mode off:\n%s", buf.String())
	}
}

// TestRunDaemonStop_LaunchdBootsOut asserts that when the launch agent is
// loaded, stop boots it out (rather than SIGTERM, which KeepAlive would
// undo) and reports the launchd-managed stop.
func TestRunDaemonStop_LaunchdBootsOut(t *testing.T) {
	// Point the PID file at a temp path so the best-effort DeletePID does
	// not touch a real daemon's PID file on the dev host.
	t.Setenv("MAZ_PIDFILE", filepath.Join(t.TempDir(), "maz.pid"))

	prevLoaded, prevBootout := launchAgentLoadedFn, bootoutLaunchAgentFn
	t.Cleanup(func() { launchAgentLoadedFn, bootoutLaunchAgentFn = prevLoaded, prevBootout })

	booted := false
	launchAgentLoadedFn = func(context.Context) (bool, error) { return true, nil }
	bootoutLaunchAgentFn = func(context.Context) error { booted = true; return nil }

	var buf bytes.Buffer
	cmd := newLogsShowTestCmd(&buf)
	if err := runDaemonStop(cmd, &buf); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if !booted {
		t.Error("expected bootout to be called for a launchd-managed daemon")
	}
	// Off-TTY the renderer emits JSON; assert the launchd-managed stop is
	// reported either way.
	if got := buf.String(); !strings.Contains(got, "launchd") || !strings.Contains(got, "stopped") {
		t.Errorf("stop output should report a launchd-managed stop:\n%s", got)
	}
}

// TestRunDaemonStop_LaunchdBootoutError surfaces a bootout failure rather
// than silently falling through to the PID path.
func TestRunDaemonStop_LaunchdBootoutError(t *testing.T) {
	t.Setenv("MAZ_PIDFILE", filepath.Join(t.TempDir(), "maz.pid"))
	prevLoaded, prevBootout := launchAgentLoadedFn, bootoutLaunchAgentFn
	t.Cleanup(func() { launchAgentLoadedFn, bootoutLaunchAgentFn = prevLoaded, prevBootout })

	launchAgentLoadedFn = func(context.Context) (bool, error) { return true, nil }
	bootoutLaunchAgentFn = func(context.Context) error { return errors.New("permission denied") }

	var buf bytes.Buffer
	cmd := newLogsShowTestCmd(&buf)
	if err := runDaemonStop(cmd, &buf); err == nil {
		t.Fatal("expected bootout error to propagate, got nil")
	}
}

// TestRunDaemonStop_LaunchdProbeError surfaces a launchctl probe failure
// rather than falling through to the SIGTERM path — a KeepAlive-managed
// daemon would otherwise just be relaunched after the signal.
func TestRunDaemonStop_LaunchdProbeError(t *testing.T) {
	t.Setenv("MAZ_PIDFILE", filepath.Join(t.TempDir(), "maz.pid"))
	prevLoaded, prevBootout := launchAgentLoadedFn, bootoutLaunchAgentFn
	t.Cleanup(func() { launchAgentLoadedFn, bootoutLaunchAgentFn = prevLoaded, prevBootout })

	bootoutCalled := false
	launchAgentLoadedFn = func(context.Context) (bool, error) { return false, errors.New("launchctl wedged") }
	bootoutLaunchAgentFn = func(context.Context) error { bootoutCalled = true; return nil }

	var buf bytes.Buffer
	cmd := newLogsShowTestCmd(&buf)
	if err := runDaemonStop(cmd, &buf); err == nil {
		t.Fatal("expected launchd probe error to propagate, got nil")
	}
	if bootoutCalled {
		t.Error("bootout should not run when the probe errored")
	}
}
