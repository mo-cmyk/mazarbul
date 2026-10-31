//go:build darwin

package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLaunchctl scripts launchctl responses per subcommand. Each call to
// a subcommand consumes the next scripted response; the last response
// repeats once the script is exhausted. Every invocation is recorded.
type fakeLaunchctl struct {
	mu      sync.Mutex
	script  map[string][]fakeResp
	counter map[string]int
	calls   []string
}

type fakeResp struct {
	out string
	err error
}

var (
	respOK       = fakeResp{}
	respNotFound = fakeResp{out: `Bad request.` + "\n" + `Could not find service "io.mazarbul.daemon" in domain for user gui: 501`, err: errors.New("exit status 113")}
	respIOError  = fakeResp{out: "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors.", err: errors.New("exit status 5")}
	respDenied   = fakeResp{out: "Bootstrap failed: 1: Operation not permitted", err: errors.New("exit status 1")}
)

func (f *fakeLaunchctl) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := args[0]
	f.calls = append(f.calls, sub)
	resps := f.script[sub]
	if len(resps) == 0 {
		return "", nil
	}
	i := f.counter[sub]
	if i >= len(resps) {
		i = len(resps) - 1
	}
	f.counter[sub]++
	return resps[i].out, resps[i].err
}

func (f *fakeLaunchctl) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == sub {
			n++
		}
	}
	return n
}

// installFakeLaunchctl swaps the launchctl seam and shrinks the wait /
// retry timing for the duration of the test. Not parallel-safe: the
// seams are package globals.
func installFakeLaunchctl(t *testing.T, script map[string][]fakeResp) *fakeLaunchctl {
	t.Helper()
	f := &fakeLaunchctl{script: script, counter: map[string]int{}}
	origExec, origTimeout, origPoll, origBackoff := launchctlExec, launchdUnloadTimeout, launchdPollInterval, bootstrapRetryBackoff
	launchctlExec = f.run
	launchdUnloadTimeout = 50 * time.Millisecond
	launchdPollInterval = time.Millisecond
	bootstrapRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() {
		launchctlExec, launchdUnloadTimeout, launchdPollInterval, bootstrapRetryBackoff = origExec, origTimeout, origPoll, origBackoff
	})
	return f
}

func TestWaitLaunchdJobGone(t *testing.T) {
	tests := []struct {
		name       string
		print      []fakeResp
		wantErr    bool
		wantPrints int // exact count; 0 means "at least 2" (timeout case)
	}{
		{name: "gone immediately", print: []fakeResp{respNotFound}, wantPrints: 1},
		{name: "gone after async teardown", print: []fakeResp{respOK, respOK, respOK, respNotFound}, wantPrints: 4},
		{name: "never gone times out", print: []fakeResp{respOK}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := installFakeLaunchctl(t, map[string][]fakeResp{"print": tc.print})
			err := waitLaunchdJobGone(context.Background(), "gui/501", LaunchAgentLabel)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantPrints > 0 && f.count("print") != tc.wantPrints {
				t.Fatalf("print calls = %d, want %d", f.count("print"), tc.wantPrints)
			}
			if tc.wantErr && f.count("print") < 2 {
				t.Fatalf("timeout path polled only %d time(s)", f.count("print"))
			}
		})
	}
}

func TestBootstrapWithRetry(t *testing.T) {
	tests := []struct {
		name           string
		bootstrap      []fakeResp
		wantErr        bool
		wantBootstraps int
	}{
		{name: "succeeds first try", bootstrap: []fakeResp{respOK}, wantBootstraps: 1},
		{name: "retries transient exit 5", bootstrap: []fakeResp{respIOError, respIOError, respOK}, wantBootstraps: 3},
		{name: "gives up after backoff exhausted", bootstrap: []fakeResp{respIOError}, wantErr: true, wantBootstraps: 4},
		{name: "non-transient error is not retried", bootstrap: []fakeResp{respDenied}, wantErr: true, wantBootstraps: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := installFakeLaunchctl(t, map[string][]fakeResp{"bootstrap": tc.bootstrap})
			err := bootstrapWithRetry(context.Background(), "gui/501", "/tmp/x.plist")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := f.count("bootstrap"); got != tc.wantBootstraps {
				t.Fatalf("bootstrap calls = %d, want %d", got, tc.wantBootstraps)
			}
		})
	}
}

func TestBootstrapOrReload(t *testing.T) {
	tests := []struct {
		name      string
		script    map[string][]fakeResp
		wantErr   bool
		wantCalls []string
	}{
		{
			name:      "not loaded bootstraps directly",
			script:    map[string][]fakeResp{"print": {respNotFound}, "bootstrap": {respOK}},
			wantCalls: []string{"print", "bootstrap"},
		},
		{
			name: "loaded job is booted out and awaited before bootstrap",
			script: map[string][]fakeResp{
				"print":     {respOK, respOK, respNotFound},
				"bootout":   {respOK},
				"bootstrap": {respOK},
			},
			wantCalls: []string{"print", "bootout", "print", "print", "bootstrap"},
		},
		{
			name: "lingering registration recovers via retry",
			script: map[string][]fakeResp{
				"print":     {respNotFound},
				"bootstrap": {respIOError, respOK},
			},
			wantCalls: []string{"print", "bootstrap", "bootstrap"},
		},
		{
			name: "bootout that never settles fails without bootstrapping",
			script: map[string][]fakeResp{
				"print":   {respOK},
				"bootout": {respOK},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := installFakeLaunchctl(t, tc.script)
			err := bootstrapOrReload(context.Background(), "gui/501", LaunchAgentLabel, "/tmp/x.plist")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if f.count("bootstrap") != 0 {
					t.Fatalf("bootstrap ran despite unsettled bootout: %v", f.calls)
				}
				return
			}
			if strings.Join(f.calls, ",") != strings.Join(tc.wantCalls, ",") {
				t.Fatalf("calls = %v, want %v", f.calls, tc.wantCalls)
			}
		})
	}
}

// codesignPrintFixture is trimmed `launchctl print` output from a real
// machine after `brew upgrade` replaced the maz binary under a loaded
// agent: launchd keeps the job in "spawn scheduled" and refuses to exec.
const codesignPrintFixture = `gui/501/io.mazarbul.daemon = {
	active count = 0
	path = /Users/me/Library/LaunchAgents/io.mazarbul.daemon.plist
	type = LaunchAgent
	state = spawn scheduled

	program = /Users/me/.local/bin/maz
	resource coalition = {
		ID = 7130
		type = resource
		state = active
	}
	runs = 3
	last exit code = 0
	last exit reason = OS_REASON_CODESIGNING
	properties = runatload | inferred program | needs LWCR update | managed LWCR | has LWCR
}
`

const runningPrintFixture = `gui/501/io.mazarbul.daemon = {
	state = running
	pid = 61546
	resource coalition = {
		state = active
	}
	last exit code = (never exited)
}
`

func TestParseLaunchctlPrint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		in         string
		wantState  string
		wantPID    int
		wantReason string
	}{
		{name: "codesign blocked", in: codesignPrintFixture, wantState: "spawn scheduled", wantReason: "OS_REASON_CODESIGNING"},
		{name: "running", in: runningPrintFixture, wantState: "running", wantPID: 61546},
		{name: "empty", in: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state, pid, reason := parseLaunchctlPrint(tc.in)
			if state != tc.wantState || pid != tc.wantPID || reason != tc.wantReason {
				t.Fatalf("got (%q, %d, %q), want (%q, %d, %q)", state, pid, reason, tc.wantState, tc.wantPID, tc.wantReason)
			}
		})
	}
}

func TestLaunchAgentState_CodesignBlocked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		st   LaunchAgentState
		want bool
	}{
		{name: "blocked", st: LaunchAgentState{Installed: true, Loaded: true, LastExitReason: "OS_REASON_CODESIGNING"}, want: true},
		{name: "running despite old reason", st: LaunchAgentState{Installed: true, Loaded: true, PID: 42, LastExitReason: "OS_REASON_CODESIGNING"}},
		{name: "not loaded", st: LaunchAgentState{Installed: true, LastExitReason: "OS_REASON_CODESIGNING"}},
		{name: "plist removed", st: LaunchAgentState{Loaded: true, LastExitReason: "OS_REASON_CODESIGNING"}},
		{name: "other exit reason", st: LaunchAgentState{Installed: true, Loaded: true, LastExitReason: "JETSAM_REASON_MEMORY_IDLE_EXIT"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.st.CodesignBlocked(); got != tc.want {
				t.Fatalf("CodesignBlocked() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseDaemonPlist_RoundTrip guards ReregisterLaunchAgent: the
// options recovered from a rendered plist must reproduce the install
// (same binary path, data dir, env, log), including XML-escaped and
// space-bearing paths, with the token left for a fresh mint.
func TestParseDaemonPlist_RoundTrip(t *testing.T) {
	t.Parallel()
	in := &LaunchAgentOptions{
		ExecutablePath: "/Users/me/.local/bin/maz",
		DataDir:        "/Users/me/Library/Application Support/R&D <book>",
		Token:          "deadbeefcafef00d",
		LogPath:        "/Users/me/Library/Application Support/R&D <book>/daemon-launchd.log",
		Home:           "/Users/me",
		Path:           "/opt/homebrew/bin:/usr/bin",
	}
	body, err := RenderLaunchAgentPlist(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := parseDaemonPlist(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := *in
	want.Token = ""
	if *got != want {
		t.Fatalf("parsed = %+v, want %+v", *got, want)
	}
	if _, err := parseDaemonPlist([]byte(`<plist><dict><key>Label</key><string>x</string></dict></plist>`)); err == nil {
		t.Fatal("plist without ProgramArguments parsed without error")
	}
}
