# TUI design system

Mazarbul's terminal interface is the front door. It must feel calm, modern, and as polished as Claude Code. This document explains the design choices, hub architecture, and conventions every contributor needs to know before touching `internal/tui/`.

## Reference inspiration

Claude Code is the reference for terminal polish. Match its feel: generous whitespace, restrained colors, gentle animations, never noisy. A user should be able to keep `maz` running in a tmux pane without the visual rhythm becoming distracting.

Adjacent inspirations: `gh` for plain CLI output discipline; `lazygit` for keybinding density without clutter; `gum` for the small surprise of a polished primitive.

---

## Three entry modes

Mazarbul commands run in one of three modes:

| Mode | When it activates | Effect |
|---|---|---|
| **Hub** | bare `maz` + stdout is a TTY + `DetectHubMode` returns true | Full interactive Bubble Tea hub (`internal/tui/hub/hub.go`) |
| **One-shot TUI** | `maz <cmd>` + stdout is a TTY | Dedicated Bubble Tea program for that command (spinner, progress, forms) |
| **Headless / plain** | `--headless` flag, `MAZ_HEADLESS=1`, `cfg.TUI.Interactive=false`, stdout not a TTY, `--no-tui`, or `NO_COLOR` | Plain text / JSONL output; no escape sequences; agent-callable |

Mode detection lives in `internal/tui/mode.go`. The resolution order for the per-command mode:

1. `--json` → `ModeJSON` (machine-readable JSONL).
2. `--no-tui` → `ModeCLI` (plain text).
3. `NO_COLOR` env set → `ModeCLI`.
4. `term.IsTerminal(os.Stdout.Fd())` false → `ModeCLI`.
5. Otherwise → `ModeTUI`.

Hub-mode detection (`DetectHubMode`) adds two more suppressors on top: `--headless` / `--no-hub` flags, and `MAZ_HEADLESS=1` / `MAZ_HEADLESS=true` env. The boolean `interactive` parameter comes from `cfg.TUI.Interactive` in `~/.config/mazarbul/config.yaml` — when that field is `false`, the hub never opens even on a TTY.

**Never make TUI exclusive.** Every interaction reachable in hub or one-shot TUI mode must be reachable headlessly via flags or piped input.

---

## Color palette

Mazarbul ships **one two-tier palette** — a bright orange brand accent with git-style semantic type hues, over structural chrome addressed by ANSI index. There is no theme registry, no `--theme` flag, no `MAZ_THEME` env var, and no user-authored theme files. If alternate palettes are wanted later, they slot into `internal/tui/style/colors.go` directly. The package doc of `style.go` makes this explicit.

### Tier 1 — truecolour brand and type hues

Declared with `adapt(light, dark string)` in `colors.go`; lipgloss down-samples to the terminal's colour profile (24-bit → 256 → ANSI as needed):

| Function | Light | Dark | Semantic role |
|---|---|---|---|
| `Accent()` | `#f97316` | `#fb923c` | Brand mark, active tab, selection caret, identity bullet, spinner glyphs |
| `AccentDeep()` | `#ea580c` | `#fdba74` | Hover / focused accent variant |
| `Ink()` | `#1A1816` | `#F5F1EB` | Body text |
| `Paper()` | `#FAF7F2` | `#1A1816` | Background |
| `SubtleColor()` | `#6B6660` | `#A8A39C` | Secondary text, labels, hints |
| `FaintColor()` | `#B8B2A8` | `#4A4641` | Borders, separators, disabled text |
| `SuccessColor()` | `#5A7A50` | `#8FAB7E` | OK badge on cards and detail views |
| `WarningColor()` | `#A8742A` | `#D4A574` | Soft warnings |
| `ErrorColor()` | `#A04848` | `#C87878` | Error text |
| `InfoColor()` | `#3A5D7A` | `#8AA8C0` | Informational, used sparingly |
| `PurpleColor()` | `#a855f7` | `#c084fc` | Session type, supersede edges, detail-card chrome |
| `BlueColor()` | `#3b82f6` | `#60a5fa` | Commit/relation type, markdown links |
| `CyanColor()` | `#06b6d4` | `#22d3ee` | Metadata and structure (colorizer `Meta`) |
| `IndigoColor()` | `#6366f1` | `#818cf8` | Keywords and enumerations |
| `AmberColor()` | `#f59e0b` | `#fbbf24` | Identifiers, ULIDs (colorizer `Ident` and `Changed`) |
| `DiffAddedColor()` | `#16a34a` | `#22C55E` | Inserted content in diffs and chroma blocks |
| `DiffRemovedColor()` | `#dc2626` | `#EF4444` | Deleted content in diffs and chroma blocks |

`AgentColor(name string)` maps a known AI writer identity (`claude`, `gemini`, `amp`, `codex`, `opencode`, `copilot`, `pi`, `cursor`, `droid`, `kiro`) to a stable brand hue; unknown identities resolve to ANSI 245. Collapses under `NO_COLOR`.

### Tier 2 — structural chrome

Declared with `ansi(index string)` in `colors.go`; addressed by ANSI index so the user's terminal theme is inherited rather than pinned to a truecolour value:

| Function | ANSI index | Semantic role |
|---|---|---|
| `ANSISuccess()` | 2 (green) | ✓ success bullet, +N stats |
| `ANSIError()` | 1 (red) | ✗ failure bullet, conflicts |
| `ANSIWarning()` | 3 (yellow) | ⚠ warning bullet, stale state |
| `ANSIDim()` | 8 (bright-black) | Hashes, units, table headers |
| `ANSIMuted()` | 245 | Help-key labels, placeholders |
| `ANSIRule()` | 241 | Faint separators and section rules |
| `ANSIIdentValue()` | 7 (white) | Primary identifier value |

### NO_COLOR and colour access

**`NO_COLOR` wins unconditionally.** Both `adapt()` and `ansi()` check `os.Getenv("NO_COLOR")` on every call and return `lipgloss.NoColor{}` when set. Tests flip it via `t.Setenv("NO_COLOR", "1")`.

Color access is through typed functions in the `style` package — no raw hex literals outside `internal/tui/style/`:

```go
import "github.com/mo-cmyk/mazarbul/internal/tui/style"

lipgloss.NewStyle().Foreground(style.Accent())
lipgloss.NewStyle().Foreground(style.CyanColor())
lipgloss.NewStyle().Foreground(style.ANSISuccess())
```

Higher-level pre-built styles (`style.Brand()`, `style.Heading()`, `style.Subtle()`, …) wrap the colour accessors for common cases.

---

## CLI semantic color and gating

Command output (diffs, status, logs, recall, list) uses a **git-style semantic color** API in `internal/cli/colorizer/`. The caller decides whether color is on; when disabled every method returns its input byte-for-byte unchanged, safe for `--json` and piped pipelines.

### Gating: `internal/cli/interactive`

```go
import "github.com/mo-cmyk/mazarbul/internal/cli/interactive"

enabled := interactive.ShouldStyle(cmd, os.Stdout)
```

`ShouldStyle` resolves in this precedence order (first match wins):

1. `NO_COLOR` set → **false** (wins unconditionally).
2. `--color=never` → false.
3. `--color=always` → true.
4. `CLICOLOR_FORCE` or `FORCE_COLOR` non-empty → true.
5. `TERM=cygwin` → false.
6. `CLICOLOR=0` → false.
7. Otherwise → `IsTTY(w)`.

`CanPromptInteractively()` is the companion before any Huh prompt. It gates on `MAZ_TEST_TTY`, `testing.Testing()`, agent sentinels (`GEMINI_CLI`, `COPILOT_CLI`, `PI_CODING_AGENT`, `CLAUDECODE`), `GIT_TERMINAL_PROMPT=0`, `CI`, and `/dev/tty` openability — so prompts never hang under CI or agent invocations.

### Colorizer: `internal/cli/colorizer`

```go
import "github.com/mo-cmyk/mazarbul/internal/cli/colorizer"

c := colorizer.New(interactive.ShouldStyle(cmd, os.Stdout))
fmt.Println(c.Added("+entity created"))
fmt.Println(c.Removed("-entity forgotten"))
fmt.Println(c.Ident(uid))
```

| Method | Colour source | Semantic meaning |
|---|---|---|
| `Added(s)` | `DiffAddedColor` | Inserted / created content |
| `Removed(s)` | `DiffRemovedColor` | Deleted / forgotten content |
| `Changed(s)` | `AmberColor` | Modified content |
| `Superseded(s)` | `PurpleColor` | Superseded content |
| `Conflict(s)` | `ANSIError` bold | Sync conflict |
| `Ident(s)` | `AmberColor` | Identifier, ULID |
| `Meta(s)` | `CyanColor` | Metadata, structure |
| `Dim(s)` | `ANSIDim` | Secondary text |
| `Accent(s)` | `Accent` | Active / current element (never "success") |
| `Success(s)` | `ANSISuccess` | Success state |
| `Warn(s)` | `ANSIWarning` | Warning state |
| `Err(s)` | `ANSIError` | Error state |

### Status lines: `internal/cli/status`

`status.Printer` renders symbol-first status lines. The glyph carries the semantic meaning; colour is layered on top as an accelerator.

```go
p := status.New(os.Stdout, interactive.ShouldStyle(cmd, os.Stdout))
fmt.Println(p.IdentityBullet("daemon", "01ABCDEF"))   // ● daemon 01ABCDEF
fmt.Println(p.SuccessBullet("migration applied"))      // ✓ migration applied
fmt.Println(p.FailureBullet("sync failed"))            // ✗ sync failed
fmt.Println(p.WarnBullet("stale data detected"))       // ⚠ stale data detected
fmt.Println(p.SectionRule("Connected sources"))        // ── Connected sources ──────
fmt.Println(p.HorizontalRule())                        // ────────────────────────────
p.MetadataRows([][2]string{{"entity", uid}, {"kind", "person"}})
```

Width is auto-detected from the terminal (capped at 80; falls back to 60 for non-TTY writers).

### Themed forms: `internal/cli/uiform`

All interactive CLI prompts go through `uiform`, which maps Mazarbul's orange palette onto Huh's theme and short-circuits non-interactively:

```go
import "github.com/mo-cmyk/mazarbul/internal/cli/uiform"

ok, err := uiform.PromptYN(os.Stdout, "Continue?", false)
choice, err := uiform.PromptSelect(os.Stdout, "Pick a scope", opts)
```

`ACCESSIBLE=1` or `MAZ_ACCESSIBLE=1` activates plain-text mode for screen-reader users.

### Symbol-first discipline

**Colour is always layered on top of a glyph or word.** Turning off `NO_COLOR` must never lose information. JSON and ndjson output is never coloured regardless of the `--color` flag.

---

## Typography and animation

Lean on lipgloss `Bold`, `Italic`, `Faint`, `Underline`. Avoid emoji anywhere in routine output. Spinners use their built-in glyphs; these are not emoji.

Use the `internal/tui/spinner` package for all spinning indicators. It wraps `charm.land/bubbles/v2/spinner` with Mazarbul defaults (brand-accent styled, four preset kinds).

Mazarbul's motion is calm, not jittery:

- **Thinking states / long waits**: `spinner.Moon` (moon phase glyphs; fitting for Khazad-dûm).
- **Short waits** (under two seconds): `spinner.Dot`.
- **Sync or remote work**: `spinner.Globe`.
- **Ambient background activity**: `spinner.Pulse`.
- **Non-Bubble-Tea CLI commands** (a single blocking call): `spinner.NewInline(w, label, enabled)` — an eight-phase braille spinner that writes directly to an `io.Writer`, with a 250 ms delay before the first frame so fast operations finish silently. Call `.Start()` before the work and `.Done(msg)` after.
- **Embedded loading card** inside a larger Bubble Tea model: `spinner.NewCard(label, subtitle, width)` — a MiniDot spinner with a two-line borderless card layout.
- **Measurable work**: `charm.land/bubbles/v2/progress` with a percentage label.
- **Reveal / collapse**: lipgloss height-shrinking borders driven by `tea.Tick`.

Every blocking operation longer than **200 ms** gets a spinner. Spinner labels say what's happening, never just "Loading...". Examples: "Fetching the manifest...", "Verifying the signature...", "Inscribing the first page..."

---

## Hub architecture

The hub is a single long-lived Bubble Tea program launched by bare `maz`. Its root model lives in `internal/tui/hub/hub.go`; the layout is:

```
┌─────────────────────────────────────────────────────────────┐
│  top bar: brand · active panel title · status · version     │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│                    active panel body                        │
│              (View(width, bodyHeight))                      │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│  slash bar: ❯ / for commands, free text for /ask            │
│  tab strip: home  browse  doctor  usage  jobs  mcp  settings│
└─────────────────────────────────────────────────────────────┘
```

`bodyHeight = terminalHeight - 4` (top bar + separator + slash row + tab strip).

### Key packages

| Package | Role |
|---|---|
| `internal/tui/hub/hub.go` | Root `model`, `Run`, key dispatch, panel lifecycle |
| `internal/tui/hub/panelapi/` | `Panel` interface contract (leaf package, no import cycles) |
| `internal/tui/hub/panels/<name>/` | One panel per directory |
| `internal/tui/hub/command/` | Slash-command registry, `Entry`, `Outbox`, `History` |
| `internal/tui/hub/slashbar/` | Slash prompt + floating autocomplete overlay |
| `internal/tui/hub/layout.go` | `topBar`, `tabStrip` render helpers |
| `internal/tui/hub/keymap.go` | Hub-level `KeyMap` and `DefaultKeyMap` |
| `internal/tui/hub/messages.go` | `SwitchPanelMsg`, `StatusMsg`, `QuitMsg` |
| `internal/tui/hub/adapters.go` | Adapts kernel types to panel-facing shapes |

### The Panel interface

```go
type Panel interface {
    Init() tea.Cmd
    Update(msg tea.Msg) (Panel, tea.Cmd)
    View(width, height int) string
    Title() string
    Slash() string
    Help() []key.Binding
}
```

Every panel returns itself from `Update` (value semantics via the interface). The hub holds a `[]panelapi.Panel` slice and replaces each element on every update tick.

### Hub-level key bindings

| Key | Action |
|---|---|
| `Tab` | Next panel |
| `Shift+Tab` | Previous panel |
| `/<name>` + Enter | Switch to named panel directly |
| `?` | Toggle help overlay (aggregates global + active-panel bindings) |
| `Esc` | Clear slash bar / close help overlay |
| `Ctrl+G` | Cancel current running slash command |
| `Ctrl+L` | Clear Home scrollback |
| `Ctrl+C` | Quit |

### Slash commands

Commands are registered in `internal/tui/hub/command/builtins.go` via `command.NewRegistry()`. The slash bar autocompletes `/` prefixes against the registry. On Enter, the hub calls `dispatchSlash`, which parses the line with `command.Parse(line)` and calls `entry.Run(ctx, args, outbox)`.

`Outbox` is the interface commands use to post back into the hub: `AppendUser`, `AppendSystem`, `AppendError`, `Switch`, `SetStatus`, `Quit`.

### Async-cancel pattern

Long-running commands (e.g. `/ask`, `/recall`) should hold a `context.WithCancel`-derived context. The hub stores the current `cancelFunc` on the model. `Ctrl+G` calls it, producing immediate cancellation without killing the whole hub. The command's `tea.Cmd` returns an error message via `AppendError` when its context is cancelled, so the user sees feedback.

```go
// Sketch — actual implementation varies per command
func (e *Entry) Run(ctx context.Context, args []string, out command.Outbox) tea.Cmd {
    ctx, cancel := context.WithCancel(ctx)
    hub.SetCancel(cancel) // hub stores and calls this on Ctrl+G
    return func() tea.Msg {
        result, err := doWork(ctx, args)
        if err != nil {
            return errMsg{err}
        }
        return resultMsg{result}
    }
}
```

---

## The 12 panels

Panels are registered in `hub.Run` in the order they appear in the tab strip.

| Slash | Title | Package | Responsibility |
|---|---|---|---|
| `/home` | home | `panels/home/` | Chat-style scrollback of user commands and system responses; the hub's default view |
| `/browse` | browse | `panels/browse/` | Three-way browser over the event log: Episodes / Entities / Relations inner tabs |
| `/doctor` | doctor | `panels/doctor/` | Live health grid: one row per subsystem check, spinner while running, tick/cross on done |
| `/usage` | usage | `panels/usage/` | Today cards, 30-day sparklines, per-storage-layer capacity bars, top-K entities by recall |
| `/cost` | cost | `panels/usage/cost/` | Cost drill-down: MTD totals, budget bar, per-model token splits, cache analytics, projection |
| `/jobs` | jobs | `panels/jobs/` | In-flight background job progress bars + recent history tail |
| `/mcp` | mcp | `panels/mcp/` | MCP transport listeners (stdio / unix / http) + connected clients + tool catalog |
| `/settings` | settings | `panels/settings/` | Five inner tabs: General · Providers · Integrations · Connectors · Advanced |
| — | (Settings: General) | settings inner tab 1 | Data dir, launch mode, appearance, privacy |
| — | (Settings: Providers) | settings inner tab 2 | Chat provider + model + masked API key; embedding provider + model + dimensions |
| — | (Settings: Integrations) | settings inner tab 3 | Embedded connect picker — auto-register Mazarbul MCP with AI clients |
| — | (Settings: Connectors) | settings inner tab 4 | Source connector catalog (Gmail, Slack, Notion, …) |
| — | (Settings: Advanced) | settings inner tab 5 | Budget cap, alert threshold, log level and path |
| `/help` | (overlay) | `panels/help/` | `?` modal: aggregates global + active-panel key bindings; not a real panel, rendered by hub |

The Settings panel's five inner tabs cycle with `1`–`5` or `h`/`l` / `←`/`→`. The Integrations tab (tab 3) embeds the connect picker from `internal/tui/connect/picker.go` via `NewEmbedded`.

The canonical visual contract for all panels is the **M1–M12 mockup gallery** in `.claude/plans/let-s-redesign-the-tui-staged-breeze.md`. When implementing a panel, match that mockup; if the mockup and this document conflict, the mockup wins.

---

## Worked example: Home panel

The Home panel (`internal/tui/hub/panels/home/`) is the hub's default screen. It holds a scrollback of `Turn` values rendered into a `bubbles/viewport`.

### Turn shape

```go
type Turn struct {
    Kind      Kind       // KindUser | KindSystem | KindError
    Command   string     // slash name, e.g. "/recall"
    Body      string     // free-form markdown or plain text
    CreatedAt time.Time
}
```

### Rendering

`renderTurn` (in `render.go`) produces one block per turn:

- **Header line**: `HH:MM:SS  /command-name` in `FaintColor`.
- **KindUser**: body prefixed with `❯` in `SubtleColor`. No separator rule — user input is visually lighter.
- **KindSystem**: separator rule in `FaintColor`, then body rendered through glamour (`glamour.WithStandardStyle("dark")`), final style in `Ink`. The glamour renderer is process-local, constructed once via `sync.Once`.
- **KindError**: separator rule, then body prefixed with `✗` in `ErrorColor`.

### Sticky auto-scroll

The `Model` holds `sticky: true`. Every `AppendUser`, `AppendSystem`, or `AppendError` call invokes `reflow()`, which rebuilds the viewport content and calls `vp.GotoBottom()` when `sticky` is true. The user can scroll up with `k`/`↑` (sticky becomes false implicitly when the viewport is not at the bottom); pressing `G` snaps back to the bottom and re-enables sticky.

### Clear

`Ctrl+L` dispatches `command.ClearScrollbackMsg` which the Home model catches to call `m.Clear()`, resetting `turns` to nil and reflowing the empty viewport. The greeting string reappears.

---

## Forms

**Always Huh** for any input collection in one-shot TUI mode. Never raw `bufio.Scanner`. Huh handles validation, navigation between fields, keyboard accessibility, and screen reader output.

`internal/tui/wizard/` wraps Huh with project defaults (theme, navigation hints, error display).

---

## Accessibility

- Honor `NO_COLOR` (lipgloss auto-detects; `style.adapt` enforces it on every call).
- Every TUI command has a `--no-tui` flag that maps to CLI mode. Required for screen readers and CI logs.
- Never communicate state via color alone; always pair color with text or a shape glyph (`OK`, `FAIL`, tick, cross).
- Exit codes: `0` success, `1` expected failure (invalid input, missing config), `2` unexpected failure (bug), `130` user-cancelled (Ctrl+C).
- Confirm dialogs require explicit `y`/`n`; default action on Enter is the safe option.
- Keybindings are discoverable: `?` opens the keymap overlay over any panel.

---

## Testing

Every Bubble Tea model gets a `teatest` snapshot test (`github.com/charmbracelet/x/exp/teatest`). Snapshots commit to `internal/tui/<screen>/testdata/` next to the model, with one file per rendering context using a `.txt` extension.

Snapshot naming convention: `<screen>_dark.txt` and `<screen>_nocolor.txt`. The `nocolor` snapshot is the human-reviewable one (no escape sequences). Add a `_light.txt` variant only when the screen's appearance changes meaningfully in light mode.

Current examples from the wizard:

```
internal/tui/wizard/testdata/welcome_dark.txt
internal/tui/wizard/testdata/welcome_nocolor.txt
internal/tui/wizard/testdata/datadir_dark.txt
internal/tui/wizard/testdata/datadir_nocolor.txt
```

Update snapshots via `mise run tui:snap`. Run TUI tests as part of `mise run test`; race detector always on.

Cancel paths (`Esc`, `Ctrl+C`) are covered by behavior tests, not snapshots, because they trigger `tea.Quit` immediately.

---

## Demo recording

`mise run demo` invokes VHS (`charmbracelet/vhs`) to render every `.tape` file in `docs/demos/` into a GIF. Tape files live alongside the docs they illustrate. Commit the GIFs (`*.gif` marked as binary in `.gitattributes`) so they render in Mintlify and the README.

---

## Naming conventions

- Hub source: `internal/tui/hub/` — root model, keymap, layout, messages, adapters.
- Panels: `internal/tui/hub/panels/<name>/` — one package per panel.
- Panel API: `internal/tui/hub/panelapi/` — leaf package; import this, not `hub`, from panels.
- Slash registry: `internal/tui/hub/command/` — `Entry`, `Outbox`, `Registry`, `History`.
- Slash bar widget: `internal/tui/hub/slashbar/`.
- One-shot TUI screens: `internal/tui/<screen>/` — one Bubble Tea model per screen.
- Shared styles: `internal/tui/style/` — two-tier palette (`colors.go`) and pre-built style helpers (`style.go`).
- Glamour markdown renderer: `internal/tui/style/mdrender/` — `Render(md, width, dark)` and `RenderForWriter(w, md)`; palette-themed, NO_COLOR-aware, chroma "mazarbul" style registered once via `sync.Once`.
- Shared widgets: `internal/tui/widget/`.
- Generic picker: `internal/tui/picker/` — `Model[T]` with search, multi-select, accessible view, and a shared `KeyMap` + `Help()` footer.
- Spinner: `internal/tui/spinner/` — Bubble Tea `Model` (Moon/Dot/Globe/Pulse), `Inline` braille spinner for non-Bubble-Tea contexts, `Card` MiniDot embed.
- CLI color gating: `internal/cli/interactive/` — `ShouldStyle(cmd, w)` and `CanPromptInteractively()`.
- CLI semantic colorizer: `internal/cli/colorizer/` — git-style `Added`/`Removed`/`Changed`/`Ident`/`Meta`/… API.
- CLI status lines: `internal/cli/status/` — symbol-first `Printer` (identity bullet, success/fail/warn bullets, metadata rows, section rule, horizontal rule).
- CLI prompt surface: `internal/cli/uiform/` — themed Huh forms, `PromptYN`, `PromptSelect`, `PromptMultiSelect`, `HandleFormCancellation`.

---

## Reference reading

The charm stack was migrated from `github.com/charmbracelet/*` to `charm.land/*/v2` in this codebase. Use the v2 import paths in all new code:

- `charm.land/bubbles/v2` — spinner, progress, list, viewport, textinput, table, paginator.
- `charm.land/huh/v2` — forms (all CLI prompts must go through `internal/cli/uiform`).
- `charm.land/lipgloss/v2` — layout primitives.
- `charm.land/glamour/v2` — markdown rendering (use `internal/tui/style/mdrender`, not glamour directly).
- `charm.land/bubbletea/v2` — the Elm-architecture runtime.

The `github.com/charmbracelet/x/…` utility packages (`ansi`, `term`, `exp/golden`) remain at the `charmbracelet` host — check `go.mod` for the current versions.

Pick an existing component before writing a custom one. The `tui-component-browser` skill enforces this discipline.
