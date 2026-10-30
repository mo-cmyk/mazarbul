//go:build darwin

package daemon_test

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// TestRenderOllamaLaunchAgentPlist_AppliesResourceCaps is the
// regression gate that F3 carries the same memory caps F1 baked into
// the in-process supervisor. If a future refactor drops one of the
// four OLLAMA_* keys from the plist template the launchd-managed
// Ollama would silently revert to its multi-GB defaults — the exact
// macOS-freeze mode F1 fixed.
func TestRenderOllamaLaunchAgentPlist_AppliesResourceCaps(t *testing.T) {
	t.Parallel()

	body, err := daemon.RenderOllamaLaunchAgentPlist(daemon.OllamaLaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/ollama",
		LogPath:        "/tmp/ollama-launchd.log",
	})
	if err != nil {
		t.Fatalf("RenderOllamaLaunchAgentPlist: %v", err)
	}
	got := string(body)

	required := []string{
		"<key>OLLAMA_MAX_LOADED_MODELS</key><string>2</string>",
		"<key>OLLAMA_NUM_PARALLEL</key><string>1</string>",
		"<key>OLLAMA_KEEP_ALIVE</key><string>30s</string>",
		"<key>OLLAMA_FLASH_ATTENTION</key><string>1</string>",
		"<string>" + daemon.OllamaLaunchAgentLabel + "</string>",
		"<string>/usr/local/bin/ollama</string>",
		"<string>/tmp/ollama-launchd.log</string>",
	}
	for _, want := range required {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	// KeepAlive on non-success is what gives launchd the restart-on-crash
	// behaviour; lose this and the agent becomes a one-shot.
	if !strings.Contains(got, "<key>KeepAlive</key>") || !strings.Contains(got, "<key>SuccessfulExit</key>") {
		t.Errorf("plist missing KeepAlive { SuccessfulExit = false }")
	}
}

// TestRenderOllamaLaunchAgentPlist_NumParallel pins the T0.2 serving knob:
// a configured num_parallel reaches the launchd child's env verbatim, and
// non-positive values keep the historical pinned default of 1 (the F1
// memory-cap posture) rather than leaving the key unset.
func TestRenderOllamaLaunchAgentPlist_NumParallel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   int
		want string
	}{
		{"configured", 4, "<key>OLLAMA_NUM_PARALLEL</key><string>4</string>"},
		{"zero keeps pinned default", 0, "<key>OLLAMA_NUM_PARALLEL</key><string>1</string>"},
		{"negative keeps pinned default", -3, "<key>OLLAMA_NUM_PARALLEL</key><string>1</string>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := daemon.RenderOllamaLaunchAgentPlist(daemon.OllamaLaunchAgentOptions{
				ExecutablePath: "/usr/local/bin/ollama",
				LogPath:        "/tmp/ollama-launchd.log",
				NumParallel:    tc.in,
			})
			if err != nil {
				t.Fatalf("RenderOllamaLaunchAgentPlist: %v", err)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("plist missing %q", tc.want)
			}
		})
	}
}

// TestRenderOllamaLaunchAgentPlist_EscapesXMLMetacharacters verifies
// that a path containing XML metacharacters does not break the plist.
// launchctl silently rejects malformed XML, so an unescaped `&` in a
// LogPath would produce an agent that fails to load with a useless
// "plist parse failure" error.
func TestRenderOllamaLaunchAgentPlist_EscapesXMLMetacharacters(t *testing.T) {
	t.Parallel()

	body, err := daemon.RenderOllamaLaunchAgentPlist(daemon.OllamaLaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/ollama",
		LogPath:        "/tmp/a&b<c>d.log",
	})
	if err != nil {
		t.Fatalf("RenderOllamaLaunchAgentPlist: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "&amp;") || !strings.Contains(got, "&lt;") || !strings.Contains(got, "&gt;") {
		t.Errorf("plist did not XML-escape & < > metacharacters:\n%s", got)
	}
	if strings.Contains(got, "a&b<c>d.log") {
		t.Errorf("plist contains raw, unescaped metacharacters:\n%s", got)
	}
}

// TestRenderOllamaLaunchAgentPlist_PreservesOllamaEnvWhenSet is the
// Greptile P2 regression gate: when the user configured OLLAMA_HOST or
// OLLAMA_MODELS in their shell, the launchd plist must propagate those
// values so the launchd-managed Ollama keeps the same bind address and
// model directory the in-process supervisor inherited via os.Environ.
// Without this, `maz ollama install` silently moves Ollama to a default
// model directory (installed models appear missing) or default bind
// address (the daemon supervises the wrong endpoint).
func TestRenderOllamaLaunchAgentPlist_PreservesOllamaEnvWhenSet(t *testing.T) {
	t.Parallel()
	body, err := daemon.RenderOllamaLaunchAgentPlist(daemon.OllamaLaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/ollama",
		LogPath:        "/tmp/ollama-launchd.log",
		OllamaHost:     "127.0.0.1:11500",
		OllamaModels:   "/Users/me/.ollama-custom",
	})
	if err != nil {
		t.Fatalf("RenderOllamaLaunchAgentPlist: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"<key>OLLAMA_HOST</key><string>127.0.0.1:11500</string>",
		"<key>OLLAMA_MODELS</key><string>/Users/me/.ollama-custom</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q; user override not preserved", want)
		}
	}
}

// TestRenderOllamaLaunchAgentPlist_OmitsOllamaEnvKeysWhenUnset asserts
// the symmetric case: an unset OLLAMA_HOST / OLLAMA_MODELS must NOT
// appear in the plist, so launchd leaves Ollama to its compiled-in
// defaults. Writing an empty string would override the default to
// "" — almost certainly worse than just omitting the key.
func TestRenderOllamaLaunchAgentPlist_OmitsOllamaEnvKeysWhenUnset(t *testing.T) {
	t.Parallel()
	body, err := daemon.RenderOllamaLaunchAgentPlist(daemon.OllamaLaunchAgentOptions{
		ExecutablePath: "/usr/local/bin/ollama",
		LogPath:        "/tmp/ollama-launchd.log",
	})
	if err != nil {
		t.Fatalf("RenderOllamaLaunchAgentPlist: %v", err)
	}
	got := string(body)
	for _, forbidden := range []string{"OLLAMA_HOST", "OLLAMA_MODELS"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("plist contains %q with no user value; should be omitted entirely:\n%s", forbidden, got)
		}
	}
}

// TestRenderOllamaLaunchAgentPlist_RequiresPaths verifies the
// pre-condition errors fire so callers cannot accidentally ship a
// plist with empty Program or StandardOutPath fields.
func TestRenderOllamaLaunchAgentPlist_RequiresPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts daemon.OllamaLaunchAgentOptions
	}{
		{name: "missing exe", opts: daemon.OllamaLaunchAgentOptions{LogPath: "/tmp/x.log"}},
		{name: "missing log", opts: daemon.OllamaLaunchAgentOptions{ExecutablePath: "/usr/bin/ollama"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := daemon.RenderOllamaLaunchAgentPlist(tc.opts); err == nil {
				t.Error("expected error; got nil")
			}
		})
	}
}
