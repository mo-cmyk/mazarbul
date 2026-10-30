#!/usr/bin/env bash
# smoke-omnigraph-wave.sh — end-to-end smoke test for the Omnigraph-parity wave.
#
# Exercises the CLI surface added by the wave against a freshly-initialized,
# isolated data directory using the real binary. Runs with --embed none so it
# needs no Ollama (recall-specific paths are covered by the Go integration
# suite). Asserts the contracts that matter most for the wave:
#   - schema preview computes a plan without writing
#   - saved recalls round-trip
#   - the dynamic feedback loop auto-promotes a recurring rule and the
#     export is allowlist-redacted (NO rule/scope/principal/team fields)
#   - load is idempotent in merge mode and writes nothing on --dry-run
#   - draft staging hides records from default reads, the read verbs REFUSE
#     scope=draft (E_VALIDATION), promote makes a draft visible, discard removes it
#
# Exit codes:
#   0   smoke test passed
#   1   smoke test failed (a contract was violated)
#   2   unexpected error (build failure, dependency missing)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA=$(mktemp -d)
BIN=$(mktemp -u "/tmp/maz-wave-smoke-XXXXXX")
FIX=$(mktemp -u "/tmp/maz-wave-fixture-XXXXXX.jsonl")
EXPORT=$(mktemp -u "/tmp/maz-wave-export-XXXXXX.jsonl")

cleanup() {
  rm -rf "$DATA"
  rm -f "$BIN" "$FIX" "$EXPORT"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

maz() { "$BIN" --data-dir "$DATA" "$@"; }

echo "==> Building maz binary..."
( cd "$REPO_ROOT" && go build -tags=sqlite_fts5 -o "$BIN" ./cmd/maz ) || exit 2
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 2; }

echo "==> Initializing data directory (embed=none, daemon=off): $DATA"
maz init --yes --provider ollama --embed none --daemon off >/dev/null

echo "==> schema preview computes a plan without writing..."
plan=$(maz schema preview kind vehicle --description "a car or truck the user owns" --output json)
echo "$plan" | jq -e '.plan.action == "mint"' >/dev/null || fail "schema preview did not return a mint plan: $plan"

echo "==> saved recall round-trips..."
maz saved add fav --query "grey havens" >/dev/null
maz saved list --output json | jq -e 'any(.[]; .name == "fav")' >/dev/null || fail "saved recall 'fav' not listed"

echo "==> dynamic feedback loop auto-promotes a recurring rule..."
for _ in 1 2 3; do
  maz feedback record \
    --category lesson --severity medium --surface recall \
    --summary "recall missed an exact entity name" \
    --rule "prefer get when you already hold an exact name" >/dev/null
done
gotchas=$(maz feedback gotchas --output json)
echo "$gotchas" | jq -e '.gotchas | length >= 1' >/dev/null || fail "feedback did not auto-promote after 3 recordings: $gotchas"
echo "$gotchas" | jq -e '.gotchas[0].recurrence >= 3' >/dev/null || fail "promoted rule recurrence < 3: $gotchas"

echo "==> feedback export is allowlist-redacted..."
maz feedback export --format jsonl -o "$EXPORT" >/dev/null
python3 - "$EXPORT" <<'PY' || fail "feedback export leaked a non-allowlisted field"
import json, sys
allow = {"id", "ts", "category", "severity", "surface", "summary", "detail", "keywords", "version", "activePreset"}
forbidden = {"uid", "rule", "clusterKey", "scope", "principal", "principalUlid", "team", "teamScope"}
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    keys = set(json.loads(line).keys())
    if keys & forbidden:
        sys.exit(f"LEAK: forbidden keys {sorted(keys & forbidden)}")
    extra = keys - allow
    if extra:
        sys.exit(f"LEAK: non-allowlisted keys {sorted(extra)}")
PY

echo "==> feedback veto demotes the promoted rule..."
maz feedback veto "lesson|recall|prefer get when you already hold" >/dev/null
vetoed=$(maz feedback gotchas --output json)
echo "$vetoed" | jq -e '(.gotchas // []) | length == 0' >/dev/null || fail "veto did not demote the rule: $vetoed"

echo "==> load --dry-run writes nothing; merge is idempotent..."
# Use real, already-signed events from the log as the fixture (re-loading them
# in merge mode must be a no-op). Locate the newest events JSONL robustly rather
# than guessing the date-partitioned path.
log_file=$(find "$DATA/events" -name '*.jsonl' -type f 2>/dev/null | sort | tail -1)
[ -n "$log_file" ] || fail "no events JSONL found under $DATA/events"
head -2 "$log_file" > "$FIX"
dry=$(maz load "$FIX" --dry-run --output json)
echo "$dry" | jq -e '.dryRun == true and .appended == 2' >/dev/null || fail "load --dry-run report wrong: $dry"
merge=$(maz load "$FIX" --mode merge --output json)
echo "$merge" | jq -e '.appended == 0 and .skipped == 2' >/dev/null || fail "load --mode merge not idempotent: $merge"

echo "==> draft staging hides records and refuses scope=draft on read verbs..."
draft_name=$(maz remember "Speculative: switch to Rust in Q3" --scope draft --output json | jq -r '.name')
draft_uid="${draft_name#episodes/}"
maz scope draft list --output json | jq -e --arg u "$draft_uid" 'any(.draft.episodes[]; .uid == $u)' >/dev/null \
  || fail "draft episode not visible in scope draft list"
# F1: read verbs must REFUSE scope=draft.
if rej=$(maz entities --scope draft --output json 2>&1); then
  fail "entities --scope draft was NOT rejected: $rej"
fi
echo "$rej" | grep -q "E_VALIDATION" || fail "entities --scope draft did not return E_VALIDATION: $rej"
# Default read must not surface the draft.
if maz get "$draft_name" --output json >/dev/null 2>&1; then
  fail "draft episode is visible to a default-scope get before promotion"
fi

echo "==> promote makes the draft visible..."
maz scope draft promote "$draft_uid" --output json | jq -e '.toScope == "personal"' >/dev/null || fail "promote did not report personal"
maz get "$draft_name" --output json | jq -e '.episode.uid == "'"$draft_uid"'"' >/dev/null || fail "promoted episode not visible via get"

echo "==> discard removes a draft..."
discard_name=$(maz remember "Throwaway speculation" --scope draft --output json | jq -r '.name')
discard_uid="${discard_name#episodes/}"
maz scope draft discard "$discard_uid" --output json | jq -e '.status == "discarded"' >/dev/null || fail "discard did not report discarded"
if maz get "$discard_name" --output json >/dev/null 2>&1; then
  fail "discarded episode still retrievable"
fi

echo ""
echo "smoke-omnigraph-wave passed"
