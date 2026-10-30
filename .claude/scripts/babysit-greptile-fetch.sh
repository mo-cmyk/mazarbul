#!/usr/bin/env bash
# babysit-greptile-fetch.sh , Fetch Greptile bot comments on a PR since a marker.
#
# Output: newline-delimited JSON, one Greptile inline-comment per line. Compact
# (`jq -c`) so downstream `jq -s 'length'` works even when comment bodies
# contain literal newlines.
#
# Usage:
#   .claude/scripts/babysit-greptile-fetch.sh <PR> [since-iso-or-id]
#
# Examples:
#   .claude/scripts/babysit-greptile-fetch.sh 123
#   .claude/scripts/babysit-greptile-fetch.sh 123 2026-05-21T18:00:00Z
#   .claude/scripts/babysit-greptile-fetch.sh 123 9876543210
#
# Empty `since` returns every Greptile inline comment on the PR. The login
# filter matches `greptile-apps[bot]` case-insensitively (`test("greptile";"i")`)
# to survive any future bot-name renames.

set -euo pipefail

PR="${1:?usage: babysit-greptile-fetch.sh <PR> [since]}"
SINCE="${2:-}"

repo=$(gh repo view --json owner,name -q '"\(.owner.login)/\(.name)"')

# `gh api --paginate --jq '.[]'` yields one object per line (gh streams each
# page's array elements). We then pipe through `jq -c` to get compact, single-
# line output that downstream slurp-style consumers can parse safely.
if [ -n "$SINCE" ]; then
  if [[ "$SINCE" =~ ^[0-9]+$ ]]; then
    gh api "repos/${repo}/pulls/${PR}/comments" --paginate --jq '.[]' \
      | jq -c "select(.user.login | test(\"greptile\";\"i\")) | select(.id > ${SINCE})"
  else
    gh api "repos/${repo}/pulls/${PR}/comments" --paginate --jq '.[]' \
      | jq -c "select(.user.login | test(\"greptile\";\"i\")) | select(.created_at > \"${SINCE}\")"
  fi
else
  gh api "repos/${repo}/pulls/${PR}/comments" --paginate --jq '.[]' \
    | jq -c 'select(.user.login | test("greptile";"i"))'
fi
