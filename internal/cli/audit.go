package cli

import (
	"github.com/spf13/cobra"
)

// newAuditCmd is an alias of `maz log` semantics — historical / time-bounded
// reads of the JSONL event log. Keeping it as a thin wrapper means callers
// who think in audit terms ("show me everything between Jan 1 and now")
// have an ergonomic entry point without forcing them through the log
// subcommand.
func newAuditCmd() *cobra.Command {
	flags := &logFlags{limit: 0}
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Time-bounded read of the JSONL event log (alias for `maz log` with no default limit)",
		Long: `Audit is a thin wrapper around maz log with the default limit removed.
Use --since / --until / --kind to scope to a specific window or class of
event. Pair with --json for grep-friendly output.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLog(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.since, "since", "", "RFC 3339 lower bound on createTime (inclusive)")
	cmd.Flags().StringVar(&flags.until, "until", "", "RFC 3339 upper bound on createTime (exclusive)")
	cmd.Flags().StringSliceVar(&flags.kinds, "kind", nil, "filter by event kind (repeatable)")
	cmd.Flags().IntVar(&flags.limit, "limit", 0, "maximum events to print (0 = unlimited)")
	return cmd
}
