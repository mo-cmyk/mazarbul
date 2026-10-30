---
name: tui-design-lab
description: Generate multiple Bubble Tea screen variants for side-by-side comparison. USE WHEN exploring multiple TUI design directions for a Mazarbul command, redesigning an existing screen, or when user says 'show me some options'. Conducts a brief design interview, produces N variants under .claude-design/tui/, collects feedback, finalizes one. NOT FOR single-direction iterative refinement.
---

# TUI Design Lab

Mazarbul's TUI variant explorer. Run this skill when a TUI screen needs more than one direction tried before committing.

## CRITICAL: Cleanup Behavior

All temporary files MUST be deleted when the process ends:
- User confirms a final variant -> cleanup, then write the implementation plan.
- User aborts -> cleanup immediately, no plan written.

Never leave `.claude-design/tui/` behind.

## Phase 0: Detect context

- Read `agent-docs/tui-design.md` for the design system.
- Read `internal/tui/style/themes/defender.go` for the default theme.
- Identify the target command (e.g. `maz init`, `maz review`, `maz install`) and the file path under `internal/tui/<screen>/` (or proposed path for a new screen).
- Confirm the user is in TTY mode; the variants are only meaningful when rendered live.

## Phase 1: Brief interview

Use the AskUserQuestion tool. Keep it tight; this is exploration, not a requirements doc.

**Q1: Target.** Which command's TUI are we designing? Existing or new screen?

**Q2: Primary task.** What is the user accomplishing? One sentence.

**Q3: Pain points.** If redesigning, what feels wrong about the current screen? (Multi-select: too dense, too sparse, hard to scan, slow to interact, unclear hierarchy, missing affordance.)

**Q4: Inspiration.** Which TUIs feel right for this surface? (Multi-select: Claude Code, gh dash, lazygit, k9s, gitui, charm/glow, charm/gum.)

**Q5: Density.** Compact, comfortable, or spacious?

**Q6: Animation.** Static, gentle (default), or theatrical?

**Q7: Number of variants.** Default 4, max 6.

## Phase 2: Generate variants

Create `.claude-design/tui/` with one Go file per variant:

```
.claude-design/tui/
  variant-a/
    model.go            # Bubble Tea model
    view.go             # View function
    rationale.md        # Why this variant exists
  variant-b/
    ...
  variant-c/
    ...
  variant-d/
    ...
  brief.md              # The interview output
  lab.go                # Side-by-side renderer
```

Each variant must explore a different axis:

- **Variant A: Layout** , different spatial structure (panel vs split vs stacked).
- **Variant B: Density** , push one direction (more compact or more spacious than the brief asked).
- **Variant C: Interaction model** , different keybindings or different primary action (e.g. select-and-confirm vs auto-act).
- **Variant D: Expressive** , push the brand direction (theme colors, animations, glyphs) further.
- **Variant E (optional): Minimal** , strip everything non-essential.
- **Variant F (optional): Power-user** , keyboard-only, dense, expert affordances.

Each variant uses:
- `internal/tui/style.Theme` for all colors. Never raw hex.
- `bubbles` components first. Custom only if no component fits.
- `huh` for any input collection.
- One `Init -> Update -> View` per variant. No cross-variant state sharing.

`lab.go` renders all variants in a grid (or sequentially with `tab` to switch) so the user can compare without leaving the terminal.

## Phase 3: Present and collect feedback

Output:
```
Design Lab created at .claude-design/tui/
Run: go run ./.claude-design/tui/lab.go
Tab through variants, then return here with:
  - Which variant wins (A-F)
  - What to tweak
```

Wait for the user. Do not start a process that blocks.

## Phase 4: Synthesize (if needed)

If the user wants a hybrid:
- Create a new variant under `.claude-design/tui/variant-final/` combining the parts called out.
- Update `lab.go` to show the synthesized variant alongside the closest original.
- Loop until the user picks one.

## Phase 5: Finalize

When the user confirms:

1. **Write the implementation plan** to `.claude/plans/NNN-tui-<screen>.md`:
   - Target file path (`internal/tui/<screen>/`).
   - Components used (from bubbles, huh).
   - Theme accessors touched.
   - Snapshot test plan (`teatest`).
   - Animation budget.
   - Open questions.

2. **Delete** `.claude-design/tui/` entirely.

3. **Hand off** to `@tui-developer` to implement.

## Abort

If the user says "cancel" / "abort" / "stop" / "nevermind":
1. Confirm.
2. Delete `.claude-design/tui/` entirely.
3. Acknowledge and stand by.

## Gotchas

- **Variants must be meaningfully distinct.** Color-swap variations don't count.
- **Lab renderer must work without external dependencies** beyond what's already in `go.mod`.
- **Snapshot every variant** before deleting, save them as references in `internal/tui/<screen>/testdata/` so the chosen variant carries forward.
