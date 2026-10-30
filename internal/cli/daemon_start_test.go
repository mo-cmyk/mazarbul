package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// TestResolveDataDir_DefaultsWhenFlagEmpty pins the fix for the
// "default start times out" bug: when `maz daemon start` is invoked without
// --data-dir, the parent process must resolve the data directory to the same
// default the child daemon uses, so DataDirPIDPath points to the right file.
// Previously the parent passed "" to spawnDaemonOnce, which called
// DataDirPIDPath("") and polled a PID file in the current directory while the
// child wrote its PID file under config.Defaults().DataDir.
func TestResolveDataDir_DefaultsWhenFlagEmpty(t *testing.T) {
	t.Parallel()

	t.Run("explicit flag is returned unchanged", func(t *testing.T) {
		t.Parallel()
		root := &cobra.Command{Use: "maz"}
		root.PersistentFlags().String("data-dir", "/explicit/path", "")
		child := &cobra.Command{Use: "start"}
		root.AddCommand(child)
		// Parse so PersistentFlags are visible to child.
		_ = root.ParseFlags([]string{"--data-dir", "/explicit/path"})

		got, err := resolveDataDir(child)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != "/explicit/path" {
			t.Errorf("resolveDataDir = %q, want /explicit/path", got)
		}
	})

	t.Run("empty flag resolves to config default", func(t *testing.T) {
		t.Parallel()
		// Snapshot the default before constructing the command so both sides
		// read the same env state (no mutation needed, avoids t.Setenv
		// restrictions).
		want := config.Defaults().DataDir

		root := &cobra.Command{Use: "maz"}
		// Register the flag with an empty default value, mirroring what
		// root.go does when MAZ_DATA_DIR is unset.
		root.PersistentFlags().String("data-dir", "", "")
		child := &cobra.Command{Use: "start"}
		root.AddCommand(child)

		got, err := resolveDataDir(child)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != want {
			t.Errorf("resolveDataDir = %q, want %q (config default)", got, want)
		}
		if got == "" {
			t.Error("resolveDataDir returned empty string; parent and child would disagree on PID path")
		}
	})
}

// TestBuildDaemonChildArgs_ForwardsDataDir pins the Greptile #110 P1 fix:
// `maz --data-dir /custom daemon start` must propagate `--data-dir` to the
// child re-exec. Without forwarding, the child reads its own (empty) root
// flag, falls back to config.Defaults().DataDir, and opens index.db at the
// wrong path, leaving the user's recall queries reading from the wrong
// kernel store entirely.
func TestBuildDaemonChildArgs_ForwardsDataDir(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		token       string
		dataDir     string
		httpAddr    string
		wantContain []string
		wantOrder   string // "data-dir-first" | "no-data-dir"
	}{
		{
			name:        "data-dir set",
			token:       "abc123",
			dataDir:     "/tmp/maz-custom",
			httpAddr:    "",
			wantContain: []string{"--data-dir", "/tmp/maz-custom", "internal-daemon-serve", "--token", "abc123"},
			wantOrder:   "data-dir-first",
		},
		{
			name:        "data-dir empty preserves legacy argv",
			token:       "deadbeef",
			dataDir:     "",
			httpAddr:    "",
			wantContain: []string{"internal-daemon-serve", "--token", "deadbeef"},
			wantOrder:   "no-data-dir",
		},
		{
			name:        "http-addr forwarded",
			token:       "cafebabe",
			dataDir:     "/tmp/maz-iso",
			httpAddr:    "127.0.0.1:9999",
			wantContain: []string{"--data-dir", "/tmp/maz-iso", "internal-daemon-serve", "--token", "cafebabe", "--http-addr", "127.0.0.1:9999"},
			wantOrder:   "data-dir-first",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildDaemonChildArgs(tc.token, tc.dataDir, tc.httpAddr)
			for _, want := range tc.wantContain {
				if !slices.Contains(got, want) {
					t.Errorf("argv missing %q: got %v", want, got)
				}
			}
			joined := strings.Join(got, " ")
			switch tc.wantOrder {
			case "data-dir-first":
				// Cobra reads persistent root flags BEFORE the subcommand
				// name. If --data-dir landed after `internal-daemon-serve`,
				// the child's RunE would never see it.
				if !strings.HasPrefix(joined, "--data-dir "+tc.dataDir+" internal-daemon-serve") {
					t.Errorf("--data-dir must precede subcommand; got %v", got)
				}
			case "no-data-dir":
				if strings.Contains(joined, "--data-dir") {
					t.Errorf("empty dataDir should omit --data-dir; got %v", got)
				}
			}
		})
	}
}
