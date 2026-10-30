package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/connect"
	"github.com/mo-cmyk/mazarbul/internal/connect/clients"
)

// newConnectCmd builds the `maz connect` command tree. It exposes one
// command with sub-flags rather than a tree of sub-commands, matching
// the M11 / M8b spec and keeping the agent-callable shape compact.
func newConnectCmd() *cobra.Command {
	var (
		scope     string
		project   string
		dryRun    bool
		force     bool
		uninstall bool
		listOnly  bool
		capture   bool
		warmStart bool
	)
	cmd := &cobra.Command{
		Use:   "connect [client]",
		Short: "Register Mazarbul as an MCP server with detected AI clients.",
		Long: `Connect Mazarbul to AI clients.

With no client argument, prints the live detection table for every known
client. With a client name (claude-code, claude-desktop, …) applies the
registration headlessly. Use 'all' to apply to every detected client.

Apply and uninstall honor --output=json (an array of per-client write
reports) for scripted callers; --list prints the static capability matrix.

The same pipeline backs the init wizard's Connect step.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := buildConnectService()
			if err != nil {
				return err
			}
			if listOnly {
				// --list prints the STATIC supported-agents matrix from the
				// descriptor table (no disk I/O / detection) — a README-pasteable
				// capability table that does not depend on what is installed here.
				return runConnectMatrix(cmd, cmd.OutOrStdout())
			}
			if len(args) == 0 {
				// No-arg path: print the live detection table.
				return runConnectList(cmd.Context(), cmd.OutOrStdout(), svc)
			}
			target := args[0]
			opts := connect.ApplyOptions{DryRun: dryRun, Force: force, Capture: capture, WarmStart: warmStart}
			s := connect.Scope(scope)
			if uninstall {
				return runConnectUninstall(cmd, svc, target, s, project)
			}
			applied, err := runConnectApply(cmd, svc, target, s, project, opts)
			if err != nil {
				return err
			}
			// Only advertise the capture/warm-start posture when a write actually
			// landed; a conflict leaves the config untouched, so claiming hooks
			// were written would be false confidence. Suppress it in machine
			// output modes — the apply results were already emitted as JSON, and
			// trailing human text would corrupt the document.
			if applied && !dryRun && (capture || warmStart) && output.From(cmd).Format() == output.FormatText {
				return printCapturePosture(cmd.OutOrStdout(), capture, warmStart)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "user", "registration scope (user | project)")
	cmd.Flags().StringVar(&project, "project", "", "project root for --scope=project (defaults to CWD)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the intended change without writing")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a conflicting existing mazarbul entry")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "remove the mazarbul entry (and any hooks) instead of applying")
	cmd.Flags().BoolVar(&listOnly, "list", false, "print the static supported-agents matrix and exit")
	cmd.Flags().BoolVar(&capture, "capture", false,
		"also install a passive-capture lifecycle hook (Claude Code Stop) that distills sessions into signed memory")
	cmd.Flags().BoolVar(&warmStart, "warm-start", false,
		"also install a warm-start lifecycle hook (Claude Code SessionStart) that injects a scope-filtered context brief")
	return cmd
}

// printCapturePosture prints the privacy posture after a successful
// --capture/--warm-start apply: capture defaults on (cheap), injection is
// opt-in. Surfacing this makes the always-on capture explicit and points the
// user at the off switch.
func printCapturePosture(w io.Writer, capture, warmStart bool) error {
	captureState := "off"
	if capture {
		captureState = "on — assistant turns are distilled into signed memory (actor=assistant, source=hook:claude-code/stop)"
	}
	injectionState := "off"
	if warmStart {
		injectionState = "on — a scope-filtered context brief is injected at session start"
	}
	_, err := fmt.Fprintf(w,
		"\nPassive capture: %s\nWarm-start injection: %s\n"+
			"Hooks were written to the client's settings.json (a backup was saved).\n"+
			"Disable with: maz connect claude-code --uninstall (also removes the MCP server entry).\n",
		captureState, injectionState)
	return err
}

// newConnectService constructs a connect.Service for dataDir with every
// registered client adapter. Single source of truth for the adapter
// roster so the `maz connect` command, the init wizard's Connect step,
// and detection-driven pickers all stay in sync.
func newConnectService(dataDir string) *connect.Service {
	return connect.NewService(dataDir, clients.All()...)
}

// buildConnectService constructs the connect.Service with every
// registered adapter, resolving the data dir from the loaded config.
func buildConnectService() (*connect.Service, error) {
	cfg, err := config.Load("")
	if err != nil {
		cfg = config.Defaults()
	}
	return newConnectService(cfg.DataDir), nil
}

func runConnectList(ctx context.Context, w io.Writer, svc *connect.Service) error {
	results := svc.DetectAll(ctx)
	if _, err := fmt.Fprintln(w, "ID                  Name                Status"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := fmt.Fprintln(w, "──────────────────  ──────────────────  ─────────────────"); err != nil {
		return fmt.Errorf("write divider: %w", err)
	}
	for _, r := range results {
		errLabel := ""
		if r.Err != nil {
			errLabel = "  err=" + r.Err.Error()
		}
		if _, err := fmt.Fprintf(w, "%-18s  %-18s  %-17s%s\n",
			r.Client.ID(), r.Client.DisplayName(), r.Status.String(), errLabel); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	return nil
}

// runConnectApply applies the registration to every resolved target,
// collecting per-target reports and rendering them once (text/JSON/NDJSON)
// plus a first-registration onboarding tip. It returns applied=true when at
// least one target's write actually landed (a non-conflict result) so the
// caller knows whether the capture/warm-start posture is true; a conflict
// writes neither the MCP entry nor the hooks.
func runConnectApply(cmd *cobra.Command, svc *connect.Service, target string, scope connect.Scope, project string, opts connect.ApplyOptions) (applied bool, err error) {
	targets, err := resolveConnectTargets(svc, target)
	if err != nil {
		return false, err
	}
	// Collect every per-target result, then render once. Each failure is also
	// joined into a single non-nil error returned after rendering: returning
	// early would hide later rows the user/agent needs to see, and exiting 0
	// after a silent no-write would make scripted automation mark the
	// registration complete when it is not.
	results := make([]connectResult, 0, len(targets))
	var failures []error
	for _, t := range targets {
		rep, applyErr := svc.Apply(cmd.Context(), t, scope, project, opts)
		results = append(results, connectResult{report: rep, err: applyErr})
		// Include context.Canceled in the failure set: an interrupted
		// apply did NOT complete the registration, and scripted
		// automation needs a non-zero exit so it doesn't treat a
		// partially-applied batch as a success.
		if applyErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", t, applyErr))
			continue
		}
		// A conflict short-circuits Apply before any write (MCP entry AND
		// hooks); only a non-conflict result means hooks were processed, so
		// the capture posture is accurate.
		if rep.Action != "conflict" {
			applied = true
		}
	}
	// Onboarding nudge (text mode only): registering exposes the memory tools;
	// the preset teaches the agent WHEN to call them. Shown only when a real
	// write happened with no failures — not on --dry-run, an all-noop re-run,
	// or a conflict — so it's a first-registration cue rather than steady noise.
	tip := ""
	if !opts.DryRun && len(failures) == 0 && anyWrote(results) {
		tip = "\nTip: teach your agent when to use memory — `maz preset install agent-memory`."
	}
	if err := emitConnectResults(cmd, results, tip); err != nil {
		return applied, err
	}
	if len(failures) > 0 {
		return applied, errors.Join(failures...)
	}
	return applied, nil
}

// anyWrote reports whether any result was a real config write (vs noop /
// conflict / error), gating the post-apply onboarding tip.
func anyWrote(results []connectResult) bool {
	for i := range results {
		if results[i].report.Wrote() {
			return true
		}
	}
	return false
}

func runConnectUninstall(cmd *cobra.Command, svc *connect.Service, target string, scope connect.Scope, project string) error {
	targets, err := resolveConnectTargets(svc, target)
	if err != nil {
		return err
	}
	// See runConnectApply: collect every result, render once, then surface a
	// joined error so a partial uninstall exits non-zero instead of silently
	// dropping a target.
	results := make([]connectResult, 0, len(targets))
	var failures []error
	for _, t := range targets {
		rep, uninstallErr := svc.Uninstall(cmd.Context(), t, scope, project)
		results = append(results, connectResult{report: rep, err: uninstallErr})
		// See runConnectApply: cancellation also counts as failure so
		// `maz connect <id> --uninstall` returns non-zero when the
		// removal did not complete.
		if uninstallErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", t, uninstallErr))
		}
	}
	if err := emitConnectResults(cmd, results, ""); err != nil {
		return err
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

// connectResult pairs a per-target write report with the error (if any) from
// applying or uninstalling it, so the text and JSON renderers can show both.
type connectResult struct {
	report connect.WriteReport
	err    error
}

// connectReportJSON is the machine-readable shape `maz connect` apply/uninstall
// emit under --output=json: one object per target, with the error (if any)
// inline so scripted callers branch on the document rather than parsing stderr.
type connectReportJSON struct {
	ClientID   string `json:"clientId"`
	Scope      string `json:"scope"`
	Action     string `json:"action,omitempty"`
	ConfigPath string `json:"configPath,omitempty"`
	BackupPath string `json:"backupPath,omitempty"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
}

// connectReportRow projects a connectResult into its machine-readable shape.
// Context.Canceled is rendered as a normal (non-error) row in text mode; mirror
// that here so a cancelled batch isn't double-flagged.
func connectReportRow(r *connectResult) connectReportJSON {
	row := connectReportJSON{
		ClientID:   r.report.ClientID,
		Scope:      string(r.report.Scope),
		Action:     r.report.Action,
		ConfigPath: r.report.ConfigPath,
		BackupPath: r.report.BackupPath,
		Message:    r.report.Message,
	}
	if r.err != nil && !errors.Is(r.err, context.Canceled) {
		row.Error = r.err.Error()
	}
	return row
}

// emitConnectResults renders the per-target results in the command's output
// format via the output.Writer (no human tip in machine modes — it would
// corrupt the document). JSON emits one array; NDJSON emits one object per line
// (the output package's contract); text prints one aligned row per result plus
// the trailing onboarding tip when non-empty.
func emitConnectResults(cmd *cobra.Command, results []connectResult, tip string) error {
	wr := output.From(cmd)
	switch wr.Format() {
	case output.FormatNDJSON:
		for i := range results {
			if err := wr.Emit(connectReportRow(&results[i])); err != nil {
				return fmt.Errorf("emit row: %w", err)
			}
		}
		return nil
	case output.FormatJSON:
		rows := make([]connectReportJSON, 0, len(results))
		for i := range results {
			rows = append(rows, connectReportRow(&results[i]))
		}
		if err := wr.Emit(rows); err != nil {
			return fmt.Errorf("emit rows: %w", err)
		}
		return nil
	default:
		w := cmd.OutOrStdout()
		for i := range results {
			if _, err := fmt.Fprintln(w, formatConnectReport(&results[i].report, results[i].err)); err != nil {
				return fmt.Errorf("write row: %w", err)
			}
		}
		if tip != "" {
			if _, err := fmt.Fprintln(w, tip); err != nil {
				return fmt.Errorf("write tip: %w", err)
			}
		}
		return nil
	}
}

func resolveConnectTargets(svc *connect.Service, target string) ([]string, error) {
	if target == "all" {
		ids := make([]string, 0, len(svc.Clients()))
		for _, c := range svc.Clients() {
			ids = append(ids, c.ID())
		}
		return ids, nil
	}
	if _, ok := svc.Find(target); !ok {
		known := make([]string, 0, len(svc.Clients()))
		for _, c := range svc.Clients() {
			known = append(known, c.ID())
		}
		return nil, fmt.Errorf("unknown client %q (known: %s)",
			target, strings.Join(known, ", "))
	}
	return []string{target}, nil
}

// runConnectMatrix renders the static supported-agents matrix: every client
// the descriptor table knows, with its config format, supported scopes, and
// user-scope config path. Detection-free so the output is stable and pasteable
// into the README. Honors --output=json for scripting; in text mode it also
// lists the clients we deliberately do not auto-wire (and why).
func runConnectMatrix(cmd *cobra.Command, w io.Writer) error {
	caps := clients.Matrix()
	wr := output.From(cmd)
	switch wr.Format() {
	case output.FormatNDJSON:
		// One capability object per line (the output package's NDJSON contract).
		for i := range caps {
			if err := wr.Emit(caps[i]); err != nil {
				return fmt.Errorf("emit row: %w", err)
			}
		}
		return nil
	case output.FormatJSON:
		if err := wr.Emit(caps); err != nil {
			return fmt.Errorf("emit matrix: %w", err)
		}
		return nil
	default:
		// FormatText and any unrecognised format fall back to the human table;
		// default-signifies-exhaustive keeps this within the exhaustive linter.
		return renderConnectMatrixText(cmd, caps)
	}
}

// renderConnectMatrixText prints the human supported-agents table (with ~-home
// collapsing) followed by the deliberately-unsupported footnote.
func renderConnectMatrixText(cmd *cobra.Command, caps []clients.Capability) error {
	home, _ := os.UserHomeDir() //nolint:errcheck // best-effort ~ collapsing; empty home just prints absolute paths.
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "id"},
		{Title: "name"},
		{Title: "format"},
		{Title: "scopes"},
		{Title: "config path (user)"},
	})
	for _, cap := range caps {
		path := cap.UserConfigPath
		if home != "" && strings.HasPrefix(path, home) {
			path = "~" + path[len(home):]
		}
		tbl.Row(cap.ID, cap.DisplayName, cap.Format, strings.Join(cap.Scopes, "+"), path)
	}
	if err := tbl.Render(); err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	unsupported := clients.KnownUnsupported()
	if len(unsupported) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nNot auto-wired (divergent config shape or GUI-managed):"); err != nil {
		return fmt.Errorf("write footer: %w", err)
	}
	for _, u := range unsupported {
		if _, err := fmt.Fprintf(w, "  %-12s  %s\n", u.ID, u.Reason); err != nil {
			return fmt.Errorf("write footer row: %w", err)
		}
	}
	return nil
}

func formatConnectReport(rep *connect.WriteReport, err error) string {
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Sprintf("%-18s  %s  error: %s",
			rep.ClientID, rep.Scope, err.Error())
	}
	msg := rep.Message
	if msg != "" {
		msg = "  " + msg
	}
	return fmt.Sprintf("%-18s  %-8s  %-9s  %s%s",
		rep.ClientID, rep.Scope, rep.Action, rep.ConfigPath, msg)
}
