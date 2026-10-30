#!/bin/bash
# TaskCompleted hook (Layer 3): nudge to verify before claiming done.
#
# When a task is marked completed via TaskUpdate, check if the work involved
# code changes. If yes, block with a directive to run /verify first.
#
# Input JSON contains the completed task and tool context.
# Output JSON: {"decision":"block","reason":"..."} to keep Claude going.

INPUT=$(cat)
REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(pwd)}"
cd "$REPO_ROOT" || exit 0

# Extract task description from input (best-effort).
TASK_DESC=$(echo "$INPUT" | python3 -c \
  "import sys, json
try:
    d = json.load(sys.stdin)
    t = d.get('tool_input', {})
    desc = t.get('description', '') or t.get('content', '') or ''
    print(desc[:200])
except Exception:
    print('')" 2>/dev/null || true)

# If no uncommitted diff, nothing to verify.
if git diff --quiet HEAD 2>/dev/null && git diff --quiet --cached 2>/dev/null; then
  exit 0
fi

# Check whether the diff includes code (vs. only docs / config).
CHANGED=$(git diff --name-only HEAD 2>/dev/null)
CODE_CHANGED=0
if echo "$CHANGED" | grep -qE '\.(go|sql|tf|toml|yaml|yml)$'; then
  CODE_CHANGED=1
fi

# Diff size heuristic
DIFF_LOC=$(git diff HEAD --shortstat 2>/dev/null | \
  grep -oE '[0-9]+ insertion' | grep -oE '[0-9]+' | head -1)
DIFF_LOC=${DIFF_LOC:-0}

# Skip nudge for very small diffs or doc-only changes.
if [[ "$CODE_CHANGED" -eq 0 ]] || [[ "$DIFF_LOC" -lt 10 ]]; then
  exit 0
fi

# Check if last-check.md exists and is recent (<5 min old).
LAST_CHECK=".claude/last-check.md"
FRESH=0
if [[ -f "$LAST_CHECK" ]]; then
  MTIME=$(stat -f %m "$LAST_CHECK" 2>/dev/null || stat -c %Y "$LAST_CHECK" 2>/dev/null || echo 0)
  NOW=$(date +%s)
  AGE=$((NOW - MTIME))
  if [[ "$AGE" -lt 300 ]]; then
    FRESH=1
  fi
fi

if [[ "$FRESH" -eq 1 ]]; then
  # Recent verification already ran, only block if it found failures.
  if grep -q '^## Failures' "$LAST_CHECK" && ! grep -A1 '^## Failures' "$LAST_CHECK" | grep -q '_(none)_'; then
    REASON="Task completed but .claude/last-check.md shows verification failures. Address them before continuing, see file for full output."
  else
    exit 0
  fi
else
  # No fresh verification. Suggest /verify or /go-strict based on diff size.
  if [[ "$DIFF_LOC" -gt 50 ]]; then
    REASON="Task completed with ${DIFF_LOC} LoC changed. Before claiming this work is done, invoke /go-strict to auto-iterate verify -> critique -> dead -> simplify. Substantive changes need the cleanup loop, not just a quick verify."
  else
    REASON="Task completed with ${DIFF_LOC} LoC changed across $(echo "$CHANGED" | wc -l | tr -d ' ') files. Run /verify before claiming done, Stop hook will block otherwise."
  fi
fi

ESCAPED=$(printf '%s' "$REASON" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')
echo "{\"decision\":\"block\",\"reason\":${ESCAPED}}"
exit 0
