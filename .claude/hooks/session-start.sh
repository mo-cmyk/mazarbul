#!/bin/bash
# Session start hook - Load project context

echo "=== Mazarbul Development Session ==="
echo ""
echo "Front door: AGENTS.md (CLAUDE.md is @AGENTS.md import)"
echo "Local-first agent system of record. No proto. JSONL + JSON Schema."
echo ""
echo "Skills (.claude/skills/) auto-load on file globs or via /<name>:"
echo "  go, go-backend, go-strict, aip-naming, new-resource,"
echo "  task-execution-protocol, pr-review, plan-feature, ship, review,"
echo "  write-docs, tui-design, tui-component-browser, tui-design-lab"
echo ""
echo "Sub-agents (.claude/agents/) delegate via @<name>:"
echo "  @golang-developer, @backend-engineer, @system-architect,"
echo "  @security-engineer, @qa-engineer, @ontology-architect,"
echo "  @ingestion-engineer, @mcp-developer, @cli-developer,"
echo "  @tui-developer, @plugin-developer, @docs-writer"
echo ""
# Self-healing memory: count only real LSN entries (date starts with 2, not the YYYY example)
REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(pwd)}"
LESSONS_FILE="${REPO_ROOT}/agent-docs/lessons-learned.md"
CHANGELOG_FILE="${REPO_ROOT}/agent-docs/lessons-changelog.md"
if [[ -f "$LESSONS_FILE" ]]; then
  ACTIVE_COUNT=$(grep -cE '^### LSN-2[0-9]{3}-[0-9]{2}-[0-9]{2}-[0-9]{3}' "$LESSONS_FILE" 2>/dev/null | tr -d '\n ')
  ACTIVE_COUNT=${ACTIVE_COUNT:-0}
  CHANGELOG_LINES=0
  if [[ -f "$CHANGELOG_FILE" ]]; then
    CHANGELOG_LINES=$(grep -cE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' "$CHANGELOG_FILE" 2>/dev/null | tr -d '\n ')
    CHANGELOG_LINES=${CHANGELOG_LINES:-0}
  fi
  printf 'Self-healing memory: %s active lessons, %s logged actions\n' "$ACTIVE_COUNT" "$CHANGELOG_LINES"
fi
echo "==================================="
