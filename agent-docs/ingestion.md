# Ingestion

Bootstrap ingestion is the first-hour-value engine: import a user's existing scattered context (chats, emails, documents, code) into the event log with provenance, dedup, and budget control. Detailed design lands in M1.

## Sources

The ingestion pipeline targets these sources at M1:

- Claude.ai conversation exports (JSON)
- ChatGPT conversation exports (JSON)
- Local codebases (filesystem walk + git history)
- Gmail (via Gmail MCP connector)
- Slack (via Slack MCP connector)
- Notion (via Notion MCP connector)
- Linear (via Linear MCP connector)
- GitHub (issues, PRs, comments via gh API)
- Google Drive (via Drive MCP connector)
- Local documents (PDF, Markdown, Office formats) parsed by Docling
- Browser history (SQLite read from Chrome/Firefox/Safari profiles)
- Meeting transcripts (Otter, Granola, Read.ai exports)

## Tiered extraction

To stay within budget, ingestion runs in two passes per source:

1. **Cheap pass.** Local heuristics: regex for emails, URLs, dates; YAKE / RAKE for keyword candidates; filename and metadata extraction. Produces low-confidence candidate facts and chunks the source into vector-index-ready segments. No LLM calls.
2. **Expensive pass.** LLM-driven entity resolution, relation extraction, summary, embedding. Gated by per-source budget caps.

The cheap pass is enough for full-text + vector search to work. The expensive pass enriches the graph and the entity registry.

The expensive pass needs a server-side chat model, which is off by default (`defaults.chat = "none"`). In the default setup the calling agent is the extractor: it reads the source over its own connectors and writes entities, relations, and facts through the MCP tools with `source` provenance. Server-side extraction matters only for agent-less ingestion (file watching, connector runs, background consolidation); enable it with `maz init --provider anthropic|ollama`.

<!-- TODO(m1): expand: chunking strategy, dedup via content hash, parser plugin slot, parallelism caps. -->

## Budget gates

Each ingestion run declares a budget (`--budget=10` for ten cents, `--budget=1usd`, `--budget=unlimited`). The expensive pass tracks running cost via the chat provider's per-token pricing table; when the budget is exhausted, the pass writes a resume marker and exits cleanly. The cheap pass always completes.

<!-- TODO(m1): expand: cost estimation per source type, budget split across passes, alert thresholds. -->

## Dry-run + resume

- `maz ingest <source> --dry-run` reports cheap-pass candidate counts and expensive-pass estimated cost. No writes.
- `maz ingest <source> --resume` picks up from the last resume marker. Idempotent on content hash; re-running a fully-ingested source is a no-op.

## Watchers + webhooks

For ongoing sync (after the initial bootstrap), each connector exposes one or both:

- **Watchers**: filesystem watchers (fsnotify), database polling (SQLite incremental queries), or API polling on cron.
- **Webhooks**: HTTP endpoints that connectors register with external services (GitHub webhooks, Slack Events API). `maz mcp serve` exposes a webhook receiver behind capability tokens.

<!-- TODO(m1): expand: watcher framework in `internal/ingestion/watch/`, webhook security model, idempotency keys, replay protection. -->

---

## Local-git connect-and-ingest (`internal/ingest`, shipped — #211)

The first concrete pull-based source. `internal/ingest` is the **pull-based ingestion
abstraction**: a narrow `Source` contract plus an orchestrator that owns the *universal*
half of connect-and-ingest, so every future source (Gmail, Slack, Notion — #212) reuses
the same dedup, provenance, watermark, bounding, and anti-leak machinery. It feeds the
**existing** extractor + resolver; it does not re-implement extraction or correlation.

### The `Source` contract

```go
type Source interface {
    Name() string                                   // "local-git"; tags Event.Source as "connector:local-git"
    Streams() []string                              // e.g. ["commits", "pull-requests"]
    Validate(ctx) error                             // config check WITHOUT pulling (powers dry-run / consent)
    Pull(ctx, stream, wm Watermark, limit int) (Batch, error)
}
```

`limit` is a **parameter, not config**, so backfill bounding stays at the job layer (no
change to `extractor.Config`). A `Watermark` carries an opaque, Source-defined `Cursor`
(JSON) plus a monotone `Sequence`; a `Batch` returns the items, the advanced cursor, and
`Done`.

### The orchestrator (`Orchestrator.Run`)

Per stream, until `Done` / bounds / cancel:

1. Load the persisted `Watermark` for `(connector, stream)` from `connector_watermarks`
   (missing → zero = first connect / full backfill).
2. `Pull` a slice; **validate the cursor** (`ontology.ValidateCursor`: size cap, JSON-object
   shape, strict monotonicity). A bad cursor **FAILs the job** — no silent reset, no over-pull.
3. Per item: **dedup** by `source_refs.content_hash` keyed on `SourceRefUID(dedupKey)`
   (unchanged hash → skip; changed hash → new revision); **scrub** secret-shaped strings;
   **prompt-fence** the body; normalise into an `Episode`; emit the episode and its
   `SourceRef` in **one atomic `Append`** (the SourceRef references the pre-minted episode UID).
4. **Checkpoint**: after each slice, append a `connectorWatermarkAdvanced` event (the resume
   point). The dispatcher UPSERTs `connector_watermarks` only when `excluded.sequence >` the
   stored sequence, so `maz rebuild` converges to the highest watermark idempotently.

**Bounds** (`MaxItems`, `MaxExtractionCalls`, `SliceSize`) cap the backfill at the **job
layer**. `dryRun` validates + counts would-be items and writes nothing (the consent
preview). Cancellation (`jobs.Registry.Cancel`) ends the run gracefully with partial work
preserved. **`ScopeProject` is hard-refused** — ingested external content is sensitive and
project scope is plaintext/public.

> The `git log --reverse -n N` gotcha: git applies commit limiting *before* `--reverse`, so
> `-n` would select the NEWEST N commits and skip the oldest on a forward backfill. The
> commits stream fetches all new commits oldest-first and slices in Go instead.

### `LocalGitSource`

- **commits** stream — `git log --reverse` (scrubbed env, à la `internal/cli/doc_git.go`,
  so a leaked `GIT_*` can't corrupt the real repo); `DedupKey = "git-commit:<sha>"`
  (SHA is the content address, so dedup is globally correct).
- **pull-requests** stream — owner/repo parsed from the `origin` remote (github.com only);
  PR title+body via a tiny injectable GitHub REST client. An optional `github.token` from
  the `secret.Provider` chain lifts rate limits / unlocks private PRs; its absence is fine
  (public repos ingest unauthenticated). A non-GitHub origin soft-skips the PR stream — it
  never fails the whole job.

The watermark key is `(connector, stream)`, so v1 tracks **one local repo per connector**;
named source instances (multiple repos / remote sources) are #212.

### Untrusted-input hardening

External bodies are untrusted, so every ingested fact is structurally distrusted:

- **Scrubbed** of secret-shaped strings (`internal/ingest/scrub.go`) before the event log.
- **Prompt-fenced** as DATA (`WrapUntrusted`) so an embedded "ignore previous instructions"
  payload is framed, not followed (defence-in-depth — the structural gates below are authority).
- **Confidence-capped** at `ontology.UntrustedConfidenceCap` (0.5).
- Tagged `Source: "connector:local-git"`, which the resolver reads (`episodeUntrusted`) to:
  write relations with `status='proposed'`, **never supersede** an `asserted` (user) fact,
  and **never mint** a new entity kind or predicate.

A `proposed` edge stays quarantined until a human verifies it. `maz promote relations/{uid}`
lifts one edge; `maz review proposed` is the **reviewer queue** over all of them: it lists every
`status='proposed'` relation grouped by the connector that proposed it (with confidence + date),
scope-isolated and fail-closed (a team proposal never appears in a personal review). In a terminal
it opens an interactive multi-select; piped or with `--json` it prints the queue and exits without
prompting. `--approve-all` promotes every listed edge in one shot, each in its own scope; pass
`--connector connector:<name>` to narrow. Approval routes through `promoteRelation`. Rejecting a
proposal is not yet persisted (no `relationRejected` event); a declined edge simply stays proposed,
and `maz forget` retracts one outright.

### Anti-leak posture

The `connector:` source prefix (`ontology.IsConnectorSource`) is the *single* boundary shared
by the leak gate and the trust gate, so they can never drift:

- `privacy.IsSensitivePayload` flags connector-sourced events → the chunk sealer
  (`chunk.EncodeWithOpts`) fail-closes with `ErrSensitivePayloadPlaintext` rather than write
  them into a plaintext scope, and the git render path omits them.
- Default scope is `personal` (encrypted); `project` is refused.
- `maz doctor` carries two complementary anti-leak checks:
  - `sync.sensitive_payload_requires_encryption` — a **runtime scan**: FAILs if a plaintext
    scope already holds a connector-sourced chunk (e.g. pulled from a remote).
  - `sync.connector_scope_requires_encryption` — a **config-load** check: FAILs if a scope
    that has ingested connector content (read from `connector_watermarks`) is configured as a
    plaintext remote. This catches the local "default plaintext device" case the runtime scan
    cannot — the seal gate keeps connector content out of plaintext chunks, so the scan never
    sees it, yet the operator's content silently can't sync until they encrypt the scope.

### MCP tools + CLI

- `connectSource` — connect + first bounded backfill (onboarding). `dryRun` is the consent preview.
- `ingestSource` — delta from the persisted watermark (incremental). Idempotent by content hash.
- `listSources` — registered connectors + per-(connector, stream) resume watermarks.
- `getSourceRef` (#207) reads ingested provenance back; no new read tool.

CLI mirrors them: `maz source connect <connector> --repo <path> [--scope] [--dry-run]
[--max-items N] [--max-extraction-calls N]`, `maz source ingest <connector> --repo <path>
[...]`, and `maz source list`. All route through `invokeNavTool` (daemon MCP transport, or
in-process fallback) like the rest of `maz source`.

```bash
maz source connect local-git --repo . --max-items 50 --dry-run   # preview
maz source connect local-git --repo .                            # backfill
maz source ingest  local-git --repo .                            # later: delta
maz source list                                                  # what's connected
```

---

## Remote-source connectors: Granola + Claude-chats (`internal/ingest`, shipped — #212)

#212 adds the **remote half**: two more `Source` implementations on the #211 spine. The
headline property is that **#212 inherits every control for free by being a connector
source**. The orchestrator already scrubs, prompt-fences, confidence-caps, dedups, emits the
`SourceRef`, advances the watermark, and bounds the job for *any* source whose `Name()` tags
events `connector:<name>`. A new `Source` re-implements none of that — it is a thin fetcher.
The leak gate (`privacy.IsSensitivePayload`), the trust gate (`resolver.episodeUntrusted`),
and the prompt fence all key off `ontology.IsConnectorSource(ev.Source)`, so Granola and
Claude-chats content is sensitive, distrusted, and fenced automatically. There is **no new
migration, no new MCP tool, and no new privacy marker** for #212.

### `ClaudeChatsSource` — offline file import (`claude-chats`)

Imports a Claude **account-data export** (Settings → Privacy → Export data emails a `.zip`
within ~24h; the bundle holds `conversations.json`). One `Source` over a deterministic local
file — no network, no secret, no rate limit.

- **Acquisition:** `maz source import claude-chats <path>` (`.zip` bundle **or** a bare
  `conversations.json`). Re-importing the same export is a no-op; a newer export ingests only
  the new turns.
- **Granularity:** one `Item` per message turn (`DedupKey = "claude-chats:<convUUID>:<msgUUID>"`),
  so a long conversation never truncates against the 16 KiB body cap.
- **Watermark = export snapshot.** The export is a full snapshot, not a feed, so the cursor is
  `{"snapshot":"<fileSHA>","offset":N}`. Within one import `offset` paginates to honour
  `limit`; when a persisted cursor's `snapshot` ≠ the current file's hash the offset resets to
  0 and the file is re-scanned. **SourceRef dedup carries idempotency** — already-seen turns
  skip by content hash, so a re-scan only appends genuinely new turns.
- **Untrusted-file hardening** (the export schema is **unofficial** — third-party-corroborated,
  no published contract): `archive/zip` with a **zip-slip guard** (absolute / `..` entry names
  rejected), **decompression-bomb caps** (max archive bytes, max `conversations.json` bytes via
  a `readCapped` overflow check, max entry count), and a **LOUD structural drift guard** — the
  top level must be a JSON array; a conversation missing `chat_messages`, or a non-empty
  conversation that yields zero parseable turns, fails the import rather than silently dropping
  data. Unknown fields are tolerated (forward-compat); both the typed `content[]` blocks and
  the older flat `text` field parse; non-text (image-only) turns are skipped.

### `GranolaSource` — official public API (`granola`)

Pulls meeting notes + transcripts from the official Granola public REST API
(`https://public-api.granola.ai/v1`) with a first-party Bearer API key. **No OAuth.**

- **Acquisition:** minting a key requires a Granola **Business/Enterprise** plan. Store it with
  `maz source add granola` (keychain-only — see below), then `maz source connect granola` /
  `maz source ingest granola`. A missing key is an actionable error pointing at `add`; a
  rejected key (401/403) surfaces the honest "requires a Business-plan API key" precondition,
  not a raw status.
- **Granularity:** one `Item` per note (`DedupKey = "granola:<not_id>"`,
  `SourceType = "meeting_transcript"`); the body is title + AI summary (markdown preferred) +
  the diarised transcript.
- **Watermark = `created_after` high-water.** The cursor is
  `{"createdAfter":…,"page":…,"highWater":…}`: `page` is the opaque within-run pagination
  cursor; on drain `createdAfter` advances to the newest `created_at` seen, so the next run is a
  cheap delta. The sequence advances by **notes consumed**, not items emitted, so a page of
  un-transcribed (404-skipped) notes still makes monotonic progress. Note edits are not
  re-ingested (a documented `created_after`-vs-`updated_after` limitation).
- **Rate limits:** the client honours a `Retry-After` header when present, else backs off
  exponentially (sized to the published 5 rps / 25-per-5s budget) and gives up after a bounded
  number of 429s. A 404 is an un-summarised note — skipped, not a drift error. Nullable
  `title` / `summary_markdown` are tolerated.

### Credential lifecycle (keychain-only)

A connector API key lives in the OS keychain (or the `MAZ_SECRETS_DIR` FileProvider) **only** —
never config, the event log, `rendered/`, sync chunks, or logs. The canonical name is
`granola.api_key` (env override `MAZ_SECRET_GRANOLA_API_KEY`, read-only, CI-only).

- `maz source add granola` reads the key (`--api-key`, `--stdin`, or a no-echo prompt), verifies
  it against the live API (skip with `--no-verify` for offline / CI), and stores it via the
  write-targeted `secret.PersistentChain()`.
- `maz source revoke granola` deletes the key from the persistent store, then **proves it is
  gone on the read path**: it re-reads via `secret.DefaultChain()` and, if a value still
  resolves, **fails loudly** naming the `MAZ_SECRET_*` variable to unset. Deleting the keychain
  entry alone would leave a shadowing env override live — the read layer wins — so the
  verification is the whole point of the verb.

### Up-front plaintext-scope refusal

The leak gate refuses connector content at *seal time*, which would leave events stuck pending
on a plaintext device. So `connectSource` / `ingestSource` (and therefore `maz source
connect/ingest/import`) **refuse before any pull** when the target scope is configured to sync
as plaintext (a remote is bound to the scope and encryption is off), turning a silent stall
into a clean, actionable error. The standing-state case is still covered by `maz doctor`
(`sync.connector_scope_requires_encryption`).

### Deferred: OAuth lifecycle (#212 Sub-PR C — designed, NOT built)

Neither v1 source needs OAuth (Granola = first-party API key; Claude-chats = file import), so
OAuth-the-infrastructure is a **documented, deferred** sub-issue, built only behind a named
live OAuth provider (Drive / Gmail / Slack) — building it now would be speculative dead code.
When it lands it is a reusable `internal/oauth`: RFC 8252 loopback-redirect core, **mandatory
S256 PKCE**, a single-blob keychain entry `oauth.<provider>.<account>` with atomic
refresh/rotate/revoke, **RFC 7009 revocation first**, and a mandatory adversarial loopback test
suite (wrong / replayed `state`, intercepted code, `redirect_uri` mismatch, second-process
race; code / state / tokens never logged). `maz source auth [--device]` ships with that work,
not before.
