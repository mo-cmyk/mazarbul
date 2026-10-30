package cli

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// TestMCPServeCmd_Defaults pins the cobra wiring for the `maz mcp serve`
// subcommand: serve must default to stdio enabled, with HTTP and Unix
// flags off until passed. A regression in defaults would silently bind a
// daemon transport during a debug session.
func TestMCPServeCmd_Defaults(t *testing.T) {
	t.Parallel()
	mcpCmd := newMCPCmd()
	serve, _, err := mcpCmd.Find([]string{"serve"})
	if err != nil {
		t.Fatalf("Find serve: %v", err)
	}
	if got, _ := serve.Flags().GetBool("no-stdio"); got {
		t.Fatalf("no-stdio default = true, want false")
	}
	if got, _ := serve.Flags().GetString("http"); got != "" {
		t.Fatalf("http default = %q, want empty", got)
	}
	if got, _ := serve.Flags().GetString("unix"); got != "" {
		t.Fatalf("unix default = %q, want empty", got)
	}
	if got, _ := serve.Flags().GetString("name"); got != "mazarbul" {
		t.Fatalf("name default = %q, want %q", got, "mazarbul")
	}
}

// TestMCPServeCmd_BareHTTPUsesDefault asserts pflag's NoOptDefVal wiring:
// `--http` passed without a value parses to DefaultHTTPAddr, so operators
// who type the bare flag get the well-known loopback port and not an
// empty value (which would leave the transport off).
func TestMCPServeCmd_BareHTTPUsesDefault(t *testing.T) {
	t.Parallel()
	serve := newMCPServeCmd()
	if err := serve.ParseFlags([]string{"--http"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, _ := serve.Flags().GetString("http")
	if got != mcp.DefaultHTTPAddr {
		t.Fatalf("--http bare = %q, want %q", got, mcp.DefaultHTTPAddr)
	}
}

// TestMCPServeCmd_BareUnixUsesSentinel asserts that `--unix` passed
// bare parses to the documented sentinel placeholder. The sentinel is
// rewritten to <data-dir>/mazarbul.sock at runtime in resolveUnixSocket
// so the runtime --data-dir flag wins, not the build-time
// config.Defaults() snapshot.
func TestMCPServeCmd_BareUnixUsesSentinel(t *testing.T) {
	t.Parallel()
	serve := newMCPServeCmd()
	if err := serve.ParseFlags([]string{"--unix"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, _ := serve.Flags().GetString("unix")
	if got != mcpServeUnixSentinel {
		t.Fatalf("--unix bare = %q, want sentinel %q", got, mcpServeUnixSentinel)
	}
}

// TestResolveUnixSocket_RespectsDataDir pins the Greptile P1 fix on PR
// #100: the bare `--unix` value must resolve against the runtime
// `--data-dir` flag, not the build-time config snapshot. Without the
// fix, `maz --data-dir /tmp/custom mcp serve --no-stdio --unix` would
// bind under the default data-dir, not /tmp/custom.
func TestResolveUnixSocket_RespectsDataDir(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	root.SetArgs([]string{"--data-dir", "/tmp/maz-custom-dir", "mcp", "serve", "--help"})
	// Walk the tree manually to grab the serve sub-command without
	// actually executing it.
	mcpCmd, _, err := root.Find([]string{"mcp", "serve"})
	if err != nil {
		t.Fatalf("Find mcp serve: %v", err)
	}
	if err := root.PersistentFlags().Set("data-dir", "/tmp/maz-custom-dir"); err != nil {
		t.Fatalf("set data-dir: %v", err)
	}
	got := resolveUnixSocket(mcpCmd, mcpServeUnixSentinel)
	want := "/tmp/maz-custom-dir/mazarbul.sock"
	if got != want {
		t.Fatalf("resolveUnixSocket sentinel = %q, want %q", got, want)
	}
}

// TestResolveUnixSocket_PassThroughExplicit asserts an explicit
// --unix=<path> bypasses resolution and is handed to mcp.New verbatim.
func TestResolveUnixSocket_PassThroughExplicit(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	mcpCmd, _, err := root.Find([]string{"mcp", "serve"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := resolveUnixSocket(mcpCmd, "/tmp/explicit.sock")
	if got != "/tmp/explicit.sock" {
		t.Fatalf("resolveUnixSocket explicit = %q, want unchanged", got)
	}
}

// TestResolveUnixSocket_EmptyOff asserts an unset --unix returns
// empty, leaving the transport off.
func TestResolveUnixSocket_EmptyOff(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	mcpCmd, _, err := root.Find([]string{"mcp", "serve"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got := resolveUnixSocket(mcpCmd, ""); got != "" {
		t.Fatalf("resolveUnixSocket empty = %q, want empty", got)
	}
}

// TestResolveUnixSocket_FallsBackToConfigDefault asserts that when no
// --data-dir flag is set, the sentinel resolves through
// config.Defaults().DataDir. Without this fallback, an init-less
// invocation would bind under an empty path.
func TestResolveUnixSocket_FallsBackToConfigDefault(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	mcpCmd, _, err := root.Find([]string{"mcp", "serve"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := resolveUnixSocket(mcpCmd, mcpServeUnixSentinel)
	want := mcp.DefaultUnixSocket(config.Defaults().DataDir)
	if got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
}

// TestMCPServeCmd_ExplicitValues asserts explicit --http=<addr> and
// --unix=<path> override the bare defaults.
func TestMCPServeCmd_ExplicitValues(t *testing.T) {
	t.Parallel()
	serve := newMCPServeCmd()
	if err := serve.ParseFlags([]string{"--http=127.0.0.2:9000", "--unix=/tmp/x.sock", "--no-stdio"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got, _ := serve.Flags().GetString("http"); got != "127.0.0.2:9000" {
		t.Fatalf("--http = %q, want %q", got, "127.0.0.2:9000")
	}
	if got, _ := serve.Flags().GetString("unix"); got != "/tmp/x.sock" {
		t.Fatalf("--unix = %q, want %q", got, "/tmp/x.sock")
	}
	if got, _ := serve.Flags().GetBool("no-stdio"); !got {
		t.Fatalf("--no-stdio = false, want true")
	}
}

// TestMCPServeCmd_NoTransports asserts the explicit error path when
// every transport is off. Without this guard, `maz mcp serve --no-stdio`
// would call mcp.New, get an "at least one transport" validation error,
// and surface it without help-text context; the early check yields a
// clearer message.
func TestMCPServeCmd_NoTransports(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	root.SetArgs([]string{"mcp", "serve", "--no-stdio"})
	err := root.Execute()
	if err == nil {
		t.Fatalf("Execute returned nil, want transport error")
	}
	if !strings.Contains(err.Error(), "--no-stdio requires at least one of --http or --unix") {
		t.Fatalf("error %q missing guidance", err.Error())
	}
}

// TestRootCmd_RegistersMCP asserts the mcp subcommand is wired into the
// root tree. Without this test, a future refactor that drops the
// cmd.AddCommand call would silently remove the user-facing surface.
func TestRootCmd_RegistersMCP(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	mcpCmd, _, err := root.Find([]string{"mcp"})
	if err != nil {
		t.Fatalf("root.Find(mcp): %v", err)
	}
	if mcpCmd == nil || mcpCmd.Name() != "mcp" {
		t.Fatalf("found cmd = %v, want mcp", mcpCmd)
	}
}
