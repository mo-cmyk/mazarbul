package output

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// groupedTestRoot builds a minimal command tree with two groups, one common and
// one advanced-tier command, exercising the curation logic in isolation from
// the real (large) command set.
func groupedTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "maz"}
	root.AddGroup(
		&cobra.Group{ID: "capture", Title: "CAPTURE"},
		&cobra.Group{ID: "system", Title: "SYSTEM"},
	)
	root.AddCommand(
		&cobra.Command{Use: "remember", Short: "save a note", GroupID: "capture"},
		&cobra.Command{
			Use:         "migrate",
			Short:       "one-time migration",
			GroupID:     "system",
			Annotations: map[string]string{TierAnnotation: TierAdvanced},
		},
		&cobra.Command{Use: "status", Short: "show status", GroupID: "system"},
	)
	return root
}

func TestRenderGroupedCommands_DefaultHidesAdvanced(t *testing.T) {
	t.Parallel()
	out := renderGroupedCommands(groupedTestRoot(), false)

	for _, want := range []string{"CAPTURE", "remember", "SYSTEM", "status"} {
		if !strings.Contains(out, want) {
			t.Errorf("default listing missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "migrate") {
		t.Errorf("advanced command leaked into default listing:\n%s", out)
	}
	if !strings.Contains(out, `Run "maz help --all"`) {
		t.Errorf("default listing missing the --all hint:\n%s", out)
	}
}

func TestRenderGroupedCommands_AllShowsAdvanced(t *testing.T) {
	t.Parallel()
	out := renderGroupedCommands(groupedTestRoot(), true)

	if !strings.Contains(out, "migrate") {
		t.Errorf("--all listing missing the advanced command:\n%s", out)
	}
	if strings.Contains(out, "help --all") {
		t.Errorf("--all listing should not show the --all hint:\n%s", out)
	}
}

// TestRenderGroupedCommands_HiddenNeverShown verifies a cobra-Hidden command is
// excluded from the listing in both modes — Hidden is a stronger suppression
// than the advanced tier (which --all reveals).
func TestRenderGroupedCommands_HiddenNeverShown(t *testing.T) {
	t.Parallel()
	root := groupedTestRoot()
	root.AddCommand(&cobra.Command{Use: "internal-only", Short: "hidden", GroupID: "system", Hidden: true})

	for _, showAll := range []bool{false, true} {
		if out := renderGroupedCommands(root, showAll); strings.Contains(out, "internal-only") {
			t.Errorf("hidden command shown (showAll=%v):\n%s", showAll, out)
		}
	}
}

// TestRenderGroupedCommands_OrphanSurfacesUnderMore verifies a command whose
// GroupID matches no registered group is listed (under MORE) rather than
// silently dropped — the safety net behind TestEveryTopLevelCommandIsGrouped.
func TestRenderGroupedCommands_OrphanSurfacesUnderMore(t *testing.T) {
	t.Parallel()
	root := groupedTestRoot()
	root.AddCommand(&cobra.Command{Use: "stray", Short: "ungrouped"})
	out := renderGroupedCommands(root, true)

	if !strings.Contains(out, "MORE") || !strings.Contains(out, "stray") {
		t.Errorf("orphan command not surfaced under MORE:\n%s", out)
	}
}
