package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
)

// reviewConflictsFlags captures the small set of knobs the conflicts
// subcommand exposes. Emit is gated false-by-default so a stray
// `maz review conflicts` invocation never mutates the event log.
type reviewConflictsFlags struct {
	emit bool
}

func newReviewConflictsCmd() *cobra.Command {
	flags := &reviewConflictsFlags{}
	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "List functional-slot supersede conflicts after cross-device sync",
		Long: `Scan the relations table for functional predicates that have more
than one open (valid_until IS NULL) relation per (subject, predicate)
slot. Concurrent supersedes from two devices over the same fact
naturally land both arms open after pull-merge; this command surfaces
those slots so an operator can pick the winning chain via
maz review approve / reject on the loser's subject entity.

Read-only by default. Pass --emit to also append a
relationSupersedeConflict event per detected pair; the event log is
deduped against existing supersede-conflict events, so repeated --emit
runs are idempotent.

Examples:
  # Read-only: list every open functional slot conflict.
  maz review conflicts

  # Materialise events so the resolver's review queue picks them up.
  maz review conflicts --emit

  # Agent-callable, JSON line per conflict (read-only).
  maz review conflicts --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReviewConflicts(cmd, flags)
		},
	}
	cmd.Flags().BoolVar(&flags.emit, "emit", false,
		"materialise relationSupersedeConflict events for each detected pair")
	return cmd
}

type reviewConflictRow struct {
	PrimaryRelationUID     string `json:"primaryRelationUid"`
	ConflictingRelationUID string `json:"conflictingRelationUid"`
	SubjectUID             string `json:"subjectUid"`
	Predicate              string `json:"predicate"`
	Reason                 string `json:"reason"`
}

func runReviewConflicts(cmd *cobra.Command, flags *reviewConflictsFlags) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return wrapReviewFatal(cmd, err)
	}
	defer closeStoreQuietly(store)

	registry, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		return wrapReviewFatal(cmd, fmt.Errorf("load predicate registry: %w", err))
	}

	conflicts, err := resolver.DetectConcurrentSupersedeOn(ctx, store.Reader(), registry)
	if err != nil {
		return wrapReviewFatal(cmd, fmt.Errorf("detect: %w", err))
	}

	rows := make([]reviewConflictRow, len(conflicts))
	for i, c := range conflicts {
		rows[i] = reviewConflictRow{
			PrimaryRelationUID:     c.PrimaryRelationUID,
			ConflictingRelationUID: c.ConflictingRelationUID,
			SubjectUID:             c.SubjectUID,
			Predicate:              c.Predicate,
			Reason:                 c.Reason,
		}
	}

	if flags.emit {
		emitted, eerr := emitReviewConflicts(ctx, cmd, conflicts)
		if eerr != nil {
			return wrapReviewFatal(cmd, eerr)
		}
		wr := output.From(cmd)
		if wr.Format() != output.FormatJSON && wr.Format() != output.FormatNDJSON {
			//nolint:errcheck // best-effort progress write
			fmt.Fprintf(cmd.ErrOrStderr(),
				"emitted %d new supersede-conflict event(s)\n", emitted)
		}
	}

	return renderReviewConflictRows(cmd, rows)
}

// emitReviewConflicts opens the event log via kernelboot and appends
// one relationSupersedeConflict event per pair not yet on disk. Returns
// the count of newly-appended events. Kept distinct from the read path
// so a stray --emit failure never poisons the listing.
func emitReviewConflicts(
	ctx context.Context, cmd *cobra.Command, conflicts []resolver.ConcurrentSupersedeConflict,
) (int, error) {
	if len(conflicts) == 0 {
		return 0, nil
	}
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return 0, fmt.Errorf("open event log: %w", err)
	}
	defer func() {
		if cerr := closeLog(); cerr != nil {
			//nolint:errcheck // best-effort: stderr write on close failure
			fmt.Fprintf(cmd.ErrOrStderr(), "close event log: %v\n", cerr)
		}
	}()
	return resolver.EmitConcurrentSupersedeConflictsTo(
		ctx, log, zap.NewNop(), conflicts, "cli:review-conflicts", time.Now,
	)
}

func renderReviewConflictRows(cmd *cobra.Command, rows []reviewConflictRow) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no concurrent supersede conflicts")
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(w,
			"%-26s  vs  %-26s  subject=%s predicate=%s reason=%s\n",
			r.PrimaryRelationUID, r.ConflictingRelationUID,
			r.SubjectUID, r.Predicate, r.Reason); err != nil {
			return err
		}
	}
	return nil
}
