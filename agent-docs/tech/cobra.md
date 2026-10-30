# Cobra

Mazarbul uses `github.com/spf13/cobra` for the CLI command tree. Commands live in `internal/cli/`.

## Command authoring pattern

One file per top-level subcommand: `init.go`, `remember.go`, `recall.go`, etc. Each file follows the same shape:

```go
package cli

import (
    "github.com/spf13/cobra"
    "github.com/mo-cmyk/mazarbul/internal/tui"
)

func newRememberCmd() *cobra.Command {
    var fact string
    cmd := &cobra.Command{
        Use:     "remember <fact>",
        Short:   "Append a fact to the event log",
        Args:    cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            fact = args[0]
            return runRemember(cmd.Context(), fact)
        },
    }
    return cmd
}
```

Register the command in `root.go`:

```go
func newRootCmd() *cobra.Command {
    root := &cobra.Command{...}
    root.AddCommand(newRememberCmd())
    return root
}
```

## `PersistentPreRun` for mode detection

Mode detection (TUI vs CLI, theme resolution, logger setup) lives in the root command's `PersistentPreRunE`:

```go
root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
    mode := tui.DetectMode(cmd)
    cmd.SetContext(tui.WithMode(cmd.Context(), mode))
    // ... theme resolution, logger injection
    return nil
}
```

Every subcommand can then pull mode + theme + logger from `cmd.Context()`.

## RunE, not Run

Always use `RunE` (returns `error`) instead of `Run` (no return). Errors propagate to `cobra` which prints them through `cmd.PrintErrln` and exits non-zero.

## Use `cmd.OutOrStderr()` and `cmd.OutOrStdout()`

For output, prefer `cmd.OutOrStdout()` over `os.Stdout` and `cmd.OutOrStderr()` over `os.Stderr`. The command writers are swappable in tests:

```go
fmt.Fprintln(cmd.OutOrStdout(), "remembered:", id)
```

Tests then assert against a `bytes.Buffer` set via `cmd.SetOut(buf)` and `cmd.SetErr(buf)`.

## Help and reference generation

`cobra/doc` generates Markdown help pages from the command tree. Use it to populate the user-facing CLI reference at `docs/reference/cli.mdx`:

```go
import "github.com/spf13/cobra/doc"

func generateCLIReference(root *cobra.Command, outDir string) error {
    return doc.GenMarkdownTree(root, outDir)
}
```

A `mise run docs:gen` task (to be added; see `agent-docs/synthesis.md` open items) will invoke this on every release.

## Flag conventions

- Long flags use kebab-case: `--data-dir`, `--no-tui`, `--debug-addr`.
- Short flags only when ergonomic and unambiguous: `-v` for verbose, `-h` for help (cobra reserves).
- Required flags via `cmd.MarkFlagRequired("name")`.
- Bind flags to env vars via Viper, **only** for flags that genuinely want env-var configuration (data dir, theme, API keys). Per-command operational flags stay flag-only.
