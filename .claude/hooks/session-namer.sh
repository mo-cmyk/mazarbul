#!/usr/bin/env bash
# UserPromptSubmit hook: one-shot session naming via Haiku.
#
# On the user's FIRST prompt in a session, call Haiku to produce a 3-5 word
# slug, write it to .claude/session-name, and re-emit the OSC tab title so
# the terminal label reflects the task.
#
# Silent on every failure path. Bails before any network I/O if state
# markers say the work is already done.

set -euo pipefail

# Read JSON payload from stdin. Bail silently on parse failure.
INPUT=$(cat 2>/dev/null || true)
[[ -n "${INPUT}" ]] || exit 0

# Parse cwd + prompt from JSON. python3 is on every macOS.
cwd=$(printf '%s' "${INPUT}" | python3 -c \
  "import sys, json; d=json.load(sys.stdin); print(d.get('cwd', ''))" \
  2>/dev/null || true)
prompt=$(printf '%s' "${INPUT}" | python3 -c \
  "import sys, json; d=json.load(sys.stdin); print(d.get('prompt', ''))" \
  2>/dev/null || true)

[[ -n "${cwd}" && -d "${cwd}" ]] || exit 0
[[ -n "${prompt}" ]] || exit 0

# Anchor at the worktree/repo root so the marker doesn't follow the user
# into subdirectories. If cwd is a subdir of a worktree, this hook would
# otherwise create a stray .claude/ under that subdir and run again from
# each new subdir in the same session. `git -C` keeps the lookup robust
# when cwd itself is a symlink or relative path.
root=$(git -C "${cwd}" rev-parse --show-toplevel 2>/dev/null || true)
[[ -n "${root}" && -d "${root}" ]] || root="${cwd}"

marker="${root}/.claude/session-named"
namefile="${root}/.claude/session-name"

# Gate 1: one-shot marker.
[[ -f "${marker}" ]] && exit 0

# Gate 2: API key available.
[[ -n "${ANTHROPIC_API_KEY:-}" ]] || exit 0

# Cap the prompt at 2000 chars to keep latency + cost down.
prompt_capped=$(printf '%s' "${prompt}" | head -c 2000)

# Build JSON body via python3 (handles quoting correctly).
body=$(python3 -c '
import json, sys
prompt = sys.argv[1]
payload = {
    "model": "claude-haiku-4-5-20251001",
    "max_tokens": 32,
    "system": (
        "You are a session-naming assistant. Output ONLY a 3-5 word slug "
        "describing the user'\''s task. Lowercase, hyphenated, no "
        "punctuation, no quotes, no explanation."
    ),
    "messages": [{"role": "user", "content": prompt}],
}
print(json.dumps(payload))
' "${prompt_capped}" 2>/dev/null || true)
[[ -n "${body}" ]] || exit 0

# Call the Messages API. 5s ceiling.
response=$(curl -sS --max-time 5 \
  -H "x-api-key: ${ANTHROPIC_API_KEY}" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d "${body}" \
  https://api.anthropic.com/v1/messages 2>/dev/null || true)
[[ -n "${response}" ]] || exit 0

# Extract the text from the response.
raw=$(printf '%s' "${response}" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    blocks = d.get("content", [])
    for b in blocks:
        if b.get("type") == "text":
            print(b.get("text", "").strip())
            break
except Exception:
    pass
' 2>/dev/null || true)
[[ -n "${raw}" ]] || exit 0

# Sanitize: lowercase, strip non-[a-z0-9-], collapse repeated hyphens,
# trim leading/trailing hyphens.
slug=$(printf '%s' "${raw}" \
  | tr '[:upper:]' '[:lower:]' \
  | tr -c 'a-z0-9-' '-' \
  | tr -s '-' \
  | sed -e 's/^-//' -e 's/-$//')

[[ -n "${slug}" ]] || exit 0

# Persist + mark, under the worktree root, not the user's cwd.
mkdir -p "${root}/.claude"
printf '%s\n' "${slug}" >"${namefile}"
: >"${marker}"

# Re-emit the OSC tab title with the new name. Read WORKTREE_NAME from
# .env.local if available; fall back to basename of root.
WORKTREE_NAME=""
if [[ -f "${root}/.env.local" ]]; then
  WORKTREE_NAME=$(grep -E '^WORKTREE_NAME=' "${root}/.env.local" 2>/dev/null \
    | head -n1 | cut -d= -f2- || true)
fi
[[ -n "${WORKTREE_NAME}" ]] || WORKTREE_NAME=$(basename "${root}")

# OSC 1 sets the tab title. Emit to stderr so it reaches the user's terminal
# without contaminating any hook stdout contract.
printf '\033]1;mazarbul \xc2\xb7 %s \xc2\xb7 %s\007' "${WORKTREE_NAME}" "${slug}" >&2

exit 0
