//go:build !windows

package daemon

import (
	"context"
	"slices"
	"testing"
)

// TestBuildOllamaServeCmd_AppliesResourceCaps verifies that the
// `ollama serve` command the supervisor builds carries the four
// OLLAMA_* caps that prevent the child from pinning enough unified
// memory to freeze macOS. This is the regression gate for issue #114
// follow-up F1.
func TestBuildOllamaServeCmd_AppliesResourceCaps(t *testing.T) {
	t.Parallel()

	cmd, logFile, err := buildOllamaServeCmd(context.Background(), t.TempDir(), 0, "")
	if err != nil {
		t.Fatalf("buildOllamaServeCmd: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	required := []string{
		"OLLAMA_MAX_LOADED_MODELS=2",
		"OLLAMA_NUM_PARALLEL=1",
		"OLLAMA_KEEP_ALIVE=30s",
		"OLLAMA_FLASH_ATTENTION=1",
	}
	for _, want := range required {
		if !slices.Contains(cmd.Env, want) {
			t.Errorf("cmd.Env missing %q", want)
		}
	}
	// Sanity: the env list must also contain the parent process's PATH
	// (or at least some inherited variable) so user overrides like
	// OLLAMA_HOST and OLLAMA_MODELS continue to flow through. Asserting
	// non-empty is enough; the per-variable override is covered by
	// TestBuildOllamaServeCmd_InheritsUserEnvOverrides.
	if len(cmd.Env) <= len(required) {
		t.Errorf("cmd.Env has %d entries; expected the four caps PLUS os.Environ inheritance", len(cmd.Env))
	}
}

// TestBuildOllamaServeCmd_InheritsUserEnvOverrides verifies that user
// shell variables that are NOT one of the four caps flow through to
// `ollama serve` unchanged. OLLAMA_HOST is the canonical example
// because users routinely override it to bind to a non-default port.
func TestBuildOllamaServeCmd_InheritsUserEnvOverrides(t *testing.T) {
	// t.Setenv requires the test not run in parallel with siblings that
	// share the process env, but our internal tests do not touch this
	// variable so this is safe.
	t.Setenv("OLLAMA_HOST", "127.0.0.1:54321")

	cmd, logFile, err := buildOllamaServeCmd(context.Background(), t.TempDir(), 0, "")
	if err != nil {
		t.Fatalf("buildOllamaServeCmd: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	if !slices.Contains(cmd.Env, "OLLAMA_HOST=127.0.0.1:54321") {
		t.Errorf("cmd.Env does not carry user OLLAMA_HOST override")
	}
}

// TestResolveOllamaURL_HostShapes verifies the OLLAMA_HOST parsing
// closes the URL-shape edge cases the F2 critique flagged: bare host,
// host:port, explicit scheme, IPv6 brackets, trailing slash. These
// run as one test with t.Setenv (no t.Parallel) because they share
// the process env.
func TestResolveOllamaURL_HostShapes(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"unset_uses_default", "", "http://127.0.0.1:11434"},
		{"bare_host_port", "10.0.0.5:11434", "http://10.0.0.5:11434"},
		{"explicit_http", "http://remote.host:11434", "http://remote.host:11434"},
		{"explicit_https", "https://remote.host:11435", "https://remote.host:11435"},
		{"ipv6_bracketed", "[::1]:11434", "http://[::1]:11434"},
		{"trailing_slash_stripped", "http://h:11434/", "http://h:11434"},
		{"only_trailing_slash_not_chargreedy", "http://h:11434//", "http://h:11434/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OLLAMA_HOST", tc.env)
			if got := resolveOllamaURL(); got != tc.want {
				t.Errorf("resolveOllamaURL() with OLLAMA_HOST=%q = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestBuildOllamaServeCmd_LogPathUnderDataDir verifies the log file is
// created at <dataDir>/ollama-serve.log and that the cmd's stdout and
// stderr both point at it. The test exists so a future refactor that
// changes the log location (or splits stdout/stderr) breaks loudly
// rather than silently.
func TestBuildOllamaServeCmd_LogPathUnderDataDir(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	cmd, logFile, err := buildOllamaServeCmd(context.Background(), dataDir, 0, "")
	if err != nil {
		t.Fatalf("buildOllamaServeCmd: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	wantPath := dataDir + "/ollama-serve.log"
	if got := logFile.Name(); got != wantPath {
		t.Errorf("log file path = %q, want %q", got, wantPath)
	}
	if cmd.Stdout != logFile || cmd.Stderr != logFile {
		t.Errorf("cmd.Stdout/Stderr not wired to the log file")
	}
}
