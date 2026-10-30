package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	eventlog "github.com/mo-cmyk/mazarbul/internal/kernel"
)

// Shell name constants used across completion functions.
const (
	shellBash = "bash"
	shellZsh  = "zsh"
	shellFish = "fish"
)

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion <shell>",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for bash, zsh, fish, or powershell.

Source the output in your shell config to enable tab-completion for all
maz subcommands and flags.

Exit codes:
  0   success
  1   unknown shell
  2   unexpected failure

Examples:
  # Bash
  maz completion bash > ~/.bash_completion.d/maz
  source ~/.bash_completion.d/maz

  # Zsh (add to ~/.zshrc):
  source <(maz completion zsh)

  # Fish
  maz completion fish | source

  # PowerShell
  maz completion powershell | Out-String | Invoke-Expression

  # Interactive install (TTY only):
  maz completion install`,
		ValidArgs: []string{shellBash, shellZsh, shellFish, "powershell"},
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return runCompletion(cmd, args[0])
		},
	}
	cmd.AddCommand(newCompletionInstallCmd())
	return cmd
}

func newCompletionInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Interactively install shell completion (TTY only)",
		Long: `Detect the active shell and offer to install the completion script
into the appropriate config file. This is a convenience wrapper around
sourcing the completion output; in scripted mode just pipe the output
of the shell-specific subcommand instead.

Exit codes:
  0   installed or skipped by user
  1   unsupported shell
  2   I/O error

Examples:
  maz completion install`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCompletionInstall(cmd)
		},
	}
}

func runCompletion(cmd *cobra.Command, shell string) error {
	root := cmd.Root()
	w := cmd.OutOrStdout()
	var err error
	switch strings.ToLower(shell) {
	case shellBash:
		err = root.GenBashCompletionV2(w, true)
	case shellZsh:
		err = root.GenZshCompletion(w)
	case shellFish:
		err = root.GenFishCompletion(w, true)
	case "powershell", "ps":
		err = root.GenPowerShellCompletionWithDesc(w)
	default:
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintf(cmd.ErrOrStderr(), "completion: unknown shell %q (want bash|zsh|fish|powershell)\n", shell)
		return silentError{code: 1, err: errors.New("unknown shell: " + shell)}
	}
	if err != nil {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintf(cmd.ErrOrStderr(), "completion: %v\n", err)
		return silentError{code: 2, err: err}
	}
	return nil
}

// runCompletionInstall detects the user's shell and offers an interactive
// install. In non-TTY / scripted mode it falls back to printing instructions.
func runCompletionInstall(cmd *cobra.Command) error {
	shell := detectShell()
	if shell == "" {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintf(cmd.ErrOrStderr(),
			"completion install: could not detect shell (SHELL=%s); run `maz completion <shell>` manually\n",
			os.Getenv("SHELL"))
		return silentError{code: 1, err: errors.New("could not detect shell")}
	}

	rcFile := shellRCFile(shell)
	if rcFile == "" {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintf(cmd.ErrOrStderr(),
			"completion install: no known rc file for %s; run `maz completion %s` manually\n",
			shell, shell)
		return silentError{code: 1, err: errors.New("no rc file for shell " + shell)}
	}

	wr := output.From(cmd)
	if !wr.IsTTY() {
		// Non-interactive path: print instructions only.
		//nolint:errcheck // best-effort stdout write
		fmt.Fprintf(cmd.OutOrStdout(),
			"To install %s completion, add the following to %s:\n\n  %s\n",
			shell, rcFile, completionSourceLine(shell))
		return nil
	}

	// Interactive confirm via the shared uiform surface.
	var confirmed bool
	form := uiform.New(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Add `%s` to %s?", completionSourceLine(shell), rcFile)).
				Value(&confirmed),
		),
	)
	if err := form.Run(); err != nil {
		// User cancelled or terminal error — treat as skip, not failure.
		return nil //nolint:nilerr // form.Run errors on ESC/Ctrl-C; treat as user cancel
	}
	if !confirmed {
		//nolint:errcheck // best-effort stdout write
		fmt.Fprintln(cmd.OutOrStdout(), "skipped")
		return nil
	}

	if err := appendToFile(rcFile, "\n# maz shell completion\n"+completionSourceLine(shell)+"\n"); err != nil {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintf(cmd.ErrOrStderr(), "completion install: write %s: %v\n", rcFile, err)
		return silentError{code: 2, err: err}
	}
	//nolint:errcheck // best-effort stdout write
	fmt.Fprintf(cmd.OutOrStdout(), "written to %s — restart your shell or run: source %s\n", rcFile, rcFile)
	return nil
}

// detectShell returns the active shell name (bash, zsh, fish) from $SHELL.
func detectShell() string {
	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case shellBash, shellZsh, shellFish:
		return shell
	default:
		return ""
	}
}

// shellRCFile returns the canonical RC file for a shell.
func shellRCFile(shell string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch shell {
	case shellBash:
		return filepath.Join(home, ".bashrc")
	case shellZsh:
		return filepath.Join(home, ".zshrc")
	case shellFish:
		return filepath.Join(home, ".config", "fish", "config.fish")
	default:
		return ""
	}
}

// appendToFile appends text to path, creating the file if absent.
func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

// ── WS1: Dynamic shell completions ───────────────────────────────────────────

// completionDaemonTimeout is the hard wall-clock budget for daemon-backed
// completions (--predicate, --kind). It must be short enough that tab
// completion feels instant even when the daemon is down.
const completionDaemonTimeout = 300 * time.Millisecond

// RegisterDynamicCompletions walks the entire command tree and registers
// flag completion functions for well-known flags. It is called once from
// newRootCmd, after all sub-commands have been added.
//
// Flags are grouped into two tiers:
//   - Static enums: values are hard-coded constants; completion is instant.
//   - Daemon-best-effort: we ask the daemon's introspect tool with a hard
//     300 ms deadline; any error/timeout silently returns nil so the shell
//     falls back to no completions rather than filename completion.
//
// Calling InitDefaultHelpFlag first merges persistent parent flags into the FlagSet.
func RegisterDynamicCompletions(root *cobra.Command) {
	walkCmds(root, func(cmd *cobra.Command) {
		// Merge parent persistent flags into cmd.Flags() so that
		// RegisterFlagCompletionFunc can find inherited flags.
		cmd.InitDefaultHelpFlag()

		// Static enums — instant, no I/O.
		tryRegisterFlagCompletion(cmd, "scope", completeScopeFlag)
		tryRegisterFlagCompletion(cmd, "output", completeOutputFlag)
		tryRegisterFlagCompletion(cmd, "sort-by", completeSortByFlag)
		tryRegisterFlagCompletion(cmd, "window", completeWindowFlag)
		tryRegisterFlagCompletion(cmd, "span", completeSpanFlag)
		tryRegisterFlagCompletion(cmd, "by", completeByFlag)

		// Daemon-best-effort — ≤300 ms or nil+NoFileComp.
		tryRegisterFlagCompletion(cmd, "predicate", completeDaemonPredicatesFlag)
		tryRegisterFlagCompletion(cmd, "kind", completeDaemonKindsFlag)
	})
}

// walkCmds visits cmd and all of its descendants depth-first.
func walkCmds(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, sub := range cmd.Commands() {
		walkCmds(sub, fn)
	}
}

// tryRegisterFlagCompletion calls RegisterFlagCompletionFunc and silently
// ignores errors (flag not present, already registered, etc.).
func tryRegisterFlagCompletion(cmd *cobra.Command, flagName string, fn func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)) {
	//nolint:errcheck // flag may not exist on this command; that is expected
	cmd.RegisterFlagCompletionFunc(flagName, fn)
}

// completeScopeFlag returns the three canonical scope values.
func completeScopeFlag(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return []string{
		string(eventlog.ScopeProject) + "\tProject scope (git-tracked, plaintext)",
		string(eventlog.ScopePersonal) + "\tPersonal scope (local, encrypted)",
		string(eventlog.ScopeTeam) + "\tTeam scope (encrypted, shared)",
	}, cobra.ShellCompDirectiveNoFileComp
}

// completeOutputFlag returns the three output format values.
func completeOutputFlag(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return []string{
		"text\tHuman-readable text (default on TTY)",
		"json\tMachine-readable JSON",
		"ndjson\tNewline-delimited JSON stream",
	}, cobra.ShellCompDirectiveNoFileComp
}

// completeSortByFlag returns the two sort-by values used by nav commands.
func completeSortByFlag(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return []string{
		"recent\tMost recently created first (default)",
		"recallCount\tMost frequently recalled first",
	}, cobra.ShellCompDirectiveNoFileComp
}

// completeWindowFlag returns the window values for --sort-by=recallCount.
func completeWindowFlag(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return []string{
		"today\tRecall counts for today",
		"last7Days\tRecall counts for the past 7 days",
		"last30Days\tRecall counts for the past 30 days",
		"lifetime\tAll-time recall counts",
	}, cobra.ShellCompDirectiveNoFileComp
}

// completeSpanFlag returns the span values used by activity/digest commands.
func completeSpanFlag(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return []string{
		"30d\t30-day span",
		"90d\t90-day span (default)",
		"52w\t52-week (one-year) span",
	}, cobra.ShellCompDirectiveNoFileComp
}

// completeByFlag returns the grouping values used by digest.
func completeByFlag(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return []string{
		"writer\tGroup by writer / author",
		"kind\tGroup by entity kind",
	}, cobra.ShellCompDirectiveNoFileComp
}

// completeDaemonPredicatesFlag asks the running daemon for the list of
// registered predicates via the introspect MCP tool. Returns nil +
// NoFileComp on any error or timeout.
func completeDaemonPredicatesFlag(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return fetchDaemonCompletions(cmd, "predicates")
}

// completeDaemonKindsFlag asks the running daemon for the list of registered
// entity kinds via the introspect MCP tool. Returns nil + NoFileComp on any
// error or timeout.
func completeDaemonKindsFlag(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return fetchDaemonCompletions(cmd, "entityKinds")
}

// fetchDaemonCompletions calls the daemon's introspect tool and extracts
// the completion candidates for the given field ("predicates" or
// "entityKinds"). The entire round-trip is bounded by completionDaemonTimeout.
// Any failure returns (nil, NoFileComp) so the shell never falls back to
// filename completion.
func fetchDaemonCompletions(cmd *cobra.Command, field string) ([]string, cobra.ShellCompDirective) {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	ctx, cancel := context.WithTimeout(context.Background(), completionDaemonTimeout)
	defer cancel()

	raw, err := invokeToolViaDaemon(ctx, dataDir, "introspect", json.RawMessage(`{}`))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var out struct {
		Predicates []struct {
			Predicate string `json:"predicate"`
		} `json:"predicates"`
		EntityKinds []struct {
			Kind string `json:"kind"`
		} `json:"entityKinds"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	switch field {
	case "predicates":
		items := make([]string, 0, len(out.Predicates))
		for _, p := range out.Predicates {
			if p.Predicate != "" {
				items = append(items, p.Predicate)
			}
		}
		return items, cobra.ShellCompDirectiveNoFileComp
	case "entityKinds":
		items := make([]string, 0, len(out.EntityKinds))
		for _, k := range out.EntityKinds {
			if k.Kind != "" {
				items = append(items, k.Kind)
			}
		}
		return items, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completionSourceLine returns the line newCompletionInstallCmd appends to a
// shell's rc file to enable completion. Fish has no process substitution, so it
// uses the idiomatic pipe form (`maz completion fish | source`); bash and zsh
// use `source <(maz completion <shell>)`. The setup.completion doctor check
// matches against this same per-shell line so it stays in lockstep with the
// installer across bash/zsh/fish.
func completionSourceLine(shell string) string {
	if shell == "fish" {
		return "maz completion fish | source"
	}
	return fmt.Sprintf("source <(maz completion %s)", shell)
}
