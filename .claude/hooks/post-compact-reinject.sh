#!/bin/bash
# PostCompact hook: fires after Claude compresses its conversation context.
# Re-injects the critical Mazarbul rules so they survive compaction.
#
# Output goes to stdout, Claude reads this as additional system context after
# compaction completes. Keep it tight (token cost on every compaction).

cat <<'EOF'
=== Mazarbul critical rules (re-injected after context compaction) ===

1. Event log is APPEND-ONLY. Never UPDATE or DELETE event rows. Supersede via newer events.
2. No proto. Schema layer is JSON Schema. Types are Go structs with json: tags.
3. AIP naming. createTime not createdAt. Plural collection paths. CRUD verbs (Create/Get/List/Update/Delete).
4. Provenance on every fact. Source, confidence, timestamp on every entity, relation, inference.
5. Local-first. SQLite + LadybugDB + sqlite-vec embedded in-process. Cloud sync is opt-in.
6. Timestamps: UTC time.Time, JSON RFC 3339. Never *_at suffixes.
7. No em-dashes in code, comments, docs, commits, or PR descriptions.
8. TUI files only reference colors via internal/tui/style/. No raw hex outside the style package.

Karpathy 4 principles still apply: Think Before Coding, Simplicity First,
Surgical Changes, Goal-Driven Execution.

Autonomous quality loop is active (see AGENTS.md "Autonomous quality loop"):
- Stop hook prompt-eval reads .claude/last-check.md + your last message and may
  return decision:block with directive reason. Follow the reason, it is the
  next step the system has chosen for you, not a suggestion.
- Key skills: /dead /critique /preflight /go-strict. Use /go-strict for
  substantive features (>50 LoC), it auto-iterates verify/critique/dead/simplify.

See AGENTS.md for the full agent context.
=== End re-injection ===
EOF

exit 0
