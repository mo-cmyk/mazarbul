#!/usr/bin/env bash
# babysit-state.sh , Per-PR state-file helpers for the babysit autonomous loop.
#
# Source this in skill bodies:
#   source .claude/scripts/babysit-state.sh <PR-number>
# Then call the helpers below.
#
# State file: .claude/babysit-state-<PR>.json (worktree-local, gitignored).
# Audit archive: .claude/babysit-audit-archive.jsonl (when >50 entries rotate).

set -euo pipefail

PR="${1:?usage: source babysit-state.sh <PR-number>}"
STATE=".claude/babysit-state-${PR}.json"

# Atomic write via temp file.
_write() { local tmp; tmp=$(mktemp); cat > "$tmp" && mv "$tmp" "$STATE"; }
_now()   { date -u +%Y-%m-%dT%H:%M:%SZ; }

state_exists() { [ -f "$STATE" ]; }

state_init() {
  local sha; sha=$(git rev-parse HEAD)
  local ts;  ts=$(_now)
  cat > "$STATE" <<EOF
{
  "pr": $PR,
  "phase": "ci_pending",
  "ci_fix_iter": 0,
  "greptile_iter": 0,
  "diff_hash_history": [],
  "last_commit_sha": "$sha",
  "greptile_last_seen_comment_id": null,
  "greptile_silent_ticks": 0,
  "last_notification_kind": null,
  "cron_job_id": null,
  "started_at": "$ts",
  "last_event_ts": "$ts",
  "audit_log": []
}
EOF
}

get_phase() { jq -r '.phase' "$STATE"; }

set_phase() {
  local ts; ts=$(_now)
  jq --arg p "$1" --arg ts "$ts" '.phase = $p | .last_event_ts = $ts' "$STATE" | _write
}

get_field() {
  # Return JSON null and missing keys as empty string. Returning the literal
  # string "null" silently propagated into ISO-date comparators
  # (`.created_at > "null"` matches nothing because "2026-..." < "null"
  # lexicographically) and zeroed real comment matches on PR #694. Empty
  # is the canonical shell sentinel; callers use `[ -n "$VAR" ]`.
  jq -r --arg k "$1" '.[$k] // empty' "$STATE"
}

set_field() {
  jq --arg k "$1" --arg v "$2" '.[$k] = $v' "$STATE" | _write
}

set_field_json() {
  # Set a numeric/bool/null field (not a string).
  jq --arg k "$1" --argjson v "$2" '.[$k] = $v' "$STATE" | _write
}

get_iter() { jq -r --arg k "$1" '.[$k] // 0' "$STATE"; }

inc_iter() {
  jq --arg k "$1" '.[$k] = (.[$k] // 0) + 1' "$STATE" | _write
}

reset_iter() {
  jq --arg k "$1" '.[$k] = 0' "$STATE" | _write
}

record_diff_hash() {
  local sha; sha=$(git rev-parse HEAD)
  jq --arg s "$sha" '.diff_hash_history = (.diff_hash_history + [$s])[-3:] | .last_commit_sha = $s' "$STATE" | _write
}

circuit_check() {
  # Exit 0 when last 3 diff hashes are identical (no progress).
  jq -e '.diff_hash_history | length == 3 and (unique | length == 1)' "$STATE" >/dev/null
}

log_audit() {
  local action="$1" note="${2:-}"
  local phase tick ts
  phase=$(get_phase)
  tick=$(jq -r '.audit_log | length' "$STATE")
  ts=$(_now)
  jq --arg p "$phase" --arg a "$action" --arg n "$note" --arg ts "$ts" --argjson t "$tick" \
    '.audit_log += [{ts: $ts, tick: $t, phase: $p, action: $a, note: $n}]' \
    "$STATE" | _write

  # Rotate after 50 entries: archive oldest 30, keep tail.
  local count; count=$(jq -r '.audit_log | length' "$STATE")
  if [ "$count" -gt 50 ]; then
    jq -c '.audit_log[:30][]' "$STATE" >> .claude/babysit-audit-archive.jsonl
    jq '.audit_log = .audit_log[30:]' "$STATE" | _write
  fi
}
