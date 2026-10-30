package cli

import (
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/doctor"
)

// runDoctorWatch is the --watch entry point. It rebuilds the runner every
// interval and re-renders via the CLI renderer. The loop terminates on
// Ctrl-C / SIGTERM or a context cancellation. Exit code follows the last
// completed sweep so scripted callers (e.g. healthcheck wrappers) see
// consistent semantics.
func runDoctorWatch(cmd *cobra.Command, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	w := cmd.OutOrStdout()
	var lastCode int
	for ctx.Err() == nil {
		runner := &doctor.Runner{Checks: doctor.DefaultChecks(dataDir)}
		results := runner.Run(ctx)
		if err := renderDoctorResults(cmd, w, results); err != nil {
			//nolint:errcheck // best-effort stderr on fatal path
			fmt.Fprintf(cmd.ErrOrStderr(), "doctor: %v\n", err)
			return silentError{code: 2, err: err}
		}
		if _, err := fmt.Fprintf(w, "\n--- refreshed at %s; next sweep in %s; Ctrl-C to exit ---\n",
			time.Now().UTC().Format(time.RFC3339), interval); err != nil {
			return err
		}
		lastCode = doctor.ExitCode(results)

		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	if lastCode != 0 {
		return &doctorExitError{code: lastCode}
	}
	return nil
}
