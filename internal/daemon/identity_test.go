package daemon_test

import (
	"os"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

func TestIsMazDaemon_DeadPIDIsFalse(t *testing.T) {
	pid := staleDeadPID(t)
	if got := daemon.IsMazDaemon(pid, ""); got {
		t.Errorf("IsMazDaemon(dead pid %d) = true, want false", pid)
	}
}

func TestIsMazDaemon_TestSeamHonored(t *testing.T) {
	// The test process is the Go test runner, not a maz daemon, so the
	// real ps-based probe should return false. Stub it to return true
	// and confirm the public function honors the seam.
	restore := daemon.SetIsMazDaemonForTest(func(int, string) bool { return true })
	defer daemon.SetIsMazDaemonForTest(restore)

	if got := daemon.IsMazDaemon(os.Getpid(), ""); !got {
		t.Errorf("IsMazDaemon with stubbed-true seam returned false")
	}
}

func TestIsMazDaemon_RealProbeRejectsNonDaemon(t *testing.T) {
	// The test runner's argv contains "go.test" or similar, never the
	// daemon marker. Real probe must reject it.
	if got := daemon.IsMazDaemon(os.Getpid(), ""); got {
		t.Errorf("IsMazDaemon(self test pid) = true, want false; the test process is not a daemon")
	}
}

func TestMatchesDaemonArgv_EmptyTokenAlwaysRejects(t *testing.T) {
	// Greptile P1 #107 round 6 (security): tokenless PID files MUST
	// be treated as unowned. The previous adjacent-pair fallback
	// was satisfiable by an unrelated `maz` process whose argv
	// happened to contain `<...>/maz internal-daemon-serve` (e.g.
	// `maz daemon logs --follow /tmp/maz internal-daemon-serve`).
	// Every input — including ones that look like a real daemon
	// argv — must return false when token is empty.
	tests := []string{
		"",
		"grep internal-daemon-serve /tmp/x",
		"/usr/local/bin/maz internal-daemon-serve",
		"maz internal-daemon-serve",
		"/tmp/Maz Build/maz internal-daemon-serve",
		"/opt/maz daemon logs --follow /tmp/maz internal-daemon-serve",
		"/opt/maz internal-daemon-serve --token deadbeef",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if got := daemon.MatchesDaemonArgvForTest(raw, ""); got {
				t.Errorf("matchesDaemonArgv(%q, \"\") = true, want false (tokenless inputs must always reject)", raw)
			}
		})
	}
}

func TestMatchesDaemonArgv_TokenDelimitedPair(t *testing.T) {
	// Greptile P1 #107 round 5 (security): the round-3 adjacent-pair
	// check could be widened by an unrelated `maz` process whose
	// argv happened to contain a `/.../maz internal-daemon-serve`
	// pair anywhere (e.g. `maz daemon logs --follow /tmp/maz
	// internal-daemon-serve`). The token path requires the per-launch
	// 128-bit random token as a delimited " --token <hex>" pair —
	// either as the final argument OR immediately followed by another
	// flag (the daemon child appends "--http-addr <addr>" after the
	// token). A token absent from the argv can never be forged.
	const tok = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		// Real daemon argv shapes with this token.
		{name: "basic", raw: "/usr/local/bin/maz internal-daemon-serve --token " + tok, want: true},
		{name: "bare basename", raw: "maz internal-daemon-serve --token " + tok, want: true},
		{name: "path with spaces", raw: "/tmp/Maz Build/maz internal-daemon-serve --token " + tok, want: true},
		{name: "trailing whitespace", raw: "/opt/maz internal-daemon-serve --token " + tok + "\n", want: true},
		// Real daemon shape with an HTTP listener: --http-addr follows
		// --token. This is the default `daemon start` argv and MUST match
		// (regression: a strict-suffix check rejected it, breaking
		// `daemon status`/`daemon stop`).
		{name: "http-addr after token", raw: "/usr/local/bin/maz internal-daemon-serve --token " + tok + " --http-addr 127.0.0.1:7842", want: true},
		{name: "trailing flag after token", raw: "/opt/maz internal-daemon-serve --token " + tok + " --extra", want: true},

		// Round-3 false positives now rejected.
		{name: "round-3 widens-again attack", raw: "/opt/maz daemon logs --follow /tmp/maz internal-daemon-serve", want: false},
		{name: "no token suffix", raw: "/opt/maz internal-daemon-serve", want: false},

		// Wrong token (e.g., other daemon instance, attacker guess).
		{name: "different token", raw: "/opt/maz internal-daemon-serve --token deadbeefdeadbeefdeadbeefdeadbeef", want: false},

		// Token that is only a PREFIX of a longer argv token must not
		// match: the trailing delimiter (space or end) is required.
		{name: "token is prefix of longer arg", raw: "/opt/maz internal-daemon-serve --token " + tok + "deadbeef", want: false},

		// Subcommand mentioned but no token flag.
		{name: "substring no flag", raw: "grep internal-daemon-serve " + tok, want: false},

		// Empty input.
		{name: "empty", raw: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemon.MatchesDaemonArgvForTest(tc.raw, tok); got != tc.want {
				t.Errorf("matchesDaemonArgv(%q, %q) = %v, want %v", tc.raw, tok, got, tc.want)
			}
		})
	}
}
