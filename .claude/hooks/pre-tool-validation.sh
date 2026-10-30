#!/bin/bash
# PreToolUse hook: block Claude from writing to forbidden or auto-generated paths.
# Exits 2 to block the tool call and show the reason.

INPUT=$(cat)

# Extract file_path from the tool input JSON (works for both Write and Edit tools).
FILE_PATH=$(echo "$INPUT" | python3 -c \
  "import sys, json; d=json.load(sys.stdin); print(d.get('tool_input', {}).get('file_path', ''))" \
  2>/dev/null || true)

[[ -z "$FILE_PATH" ]] && exit 0  # Unknown path, allow.

# Paths that must never be manually written via Claude's Write/Edit tools.
FORBIDDEN=(
  "/.git/"
  "/node_modules/"
  "/dist/"
  "/.next/"
  "/bin/"
  "/.mazarbul-data/"
)

for pattern in "${FORBIDDEN[@]}"; do
  if [[ "$FILE_PATH" == *"$pattern"* ]]; then
    echo "Blocked: $FILE_PATH matches forbidden path pattern '$pattern'." >&2
    exit 2
  fi
done

exit 0
