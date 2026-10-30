---
name: lessons
description: Self-healing memory loop. USE WHEN route-prompt.sh injects CORRECTION DETECTED directive, OR when you need to manually log a lesson from a non-correction signal (a CI failure that wasn't caught, a real bug landed in main). Greps lessons.md + skill Gotchas, updates or appends an entry, auto-promotes at recurrence thresholds, logs to lessons-changelog.md. NOT FOR routine work , only invoke when there's a durable rule worth capturing.
allowed-tools: Bash(grep *), Bash(date *), Read, Edit, Write
---

# /lessons , Compounding-engineering auto-loop

This skill is the formal workflow Claude follows when the correction-detection hook fires. The hook injects a short pointer; this skill holds the detailed procedure so the hook prompt stays small.

## When this runs

`route-prompt.sh` injects a directive when the user's prompt matches correction signals: "no", "stop", "actually", "wrong", "missed", "forgot", "again", "you should have", "didn't I tell you", "we discussed this". The directive points here.

Claude then runs this skill's procedure **silently** , the user does not see logging activity, only the answer to their actual question. The logging happens after the user-facing reply, as a separate tool sequence.

## The procedure

### Step 1 , Identify what the correction is about

Extract the core lesson in one sentence. Example: user says "I told you new RPCs need permission.go" → lesson = "new MCP tool methods require an entry in `internal/auth/permission.go` or they return PermissionDenied".

Then pick:
- **3-6 keywords** that capture the rule (e.g. `JSON-RPC, permission.go, PermissionDenied, deny-by-default`)
- **Compliance flag** , `true` if it touches the event log, scope enforcement, append-only invariants, or any safety-critical area. `false` otherwise.
- **Scope** , `skill-specific` (name the skill) / `cross-cutting` (AGENTS.md) / `subagent-specific` (name the agent) / `project-context` (name the agent-docs file)

### Step 2 , Grep before write

Search all four locations in parallel:

```bash
grep -i -l "<keyword1>" agent-docs/lessons.md .claude/skills/*/SKILL.md AGENTS.md .claude/agents/*.md
grep -i -l "<keyword2>" agent-docs/lessons.md .claude/skills/*/SKILL.md AGENTS.md .claude/agents/*.md
# ...etc
```

Also grep the archive:

```bash
grep -i -l "<keyword>" agent-docs/lessons-archive.md
```

If you find a matching entry, go to **Step 3a (update)**. If multiple matches across files, go to **Step 3b (merge)**. If nothing matches (including archive), go to **Step 3c (append)**.

### Step 3a , Update existing entry

Edit the matching entry:
- Increment `Recurrence: N` → `Recurrence: N+1`
- Refresh `Last seen: YYYY-MM-DD`
- Add any new keywords (union, dedup)
- If the symptom/fix needs refinement, edit those lines

Then go to **Step 4 (promotion check)**.

If the match was in `lessons-archive.md`, this is a *resurrection*: move the entry back to `lessons.md`, add `**Resurrected**: YYYY-MM-DD`, log `resurrect` to changelog, then promotion check.

### Step 3b , Merge multiple matches

If the same keyword cluster appears in 2+ active entries:
1. Pick the entry with highest `Recurrence` as the survivor
2. Union all `Keywords` lists
3. Use the latest `Last seen`
4. Concatenate `Symptom` + `Fix` with a `**Merged from**: LSN-a, LSN-b` line
5. Delete the absorbed entries
6. Log `merge` to changelog with all involved LSN ids

### Step 3c , Append new entry

Generate a new ID: `LSN-YYYY-MM-DD-NNN` where NNN is the next sequential number for today (grep `LSN-$(date +%Y-%m-%d)` and increment).

Append at the end of the `## Active entries` section in `lessons.md`:

```markdown
### LSN-YYYY-MM-DD-NNN , <one-line summary>
- **First seen**: YYYY-MM-DD
- **Last seen**: YYYY-MM-DD
- **Recurrence**: 1
- **Keywords**: kw1, kw2, kw3
- **Compliance**: true | false
- **Symptom**: <one or two sentences>
- **Fix**: <one or two actionable sentences>
- **Promoted to**: _(empty)_
```

Log `append` to changelog.

### Step 4 , Promotion check (auto, no user prompt)

Read the new `Recurrence` value. Check the thresholds from `agent-docs/lessons.md`:

| Compliance | Scope | Threshold |
|---|---|---|
| true | skill-specific or cross-cutting | ≥ 2 |
| false | skill-specific | ≥ 3 |
| false | cross-cutting | ≥ 4 |
| any | subagent or project-context | ≥ 3 |

If threshold met:
1. Identify the destination file based on Scope.
2. Check the destination's cap (skill `## Gotchas` ≤10 items, AGENTS.md Critical rules ≤10 items).
3. **If at cap**: auto-merge the two least-recent items at the destination into one consolidated bullet, freeing a slot. Log the merge.
4. Append the lesson to the destination's `## Gotchas` (or Critical rules) as a one-line bullet starting with `**<short rule>**`. Keep it tight , the destination is not a copy of lessons.md.
5. Fill `**Promoted to**: <destination-file-path>` on the lessons.md entry.
6. Log `promote` to changelog.

Do not delete the lessons.md entry immediately , the rotation hook will archive it 7 days later (audit trail).

### Step 5 , Log to changelog

Append one line to `agent-docs/lessons-changelog.md`:

```
2026-05-21T09:42:13Z | append | LSN-2026-05-21-003 | new MCP tool methods need permission.go entry
2026-05-21T09:42:13Z | promote | LSN-2026-05-21-003 → .claude/skills/new-resource/SKILL.md
```

(Multi-action sequences get multiple lines.)

### Step 6 , Reply to the user about the actual correction

The user asked something or corrected you. They want an answer or fix , they don't want a logging narration. Answer/fix first, then say (optionally) "Noted , logged for future reference." in one short sentence. Never paste the LSN id or the schema at the user.

## Edge cases

**Many corrections in one prompt** , pick the most actionable one and log it. The others get their turn the next time they recur.

**The correction contradicts an existing rule** , log it AND log a `conflict` line to changelog. Set `Promoted to: CONFLICT , <existing-rule-location>` so the next session knows to reconcile. The pruning script will surface conflicts.

**The user explicitly says "don't log this" or "ignore"** , don't log. Trust the user.

**Keywords too generic** (e.g. "test", "fix") , refuse to log under generic keys. Require 2+ specific keywords (file names, function names, concept terms). If the correction is too vague, ask the user to clarify before logging.

**Compliance flag uncertain** , default to `true`. Better to mark something immortal incorrectly than to lose a safety-critical rule.

## Gotchas

- **Do NOT narrate logging to the user** , they corrected you; they want the fix, not your bookkeeping.
- **Do NOT log every "no"** , only log when the correction reveals a *durable rule*. "No, use red instead of blue" is not a rule. "No, you didn't update permission.go" is a rule.
- **Do NOT skip the grep step** , duplicate entries are the #1 cause of bloat. Updating an existing entry is always cheaper than appending a new one.
- **Do NOT manually invoke `/lessons` from the conversation** , if you find yourself doing this, the hook is broken. Investigate the hook, not the workaround.
- **Do NOT touch lessons-archive.md or lessons-changelog.md directly** , only `lessons-rotate.sh` and this skill (via the standard procedure) write to them. Hand-edits break the audit trail.
