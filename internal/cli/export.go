package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

func newExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export",
		Short: "Dump the JSONL event log to stdout for backup",
		Long: `Stream every event from the JSONL log to stdout as newline-delimited
JSON. Pipe to a file:

  maz export > backup.ndjson

The output is a portable snapshot of the event log. To restore on another
machine, copy the data dir's events/ directory tree directly and then run
'maz rebuild' to reconstruct the four derived stores from those files.

This is a whole-log BACKUP, not a search export. To export search results as a
spreadsheet, use recall instead: maz recall "<query>" --output=csv > results.csv`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExport(cmd)
		},
	}
}

func runExport(cmd *cobra.Command) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		//nolint:errcheck // best-effort stderr on fatal path
		fmt.Fprintf(cmd.ErrOrStderr(), "export: %v\n", err)
		return silentError{code: 2, err: err}
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("export: close eventlog", zap.Error(cerr))
		}
	}()

	w := cmd.OutOrStdout()
	enc := json.NewEncoder(w)
	for ev, ferr := range log.Read(ctx, kernel.ReadOpts{}) {
		if ferr != nil {
			//nolint:errcheck // best-effort stderr on fatal path
			fmt.Fprintf(cmd.ErrOrStderr(), "export: %v\n", ferr)
			return silentError{code: 2, err: ferr}
		}
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}
