#!/bin/bash
# PostToolUse hook (Layer 1): format + per-file lint with hard-block feedback.
#
# Behavior:
#   1. Format the file (gofumpt + goimports)
#   2. Lint just that file with --fix where supported
#   3. If unrecoverable errors remain, emit JSON to stdout:
#        {"decision":"block","reason":"<diagnostic>"}
#      Claude Code surfaces the reason inline and forces Claude to fix
#      before its next tool call.
#
# Exit 0 always, JSON output drives the decision, not exit codes.
# (Per Anthropic docs: "Don't mix exit 2 with JSON output".)

INPUT=$(cat)

FILE_PATH=$(echo "$INPUT" | python3 -c \
  "import sys, json; d=json.load(sys.stdin); print(d.get('tool_input', {}).get('file_path', ''))" \
  2>/dev/null || true)

[[ -z "$FILE_PATH" ]] && exit 0
[[ ! -f "$FILE_PATH" ]] && exit 0

REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(pwd)}"
LINT_OUTPUT=""
LINT_FAILED=0

emit_block() {
  local reason="$1"
  # JSON-escape the reason
  local escaped
  escaped=$(printf '%s' "$reason" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')
  echo "{\"decision\":\"block\",\"reason\":${escaped}}"
  exit 0
}

case "$FILE_PATH" in
  *.go)
    # Step 1: format
    if command -v gofumpt &>/dev/null; then
      gofumpt -w "$FILE_PATH" 2>/dev/null || true
    elif command -v gofmt &>/dev/null; then
      gofmt -w "$FILE_PATH" 2>/dev/null || true
    fi
    if command -v goimports &>/dev/null; then
      goimports -w -local github.com/mo-cmyk/mazarbul "$FILE_PATH" 2>/dev/null || true
    fi

    # Step 2: lint with --fix (only new issues from HEAD~1).
    # Lint the PACKAGE (file's directory) rather than the file alone, file-
    # scope lint can't resolve same-package symbols, generating dozens of
    # false-positive "undefined:" errors for adapters/types defined in
    # sibling files. Package-mode is correct + only slightly slower.
    if command -v golangci-lint &>/dev/null; then
      PKG_DIR=$(dirname "$FILE_PATH")
      # Portable rel-path (no GNU realpath dependency): strip the REPO_ROOT prefix.
      REL_PKG_DIR="${PKG_DIR#$REPO_ROOT/}"
      [[ "$REL_PKG_DIR" = "$PKG_DIR" ]] && REL_PKG_DIR="." || REL_PKG_DIR="./$REL_PKG_DIR"
      LINT_OUTPUT=$(cd "$REPO_ROOT" && golangci-lint run --fix --new-from-rev=HEAD~1 \
        --timeout=60s "${REL_PKG_DIR}/..." 2>&1)
      LINT_FAILED=$?
    fi

    # Environmental guard: detect package-load failures (broken imports,
    # context deadline, missing module path) and skip the block. These are
    # worktree-state symptoms, not edit-quality issues, blocking on them
    # forces a fix that doesn't belong to the current change.
    if echo "$LINT_OUTPUT" | grep -qE 'context loading failed|context deadline exceeded|could not import|BrokenImport|no required module provides package|failed to load packages|does not contain main module or its selected dependencies'; then
      echo "[post-tool-format] golangci-lint package-load failure (worktree state). Run 'go mod tidy' to fix; not blocking on environmental issue." >&2
      exit 0
    fi

    if [[ $LINT_FAILED -ne 0 && -n "$LINT_OUTPUT" ]]; then
      # Tail to keep reason concise
      DIAG=$(echo "$LINT_OUTPUT" | tail -20)
      emit_block "Lint failed in ${FILE_PATH}:

${DIAG}

Fix these lint errors before continuing. Run \`golangci-lint run --fix ${FILE_PATH}\` to attempt auto-fix on more, then address remaining manually."
    fi
    ;;

  *.json)
    # Skip, risk of breaking config
    ;;
esac

exit 0
