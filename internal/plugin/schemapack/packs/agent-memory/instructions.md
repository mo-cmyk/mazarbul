# Memory operating instructions

You have a persistent memory (Mazarbul) exposed through MCP tools. Use it so you
carry knowledge of the user across sessions instead of starting cold each time.
These rules tell you *when* to reach for each tool.

## Warm up at the start of a session

Call `getContext` once, early, before doing substantive work. It returns a
token-budgeted brief of the user's working set — pinned and recently-updated
documents, key entities, and (when you pass a `query`) the most relevant hits.
Load it verbatim. Prefer one `getContext` call over many small reads.

## Recall before you answer

Before answering anything that touches the user's projects, preferences, people,
tools, or past decisions, call `recall` with a short query first. The user
expects you to already know what they have told you. Do not guess or ask for
something you can look up. If `recall` returns nothing relevant, then proceed
and ask.

## Remember after you learn something durable

When the user states a stable fact about themselves or their work — a
preference, a decision, a relationship, a project constraint, a name — call
`remember` to persist it. Capture the fact in one clear sentence; include the
why when it matters. Do this as you go, not only when asked.

Remember:

- Durable facts: "uses pnpm, not npm", "the staging DB is read-only", "Ana owns
  billing".
- Decisions and their rationale.
- Stable preferences about how you should work.

Do not remember:

- Transient state, secrets, or one-off scratch values.
- Anything already obvious from the current files or conversation.

## You are the brain, Mazarbul is the memory

Mazarbul stores, indexes, recalls, and keeps provenance. The reasoning is yours.
Usually no server-side model runs, so `remember` makes the text recallable but
does not extract entities or relations from it. When you can see structure,
write it yourself:

- `entities` to find an existing anchor; `createEntity` only when none matches.
- `relate` (or `relateBatch`) for relationships: "Ana owns billing".
- `fact` for attributes and state: a project's status, a person's role.
- `addIdentifier` for aliases, emails, and external IDs.

Pass `source` on `remember`, `relate`, and `fact` (the file, URL, meeting, or
conversation it came from) so every record can be traced. Answer questions
from what `recall` and `getContext` return; do not expect Mazarbul to compose
the answer for you.

## Keep it honest

Reuse existing entities instead of minting near-duplicates — search by name
first. Record what is true now; if a fact changes, write the new version rather
than silently overwriting context. Memory is most useful when it is trustworthy.

## External capture protocol

Before you fetch anything from a live connector, call `recall` first. If the
result carries `freshness: fresh`, reuse the cached summary without re-fetching.
Only go live when freshness is `stale`, `access-unverified`, or `unknown`.

After fetching a durable artifact, call `captureExternal` once to store a pointer,
your summary, and a freshness anchor. maz never copies the body. It indexes a
searchable pointer so future sessions can recall and verify it.

Capture durable artifacts worth indexing across sessions:

- Notion pages and databases (sourceType: notion_page, notion_database)
- Linear issues and projects (sourceType: linear_issue)
- Granola meeting notes (sourceType: meeting_notes)
- Drive documents (sourceType: drive_doc)
- GitHub issues, pull requests, and files (sourceType: github_issue, github_pr)

Do not capture ephemeral, real-time sources; read those live each time:

- Gmail (email threads)
- Google Calendar (events)
- Slack (channel threads)

`captureExternal` refuses ephemeral connectors at the validation layer.

Idempotency: pass a stable `externalId` (the source-system id, not a URL) and
omit `dedupKey`; maz mints `<connector>:<externalId>:<scope>`. Recapturing the
same item must reuse the same externalId so the capture deduplicates rather than
duplicates. Include the stable id in your summary prose so `recall` can find it.

Content hash: pass `contentHash` (SHA-256 hex) over a stable subset so unchanged
items dedup within the connector TTL. Per-connector stable subsets: Linear =
`title + description + state + updatedAt`; Notion = `last_edited_time + page_id`;
Granola = transcript id (immutable after the meeting); Drive = `modifiedTime + id`;
GitHub = `updated_at + node_id`. Compute it as a real 64-char hex SHA-256 over
that subset; never fabricate or truncate it (a non-64-hex value is rejected). If
you cannot compute one, OMIT it: omitting falls back to a hash of the summary
text, which causes false revisions when wording changes without the source changing.

Freshness budgets (TTLs): notion 24h, linear 2h, granola 720h, drive 24h,
github 24h. When recall returns `stale`, re-fetch live and recapture with the
same dedupKey. When recall returns `access-unverified`, verify access and
recapture with `status=access-revoked` if still 403.

Captures land `status=proposed` (untrusted), tagged `connector:<name>`. This is
NOT the trusted posture `remember` gives. Recall surfaces an inline `trust` label
and a `freshness` block on every capture hit so provenance and age are always
visible. Verify a captured fact before treating it as authoritative; use
`promoteRelation` when confirmed.

Tombstoning: when re-fetching an item fails with 404, call `captureExternal` with
`status=deleted` (same uri, connector, and externalId); for 403, use
`status=access-revoked`. maz flags the prior capture `access-unverified` in recall
rather than dropping it silently. Never leave a stale capture un-tombstoned.
