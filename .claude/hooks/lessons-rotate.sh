#!/bin/bash
# Lessons rotation: archive stale entries, evict over-cap, log to changelog.
#
# Called from stop-verify.sh every 20th invocation (counter in .claude/stop-counter.txt).
# Safe to run more often, it's idempotent (no-op when nothing meets criteria).
#
# Criteria:
#   1. Last seen >30d ago AND Recurrence <=2 AND Compliance: false => archive
#   2. Promoted to: filled >7d ago => archive (the rule already lives at its destination)
#   3. lessons-learned.md > 50 entries => archive lowest-Recurrence + oldest until count = 50
#
# Compliance: true entries are immortal, never auto-archive.
# Output: nothing on stdout (silent). All actions logged to lessons-changelog.md.

REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(pwd)}"
cd "$REPO_ROOT" || exit 0

LESSONS="agent-docs/lessons-learned.md"
ARCHIVE="agent-docs/lessons-archive.md"
CHANGELOG="agent-docs/lessons-changelog.md"

[[ ! -f "$LESSONS" ]] && exit 0
[[ ! -f "$ARCHIVE" ]] && exit 0
[[ ! -f "$CHANGELOG" ]] && exit 0

python3 <<'PYEOF'
import os
import re
from datetime import datetime, timezone, timedelta

LESSONS = "agent-docs/lessons-learned.md"
ARCHIVE = "agent-docs/lessons-archive.md"
CHANGELOG = "agent-docs/lessons-changelog.md"

CAP = 50
STALE_DAYS = 30
PROMOTED_GRACE_DAYS = 7

with open(LESSONS) as f:
    text = f.read()

# Split at "## Active entries", everything before is preamble, after is entries
parts = text.split("## Active entries", 1)
if len(parts) != 2:
    # Schema missing, bail silently
    raise SystemExit(0)

preamble = parts[0] + "## Active entries\n"
entries_text = parts[1]

# Parse entries by ### LSN- header
entry_pattern = re.compile(r"^### (LSN-\d{4}-\d{2}-\d{2}-\d{3})", re.MULTILINE)
matches = list(entry_pattern.finditer(entries_text))

if not matches:
    raise SystemExit(0)

entries = []
# Find any "## " heading after the entries (e.g. "## What self-healing means")
# to bound the last entry's block.
trailing_section = re.search(r"^## ", entries_text, re.MULTILINE)
trailing_start = trailing_section.start() if trailing_section else len(entries_text)
# Recompute entry matches but only within the [0, trailing_start) range
entry_matches_bounded = [m for m in matches if m.start() < trailing_start]
for i, m in enumerate(entry_matches_bounded):
    start = m.start()
    if i + 1 < len(entry_matches_bounded):
        end = entry_matches_bounded[i + 1].start()
    else:
        end = trailing_start
    block = entries_text[start:end].rstrip() + "\n"
    lsn = m.group(1)

    def field(name):
        rx = re.compile(rf"^- \*\*{re.escape(name)}\*\*:\s*(.+?)$", re.MULTILINE)
        mm = rx.search(block)
        return mm.group(1).strip() if mm else ""

    last_seen = field("Last seen")
    recurrence_raw = field("Recurrence")
    compliance = field("Compliance").lower() == "true"
    promoted_to = field("Promoted to")
    # Strip "_(empty)_" markers
    if "empty" in promoted_to.lower() or not promoted_to:
        promoted_to = ""

    try:
        recurrence = int(recurrence_raw)
    except ValueError:
        recurrence = 1

    try:
        last_seen_dt = datetime.strptime(last_seen, "%Y-%m-%d").replace(tzinfo=timezone.utc)
    except ValueError:
        last_seen_dt = datetime.now(timezone.utc)

    entries.append(
        {
            "lsn": lsn,
            "block": block,
            "last_seen": last_seen_dt,
            "recurrence": recurrence,
            "compliance": compliance,
            "promoted_to": promoted_to,
            "summary": m.string[m.start() : end].split("\n", 1)[0].lstrip("# ").strip(),
        }
    )

now = datetime.now(timezone.utc)
to_archive = []
to_keep = []

# Pass 1: criteria-based archival
for e in entries:
    age_days = (now - e["last_seen"]).days

    # Rule 2: promoted >7d ago => archive
    if e["promoted_to"] and age_days >= PROMOTED_GRACE_DAYS:
        to_archive.append((e, "promoted-stale"))
        continue

    # Rule 1: stale + low-recurrence + non-compliance => archive
    if age_days >= STALE_DAYS and e["recurrence"] <= 2 and not e["compliance"]:
        to_archive.append((e, "stale"))
        continue

    to_keep.append(e)

# Pass 2: enforce cap, evict lowest-recurrence-and-oldest from non-compliance keepers
if len(to_keep) > CAP:
    over = len(to_keep) - CAP
    # Sort by (compliance desc, recurrence desc, last_seen desc), evict from the tail of non-compliance
    non_compliance = [e for e in to_keep if not e["compliance"]]
    compliance = [e for e in to_keep if e["compliance"]]
    non_compliance.sort(key=lambda x: (x["recurrence"], x["last_seen"]))
    evicted = non_compliance[:over]
    to_archive.extend([(e, "cap-eviction") for e in evicted])
    to_keep = compliance + [e for e in non_compliance if e not in evicted]

if not to_archive:
    raise SystemExit(0)  # No-op

# Re-sort kept entries by last_seen ascending (oldest first, like a log)
to_keep.sort(key=lambda x: x["last_seen"])

# Write back lessons-learned.md
with open(LESSONS, "w") as f:
    f.write(preamble)
    f.write("\n")
    if to_keep:
        for e in to_keep:
            f.write(e["block"])
            f.write("\n")
    else:
        f.write("_(none yet, automation will populate this section)_\n")
    f.write("\n## What \"self-healing\" means here\n\n")
    f.write("- **Self-update**: existing entries get refreshed instead of duplicated\n")
    f.write("- **Self-promote**: recurring lessons migrate to permanent homes without ceremony\n")
    f.write("- **Self-archive**: forgotten rules don't bloat context (but stay searchable in archive)\n")
    f.write("- **Self-merge**: near-duplicates consolidate with audit trail\n")
    f.write("- **Self-validate**: `session-start.sh` checks schema integrity on every session\n\n")
    f.write("See `agent-docs/lessons-changelog.md` for the audit log. Git history is the deeper safety net.\n")

# Append to archive
with open(ARCHIVE, "a") as f:
    for e, reason in to_archive:
        f.write("\n")
        f.write(e["block"].rstrip())
        f.write(f"\n- **Archived**: {now.strftime('%Y-%m-%d')} (reason: {reason})\n")

# Append to changelog
with open(CHANGELOG, "a") as f:
    ts = now.strftime("%Y-%m-%dT%H:%M:%SZ")
    for e, reason in to_archive:
        f.write(f"{ts} | archive | {e['lsn']} | {reason}\n")

PYEOF

exit 0
