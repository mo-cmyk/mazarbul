# Init Wizard Design Document

**Status**: Draft
**Author**: Moritz
**Created**: 2026-05-24
**Last updated**: 2026-05-24

## Context

`maz init` is the first emotional moment in Mazarbul. Per `agent-docs/lifecycle-theming.md`, init is a "welcome moment" where the slogan goes front-and-centre and a single scroll glyph is allowed. Before this PR, `internal/cli/init.go:runInitTUI` rendered the slogan with a single `style.Brand().Render(slogan)` print, then ran a one-shot Huh form for data-dir + provider + confirm, then a spinner, then a final panel. There was no welcome splash and no Bubble Tea program shell.

Epic [#17](https://github.com/mo-cmyk/mazarbul/issues/17) splits that one-shot Huh form into four per-step Bubble Tea models, each shipped as its own child issue: welcome splash, data-dir confirmation, provider + model selection, first-page inscription. This doc covers the wizard as a whole and is updated by every child PR; the first contribution (issue [#24](https://github.com/mo-cmyk/mazarbul/issues/24)) is the welcome splash.

## Goals

- [x] Welcome splash that renders the project name, the slogan from `agent-docs/synthesis.md`, a one-paragraph framing, a scroll glyph, and a "press any key to continue" hint.
- [x] Theme-aware via the existing resolver: `--theme` flag → `MAZ_THEME` env → config → `defender`; `NO_COLOR=1` forces `nocolor`.
- [x] Any non-modifier key advances; Ctrl-C and `q` cancel cleanly with exit code 0 and a one-line message.
- [x] teatest snapshots for the `defender` and `nocolor` themes committed under `internal/tui/wizard/testdata/`.
- [ ] Data-dir confirmation step (peer child).
- [ ] Provider + model selection step (peer child).
- [ ] First-page inscription step (peer child; depends on event-log writer + Episode schema).

## Non-goals

- Theme picker UI (theme stays at `defender` for M1; pickable via `--theme` flag).
- Watchword recovery seed (M4).
- Sync pairing via `mellon` (M4).
- Refactoring the existing Huh form in `runInitTUI`; peer children replace each step one at a time.

## Proposal

### Architecture

The wizard is a sequence of Bubble Tea models, one per step, each owned by its own file in `internal/tui/wizard/`. Each step model exposes:

- A constructor returning a `tea.Model`.
- A `RunStep(ctx) (advance bool, err error)` bridge function that wraps the model in a `tea.NewProgram`, runs it, and inspects the final model state to return a typed advance/cancel decision.

The bridge functions are called sequentially from `internal/cli/init.go:runInitTUI`. Falling out of any step on cancel prints `"init cancelled; rerun anytime with `maz init`"` and returns `nil`.

### Integration with the existing Huh flow

The pre-#24 `runInitTUI` already gathered data-dir, provider, and a confirm via a single Huh form. **The welcome step is inserted ahead of the existing Huh form, not in place of it.** Peer children under epic #17 replace each Huh step with a Bubble Tea model one at a time; until they land, the Huh form stays as the transitional bridge so a user running `maz init` keeps the ability to override defaults.

Flow for this PR:

1. `wizard.RunWelcome(ctx)` — the splash. Cancel (Ctrl-C / `q`) exits with the friendly message; advance returns to `runInitTUI`.
2. `huh.NewForm(...).Run()` — preserved data-dir / provider / confirm prompts. Declining the confirm prints the same `init cancelled; rerun anytime with \`maz init\`` line.
3. `spinner.New().Title("Inscribing the first page...").Run()` — preserved.
4. `panel.Render(panel.Neutral, body)` — preserved final result panel.

### Step model contract

Every step model embeds the following pattern:

```go
type outcome int

const (
    outcomeNone outcome = iota
    outcomeAdvance
    outcomeCancel
)

type AdvanceMsg struct{}
type CancelMsg struct{}
```

`AdvanceMsg` and `CancelMsg` are exported so future composite wizard models (parent program embedding child steps) can route them. The current per-step bridge does not emit them as `tea.Msg`s; it inspects the model's `outcome` field after `tea.Quit`. The types exist now so the contract is locked.

### Welcome step (issue #24, this PR)

**File:** `internal/tui/wizard/welcome.go`

**Visual elements** (top to bottom, left-aligned with a two-space indent; no centering, no emoji):

1. Slogan line: `style.Brand()`-rendered `"Mazarbul"` (the project name) followed by plain text `", the system of records for the era of agents."`. The synthesis.md slogan in full, with the project name pulled out so it carries the only colour on the screen.
2. Blank line.
3. Framing paragraph, hand-broken into two plain lines: `"We will pick where Mazarbul stores its book, which provider"` and `"it talks to, and write the first page together."`. No styling, terminal default foreground.
4. Blank line.
5. `style.Faint()`-rendered hint: `"press any key to continue  ·  q to cancel"`.

**Key bindings:**

- `Ctrl-C` → cancel (exit code 0, friendly message).
- `q` (single rune, no modifiers) → cancel.
- Everything else (including `Enter`, `Space`, `Esc`, letter keys, arrow keys) → advance.

Bare modifier presses (Shift, Ctrl, Alt without a paired key) do not produce a `tea.KeyMsg`, so the "any non-modifier key" requirement is naturally enforced.

**Render mode:** inline (no `tea.WithAltScreen()`). The splash stays in terminal scrollback after the program exits, matching how the existing Huh form renders today.

### API

The package exports the following surface from `internal/tui/wizard/welcome.go`:

```go
// AdvanceMsg signals the wizard should advance to the next step.
type AdvanceMsg struct{}

// CancelMsg signals the wizard was cancelled.
type CancelMsg struct{}

// RunWelcome runs the welcome splash as a one-shot Bubble Tea program. It
// returns (true, nil) when the user advances, (false, nil) when the user
// cancels with Ctrl-C or q, and (false, err) on runtime error.
func RunWelcome(ctx context.Context) (advance bool, err error)
```

The model type (`welcomeModel`) and its constructor (`newWelcome`) stay unexported; only the bridge function and the message types are public.

### Storage impact

None. The welcome step is pure UI; no event-log writes, no graph mutations, no vector index activity.

### Privacy and provenance

| Concern | Impact | Mitigation |
|---|---|---|
| Provenance attached | N/A | UI-only step |
| Scope tagging | N/A | UI-only step |
| Append-only invariant | N/A | No writes |
| Audit log entry | N/A | No reads of user data |

### TUI impact

This is the first Bubble Tea screen to ship under `internal/tui/wizard/`. The directory previously held a 31-LoC Huh-helpers file (`wizard.go`); that file is untouched by this PR but will be retired or repurposed as peer children land per-step Bubble Tea models.

The `AdvanceMsg` / `CancelMsg` / `outcome` pattern documented above is the reference for any future multi-step wizard in this codebase. See the worked example in `agent-docs/tui-design.md`.

## Alternatives

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Single composite wizard model from day one | One parent model coordinates all steps; cleaner state machine | Requires landing all four children together; defeats the per-issue split that epic #17 commits to | Rejected: contradicts the epic's slice-by-step plan |
| Alt-screen rendering | "Moment" feel; cleaner snapshot output | Splash wiped on exit; user does not see it in scrollback after `q`/Ctrl-C | Rejected by user preference: inline keeps the splash visible after exit |
| Strip scroll glyph in nocolor theme | Maximally minimal nocolor output | The glyph is information, not decoration; lipgloss NoColor strips colour, not glyphs; consistency across themes wins | Rejected by user preference: glyph stays in all themes |
| Sentinel-error bridge (return `errCancelled`) | Idiomatic Go cancel signalling | Requires exporting an error variable; couples the bridge to error handling | Rejected: outcome enum on the model is cleaner and matches Bubble Tea's "inspect final model" idiom |

## Risks

- **Risk:** The next peer child issue (data-dir step) inherits the same model-outcome pattern; a divergent implementation would create wizard-step heterogeneity. **Mitigation:** this doc documents the contract; peer children are expected to copy it.
- **Risk:** Inline rendering on a small terminal could clip the framing paragraph. **Mitigation:** `style.Subtle().Width(60).Align(lipgloss.Center)` constrains the framing to 60 columns; lipgloss wraps gracefully.
- **Risk:** `tea.WithContext` cancellation racing with key press. **Mitigation:** the bridge returns `(false, nil)` for `outcomeNone` (the default), so a context-cancel that beats any key press is treated as cancel and prints the cancel message.

## Migration

None. Greenfield surface.

## Acceptance criteria

- [ ] Phase 1: JSON Schema / event type defined — N/A (UI-only)
- [ ] Phase 2: Kernel implementation — N/A
- [ ] Phase 3: Index hooks — N/A
- [ ] Phase 4: MCP tool / CLI command — modified `internal/cli/init.go:runInitTUI`
- [ ] Phase 5: TUI surface — `internal/tui/wizard/welcome.go`
- [ ] Phase 6: Tests — teatest snapshots + behaviour tests in `internal/tui/wizard/welcome_test.go`
- [ ] Phase 7: Docs — this design doc + worked example in `agent-docs/tui-design.md` + `agent-docs/mintlify-backlog.md` row update

Estimated effort: about half a day.

## Open questions

- [ ] Should `Esc` cancel rather than advance? Issue body explicitly lists only Ctrl-C and `q` as cancel keys, so the current implementation advances on `Esc`. Revisit when the next step lands and users have hands-on feedback. (owner: @tui-developer)
- [ ] Snapshot terminal size. Current tests use 80×24 (matches teatest default). Confirm this is the right baseline once we have screens that need more room. (owner: @tui-developer)

## Approval

- [ ] Reviewed by:
- [ ] Architecture decisions agreed upon
