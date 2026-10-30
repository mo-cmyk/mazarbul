//go:build darwin

package daemon_test

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// TestRenderLaunchAgentPlist_MirrorsChildArgs is the regression gate that
// the launchd-spawned daemon is the same process `maz daemon start`
// would fork: the plist argv must carry --data-dir, internal-daemon-serve,
// and --token in the order buildDaemonChildArgs produces. Drift here means
// a launchd daemon opens the wrong store or fails the ownership probe.
func TestRenderLaunchAgentPlist_MirrorsChildArgs(t *testing.T) {
	t.Parallel()

	body, err := daemon.RenderLaunchAgentPlist(&daemon.LaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/maz",
		DataDir:        "/Users/me/Library/Application Support/Mazarbul",
		Token:          "deadbeefcafef00d",
		LogPath:        "/tmp/daemon-launchd.log",
		Home:           "/Users/me",
		Path:           "/usr/local/bin:/usr/bin",
	})
	if err != nil {
		t.Fatalf("RenderLaunchAgentPlist: %v", err)
	}
	got := string(body)

	required := []string{
		"<string>" + daemon.LaunchAgentLabel + "</string>",
		"<string>/usr/local/bin/maz</string>",
		"<string>--data-dir</string>",
		"<string>internal-daemon-serve</string>",
		"<string>--token</string>",
		"<string>deadbeefcafef00d</string>",
		"<key>HOME</key><string>/Users/me</string>",
		"<key>PATH</key><string>/usr/local/bin:/usr/bin</string>",
		"<string>/tmp/daemon-launchd.log</string>",
	}
	for _, want := range required {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q\n%s", want, got)
		}
	}
	// --token must follow internal-daemon-serve, matching the cobra parse
	// order (persistent --data-dir before the subcommand, --token after).
	dataIdx := strings.Index(got, "<string>--data-dir</string>")
	serveIdx := strings.Index(got, "<string>internal-daemon-serve</string>")
	tokIdx := strings.Index(got, "<string>--token</string>")
	if dataIdx >= serveIdx || serveIdx >= tokIdx {
		t.Errorf("argv order wrong: --data-dir(%d) internal-daemon-serve(%d) --token(%d)", dataIdx, serveIdx, tokIdx)
	}
}

// TestRenderLaunchAgentPlist_PersistenceKeys pins the keys that make the
// daemon start-now-and-on-login: RunAtLoad plus KeepAlive on non-success.
// Lose either and the agent becomes a one-shot that does not survive a
// crash or a logout.
func TestRenderLaunchAgentPlist_PersistenceKeys(t *testing.T) {
	t.Parallel()

	body, err := daemon.RenderLaunchAgentPlist(&daemon.LaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/maz",
		DataDir:        "/data",
		Token:          "abc123",
		LogPath:        "/tmp/d.log",
	})
	if err != nil {
		t.Fatalf("RenderLaunchAgentPlist: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "<key>RunAtLoad</key>") || !strings.Contains(got, "<true/>") {
		t.Errorf("plist missing RunAtLoad=true (start-now / start-on-login)")
	}
	if !strings.Contains(got, "<key>KeepAlive</key>") || !strings.Contains(got, "<key>SuccessfulExit</key>") {
		t.Errorf("plist missing KeepAlive { SuccessfulExit = false } (restart-on-crash)")
	}
}

// TestRenderLaunchAgentPlist_EscapesXMLMetacharacters verifies a data dir
// or log path with XML metacharacters does not produce malformed XML
// (launchctl silently rejects it). A data dir under a home with a `&` is
// rare but possible.
func TestRenderLaunchAgentPlist_EscapesXMLMetacharacters(t *testing.T) {
	t.Parallel()

	body, err := daemon.RenderLaunchAgentPlist(&daemon.LaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/maz",
		DataDir:        "/data/a&b<c>d",
		Token:          "tok",
		LogPath:        "/tmp/d.log",
	})
	if err != nil {
		t.Fatalf("RenderLaunchAgentPlist: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "&amp;") || !strings.Contains(got, "&lt;") || !strings.Contains(got, "&gt;") {
		t.Errorf("plist did not XML-escape & < > metacharacters:\n%s", got)
	}
	if strings.Contains(got, "a&b<c>d") {
		t.Errorf("plist contains raw, unescaped metacharacters:\n%s", got)
	}
}

// TestRenderLaunchAgentPlist_OmitsEnvKeysWhenUnset asserts that an empty
// Home / Path omits the key entirely rather than writing an empty string
// (which would override the inherited value to "").
func TestRenderLaunchAgentPlist_OmitsEnvKeysWhenUnset(t *testing.T) {
	t.Parallel()

	body, err := daemon.RenderLaunchAgentPlist(&daemon.LaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/maz",
		DataDir:        "/data",
		Token:          "tok",
		LogPath:        "/tmp/d.log",
	})
	if err != nil {
		t.Fatalf("RenderLaunchAgentPlist: %v", err)
	}
	got := string(body)
	for _, forbidden := range []string{"<key>HOME</key>", "<key>PATH</key>"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("plist contains %q with no value; should be omitted:\n%s", forbidden, got)
		}
	}
}

// TestRenderLaunchAgentPlist_RequiresFields verifies the pre-condition
// errors fire so callers cannot ship a plist with an empty data dir or
// token (an untokened daemon defeats the ownership probe).
func TestRenderLaunchAgentPlist_RequiresFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts daemon.LaunchAgentOptions
	}{
		{name: "missing exe", opts: daemon.LaunchAgentOptions{DataDir: "/d", Token: "t", LogPath: "/l"}},
		{name: "missing data dir", opts: daemon.LaunchAgentOptions{ExecutablePath: "/maz", Token: "t", LogPath: "/l"}},
		{name: "missing token", opts: daemon.LaunchAgentOptions{ExecutablePath: "/maz", DataDir: "/d", LogPath: "/l"}},
		{name: "missing log", opts: daemon.LaunchAgentOptions{ExecutablePath: "/maz", DataDir: "/d", Token: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			if _, err := daemon.RenderLaunchAgentPlist(&opts); err == nil {
				t.Error("expected error; got nil")
			}
		})
	}
}
