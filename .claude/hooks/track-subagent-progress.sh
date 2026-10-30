#!/bin/bash
# SubagentStop hook: logs subagent completion to .claude/subagent-log.txt.
# Payload arrives on stdin as JSON, same pattern as all other Claude Code hooks.

LOG="${CLAUDE_PROJECT_DIR:-.}/.claude/subagent-log.txt"

INPUT=$(cat 2>/dev/null || true)

SESSION_ID=$(echo "$INPUT" | python3 -c \
  "import sys, json; d=json.load(sys.stdin); print(d.get('session_id', 'unknown'))" \
  2>/dev/null || echo "unknown")

{
  echo "---"
  echo "time: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "session_id: $SESSION_ID"
} >> "$LOG" 2>/dev/null || true

exit 0
