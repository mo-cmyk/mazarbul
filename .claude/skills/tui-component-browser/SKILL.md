---
name: tui-component-browser
description: Before writing a new TUI feature, browse the Charm component libraries for existing components. USE WHEN starting a new Bubble Tea screen, picking between custom and existing components, or unsure which component fits. Uses WebFetch to grab the source of relevant components from charmbracelet/bubbles and charmbracelet/huh.
allowed-tools: Read, Glob, Grep, WebFetch
---

# TUI Component Browser

Before writing a custom Bubble Tea component, check the Charm libraries. Reinventing burns time and ships inconsistent UX.

## Workflow

1. **Identify the need.** What does the user need to do? (Pick, type, scroll, watch progress, see a list, edit a long string, confirm.)

2. **Browse the catalog.** Fetch the index of each library:
   - `WebFetch https://github.com/charmbracelet/bubbles` -> see the README's component list.
   - `WebFetch https://github.com/charmbracelet/huh` -> see the README's form primitives.

3. **Match the need to a component.** Use the table below as a starting point.

4. **Read the component source.** If a candidate exists, fetch its source to confirm the API fits:
   - `WebFetch https://github.com/charmbracelet/bubbles/blob/master/<component>/<component>.go`
   - `WebFetch https://github.com/charmbracelet/huh/blob/main/<primitive>.go`

5. **Decide.** Use the existing component unless it provably doesn't fit. "It would feel slightly different" is not a reason to roll your own.

## Need -> component map

| Need | Library | Component | Notes |
|---|---|---|---|
| Pick from a list (single) | huh | `huh.NewSelect[T]` | Form primitive, embed in form or use standalone. |
| Pick from a list (multi) | huh | `huh.NewMultiSelect[T]` | Same as above. |
| Free text input | huh | `huh.NewInput` | Validation, masking, placeholder support. |
| Confirm yes/no | huh | `huh.NewConfirm` | Default focus on the safe option. |
| Note / instruction | huh | `huh.NewNote` | Markdown-style content step. |
| Loading spinner | bubbles | `spinner.New(spinner.Moon)` | Moon for thinking, Dot for short waits. |
| Measurable progress | bubbles | `progress.New()` | Animate via `progress.IncrPercent`. |
| Scrollable view | bubbles | `viewport.New(w, h)` | Set content via `viewport.SetContent`. |
| Selectable list | bubbles | `list.New(items, delegate, w, h)` | Built-in filter and pagination. |
| Tabular data | bubbles | `table.New(...)` | Sortable columns, keyboard navigation. |
| Pagination control | bubbles | `paginator.New()` | Dots-style page indicator. |
| Single-line text input | bubbles | `textinput.New()` | Lower-level than `huh.NewInput`. |
| Multi-line text input | bubbles | `textarea.New()` | For longer-form editing. |
| File picker | huh | `huh.NewFilePicker` | Browse + pick a path. |
| Key bindings | bubbles | `key.NewBinding` | Centralizes help text and binding. |
| Help panel | bubbles | `help.New()` | Renders key bindings as a help row. |

## When to roll your own

Custom components are justified when:
- The interaction model doesn't exist in either library (rare).
- The visual treatment requires a layout primitive not in lipgloss.
- The component is genuinely Mazarbul-specific (e.g. the Watchword block, the entity provenance trail).

If you're rolling your own:
- Live under `internal/tui/<screen>/components/` if scoped to one screen.
- Live under `internal/tui/components/` if shared across screens.
- Follow the same `Init/Update/View` pattern as bubbles.
- Add a snapshot test.

## Reading the source

Charm code is small and readable. When in doubt, read the component's `view.go` and `update.go`. The conventions you'll pick up apply across the whole stack.

## Gotchas

- **Different `Update` signatures.** Some components return `tea.Cmd`, some return `(Model, tea.Cmd)`. Read before integrating.
- **Theme integration.** Some components accept a styles struct (e.g. `list.DefaultStyles()`). Override via `internal/tui/style.Theme` accessors, not raw hex.
- **Key binding conflicts.** Embedded components may bind keys that the host model also wants. Use `key.Binding.SetEnabled(false)` to suppress when the host has focus.
- **Help text consistency.** Use `bubbles/help.Model` with `key.NewBinding(key.WithHelp(...))` so every screen's help row reads the same way.
