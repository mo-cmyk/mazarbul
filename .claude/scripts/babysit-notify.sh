#!/usr/bin/env bash
# babysit-notify.sh , Dedup gate for the 7 babysit notification kinds.
#
# Usage: ./scripts/babysit-notify.sh <PR> <kind>
# Output: prints "send" or "skip" to stdout.
#
# The skill body should call PushNotification ONLY when this prints "send".
# This script writes last_notification_kind on send so the next tick dedups.
#
# Allowed kinds (defined in the plan):
#   pr_opened, ci_blocked_after_5, circuit_breaker_tripped,
#   greptile_not_converged, merge_ready, deploy_success, deploy_failure
#
# deploy_failure is exempt from dedup (different workflows can fail
# independently and each one deserves a fresh ping).

set -euo pipefail

PR="${1:?usage: babysit-notify.sh <PR> <kind>}"
KIND="${2:?usage: babysit-notify.sh <PR> <kind>}"
STATE=".claude/babysit-state-${PR}.json"

[ -f "$STATE" ] || { echo "send"; exit 0; }  # No state → first run → send

last=$(jq -r '.last_notification_kind // "none"' "$STATE")

if [ "$last" = "$KIND" ] && [ "$KIND" != "deploy_failure" ]; then
  echo skip
  exit 0
fi

# Update last_notification_kind atomically.
tmp=$(mktemp)
jq --arg k "$KIND" '.last_notification_kind = $k' "$STATE" > "$tmp"
mv "$tmp" "$STATE"
echo send
