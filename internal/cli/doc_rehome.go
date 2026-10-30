package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
)

// docRehomeFlags holds the parsed flag values for `maz doc rehome`.
type docRehomeFlags struct {
	dryRun bool
	yes    bool
}

// newDocRehomeCmd returns the `maz doc rehome` subcommand. It opens the kernel
// store in-process and calls document.SweepObservationFacts to re-home
// observation facts from document entities onto their about: subjects.
func newDocRehomeCmd() *cobra.Command {
	flags := &docRehomeFlags{}
	cmd := &cobra.Command{
		Use:   "rehome",
		Short: "Re-home document observation facts onto their about: subjects",
		Long: `Retroactively re-home observation facts that are currently homed on a document
entity onto that document's about: subject(s) (the describes edges). Each moved
fact produces a factRehomed event in the append-only event log.

IMPORTANT: this command opens the kernel store in-process. A running Mazarbul
daemon holds an exclusive lock on the store. Stop it first:

  maz daemon stop

Then run the sweep:

  maz doc rehome --yes

The event log is the source of truth. Every factRehomed event is appended and
audited. The sweep is idempotent: a second run after full convergence emits
zero events. Documents that have no about: subjects are unaffected (their
observation facts stay on the document).

Use --dry-run to inspect what would happen without emitting any events.

Exit codes:
  0   sweep complete (or dry-run complete)
  1   confirmation declined or scripted without --yes
  2   fatal storage or sweep error

Examples:
  # Inspect without mutating
  maz doc rehome --dry-run

  # Scripted sweep (no prompt)
  maz doc rehome --yes

  # Machine-readable output
  maz doc rehome --yes --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDocRehome(cmd, flags)
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false,
		"report facts that would be re-homed without emitting any events")
	cmd.Flags().BoolVar(&flags.yes, "yes", false,
		"skip the confirmation prompt (required in non-TTY / scripted mode)")
	return cmd
}

// runDocRehome is the command body. Routes to the dry-run or mutating path.
func runDocRehome(cmd *cobra.Command, flags *docRehomeFlags) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	if flags.dryRun {
		return runDocRehomeDryRun(cmd, dataDir)
	}
	return runDocRehomeMutate(cmd, flags, dataDir)
}

// runDocRehomeDryRun opens the dispatcher read-only, queries sweep stats, and
// reports what would happen without appending any events.
func runDocRehomeDryRun(cmd *cobra.Command, dataDir string) error {
	ctx := cmd.Context()
	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		return wrapDocRehomeFatal(cmd, rebuildStorageOpenError(dataDir, err))
	}
	defer func() {
		if cerr := disp.Close(); cerr != nil {
			//nolint:errcheck // best-effort: stderr write on close failure
			fmt.Fprintf(cmd.ErrOrStderr(), "close storage: %v\n", cerr)
		}
	}()

	reader, err := disp.SQLiteReader()
	if err != nil {
		return wrapDocRehomeFatal(cmd, fmt.Errorf("open sqlite reader: %w", err))
	}

	stats, err := document.QuerySweepStats(ctx, reader)
	if err != nil {
		return wrapDocRehomeFatal(cmd, fmt.Errorf("query sweep stats: %w", err))
	}
	// In dry-run the "rehomed" slot carries the PROJECTED count (FactsToRehome):
	// nothing is actually moved, but the operator needs to see how many would be.
	return renderDocRehomeSummary(cmd, stats.FactsOnDocuments, stats.FactsToRehome, stats.DocumentsAffected, true)
}

// runDocRehomeMutate confirms, opens the event log + dispatcher, queries
// pre-sweep stats, then runs SweepObservationFacts and renders the summary.
func runDocRehomeMutate(cmd *cobra.Command, flags *docRehomeFlags, dataDir string) error {
	if confirmed, err := confirmDocRehome(cmd, flags.yes); err != nil {
		return err
	} else if !confirmed {
		return nil // user declined; exit 0
	}

	ctx := cmd.Context()

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return wrapDocRehomeFatal(cmd, fmt.Errorf("open event log: %w", err))
	}
	defer func() {
		if cerr := closeLog(); cerr != nil {
			//nolint:errcheck // best-effort: stderr write on close failure
			fmt.Fprintf(cmd.ErrOrStderr(), "close event log: %v\n", cerr)
		}
	}()

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		return wrapDocRehomeFatal(cmd, rebuildStorageOpenError(dataDir, err))
	}
	defer func() {
		if cerr := disp.Close(); cerr != nil {
			//nolint:errcheck // best-effort: stderr write on close failure
			fmt.Fprintf(cmd.ErrOrStderr(), "close storage: %v\n", cerr)
		}
	}()

	reader, err := disp.SQLiteReader()
	if err != nil {
		return wrapDocRehomeFatal(cmd, fmt.Errorf("open sqlite reader: %w", err))
	}

	// Snapshot counts before the sweep so factsOnDocuments reflects the
	// pre-sweep state (the projection updates synchronously on Append/Apply).
	statsBefore, err := document.QuerySweepStats(ctx, reader)
	if err != nil {
		return wrapDocRehomeFatal(cmd, fmt.Errorf("pre-sweep stats: %w", err))
	}

	newUID := func() (string, error) { return ulid.Make().String(), nil }
	if err := document.SweepObservationFacts(
		ctx, reader, log,
		func() time.Time { return time.Now().UTC() },
		newUID,
		zap.NewNop(),
	); err != nil {
		return wrapDocRehomeFatal(cmd, fmt.Errorf("sweep: %w", err))
	}

	return renderDocRehomeSummary(
		cmd,
		statsBefore.FactsOnDocuments,
		statsBefore.FactsToRehome,
		statsBefore.DocumentsAffected,
		false,
	)
}

// confirmDocRehome enforces the confirmation gate for the mutating sweep,
// following the same pattern as confirmRebuild.
func confirmDocRehome(cmd *cobra.Command, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
			"doc rehome appends factRehomed events to the event log; pass --yes to proceed"); werr != nil {
			return false, werr
		}
		return false, &rehomeExitError{code: 1}
	}
	proceed, err := uiform.PromptYN(cmd.ErrOrStderr(),
		"Re-home document observation facts onto their about: subjects? This appends events to the log.", false)
	if err != nil {
		return false, err
	}
	if !proceed {
		if _, werr := fmt.Fprintln(cmd.OutOrStdout(), "doc rehome cancelled"); werr != nil {
			return false, werr
		}
		return false, nil // exit 0
	}
	return true, nil
}

// renderDocRehomeSummary writes the result in text or JSON to cmd's stdout.
func renderDocRehomeSummary(cmd *cobra.Command, factsOnDocuments, factsRehomed, documentsAffected int, dryRun bool) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		return enc.Encode(map[string]any{
			"factsOnDocuments":  factsOnDocuments,
			"factsRehomed":      factsRehomed,
			"documentsAffected": documentsAffected,
			"dryRun":            dryRun,
		})
	}

	if dryRun {
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"dry-run: %d fact(s) on %d document(s) would be re-homed (%d total on documents)\n",
			factsRehomed, documentsAffected, factsOnDocuments)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"ok  doc rehome: %d fact(s) re-homed across %d document(s)\n",
		factsRehomed, documentsAffected)
	return err
}

// rehomeExitError is the sentinel error returned by runDocRehome when the
// command exits with a non-zero code without a fatal failure (e.g., user
// declined confirmation in scripted mode). Mirrors rebuildExitError.
type rehomeExitError struct{ code int }

func (e *rehomeExitError) Error() string {
	return fmt.Sprintf("doc rehome: exit code %d", e.code)
}

// Code returns the process exit code embedded in the error.
func (e *rehomeExitError) Code() int { return e.code }

// wrapDocRehomeFatal prints the error to stderr and returns a silentError
// with code 2 so Execute exits with the right code without a duplicate message.
func wrapDocRehomeFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "doc rehome", err)
}
