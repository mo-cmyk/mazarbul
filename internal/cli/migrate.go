package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// newMigrateCmd is the parent for one-time data-format migrations. Today it
// carries only sign-events; future schema-shape migrations hang off it too.
func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "One-time event-log data migrations",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newMigrateSignEventsCmd())
	return cmd
}

// newMigrateSignEventsCmd wires `maz migrate sign-events`.
func newMigrateSignEventsCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "sign-events",
		Short: "Retro-fit a signed author onto every legacy (unsigned) event",
		Long: `Add a signed EventAuthor to every legacy event in the JSONL log that
predates per-writer signing. Each unsigned event is signed in place under your
device's identity; the UID, timestamp, scope, and verbatim payload are
preserved. Already-signed events are left untouched (the command is idempotent
and safe to re-run).

This rewrites event-log files, so the daemon MUST be stopped first. The bbolt
offset sidecar is invalidated afterwards and rebuilt on the next daemon start.

Requires a Watchword (run 'maz init' first): the signing key is derived from it.

In a TTY you are prompted to confirm before any file is rewritten; pass --yes
to skip the prompt (required when scripted or piped).

Exit codes:
  0   migration complete (including a no-op when already signed)
  1   user cancelled, or scripted without --yes
  2   migration failed (no Watchword, I/O error, signing fault)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrateSignEvents(cmd, yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip destructive-action confirmation")
	return cmd
}

// runMigrateSignEvents resolves the data dir, gates on confirmation, builds the
// signer from the device identity, and re-stamps the log.
func runMigrateSignEvents(cmd *cobra.Command, yes bool) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	ctx := cmd.Context()

	if confirmed, err := confirmSignEvents(cmd, yes); err != nil {
		return err
	} else if !confirmed {
		return nil // user declined; exit 0
	}

	chain := secret.DefaultChain()
	signer, err := kernelboot.SignerForReStamp(ctx, identity.New(chain), dataDir)
	if err != nil {
		if errors.Is(err, identity.ErrNoWatchword) {
			//nolint:errcheck // best-effort: stderr write on fatal path
			fmt.Fprintln(cmd.ErrOrStderr(), "migrate: no Watchword set; run `maz init` first")
			return silentError{code: 2, err: err}
		}
		//nolint:errcheck // best-effort: stderr write on fatal path
		fmt.Fprintf(cmd.ErrOrStderr(), "migrate: build signer: %v\n", err)
		return silentError{code: 2, err: err}
	}

	n, err := kernel.ReStampAuthors(dataDir, signer)
	if err != nil {
		//nolint:errcheck // best-effort: stderr write on fatal path
		fmt.Fprintf(cmd.ErrOrStderr(), "migrate: %v\n", err)
		return silentError{code: 2, err: err}
	}

	return renderSignEventsResult(cmd, n)
}

// renderSignEventsResult prints the migration summary in the active output mode.
func renderSignEventsResult(cmd *cobra.Command, signed int) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return wr.Emit(map[string]any{"type": "summary", "signed": signed})
	}
	if signed == 0 {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), "ok  event log already signed (no changes)")
		return werr
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(), "ok  signed %d legacy event(s); run `maz daemon start` to rebuild the offset index\n", signed)
	return werr
}

// confirmSignEvents enforces the confirmation gate, mirroring confirmRebuild:
// (true, nil) to proceed, (false, nil) on an interactive decline, (false, err)
// when scripted without --yes or on a write error.
func confirmSignEvents(cmd *cobra.Command, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
			"sign-events rewrites event-log files; stop the daemon and pass --yes to proceed"); werr != nil {
			return false, werr
		}
		return false, silentError{code: 1, err: errors.New("sign-events: --yes required")}
	}
	proceed, err := uiform.PromptYN(cmd.ErrOrStderr(),
		"Sign-events rewrites the event log in place (daemon must be stopped). Proceed?", false)
	if err != nil {
		return false, err
	}
	if !proceed {
		if _, werr := fmt.Fprintln(cmd.OutOrStdout(), "sign-events cancelled"); werr != nil {
			return false, werr
		}
		return false, nil
	}
	return true, nil
}
