package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	clistatus "github.com/mo-cmyk/mazarbul/internal/cli/status"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/connect"
	"github.com/mo-cmyk/mazarbul/internal/connect/clients"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/doctor"
)

// doctorCLIStatusBullet returns the appropriate status bullet for a doctor result.
//

func doctorCLIStatusBullet(p *clistatus.Printer, r *doctor.Result) string {
	line := r.Name + "   " + detailWithDuration(*r)
	switch r.Status {
	case doctor.StatusOK:
		return p.SuccessBullet(line)
	case doctor.StatusWarn:
		return p.WarnBullet(line)
	default:
		return p.FailureBullet(line)
	}
}

// doctorSubsystemOrder is the canonical rendering order for doctor subsystems.
// Extracted from tui/doctor so the CLI path produces a consistent layout.
// "setup" is last — it is advisory-only (WARN, never FAIL) and reports on
// optional conveniences such as the launch-agent, MCP client freshness, and
// shell completion.
//
//nolint:gochecknoglobals // package-level constant slice; immutable after init
var doctorSubsystemOrder = []string{
	"storage",
	"identity",
	"mcp",
	"chat",
	"embed",
	"log",
	"setup",
}

// doctorExitError is the sentinel error returned by the doctor RunE
// when the report contains a FAIL. We carry the exit code through the
// error chain rather than calling os.Exit directly so cobra's RunE
// contract stays clean and tests can intercept the value.
type doctorExitError struct{ code int }

func (e *doctorExitError) Error() string {
	return fmt.Sprintf("doctor: %d check(s) failed", e.code)
}

// Code returns the process exit code embedded in the error.
func (e *doctorExitError) Code() int { return e.code }

// ResolveDoctorExitCode unwraps a doctorExitError returned from
// runDoctor to its embedded code. Returns (0, false) for nil or any
// non-doctor error. Used by cmd/maz/main.go to translate the cobra
// error into the process exit code without leaking the sentinel type.
func ResolveDoctorExitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var d *doctorExitError
	if errors.As(err, &d) {
		return d.Code(), true
	}
	return 0, false
}

func newDoctorCmd() *cobra.Command {
	var watch bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run Mazarbul health checks across storage, MCP, and providers",
		Long: `Run a sweep of health checks: storage layers (SQLite, sqlite-vec, FTS5,
LadybugDB), MCP transports (Unix socket, /healthz), the configured chat
provider (a 1-token completion that costs ~$0.00001 for cloud providers),
the embedding provider, and the log file.

Default output is a colored table grouped by subsystem; --output=json emits
a single JSON array of check objects (one value, parseable with JSON.parse);
--output=ndjson streams one JSON object per check on its own line. Exit code
is 0 when every check is OK or WARN, 1 when any check is FAIL.

Examples:
  maz doctor
  maz doctor --output=json | jq '.[].status'
  maz doctor --output=ndjson | jq 'select(.status=="fail")'
  maz doctor --watch --interval=10s

Use --watch to refresh continuously; the table re-runs every check on the
chosen --interval until Ctrl-C.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if watch {
				return runDoctorWatch(cmd, interval)
			}
			return runDoctor(cmd)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "refresh every --interval (default 5s) until Ctrl-C")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "refresh interval when --watch is set")
	cmd.AddCommand(newDoctorFixCmd())
	return cmd
}

// buildDoctorChecks combines the standard doctor checks with the WS12 setup
// checks. It is the single source-of-truth for the non-watch runDoctor path;
// --watch still uses doctor.DefaultChecks directly (via doctor_watch.go which
// is not an owned file in this lane).
func buildDoctorChecks(dataDir string) []doctor.Check {
	return append(doctor.DefaultChecks(dataDir), setupChecks(dataDir)...)
}

// runDoctor resolves the data directory, builds the standard check set,
// runs them under the active output mode, and returns a doctorExitError
// when any check fails.
func runDoctor(cmd *cobra.Command) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	runner := &doctor.Runner{Checks: buildDoctorChecks(dataDir)}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	results := runner.Run(ctx)
	if err := renderDoctorResults(cmd, w, results); err != nil {
		return err
	}

	if code := doctor.ExitCode(results); code != 0 {
		return &doctorExitError{code: code}
	}
	return nil
}

// renderDoctorResults writes results in the format resolved from cmd.
func renderDoctorResults(cmd *cobra.Command, w io.Writer, results []doctor.Result) error {
	wr := output.From(cmd)
	switch wr.Format() {
	case output.FormatJSON:
		return renderDoctorJSONArray(w, results)
	case output.FormatNDJSON:
		return renderDoctorNDJSON(w, results)
	default:
		enabled := interactive.ShouldStyle(cmd, w)
		return renderDoctorCLI(w, results, enabled)
	}
}

// groupBySubsystem partitions results by subsystem while preserving the
// registration order inside each group. Subsystems are returned in
// doctorSubsystemOrder; any unknown subsystem (defensive against
// typos in new checks) tails the list in encounter order.
func groupBySubsystem(results []doctor.Result) [][]doctor.Result {
	order := doctorSubsystemOrder
	by := make(map[string][]doctor.Result, len(order))
	var extras []string
	for _, r := range results {
		if _, seen := by[r.Subsystem]; !seen {
			knownInOrder := false
			for _, s := range order {
				if s == r.Subsystem {
					knownInOrder = true
					break
				}
			}
			if !knownInOrder {
				extras = append(extras, r.Subsystem)
			}
		}
		by[r.Subsystem] = append(by[r.Subsystem], r)
	}
	groups := make([][]doctor.Result, 0, len(order)+len(extras))
	for _, s := range order {
		if rs, ok := by[s]; ok {
			groups = append(groups, rs)
		}
	}
	for _, s := range extras {
		groups = append(groups, by[s])
	}
	return groups
}

// detailWithDuration formats the result detail and duration for table rendering.
//
//nolint:gocritic // hugeParam: Result is an aggregate value; pointer would obscure ownership
func detailWithDuration(r doctor.Result) string {
	d := fmt.Sprintf("%dms", r.DurationMs)
	if r.Detail == "" {
		return d
	}
	return r.Detail + " (" + d + ")"
}

// renderDoctorJSONArray emits a single JSON array containing all results.
// One call to JSON.parse / json.Unmarshal on stdout succeeds without "Extra data".
func renderDoctorJSONArray(w io.Writer, results []doctor.Result) error {
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal doctor results: %w", err)
	}
	b = append(b, '\n')
	_, werr := w.Write(b)
	return werr
}

// renderDoctorNDJSON emits the canonical JSON Lines stream. One Result per
// line, lowerCamelCase fields per AIP-122.
func renderDoctorNDJSON(w io.Writer, results []doctor.Result) error {
	enc := json.NewEncoder(w)
	for i := range results {
		if err := enc.Encode(results[i]); err != nil {
			return fmt.Errorf("encode result %d: %w", i, err)
		}
	}
	return nil
}

// renderDoctorCLI renders a symbol-first, section-grouped view of the doctor
// results. Each subsystem gets a section rule header; each check is rendered
// as a ✓/⚠/✗ bullet. Colour is gated by enabled; plain output still carries
// the glyphs so meaning is never conveyed by colour alone.
func renderDoctorCLI(w io.Writer, results []doctor.Result, enabled bool) error {
	p := clistatus.New(w, enabled)
	groups := groupBySubsystem(results)
	for _, group := range groups {
		if _, err := fmt.Fprintln(w, p.SectionRule(group[0].Subsystem)); err != nil {
			return err
		}
		for i := range group {
			if _, err := fmt.Fprintln(w, doctorCLIStatusBullet(p, &group[i])); err != nil {
				return err
			}
		}
	}
	s := doctor.Summarize(results)
	_, err := fmt.Fprintf(w, "\nsummary: %d checks · %d ok · %d warn · %d fail\n",
		s.Total, s.OK, s.Warn, s.Fail,
	)
	return err
}

// ── maz doctor fix: confirmed per-check remediation ──────────────────────────

// newDoctorFixCmd registers `maz doctor fix`: re-run the health checks and, for
// each FAILING check that can repair itself, confirm and apply the fix. Plain
// `maz doctor` is unchanged (diagnose-only).
func newDoctorFixCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Confirm and apply remediations for failing health checks",
		Long: `Run the health checks, then for each FAILING check that knows how to
repair itself, prompt to apply the fix ([y/N]). Checks with no automatic
remediation print manual guidance instead.

--yes applies the SAFE remediations without prompting. Unsafe remediations
(migration, audit, and permission repairs) are FLAGGED but never auto-applied,
even with --yes; they always require an explicit interactive confirm.

Examples:
  maz doctor fix          # confirm each fix interactively
  maz doctor fix --yes    # apply safe fixes without prompting`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctorFix(cmd, yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "apply safe remediations without prompting")
	return cmd
}

// runDoctorFix runs the checks, applies confirmed remediations to the failing
// ones, and prints a fix summary. It returns a doctorExitError when any failure
// remains unresolved after the pass, so scripts and CI observe exit code 1.
func runDoctorFix(cmd *cobra.Command, yes bool) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	runner := &doctor.Runner{Checks: buildDoctorChecks(dataDir)}
	checks := runner.CheckList()
	results := runner.Run(ctx)

	p := clistatus.New(w, interactive.ShouldStyle(cmd, w))
	failing := failingResults(results)
	if len(failing) == 0 {
		fixln(w, p.SuccessBullet("all checks healthy; nothing to fix"))
		return nil
	}

	tally := applyDoctorFixes(ctx, w, p, checks, failing, yes)
	fixf(w, "\nfix: %d applied · %d skipped · %d flagged · %d manual · %d failed\n",
		tally.applied, tally.skipped, tally.flagged, tally.guidance, tally.failed)
	if tally.unresolved() > 0 {
		return &doctorExitError{code: 1}
	}
	return nil
}

// failingResults returns the StatusFail subset of results, preserving
// registration order so the fix pass walks checks top to bottom.
func failingResults(results []doctor.Result) []doctor.Result {
	out := make([]doctor.Result, 0, len(results))
	for i := range results {
		if results[i].Status == doctor.StatusFail {
			out = append(out, results[i])
		}
	}
	return out
}

// fixTally counts the outcome of a `maz doctor fix` pass. Unresolved sums every
// failure the pass did not repair so the caller can map it to an exit code.
type fixTally struct {
	applied  int // remediation applied and the check now passes
	skipped  int // user declined, or non-interactive without --yes
	flagged  int // unsafe remediation skipped under --yes
	guidance int // failing check with no remediation
	failed   int // remediation errored, or check still failing afterwards
}

// unresolved sums the failures the fix pass did not repair.
func (t fixTally) unresolved() int { return t.skipped + t.flagged + t.guidance + t.failed }

// fixOutcome is the result of attempting to fix one failing check.
type fixOutcome int

const (
	outcomeApplied fixOutcome = iota
	outcomeSkipped
	outcomeFlagged
	outcomeGuidance
	outcomeFailed
)

// applyDoctorFixes walks the failing checks and offers/applies each
// remediation, accumulating the outcomes into a fixTally.
func applyDoctorFixes(ctx context.Context, w io.Writer, p *clistatus.Printer, checks []doctor.Check, failing []doctor.Result, yes bool) fixTally {
	var t fixTally
	for i := range failing {
		res := failing[i]
		switch fixOneCheck(ctx, w, p, checks[res.Index], &res, yes) {
		case outcomeApplied:
			t.applied++
		case outcomeSkipped:
			t.skipped++
		case outcomeFlagged:
			t.flagged++
		case outcomeGuidance:
			t.guidance++
		case outcomeFailed:
			t.failed++
		}
	}
	return t
}

// fixOneCheck prints the failing check, decides whether to apply its
// remediation (auto under --yes for safe fixes, otherwise an interactive
// confirm), applies it, and re-runs the single check to confirm the repair.
// Passing res by pointer avoids copying the aggregate Result.
func fixOneCheck(ctx context.Context, w io.Writer, p *clistatus.Printer, check doctor.Check, res *doctor.Result, yes bool) fixOutcome {
	fixln(w, p.FailureBullet(res.Name+": "+res.Detail))

	rem := doctor.RemediationOf(check)
	if rem == nil {
		fixln(w, "  → no automatic fix; resolve manually per the detail above")
		return outcomeGuidance
	}

	apply, declined := confirmFix(w, p, rem, yes)
	if !apply {
		return declined
	}
	if err := rem.Apply(ctx); err != nil {
		fixln(w, p.FailureBullet("  fix failed: "+err.Error()))
		return outcomeFailed
	}
	return verifyFix(ctx, w, p, check, rem)
}

// confirmFix decides whether fixOneCheck should apply rem. The returned bool is
// the decision; the fixOutcome is meaningful only when the decision is false
// (it names why the fix was not applied). The safety contract lives here: an
// unsafe remediation is never auto-applied under --yes, only after a confirm.
func confirmFix(w io.Writer, p *clistatus.Printer, rem *doctor.Remediation, yes bool) (bool, fixOutcome) {
	if yes {
		if !rem.Safe {
			fixln(w, p.WarnBullet(
				"  flagged: "+rem.Description+" — not auto-fixed; rerun `maz doctor fix` to confirm interactively"))
			return false, outcomeFlagged
		}
		fixln(w, "  → applying: "+rem.Description)
		return true, outcomeApplied
	}
	ok, err := uiform.PromptYN(w, "fix: "+rem.Description, false)
	if err != nil {
		fixln(w, p.FailureBullet("  prompt failed: "+err.Error()))
		return false, outcomeFailed
	}
	if !ok {
		fixln(w, "  → skipped")
		return false, outcomeSkipped
	}
	return true, outcomeApplied
}

// verifyFix re-runs the single check after a successful Apply and reports
// whether the repair landed. A still-failing check counts as a failure so the
// exit code reflects the unresolved problem.
func verifyFix(ctx context.Context, w io.Writer, p *clistatus.Printer, check doctor.Check, rem *doctor.Remediation) fixOutcome {
	status, detail, err := check.Run(ctx)
	if err != nil || status == doctor.StatusFail {
		msg := detail
		if err != nil {
			msg = err.Error()
		}
		fixln(w, p.WarnBullet("  applied but check still failing: "+msg))
		return outcomeFailed
	}
	fixln(w, p.SuccessBullet("  fixed: "+rem.Description))
	return outcomeApplied
}

// fixln writes a progress line for the fix command, discarding the write error:
// these are best-effort messages on a TTY/pipe, not load-bearing output.
func fixln(w io.Writer, s string) {
	_, _ = fmt.Fprintln(w, s) //nolint:errcheck // best-effort progress line
}

// fixf is the formatted sibling of fixln, used for the trailing summary line.
func fixf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...) //nolint:errcheck // best-effort progress line
}

// ── WS12: Setup drift doctor checks ──────────────────────────────────────────

const doctorSubsystemSetup = "setup"

// setupChecks returns the WS12 advisory checks. Every check in this group
// reports WARN on a miss — never FAIL — so the overall exit code stays 0
// when only setup items are outstanding. This keeps `maz doctor` green in
// CI environments that deliberately omit the launch-agent or completion.
func setupChecks(dataDir string) []doctor.Check {
	all := clients.All()
	checks := make([]doctor.Check, 0, len(all)+2)
	checks = append(checks, newSetupDaemonAgentCheck())
	for _, c := range all {
		checks = append(checks, newSetupMCPClientCheck(c, dataDir))
	}
	checks = append(checks, newSetupCompletionCheck())
	return checks
}

// newSetupDaemonAgentCheck returns a doctor.Check that verifies the Mazarbul
// daemon is registered as a persistent launch-agent (launchd on macOS; a no-op
// stub on other platforms that always returns OK).
//
//nolint:ireturn // returns doctor.Check interface; concrete type is internal to doctor package
func newSetupDaemonAgentCheck() doctor.Check {
	return doctor.CheckFunc{
		N:   "setup.daemon-agent",
		Sub: doctorSubsystemSetup,
		Fn: func(ctx context.Context) (doctor.Status, string, error) {
			loaded, err := daemon.LaunchAgentLoaded(ctx)
			if err != nil {
				// Cannot determine state; treat as advisory WARN, not FAIL.
				// The error detail is embedded in the message; returning nil
				// keeps the runner from overriding Status to StatusFail.
				return doctor.StatusWarn, "cannot query launch-agent: " + err.Error(), nil //nolint:nilerr
			}
			if !loaded {
				return doctor.StatusWarn,
					"daemon not registered as a launch-agent — run `maz daemon install` to persist across reboots",
					nil
			}
			return doctor.StatusOK, "launch-agent loaded", nil
		},
	}
}

// newSetupMCPClientCheck returns a doctor.Check that verifies the given AI
// client adapter has an up-to-date Mazarbul MCP registration. It WARNs when
// the client is registered but stale (config exists but points to the wrong
// binary path or server URL) and is silent (OK) when the client is simply not
// installed on this machine.
//
//nolint:ireturn // returns doctor.Check interface; concrete type is internal to doctor package
func newSetupMCPClientCheck(c connect.Client, dataDir string) doctor.Check {
	id := c.ID()
	return doctor.CheckFunc{
		N:   "setup.mcp-client." + id,
		Sub: doctorSubsystemSetup,
		Fn: func(ctx context.Context) (doctor.Status, string, error) {
			st, err := c.Detect(ctx, dataDir)
			if err != nil {
				// Treat detection errors as advisory; embed detail in message.
				return doctor.StatusWarn, id + ": cannot detect MCP status: " + err.Error(), nil //nolint:nilerr
			}
			switch st {
			case connect.StatusRegisteredStale:
				return doctor.StatusWarn,
					id + ": MCP config is stale — run `maz connect " + id + "` to refresh",
					nil
			default:
				// StatusNotInstalled, StatusRegistered, or any future value:
				// not installed = silent OK; registered and fresh = OK.
				return doctor.StatusOK, "", nil
			}
		},
	}
}

// newSetupCompletionCheck returns a doctor.Check that verifies that the
// active shell's RC file contains the maz completion source line. It probes
// the same RC path that `maz completion install` would write.
// The check silently passes (OK) when the shell cannot be detected (e.g. in
// CI), and WARNs when the RC file exists but the completion line is absent.
//
//nolint:ireturn // returns doctor.Check interface; concrete type is internal to doctor package
func newSetupCompletionCheck() doctor.Check {
	return doctor.CheckFunc{
		N:   "setup.completion",
		Sub: doctorSubsystemSetup,
		Fn: func(_ context.Context) (doctor.Status, string, error) {
			shell := detectShell()
			if shell == "" {
				// Cannot determine shell; skip silently.
				return doctor.StatusOK, "shell not detected; skipped", nil
			}
			rcFile := shellRCFile(shell)
			if rcFile == "" {
				return doctor.StatusOK, "no rc file for " + shell + "; skipped", nil
			}
			data, err := os.ReadFile(rcFile)
			if err != nil {
				// RC file absent or unreadable — skip silently.
				if os.IsNotExist(err) {
					return doctor.StatusOK, rcFile + " not found; skipped", nil
				}
				return doctor.StatusWarn, "cannot read " + rcFile + ": " + err.Error(), nil
			}
			wantLine := completionSourceLine(shell)
			if strings.Contains(string(data), wantLine) {
				return doctor.StatusOK, shell + " completion active", nil
			}
			return doctor.StatusWarn,
				"completion not installed — run `maz completion install` or add `" + wantLine + "` to " + rcFile,
				nil
		},
	}
}
