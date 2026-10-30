package cli

// sync_repair.go — `maz sync repair` recovers a scope whose local mirror is
// wedged by a remote force-push (history rewrite). It removes the mirror so
// the next `maz sync pull` re-clones and the next `maz rebuild` replays the
// full event log cleanly. The sealed chunks and event log are never touched.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
)

func newSyncRepairCmd() *cobra.Command {
	var (
		scope string
		yes   bool
	)
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Recover a scope wedged by a remote force-push (history rewrite)",
		Long: `repair removes a scope's local git mirror that is wedged because the
remote's commit history was rewritten (force-pushed). The local mirror can no
longer fast-forward onto the rewritten history; repair drops it so the next
pull re-clones the current remote state and the next rebuild replays your
event log cleanly.

What repair touches:
  • removes <data-dir>/sync/<scope>/repo  (the local git mirror + marker file)
  • your sealed chunks and event log (the source of truth) are UNTOUCHED

Recovery steps after repair:
  1.  maz sync pull --scope <scope>   # re-clone + download all current chunks
  2.  maz rebuild                     # replay all chunks into the local graph

Stop the daemon first (maz daemon stop) — it holds the mirror open and would
race the removal. The repair flag --yes is required for non-interactive use.

Examples:
  maz daemon stop
  maz sync repair --scope personal --yes
  maz sync pull --scope personal
  maz rebuild`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runSyncRepair(cmd, dataDir, scope, yes)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "scope whose mirror is wedged (personal|project|team) — required")
	if err := cmd.MarkFlagRequired("scope"); err != nil {
		// MarkFlagRequired only errors when the flag does not exist — unreachable.
		panic(fmt.Sprintf("sync repair: MarkFlagRequired: %v", err))
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed without the interactive confirmation prompt")
	return cmd
}

func runSyncRepair(cmd *cobra.Command, dataDir, scope string, yes bool) error {
	if err := validateSyncScope(scope); err != nil {
		return err
	}
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// Refuse while the daemon owns the mirror: removing it under a live push
	// would race git and leave a half-written tree.
	if st := readDaemonState(cmd.Context(), dataDir); st.Status == daemonStatusRunning {
		return fmt.Errorf(
			"sync repair: the daemon is running (pid %d) and holds the mirror open; run `maz daemon stop` first, then retry",
			st.PID)
	}

	mirror := filepath.Join(dataDir, "sync", scope, "repo")
	proceed, err := confirmSyncRepair(cmd, scope, mirror, yes)
	if err != nil {
		return err
	}
	if !proceed {
		//nolint:errcheck // best-effort stdout
		fmt.Fprintln(cmd.OutOrStdout(), "sync repair cancelled")
		return nil
	}

	if rmErr := os.RemoveAll(mirror); rmErr != nil {
		return fmt.Errorf("sync repair: remove mirror %s: %w", mirror, rmErr)
	}

	w := output.From(cmd)
	if w.Format() == output.FormatJSON || w.Format() == output.FormatNDJSON {
		return w.Emit(map[string]any{"ok": true, "scope": scope, "mirrorRemoved": mirror})
	}
	//nolint:errcheck // best-effort stdout
	fmt.Fprintf(cmd.OutOrStdout(),
		"repaired scope %q: removed local mirror %s\nnext steps:\n  maz sync pull --scope %s   # re-clone + download all chunks\n  maz rebuild                # replay into graph\n",
		scope, mirror, scope)
	return nil
}

// confirmSyncRepair gates the destructive mirror removal. Returns (true, nil)
// to proceed, (false, nil) when the user interactively declines (exit 0), and
// a non-nil error in scripted/non-TTY mode without --yes so the caller exits
// non-zero. Mirrors confirmSyncRemoteRecreate.
func confirmSyncRepair(cmd *cobra.Command, scope, mirror string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		//nolint:errcheck // best-effort advisory before the error return
		fmt.Fprintf(cmd.ErrOrStderr(),
			"sync repair removes the local mirror %s; pass --yes to proceed\n", mirror)
		return false, fmt.Errorf("sync repair: refusing to remove mirror for scope %q without --yes", scope)
	}
	return uiform.PromptYN(cmd.ErrOrStderr(),
		fmt.Sprintf("Repair scope %q? This removes the local mirror %s (sealed chunks and event log untouched).", scope, mirror),
		false)
}
