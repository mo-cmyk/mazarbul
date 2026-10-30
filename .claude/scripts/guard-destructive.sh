#!/usr/bin/env bash
# guard-destructive.sh , Centralized confirm-or-refuse for irreversible actions.
#
# Usage: ./scripts/guard-destructive.sh <action> "<recent-user-message>"
#
# Exits 0 if action is confirmed by the user message, 1 with stderr explanation
# otherwise. Skill bodies MUST call this before any of the gated actions below.
#
# Actions:
#   merge        , gh pr merge
#   force-push   , git push --force / --force-with-lease
#   release      , gh release create
#   push-main    , push to main / master (HARD REFUSE always)

set -euo pipefail

action="${1:?usage: guard-destructive.sh <action> <user-message>}"
msg="${2:-}"
msg_lower=$(printf '%s' "$msg" | tr '[:upper:]' '[:lower:]')

confirm() { printf 'OK: %s confirmed by user message\n' "$1" >&2; exit 0; }
refuse()  { printf 'REFUSED: %s , %s\n' "$1" "$2" >&2; exit 1; }

case "$action" in
  merge)
    if printf '%s' "$msg_lower" | grep -qE '\b(merge (it|this|the pr|now)|yes merge|--yes|go ahead and merge)\b'; then
      confirm merge
    else
      refuse merge "say 'merge it' / 'go ahead and merge' / pass --yes in your message"
    fi
    ;;
  force-push)
    if printf '%s' "$msg_lower" | grep -qE '\b(force[ -]ok|--force-ok|yes force|force push|force-push)\b'; then
      confirm force-push
    else
      refuse force-push "say 'force push' / '--force-ok' / 'yes force'"
    fi
    ;;
  release)
    if printf '%s' "$msg_lower" | grep -qE '\b(yes release|--yes|cut (a |the )?release|create release)\b'; then
      confirm release
    else
      refuse release "say 'yes release' / 'cut release' / pass --yes"
    fi
    ;;
  push-main|push-master)
    refuse "$action" "direct push to main/master is forbidden , open a PR"
    ;;
  *)
    refuse "$action" "unknown destructive action"
    ;;
esac
