package cli

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// TestResolveLogLevel_EnvHonoredWhenFlagUnset asserts that MAZ_LOG_LEVEL is
// consulted when --log-level was not explicitly passed on the command line.
// This is fix A: the env var must win over the compiled-in default.
func TestResolveLogLevel_EnvHonoredWhenFlagUnset(t *testing.T) {
	// t.Setenv requires no t.Parallel at this level.
	for _, tc := range []struct {
		name      string
		env       string
		flagSet   bool
		flagVal   string
		wantLevel logging.Level
	}{
		{name: "env error no flag", env: "error", flagSet: false, flagVal: "warn", wantLevel: logging.LevelError},
		{name: "env debug no flag", env: "debug", flagSet: false, flagVal: "warn", wantLevel: logging.LevelDebug},
		{name: "env warn no flag", env: "warn", flagSet: false, flagVal: "warn", wantLevel: logging.LevelWarn},
		// Explicit flag wins even when env says something different.
		{name: "flag info beats env error", env: "error", flagSet: true, flagVal: "info", wantLevel: logging.LevelInfo},
		{name: "flag warn beats env debug", env: "debug", flagSet: true, flagVal: "warn", wantLevel: logging.LevelWarn},
		// No env, no explicit flag: one-shot default is warn.
		{name: "no env no flag defaults warn", env: "", flagSet: false, flagVal: "warn", wantLevel: logging.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAZ_LOG_LEVEL", tc.env)
			got := resolveLogLevel(tc.flagSet, tc.flagVal)
			if got != tc.wantLevel {
				t.Errorf("resolveLogLevel(flagSet=%v, flagVal=%q) with MAZ_LOG_LEVEL=%q = %v, want %v",
					tc.flagSet, tc.flagVal, tc.env, got, tc.wantLevel)
			}
		})
	}
}

// TestDefaultCLILogLevelIsWarn asserts that the default effective log level
// for a one-shot CLI invocation (no flag, no env) resolves to warn.
// This is fix I: quiet by default on one-shot commands.
func TestDefaultCLILogLevelIsWarn(t *testing.T) {
	t.Setenv("MAZ_LOG_LEVEL", "")
	// No flag set, default flag value is "warn" (the compiled-in default
	// changed from "info" to "warn" as part of fix I).
	got := resolveLogLevel(false, "warn")
	if got != logging.LevelWarn {
		t.Errorf("default one-shot level = %v, want warn", got)
	}
}

// TestLongRunningCommandUsesInfo asserts that mcp serve and watch are
// recognised as long-running paths and default to info when no flag or env
// is explicitly set.
func TestLongRunningCommandUsesInfo(t *testing.T) {
	t.Setenv("MAZ_LOG_LEVEL", "")
	for _, path := range []string{
		"maz mcp serve",
		"maz watch",
		"maz internal-daemon-serve",
	} {
		t.Run(path, func(t *testing.T) {
			got := effectiveLogLevel(false, "warn", path)
			if got != logging.LevelInfo {
				t.Errorf("effectiveLogLevel(%q) with no flag/env = %v, want info", path, got)
			}
		})
	}
}

// TestOneShotCommandUsesWarnDefault asserts that ordinary one-shot
// commands (recall, ask, remember, etc.) default to warn.
func TestOneShotCommandUsesWarnDefault(t *testing.T) {
	t.Setenv("MAZ_LOG_LEVEL", "")
	for _, path := range []string{
		"maz recall",
		"maz ask",
		"maz remember",
		"maz list",
		"maz get",
		"maz neighbors",
	} {
		t.Run(path, func(t *testing.T) {
			got := effectiveLogLevel(false, "warn", path)
			if got != logging.LevelWarn {
				t.Errorf("effectiveLogLevel(%q) with no flag/env = %v, want warn", path, got)
			}
		})
	}
}
