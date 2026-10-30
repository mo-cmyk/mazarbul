#!/bin/bash
# Stop hook (Layer 2a): deterministic verification of in-progress work.
#
# Mirrors lefthook pre-commit logic but scoped to changed surfaces only.
# Writes a punch list to .claude/last-check.md (read by Layer 2b prompt-eval).
#
# Behavior:
#   1. Detect changed surfaces (git diff --name-only HEAD).
#   2. Run per-surface checks in parallel where possible.
#   3. Write .claude/last-check.md with 3 sections:
#      Failures / Warnings / Cleanup-opportunities
#   4. If failures => stdout: {"decision":"block","reason":"<top failures>"}
#   5. Else => exit 0 (Layer 2b prompt-eval may still block on cleanup).
#
# Exit 0 always, JSON output drives the decision, not exit codes.

REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(pwd)}"
cd "$REPO_ROOT" || exit 0

# Mirror .mise.toml [env]: the mattn/go-sqlite3 driver gates FTS5 behind the
# sqlite_fts5 build tag. Hooks bypass mise activation, so set GOFLAGS here
# too. Keep in sync with .mise.toml and .golangci.yml run.build-tags.
export GOFLAGS="${GOFLAGS:--tags=sqlite_fts5}"

# Hooks also bypass mise's PATH activation, so bare `go`/`golangci-lint`/`gofumpt`
# resolve to whatever system install is on PATH. When go.mod pins a newer Go than
# the system toolchain and GOTOOLCHAIN=local, every Go check below fails spuriously
# with "go.mod requires go >= X (running Y; GOTOOLCHAIN=local)". Prepend mise's
# shim dir so the checks run on the SAME pinned toolchain as CI (go/golangci-lint/
# gofumpt from .mise.toml). Same bridging intent as GOFLAGS above.
_MISE_SHIMS="${MISE_DATA_DIR:-$HOME/.local/share/mise}/shims"
[[ -d "$_MISE_SHIMS" ]] && export PATH="$_MISE_SHIMS:$PATH"

# Agent Teams: when running as a teammate (TEAMMATE_MODE=1, set by the Lead at
# spawn time), do only fast per-surface checks and skip the heavy matrix +
# Haiku eval. The Lead runs the full check matrix once after teammate
# dismissal, running it per-teammate would multiply Haiku eval cost ~4x and
# risk concurrent `mise run ci` invocations clobbering each other.
if [[ "${TEAMMATE_MODE:-0}" == "1" ]]; then
  mkdir -p .claude
  CHANGED=$(git diff --name-only HEAD 2>/dev/null; git diff --name-only --cached 2>/dev/null)
  CHANGED=$(echo "$CHANGED" | sort -u)
  TEAMMATE_FAILURES=()
  if echo "$CHANGED" | grep -qE '\.go$' && command -v go &>/dev/null; then
    if ! go vet ./... >/dev/null 2>&1; then
      TEAMMATE_FAILURES+=("go vet failed on teammate-touched Go files")
    fi
  fi
  if (( ${#TEAMMATE_FAILURES[@]} > 0 )); then
    {
      echo "# last-check (TEAMMATE_MODE)"
      echo ""
      echo "## Failures"
      printf -- '- %s\n' "${TEAMMATE_FAILURES[@]}"
      echo ""
      echo "## Warnings"
      echo ""
      echo "## Cleanup-opportunities"
    } > .claude/last-check.md
    REASON=$(printf '%s; ' "${TEAMMATE_FAILURES[@]}" | sed 's/; $//')
    printf '{"decision":"block","reason":"Teammate checks failed: %s. Fix before reporting done."}\n' "$REASON"
    exit 0
  fi
  # Success: rewrite last-check.md so downstream hooks (task-completed-verify.sh,
  # the prompt-eval pass) don't read a stale failure section from a prior tick.
  {
    echo "# last-check (TEAMMATE_MODE)"
    echo ""
    echo "## Failures"
    echo "_(none)_"
    echo ""
    echo "## Warnings"
    echo "_(none)_"
    echo ""
    echo "## Cleanup-opportunities"
    echo "_(none)_"
  } > .claude/last-check.md
  exit 0
fi

LAST_CHECK=".claude/last-check.md"
COUNTER_FILE=".claude/stop-counter.txt"
mkdir -p .claude

# Increment Stop invocation counter. Run lessons-rotate every 20th call.
COUNTER=0
[[ -f "$COUNTER_FILE" ]] && COUNTER=$(cat "$COUNTER_FILE" 2>/dev/null | grep -E '^[0-9]+$' | head -1)
COUNTER=${COUNTER:-0}
COUNTER=$((COUNTER + 1))
echo "$COUNTER" > "$COUNTER_FILE"
if (( COUNTER % 20 == 0 )) && [[ -x ".claude/hooks/lessons-rotate.sh" ]]; then
  bash .claude/hooks/lessons-rotate.sh 2>/dev/null || true
fi

# Skip if nothing changed since last commit.
if git diff --quiet HEAD 2>/dev/null && git diff --quiet --cached 2>/dev/null; then
  cat > "$LAST_CHECK" <<'EOF'
# Last Check, no changes

No uncommitted or staged changes since HEAD. Nothing to verify.

## Failures
_(none)_

## Warnings
_(none)_

## Cleanup-opportunities
_(none)_
EOF
  exit 0
fi

CHANGED=$(git diff --name-only HEAD 2>/dev/null; git diff --name-only --cached 2>/dev/null)
CHANGED=$(echo "$CHANGED" | sort -u)

# Diff size for substantive-work heuristic
DIFF_LOC=$(git diff HEAD --shortstat 2>/dev/null | \
  grep -oE '[0-9]+ insertion' | grep -oE '[0-9]+' | head -1)
DIFF_LOC=${DIFF_LOC:-0}

FAILURES=()
WARNINGS=()
CLEANUP=()

# Helper: run a check, capture output, classify as failure or warning.
run_check() {
  local name="$1"
  local cmd="$2"
  local severity="${3:-failure}"   # failure | warning
  local out
  out=$(eval "$cmd" 2>&1)
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    local tail_out
    tail_out=$(echo "$out" | tail -15)
    if [[ "$severity" == "warning" ]]; then
      WARNINGS+=("### ${name}"$'\n```\n'"${tail_out}"$'\n```')
    else
      FAILURES+=("### ${name}"$'\n```\n'"${tail_out}"$'\n```')
    fi
  fi
}

# Go changes
# Filter the changed-Go-files list to only those that still exist on disk
# (deleted files in the diff would crash gofumpt/go-test with confusing errors,
# "stat: no such file or directory"). The filter applies to every Go check
# below so we never block on a non-existent path.
EXISTING_GO_FILES=""
if echo "$CHANGED" | grep -qE '\.go$'; then
  while IFS= read -r f; do
    [[ -f "$f" ]] && EXISTING_GO_FILES+="$f"$'\n'
  done < <(echo "$CHANGED" | grep -E '\.go$')
fi

if [[ -n "$EXISTING_GO_FILES" ]]; then
  if command -v gofumpt &>/dev/null; then
    GOFUMPT_OUT=$(gofumpt -l $(echo "$EXISTING_GO_FILES" | tr '\n' ' ') 2>&1)
    if [[ -n "$GOFUMPT_OUT" ]]; then
      FAILURES+=("### gofumpt"$'\n```\nUnformatted files:\n'"${GOFUMPT_OUT}"$'\nRun: gofumpt -w <file>\n```')
    fi
  fi
  if command -v go &>/dev/null; then
    run_check "go vet" "go vet ./... 2>&1"
  fi
  if command -v golangci-lint &>/dev/null; then
    # Capture output and reclassify: if it ends with "0 issues." anywhere in
    # the body, treat as success even when the linter timed out. The timeout
    # only affects coverage, not correctness, 0 reported issues means clean.
    GCL_OUT=$(golangci-lint run --new-from-rev=HEAD~1 --timeout=60s 2>&1)
    GCL_RC=$?
    if [[ $GCL_RC -ne 0 ]] && ! echo "$GCL_OUT" | grep -qE '^0 issues\.?$'; then
      GCL_TAIL=$(echo "$GCL_OUT" | tail -15)
      FAILURES+=("### golangci-lint (new issues)"$'\n```\n'"${GCL_TAIL}"$'\n```')
    fi
  fi
  # Package-scoped tests, only include directories that still exist.
  CHANGED_PKGS=""
  while IFS= read -r d; do
    [[ -d "$d" ]] && CHANGED_PKGS+="./$d"$'\n'
  done < <(echo "$EXISTING_GO_FILES" | xargs -n1 dirname 2>/dev/null | sort -u)
  if [[ -n "$CHANGED_PKGS" ]]; then
    # -timeout is PER-package. Heavy live-backend packages (dispatcher, daemon)
    # open real SQLite+sqlite-vec+FTS5+LadybugDB in many tests and their -short
    # suites legitimately run ~50s; the prior 30s killed them mid-run and the
    # goroutine dump reads as a FAIL (a structural false-negative, not a real
    # failure). 180s lets a slow-but-correct suite finish while still bounding a
    # genuine hang. A real test failure still fails fast and is reported as-is.
    run_check "go test -short (changed packages)" \
      "go test -short -timeout=180s $(echo "$CHANGED_PKGS" | tr '\n' ' ') 2>&1"
  fi
fi

# Test-file naming convention
if echo "$CHANGED" | grep -qE '_test\.go$'; then
  if [[ -x "$REPO_ROOT/scripts/check-test-names.sh" ]]; then
    run_check "check-test-names" "$REPO_ROOT/scripts/check-test-names.sh 2>&1"
  fi
fi

# Substantive Go diff (>30 LoC) => dead-code scan as warning
if [[ "$DIFF_LOC" -gt 30 ]] && echo "$CHANGED" | grep -qE '\.go$'; then
  if grep -q '^\[tasks.dead\]' "$REPO_ROOT/.mise.toml" 2>/dev/null; then
    DEAD_OUT=$(mise run dead 2>&1)
    if [[ $? -ne 0 ]] || echo "$DEAD_OUT" | grep -qE '^[a-zA-Z_/]+\.go:[0-9]+'; then
      DEAD_TAIL=$(echo "$DEAD_OUT" | tail -15)
      CLEANUP+=("### mise run dead (unreachable code)"$'\n```\n'"${DEAD_TAIL}"$'\n```')
    fi
  fi
fi

# Compose last-check.md
{
  echo "# Last Check, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo ""
  echo "Changed files: $(echo "$CHANGED" | wc -l | tr -d ' ')"
  echo "Diff LoC: ${DIFF_LOC}"
  echo ""
  echo "## Failures"
  if [[ ${#FAILURES[@]} -eq 0 ]]; then
    echo "_(none)_"
  else
    for f in "${FAILURES[@]}"; do
      echo "$f"
      echo ""
    done
  fi
  echo ""
  echo "## Warnings"
  if [[ ${#WARNINGS[@]} -eq 0 ]]; then
    echo "_(none)_"
  else
    for w in "${WARNINGS[@]}"; do
      echo "$w"
      echo ""
    done
  fi
  echo ""
  echo "## Cleanup-opportunities"
  if [[ ${#CLEANUP[@]} -eq 0 ]]; then
    echo "_(none)_"
  else
    for c in "${CLEANUP[@]}"; do
      echo "$c"
      echo ""
    done
  fi
} > "$LAST_CHECK"

# Block if failures exist
if [[ ${#FAILURES[@]} -gt 0 ]]; then
  REASON="Stop blocked: ${#FAILURES[@]} verification failure(s).

$(head -60 "$LAST_CHECK")

See .claude/last-check.md for full output. Fix these before claiming done."
  ESCAPED=$(printf '%s' "$REASON" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')
  echo "{\"decision\":\"block\",\"reason\":${ESCAPED}}"
  exit 0
fi

exit 0
