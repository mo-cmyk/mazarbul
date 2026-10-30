---
name: tui-design
description: Patterns and rules for building Mazarbul TUIs with the Charm stack. USE WHEN designing or implementing any Bubble Tea / Huh / lipgloss screen, picking components, applying themes, writing teatest snapshots. References agent-docs/tui-design.md for the canonical design system.
allowed-tools: Read, Glob, Grep, WebFetch
paths:
  - "internal/tui/**"
---

# Mazarbul TUI Design System

The canonical reference is `agent-docs/tui-design.md`. This skill summarizes the rules.

## The Elm Architecture

Every Bubble Tea screen has three parts:

- **Init**: returns the initial model and any startup commands.
- **Update**: pure function `(model, msg) -> (model, cmd)`. No I/O inline. Side effects are commands.
- **View**: pure function `model -> string`. Renders the current state.

Keep models small and orthogonal. If a model has more than ~10 fields, split it into sub-models.

## The Charm stack

- `github.com/charmbracelet/bubbletea` runtime.
- `github.com/charmbracelet/bubbles` components: `spinner` (Moon for thinking, Dot for waits), `progress`, `list`, `viewport`, `textinput`, `table`, `paginator`, `key`.
- `github.com/charmbracelet/lipgloss` styling, adaptive colors, layouts, borders.
- `github.com/charmbracelet/huh` forms. Always Huh for input collection in TUI mode, never raw scanners.
- `github.com/charmbracelet/glamour` markdown rendering in terminal.
- `github.com/charmbracelet/x/exp/teatest` snapshot and integration tests.
- `golang.org/x/term` TTY detection.

## Theme discipline

All colors come from `internal/tui/style.Theme`:

```go
import "github.com/mo-cmyk/mazarbul/internal/tui/style"

style.Accent()    // brand color
style.Subtle()    // secondary text
style.Faint()     // tertiary text, borders
style.Success()
style.Warning()
style.Error()
style.Info()
style.Panel()     // bordered panel style
style.Code()      // code block style
```

**Never write raw hex outside `internal/tui/style/themes/`.** CI catches this.

Built-in themes:
- `defender` (default, Range Rover Coniston green)
- `parchment` (warm scroll/book)
- `mithril` (cool silvery monochrome)
- `nocolor` (auto-selected on `NO_COLOR`)

User themes load from TOML via `--theme=<path>`.

## Component selection

Before writing a custom component, browse:
- https://github.com/charmbracelet/bubbles
- https://github.com/charmbracelet/huh

Use the `/tui-component-browser` skill, it captures the workflow.

Common picks:
- Input collection -> `huh.Form` with `huh.NewSelect`, `huh.NewInput`, `huh.NewConfirm`.
- Long-running task -> `bubbles/spinner.New(spinner.Moon)`.
- Measurable progress -> `bubbles/progress.New()`.
- Selectable list -> `bubbles/list.New()`.
- Scrollable content -> `bubbles/viewport.New()`.
- Tabular data -> `bubbles/table.New()`.

## Layout primitives

- `lipgloss.JoinHorizontal(lipgloss.Top, ...)` for side-by-side.
- `lipgloss.JoinVertical(lipgloss.Left, ...)` for stacked.
- `lipgloss.NewStyle().Border(lipgloss.RoundedBorder())` for panels.
- `lipgloss.Width(s)` plus `lipgloss.Height(s)` for measuring.

Compose layouts top-down: outer panel, then inner sections, then content.

## Animation

- Spinner Moon for "thinking" states (LLM-style work).
- Spinner Dot for short blocking work (<2s).
- `bubbles/progress` for measurable work.
- `tea.Tick` for timed reveals (welcome splash, success animation).
- No animation longer than 1 second blocks input.
- Every blocking operation >200ms shows a spinner with a label that describes what's happening.

The `/12-principles-of-animation` skill applies to terminal too: anticipation, follow-through, easing.

## TTY detection and dual-mode

Every command supports two modes:
- **TUI mode** (default when stdout is a TTY): full Bubble Tea program.
- **CLI mode** (when piped, `--no-tui`, or `--json`): plain output, no escape sequences.

Detection lives in `internal/tui/mode.go`. The `/cli-developer` skill owns the CLI side.

## Accessibility

- Honor `NO_COLOR`. lipgloss auto-detects; the `nocolor` theme is auto-selected.
- Every TUI command has `--no-tui` for screen readers and CI logs.
- Never communicate state through color alone. Pair with text or shape.
- Exit codes: 0 success, 1 expected failure, 2 unexpected, 130 user-cancelled.

## Testing

Every Bubble Tea model gets a `teatest` snapshot:

```go
func TestInit_DefaultTheme(t *testing.T) {
    tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))
    // drive it
    teatest.RequireEqualOutput(t, output)
}
```

Snapshots live under `internal/tui/<screen>/testdata/`. Update via `mise run tui:snap`.

Minimum tests per screen:
1. Default theme snapshot.
2. `parchment` theme snapshot.
3. `nocolor` (`NO_COLOR=1`) snapshot.
4. Keyboard navigation test (all interactive paths reachable without mouse).

## Demo recording

`docs/demos/<name>.tape` -> VHS renders -> `docs/demos/<name>.gif`. Run `mise run demo`.

## Common pitfalls

- **Raw hex outside the style package** breaks the theme swap. Caught by CI.
- **Animation that blocks Update** freezes the UI. Always use `tea.Cmd`.
- **Forgetting NO_COLOR**. Test with `NO_COLOR=1 ./bin/maz <command>`.
- **Bubbles components have their own message types**, when embedding a sub-model, route messages explicitly.
- **`huh.Form.Run()` is synchronous**, suitable for one-shot wizards but not embeddable in a larger Bubble Tea model. For embedding, use the `huh.Form` as a Bubble Tea model directly.
