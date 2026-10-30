# Dynamic lessons & feedback loop — Mazarbul learns how it's used

Mazarbul tracks what agents find confusing, wrong, or worth remembering about
*using Mazarbul itself*, and evolves its own per-session guidance over time. It
is not a frozen, human-curated FAQ: recurring feedback **auto-promotes** into the
live instruction surface, and the user keeps a pin / edit / veto override. The
trusted base operating manual is never rewritten by this loop — only distilled,
sanitized one-liners are appended, last, in the low-trust window.

This mirrors this repo's own `/lessons` discipline (record durable gotchas, not
every one-off disagreement), but lives *inside* maz so it works for any agent,
any client, across sessions.

## The loop

```
agent hits friction / learns a rule
        │  agentFeedback(...)
        ▼
agentFeedbackRecorded event ──► agent_feedback projection (cluster_key)
        │                              │
        │  recurrence per cluster      │
        ▼                              ▼
 threshold crossed?  ──yes──►  ActiveGotchas  ──►  "## Known gotchas & lessons"
 (>=3, or >=2 high)            (capped, sorted)     appended LAST to every new
        │                              ▲              MCP session's instructions
        │  user override               │
        ▼                              │
 feedbackReview (pin/edit/veto) ──► agent_feedback_reviews
```

Promotion is **derived from recurrence**, so it is deterministic on a full event-
log rebuild. Reviews are stored as overrides, not as edits to the promoted set.

## Intake

| Surface | Call |
|---|---|
| MCP | `agentFeedback({category, severity?, summary, surface?, detail?, rule?, keywords?, scope?})` → `{status:"recorded", clusterKey}` |
| CLI | `maz feedback record --category … --summary … [--severity …] [--surface …] [--rule …]` |

- **category** (required): `confusing | missing | wrong_result | slow | tool_error
  | suggestion | lesson`. The first six are friction; `lesson` is a durable rule
  you learned (give the distilled one-liner in `rule`).
- **severity**: `low | medium | high` (default `low`). `high` promotes at a lower
  recurrence threshold.
- File **durable** friction or rules, not one-off disagreements — the same
  `/lessons` "don't log every no" discipline. The tool description says so to the
  agent.

Each record is clustered by a deterministic **lexical** key —
`category | surface | first-6-significant-tokens` of the rule (or summary) — so
paraphrases of the same friction count toward **one** recurrence, not many. (This
is embedder-free on purpose: the cluster key must be stable on replay.)

## Auto-promotion (the dynamic part)

`feedback.ActiveGotchas` folds the `agent_feedback` rows by cluster and returns
the promoted set:

- A cluster promotes when its recurrence reaches the threshold: **3** by default,
  **2** when its highest observed severity is `high`.
- Pinned clusters are always included (exempt from the cap).
- Vetoed clusters never appear.
- The set is capped at **8** entries, sorted pinned-first then by recurrence then
  recency, deterministic on ties by cluster key.

`internal/mcp/server.go ComposeServerInstructions(preset, gotchas)` appends the
rendered set under `## Known gotchas & lessons` **after** the base manual, the
bootstrap pointer, and the preset section. The daemon instruction composer
(`newInstructionComposer`) reads `ActiveGotchas` **live, per session**, via the
existing `OnAfterInitialize` hook — so every new connection reflects the evolved
guidance with no new injection mechanism. A read failure degrades to no gotchas;
it never breaks the instructions.

## Trust boundary (why this is safe near the system prompt)

Feedback text is **untrusted**. The loop is designed so a crafted feedback string
cannot escalate into the trusted instruction window:

- Only **distilled, recurring** rules promote — never raw feedback bodies, never
  a single report.
- `RenderGotchasSection` runs every rule through `sanitizeRule`: newlines and
  backticks stripped, leading structural markdown markers (`#`, `>`, `-`, `*`,
  `+`) removed, length-capped. A rule cannot forge a heading or break the fence.
- The section is placed **last** — the truncation-sacrificable, lowest-trust part
  of the instructions — and only ever *after* the trusted base manual. Full LLM
  self-rewrite of the manual is explicitly out of scope.
- The user retains the final say via pin / edit / veto.

## Review & override

| CLI | Effect |
|---|---|
| `maz feedback gotchas` | show the active promoted set (rule, recurrence, severity, pinned, cluster) |
| `maz feedback list [--category/--severity/--surface/--since/--limit]` | raw triage rows with cluster keys |
| `maz feedback pin <cluster> ` / `unpin` | protect from / release cap eviction |
| `maz feedback veto <cluster>` / `unveto` | suppress from / restore to promotion |
| `maz feedback edit <cluster> --rule "…"` | override the distilled rule text |

Each review appends an audited `agentFeedbackReviewed` event — nothing is ever
deleted. `feedbackReview` and `feedbackQuery` are **unlisted** MCP tools: review
is a human override (not an agent action), and the agent already receives the
promoted gotchas in its session instructions, so it has no reason to call the
read tool.

## Export (shareable diagnostic)

`maz feedback export [--format json|jsonl|md] [filters] [-o FILE]` emits an
**allowlist-redacted** diagnostic. The export is a hard security contract: it
emits ONLY `{id, ts, category, severity, surface, summary, detail, keywords,
version, activePreset}`. It never emits user memory, principal ULIDs, cluster
keys, source-event ids, or team scope — the query reads the `agent_feedback`
table **alone** and never joins, so redaction is structural, not a post-filter.
(Same discipline as the SourceRef render layer, which omits `external_url` and
hashes `external_id`.)

## Events & storage

- `agentFeedbackRecorded` → `agent_feedback` (PK `uid`, `cluster_key`, `category`,
  `severity`, `surface`, `summary`, `detail`, `rule`, `keywords`, `version`,
  `active_preset`, `create_time`, `scope`).
- `agentFeedbackReviewed` → `agent_feedback_reviews` (PK `cluster_key`, `pinned`,
  `vetoed`, `edited_rule`, `last_action`, `review_time`).
- Migration `0040_agent_feedback`. Both projections are idempotent upserts, so a
  replay reconstructs identical state. The tables are NOT in the scope-enforced
  `sharedDerivedTables` set — they are read only by the dedicated `feedback`
  engine, like `source_refs`.

## Known footguns to file (and a deferred seed)

The surface is most useful once a few canonical footguns are present. These are
the obvious ones worth a `maz feedback record --category lesson` (or an
`agentFeedback` call) early in a project's life:

- A display name containing `/` is unreachable by `get` — use the bare uid.
- `defineEntityKind` runs a ~3 s semantic-dedup pass that is expected, not a hang.

**Deferred follow-up:** auto-seeding these as pinned lessons on `maz init` (so the
surface is useful on day one with zero recurrence) is intentionally NOT wired yet
— a one-time, idempotent seed hook on the genesis path is the clean place for it,
and it is held out of this change to keep the init path untouched. The dynamic
loop itself is complete; seeding is purely a convenience layer on top.
