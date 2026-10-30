#!/bin/bash
# TeammateIdle hook, informational only.
#
# When a teammate goes idle, surface uncommitted-LoC count to the Lead so it
# can decide next steps (assign a new task, request a check, shut down).
# Never blocks (exit 0 always). Never auto-merges. Never auto-tears-down the
# team, cleanup is user-driven via `mise run claude:clean` or "clean up the team".

REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(pwd)}"
cd "$REPO_ROOT" || exit 0

LOC=$(git diff HEAD --numstat 2>/dev/null | awk '{ added += $1; deleted += $2 } END { print added + deleted }')
LOC=${LOC:-0}

if [[ "$LOC" -gt 0 ]]; then
  echo "Teammate idle with ${LOC} uncommitted LoC (added + deleted). Lead: review the diff before dispatching the next task or request /verify."
fi

exit 0
