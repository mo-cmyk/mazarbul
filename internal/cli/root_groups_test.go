package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestEveryTopLevelCommandIsGrouped guards against the flat "wall of commands"
// regressing. Every top-level command must be assigned to one of the registered
// help groups; a command added without a group would otherwise drop out of the
// curated `maz help` listing into the defensive MORE bucket. When this fails,
// assign the new command a group in newRootCmd's add(...) calls.
//
// The set of valid groups is derived from root.Groups() (not hard-coded) so the
// test can't drift out of sync when a new group is added. InitDefaultHelpCmd is
// called because SetHelpCommand defers registering the help command into
// Commands() until execution — without it the custom help command (which we
// assign a group) would escape this check.
func TestEveryTopLevelCommandIsGrouped(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	root.InitDefaultHelpCmd()

	known := make(map[string]bool, len(root.Groups()))
	for _, g := range root.Groups() {
		known[g.ID] = true
	}
	if len(known) == 0 {
		t.Fatal("no command groups registered on root")
	}

	for _, c := range root.Commands() {
		switch {
		case c.GroupID == "":
			t.Errorf("command %q has no GroupID — assign it a group in newRootCmd", c.Name())
		case !known[c.GroupID]:
			t.Errorf("command %q has unregistered GroupID %q", c.Name(), c.GroupID)
		}
	}
}

// TestRootHelp_AllFlagTogglesAdvanced exercises the full flag→closure→template
// path (not just renderGroupedCommands in isolation): an advanced-tier command
// is hidden by default and revealed by --all, while the group headers are always
// present.
func TestRootHelp_AllFlagTogglesAdvanced(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		args           []string
		wantAdvanced   bool
		wantAllAllHint bool
	}{
		{"default hides advanced", []string{"help"}, false, true},
		{"all reveals advanced", []string{"help", "--all"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := newRootCmd()
			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%v): %v", tc.args, err)
			}
			out := buf.String()

			if !strings.Contains(out, "CAPTURE") {
				t.Errorf("missing CAPTURE group header:\n%s", out)
			}
			// "migrate" is an advanced-tier command.
			if got := strings.Contains(out, "\n  migrate "); got != tc.wantAdvanced {
				t.Errorf("migrate visible = %v, want %v:\n%s", got, tc.wantAdvanced, out)
			}
			if got := strings.Contains(out, `Run "maz help --all"`); got != tc.wantAllAllHint {
				t.Errorf("--all hint present = %v, want %v", got, tc.wantAllAllHint)
			}
		})
	}
}

// TestHelpSubcommandAllIsNoop guards the documented contract that --all
// forwarded to a subcommand is a deliberate no-op: subcommands carry no --all
// flag (and no groups), so `maz help sync --all` must render identically to
// `maz help sync`. If a subcommand ever gained its own --all flag, the
// forwarding in newHelpCmd would silently flip it and this test would catch it.
func TestHelpSubcommandAllIsNoop(t *testing.T) {
	t.Parallel()

	render := func(args ...string) string {
		root := newRootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute(%v): %v", args, err)
		}
		return buf.String()
	}

	if plain, withAll := render("help", "sync"), render("help", "sync", "--all"); plain != withAll {
		t.Errorf("`maz help sync --all` should equal `maz help sync` (no-op contract):\n--- plain ---\n%s\n--- --all ---\n%s", plain, withAll)
	}
}
