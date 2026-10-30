#!/bin/bash
# UserPromptSubmit hook (Layer 4): proactive skill routing.
#
# Inspect the user's prompt for intent keywords and inject a skill suggestion
# via `additionalContext`. The first turn of every session gets the right
# starting skill without the user typing the slash command.
#
# Output format: JSON to stdout with hookSpecificOutput.additionalContext.
# Exit 0 always.

INPUT=$(cat)
PROMPT=$(echo "$INPUT" | python3 -c \
  "import sys, json; d=json.load(sys.stdin); print(d.get('prompt', ''))" \
  2>/dev/null || true)

[[ -z "$PROMPT" ]] && exit 0

LOWER=$(echo "$PROMPT" | tr '[:upper:]' '[:lower:]')

CONTEXT=""

emit_context() {
  local msg="$1"
  local escaped
  escaped=$(printf '%s' "$msg" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')
  echo "{\"hookSpecificOutput\":{\"hookEventName\":\"UserPromptSubmit\",\"additionalContext\":${escaped}}}"
  exit 0
}

# NOTE: skill dispatch for /team, /architect, /complex-refactor, /pr-review (lens
# mode), and qa-engineer is handled by Claude's native description-driven matching,
# every skill's `USE WHEN` clause in its description frontmatter is what wins.
# Do NOT add regex blocks here for routing.

# --- Intent: new MCP tool / new event type / new connector ---
if echo "$LOWER" | grep -qE '(new|add|create|implement).*(mcp|tool|event|connector|resource|kind)'; then
  emit_context "Routing hint: This looks like a new resource (MCP tool, event type, connector, entity kind). Suggested workflow:
1. /new-resource - end-to-end checklist (schema -> kernel writers -> MCP tool -> tests)
2. /go-strict at the end - auto-iterates verify/critique/dead/simplify until clean

Critical: every new event type lands with provenance (source, confidence, timestamp). Every MCP tool ships with a JSON Schema and at least one error-code test."
fi

# --- Intent: feature implementation ---
if echo "$LOWER" | grep -qE '(implement|build|add).*(feature|page|component|workflow|flow|screen)'; then
  emit_context "Routing hint: This looks like a feature implementation. Suggested workflow:
1. /plan-feature - draft the design and acceptance criteria (local scratch in .claude/plans/, gitignored)
2. /task-execution-protocol - execute with TaskCreate-tracked phases
3. /go-strict - lock down quality (auto-iterates verify/critique/dead/simplify)

Per AGENTS.md: hooks will steer you, you don't need to manually invoke quality checks."
fi

# --- Intent: bug fix / debug ---
if echo "$LOWER" | grep -qE '(fix|debug|broken|failing|crash|error|bug)'; then
  emit_context "Routing hint: Bug-fix mode. Karpathy goal-driven approach:
1. First, write a failing test that reproduces the bug
2. Fix the code until the test passes
3. /verify to confirm no regressions
4. /go to ship

Don't 'improve' adjacent code, surgical changes only (Karpathy principle 3)."
fi

# --- Intent: review / audit ---
if echo "$LOWER" | grep -qE '(review|audit|check|critique).*(pr|code|diff|branch)'; then
  emit_context "Routing hint: Review task. Pick the right skill:
- /pr-review - for reviewing SOMEONE ELSE'S PR
- /critique - self-review of YOUR OWN diff (clean-eyes Explore subagent)
- /preflight - heavy pre-PR check (full lint, tests, deadcode)"
fi

# --- Intent: docs ---
if echo "$LOWER" | grep -qE '(document|docs|mintlify|user-facing)'; then
  emit_context "Routing hint: Documentation task. /write-docs auto-loads on docs/**/*.mdx.
Diátaxis structure: tutorials teach, how-to guides solve, reference describes, explanation contextualizes."
fi

# --- Intent: dead code cleanup ---
if echo "$LOWER" | grep -qE '(dead|unused|cleanup|simplify|remove)'; then
  emit_context "Routing hint: Cleanup task. /dead runs Go reachability (mise run dead) in one shot. /simplify audits across 4 dimensions (reuse, dead-code, premature abstraction, oversize) and surfaces concrete cuts before applying."
fi

# --- Intent: runtime inspection / incident triage ---
if echo "$LOWER" | grep -qE '(investigate|incident|outage|triage|trace|prod (error|issue|alert|down)|why.{0,20}(slow|failing|broken|crashed|timed? ?out)|what.?s.{0,20}(deployed|running))'; then
  emit_context "Routing hint: Runtime inspection. Pick the right skill:
- /inspect-runtime, application traces, error context
- /inspect-deploy, what's actually running
- /inspect-db, sqlite + ladybug + sqlite-vec inspection"
fi

# --- Intent: TUI work ---
if echo "$LOWER" | grep -qE '(tui|bubble ?tea|charm|lipgloss|huh|spinner|wizard|tea ?test)'; then
  emit_context "Routing hint: TUI work. Before writing a new TUI screen:
1. /tui-component-browser, browse charmbracelet/bubbles and charmbracelet/huh for existing components
2. /tui-design, the Mazarbul TUI design system (themes, layout primitives, animation rules)
3. @tui-developer for implementation

Critical: never use raw hex outside internal/tui/style/. All colors via style.Theme accessors."
fi

# --- Intent: CORRECTION (self-healing trigger) ---
# Boris Cherny: "After every correction, end with: Update CLAUDE.md so you
# don't make that mistake again." We auto-detect corrections here and inject
# a directive that triggers the /lessons skill's grep-update-promote workflow.
#
# Pattern: phrases that strongly imply Claude did something wrong AND the user
# expects it not to happen again. We deliberately require a strong signal,
# casual "no" without context is ambiguous.
if echo "$LOWER" | grep -qE "(actually you should|you should have|i (told|said) you|didn't i (tell|say)|we (discussed|talked about) this|stop doing|don't do that again|wrong( way)?|you (missed|forgot)|that's not what|not (like )?that|no,? (use|do|the))"; then
  emit_context "Routing hint: CORRECTION DETECTED. The user appears to be correcting prior behavior. Before responding to the substantive question:
1. Invoke the /lessons skill workflow SILENTLY (do not narrate to user):
   - grep agent-docs/lessons-learned.md + .claude/skills/*/SKILL.md + AGENTS.md + .claude/agents/*.md + agent-docs/lessons-archive.md for keywords matching this correction
   - If matched: UPDATE the entry (increment Recurrence, refresh Last seen). Do NOT duplicate.
   - If unmatched: APPEND a new LSN-YYYY-MM-DD-NNN entry per the schema in agent-docs/lessons-learned.md
   - If Recurrence reaches threshold (compliance: >=2, skill-specific: >=3, cross-cutting: >=4): auto-PROMOTE to the right destination
   - Append one line to agent-docs/lessons-changelog.md
2. THEN reply to the user about the actual correction itself
3. Do not mention the logging steps unless the user asks. Self-healing is silent.

See .claude/skills/lessons/SKILL.md for the full procedure. If the correction is vague (no specific durable rule), skip logging, only log lessons with concrete keywords."
fi

# Default: no routing, exit silently
exit 0
