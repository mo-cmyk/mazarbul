# Documents: the markdown / knowledge-vault layer

Documents make human-authored, human-editable markdown a **first-class citizen
of the same knowledge graph as structured memory** — not a parallel store. A
note about a person links to that *Person entity*; one search surface (coming in
P3) spans structured and unstructured alike.

This doc tracks the layer as it lands phase by phase. Today it covers **P0**
(ontology + storage) and **P1** (interop import + read + intertwining).

## The core idea: one graph, not two

A document **is an entity** (`EntityKindDocument`) whose UID is its stable
identity, plus a stream of `documentRevision` events carrying the verbatim body.
It lives in the same SQLite + FTS5 + sqlite-vec + LadybugDB the rest of memory
uses. There is no separate "document store".

Because a document is an entity, every existing read works on it for free:
`maz get entities/<docUid>`, `maz neighbors`, `maz relations`, `maz traverse`.
A document and a structured fact about the same person are two edges on one node.

## Intertwining: how `[[wikilinks]]` become graph edges

When a body contains `[[Rosie Cotton]]`, the importer resolves the target:

- resolves to an existing **Person/Org/Place/...** entity → `mentions` edge
  (`document --mentions--> entity`),
- resolves to another **document** → `links_to` edge,
- resolves to nothing → kept as an *unresolved link* and reconciled when a
  matching entity/document later appears (Obsidian's "create on click", but it
  auto-wires to whatever the link names).

Resolution is **kind-agnostic** (a wikilink may name any kind), unlike the
extractor's `resolver.matchMention` which is kind-gated on a `Mention`. Four
lanes run in order, short-circuiting on first success:

1. **Exact display-name match** — case-insensitive `ListEntities(NameExact)`.
   Ambiguous (>1 non-self hit) → unresolved.
2. **Vault-path match** — `[[notes/foo]]` resolves to the document whose path
   is `notes/foo` or `notes/foo.md`.
3. **Unique whole-word partial-name match** — a short-form link like `[[Rosie]]`
   resolves to `Rosie Cotton` when exactly one entity has the target as a
   contiguous run of whole words in its display name (case-insensitive,
   `[[Apollo Launch]]` → `Apollo-Launch Project`). Matching is token-based: both
   the target and the candidate are tokenised on punctuation/whitespace, so the
   run is compared word-by-word and punctuation differences do not matter. The
   candidate prefilter narrows on the target's **leading token** (not the raw
   string), so token-equivalent names are not dropped before the whole-word
   check runs, and uniqueness is proven over the full candidate set (an
   overflowing prefilter is treated as ambiguous rather than guessed). Deterministic
   and embedder-free: it never guesses between two candidates (an ambiguous match
   stays dangling) and never matches a sub-word (`[[Sara]]` does not match
   `Rosie Cotton`). This handles the first-name/nickname links that the embedding
   lane's conservative cosine threshold rejects, without the false positives of
   lowering that threshold.
4. **Semantic name-embedding** — embeds the target string and searches
   `entities_vec WHERE vector_type='name'` (top-2 ANN). Accepts the top hit
   only when cosine similarity ≥ 0.92 **and** the margin over the runner-up
   ≥ 0.05. Below threshold or ambiguous → link stays unresolved (dangling links
   are re-tried in the reconcile pass or by `LiveDeadLinkDocs`). An **ambiguous**
   deterministic match (lanes 1 or 3 found more than one candidate) blocks this
   lane entirely: the target already proved to name multiple entities, so the
   semantic top hit must not silently pick one.

   The semantic lane is an **optional capability** wired by `wireMemoryLayer`
   after the embedder is available. When absent (nil `SemanticNameResolver`),
   the importer and `LiveDeadLinkDocs` skip it gracefully. Embedder or
   vector-search errors are non-fatal (warn-logged, link stays dangling).
   Resolution is **synchronous** inside the atomic batch so import remains
   idempotent: unchanged content is deduped by hash before embedding occurs.
   Kind is hydrated after the ANN hit via a `kindForUID` closure to set the
   correct edge predicate without importing the resolver package into
   `internal/document`.

### Identity = one ULID

The importer mints **one ULID** per document and uses it as BOTH the
`documents.document_uid` AND the document's `EntityKindDocument` UID. The event
writer honours a caller-supplied UID, and the dispatcher keys entity rows/nodes
on the payload UID, so this unifies the document's revision stream with its
graph node. Wikilinks resolve to this UID, so they survive document renames.

### Unresolved links + reconcile (bookkeeping)

`documents.unresolved_links_json` is a **write-time snapshot** of the links the
importer could not resolve. The **relations table is the source of truth** for
what is resolved; read surfaces cross-check it. When a later import creates a
link's target, a reconcile pass re-resolves every dangling link across the vault
and emits the now-resolvable edges (cross-checking existing relations to avoid
duplicates). The snapshot is intentionally not mutated on reconcile — a stale
entry only costs a redundant existence check, never a duplicate edge. (Pruning
the snapshot is future lint/health work, P6.)

## Ontology & storage (P0)

- `internal/ontology/document.go` — `documentRevision` + `documentDeleted` event
  kinds. `DocumentRevision` carries the **verbatim** `Body`, the stable
  `DocumentUID`, `ContentHash`, parsed `Frontmatter`/`FrontmatterRaw`, the
  unresolved `Wikilinks` remainder, `Pinned`, `Tags`.
- Migration `0016_documents` — `documents` table + `documents_fts` (FTS5
  external-content) + `documents_vec` (vec0) + `documents_vec_ids` sidecar
  (per-chunk, with `heading` + `byte_offset` for section-level citations in P3).
- `dispatcher_document.go` — `applyDocumentRevision` / `applyDocumentDeleted`:
  idempotent UPSERT keyed on `document_uid`, delete-then-insert vec sidecar,
  background per-chunk embedding. Fully rebuildable from the event log.

`Body` is the **entire verbatim file** (frontmatter included) and
`ContentHash = sha256(Body)`. `FrontmatterRaw`/`Frontmatter` are derived views
(a prefix of `Body`), never used to reconstruct the file — so the P2 vault
projector is a byte-stable `write(path, Body)` and the content-hash round-trip
holds.

## Import + read surface (P1)

Parsing (`internal/document/parse.go`) is deterministic and dependency-free:
frontmatter (YAML), `[[wikilink]]` grammar (`[[target#section|alias]]` and typed
`[[relType::target]]`, skipping fenced + inline code), title (frontmatter →
first `# H1` → humanised filename), tags, content hash. Observation parsing
(`- [category] text #tag`) is deferred to **P6**.

The importer (`internal/document/importer.go`) runs a four-stage batch: dedup by
content hash → create document entities → resolve links and emit
revisions + edges → reconcile dangling links. Re-import of an unchanged file is
a no-op.

### MCP tools

| Tool | Listed? | Shape |
|---|---|---|
| `readDocument` | listed | `{path \| documentUid}` → `{document, links[], backlinks[]}` |
| `getBacklinks` | listed | `{path \| documentUid}` → `{backlinks[], links[]}` (no body) |
| `listDocuments` | listed | `{pathPrefix?, tag?, pinned?, limit?}` → `{documents[]}` |
| `writeDocument` | listed | `{path, body, title?, pinned?, tags?, frontmatter?}` → write summary + vault file |
| `editDocument` | listed | `{path, oldString, newString, replaceAll?}` (Claude-Code Edit semantics) |
| `importDocuments` | **unlisted** | `{path}` → import summary (local-fs batch; driven by CLI) |
| `materializeVault` | **unlisted** | `{path?}` → projection stats (driven by `maz doc sync` / `maz rebuild`) |

`importDocuments` / `materializeVault` are unlisted (kept off the agent tool
list) and routed through the daemon's event log so they respect the
single-writer lock; the agent surface stays focused on read/navigate/write
verbs. `writeDocument` stores the body **verbatim** (event-sourced history) and
can synthesize a YAML frontmatter block from `title`/`pinned`/`tags`/
`frontmatter` — but refuses when the body carries its own block, so there is
exactly one source of frontmatter truth per write.

### The editable vault (P2)

`internal/sync/vault` materializes documents as editable markdown files under
the vault root (`[vault] path` in config.toml; default `<dataDir>/vault`,
resolvable to an existing Obsidian vault). The projector enforces the four
invariants:

- **I1 hash idempotency** — a file is rewritten only when its on-disk SHA-256
  differs from the latest revision's `contentHash`; projecting twice is a
  byte-stable no-op (golden-tested against the real four-layer stack).
- **I2 never clobber** — a state sidecar (`.mazarbul/state.json`, path → last
  hash *we* wrote) distinguishes a stale projection (safe to advance) from a
  manual edit that was never ingested (skipped + reported as a pending edit).
  A corrupt/missing sidecar fails SAFE: unrecognized bytes are never
  overwritten.
- **I3 single writer** — one mutex serializes all projector writes.
- **I4 atomic writes** — temp file + rename; files 0600, dirs 0700.

Deletion is tombstone-driven and conservative: a file is removed only when its
document was soft-deleted AND the on-disk bytes still match a known revision.
`maz rebuild` re-materializes the vault after replay (best-effort; failures
warn without changing the rebuild exit code).

### CLI

`maz doc import <path>` · `maz doc ls [--prefix --tag --pinned --limit]` ·
`maz doc show <path>` · `maz doc links <path>` · `maz doc new <path> [--title
--pin --tag --body]` (stdin or `$VISUAL`/`$EDITOR`/vi on the vault file) ·
`maz doc edit <path>` (editor round-trip; save re-ingests verbatim) · `maz doc
path <path>` (the "use your own editor" escape hatch) · `maz doc open <path>`
(OS handler) · `maz doc sync [path]`. Commands route through the same
daemon-or-in-process tool dispatch as the navigation verbs.

### Prose extraction + frontmatter typed links (P2b)

Two deterministic lanes and one async LLM lane add graph edges beyond explicit
`[[wikilinks]]`. All three are deduped against each other and against prior
edges before appending — no duplicate `mentions` edge is ever written regardless
of which lane(s) fire.

#### Frontmatter typed links (deterministic, synchronous)

Two frontmatter keys are parsed by `parse.go` and resolved in the same
synchronous import batch as wikilinks (no goroutine, no LLM):

- **`about: <target>`** — scalar string or list, each value is a bare display
  name (`about: Rosie Cotton`) or wikilink syntax (`about: "[[Rosie Cotton]]"`).
  Section anchors (`#section`) and aliases (`|alias`) are stripped so the
  resolver sees a clean name. Multiple values become multiple `mentions` edges.
- **`entity: self`** — resolves directly to UID `"self"` (the canonical
  self-entity seeded by migration `0012_self_entity_seed`), no table lookup
  needed. Any other value under `entity:` is treated as a plain display name
  and resolved the same way `about:` targets are. This produces a
  `document --mentions--> self` edge: `self` is a valid edge **object**, not
  just a subject. The `Relation` schema accepts `"self"` in both the
  `subjectUid` and `objectUid` fields (symmetric sentinels).

Unresolvable frontmatter targets are counted in `Result.Unresolved` exactly
like unresolved wikilinks, so the import still succeeds. They join the dangling
snapshot and are reconciled when a matching entity appears.

#### Prose extraction (async, non-fatal, LLM-backed)

When a `ProseExtractor` is wired via `Importer.WithProseExtractor`, each
**changed** document (content hash differs from the prior revision) gets a
background goroutine after the deterministic batch commits. The goroutine:

1. Calls `ProseExtractor.ExtractProse(ctx, docUID, body)` — the real
   implementation is `document.ExtractorAdapter`, which wraps
   `*extractor.Extractor` and re-uses the same chat-concurrency semaphore
   (`chatSem`) as episode extraction, so Ollama is never flooded.
2. Resolves each returned `ontology.Mention` via `resolveTargetKindGated`:
   exact case-insensitive display-name match filtered by the mention's kind
   (prevents a person named "Berlin" matching the place "Berlin").
3. Checks existing edges (wikilink + frontmatter + prior prose edges) via
   `existingEdgeTargets` and skips any mention already covered.
4. Appends a batch of `mentions` Relation events for the new edges only.

Idempotency: unchanged content hash → importer marks the file as skipped →
no extraction goroutine is launched → call count stays flat. Changed body →
new goroutine → edge set converges to a superset of the wikilink set (never
shrinks existing edges; supersedes are not used here because a
removed-then-re-detected mention should remain valid as a historical edge).

The goroutine is detached with `context.WithoutCancel(ctx)` (same pattern as
`FileEventLog.applyExtraction`), so `ImportDir`/`ImportContent` return to the
caller before extraction settles. Errors are warn-logged and never propagate.

#### Wiring in the daemon

`registerNavigationTools` builds the `Importer` and stores it on `Wired.Importer`.
`wireMemoryLayer` (which runs after the embedder + chat are up) builds
`ExtractorAdapter`, sets `w.ProseExtractor`, then calls
`w.Importer.WithProseExtractor(adapter)` — which mutates the same `*Importer`
pointer all MCP tools hold, so `writeDocument`, `editDocument`, and
`importDocuments` all see prose extraction without any re-wiring.
`startVaultWatcher` is called after `wireMemoryLayer` completes so the
vault-watcher's own `Importer` instance is also wired before the first fsnotify
event fires.

### Unified recall (P3)

Documents are first-class recall candidates in the SAME ranked result set as
episodes — co-ranked, not two searches stapled together. Three document lanes
feed `HybridQuery` (`internal/kernel/query_documents.go`):

- **chunk-vec** — KNN over `documents_vec` (chunk IDs `docUid#idx`); the
  closest chunk wins per document and anchors the citation.
- **lexical** — `documents_fts` (title + body BM25), exposed through the
  optional `SearchDocuments` capability on the FTS conn.
- **wikilink-graph** — live documents whose `mentions`/`links_to` edges touch
  the query's seed entities (the entities mentioned by the top episode
  candidates), ranked by distinct seeds touched. A note about Rosie surfaces
  for a Rosie-anchored query with zero lexical overlap.

Scoring math is IDENTICAL to the episode path (same `WeightedComponents` /
`rrfComponents` / semantic gate), merged before the global
sort → recency → MMR → rerank → limit pipeline. Gating: documents participate
by default; an explicit `Kinds` filter must name `documentRevision`. The whole
lane probes for migration 0016 once and disables itself on minimal schemas
(eval no-regression: an empty doc corpus leaves episode recall byte-identical).

Document hits carry `kind: "documentRevision"`, `name: "entities/{uid}"` (the
document-as-entity — pivots straight into readDocument/neighbors/relations),
`source: {path, heading, lineStart}` (the winning chunk's section anchor),
`citations: ["events/{revisionUid}"]`, and entity anchors from the document's
wikilink edges. The composer labels document context entries
`notes/foo.md › ## Goals` and its `Citation` gained `path`/`heading`, so `ask`
answers cite `path#heading`. `maz recall` shows the vault location instead of
a UID prefix for document rows.

### Moria git tracking (P4)

Documents diff, blame, and commit as cleanly as structured memory because they
ride the existing render → commit pipeline. A document is also an entity, so its
`mentions`/`links_to` edges already render through the entity stub and
`entities.jsonl` (`kind=document`); P4 adds the one thing only the document
layer can produce — the **verbatim body**.

- **Rendered tree** — the renderer (`internal/sync/render/document.go`) emits
  `rendered/documents/<slug>.md` carrying `DocumentRevision.Body` byte-for-byte.
  This is the SAME bytes the editable vault materializes (both project the
  verbatim body, never normalized), so the two stay byte-consistent and
  `maz diff document:<path>` shows a real body diff. The slug is frozen at the
  first revision (`DocSlugs[uid]`), so a `moveDocument` (path change) never
  relocates the rendered file and orphans its git history. A tombstoned document
  is skipped, so its body file is diffed to a delete.
- **Stats + changelog** — `render.Stats` and the per-push CHANGELOG fragment
  gained `DocumentsAdded` / `DocumentsChanged` / `DocumentsDeleted`, with
  per-document bullets (`+`/`~`/`⊗`) alongside the entity/relation lines.
- **Commit messages** — the structured commit (`internal/sync/transport/git/`)
  subject gains `+N documents`; the body grows a `Documents:` section listing
  each changed document (`+`/`~`/`-` documents/{uid} path). Revision vs first
  save is read from `previousRevisionUid`; repeated revisions of one document in
  a single push collapse to one row (last write wins).
- **History** — `maz doc log <path>` lists the full revision chain straight from
  the event log (the free version history a content-addressed body earns), newest
  first, with each revision's content hash. `maz doc diff <path> [revA] [revB]`
  renders a unified body diff between two revisions (defaults to the last two;
  one arg diffs against the latest). Both are served by the unlisted
  `documentHistory` MCP tool, which reads `documentRevision`/`documentDeleted`
  events for one document UID — and still resolves the history of a *deleted*
  document via the tombstone list.
- **Git-tree diff / show / blame / github** — the Moria sync mirror holds the
  body file in git. Four commands expose it as cross-device history:
  - `maz diff document <document:target> [revspec]` — unified body diff between
    two git revisions, defaulting to `HEAD~1..HEAD`. `--format json` emits a
    stable `{uid, displayName, bodyPath, revA, revB, bodyDiff}` shape.
  - `maz show document <document:target> [--rev <rev>]` — print the verbatim body
    at any git revision. `--format json` wraps body + metadata.
  - `maz blame document <document:target>` — walk `git log --follow` for the body
    file, listing which device committed each change and when. Same output shape as
    `maz blame <entity>`.
  - `maz github url/browse --document <target>` — build the GitHub blob URL for
    `rendered/documents/<slug>.md` directly, complementing the existing
    `--entity` flag.
  - **Target syntax** — all four accept a `document:` prefix followed by any of:
    frozen slug (e.g. `document:meeting-notes__QQGAEN`), vault path matched by its
    basename (e.g. `document:notes/foo.md` resolves the document whose rendered
    slug base is `foo` — the renderer flattens bodies to `<base>__<suffix>.md`, so
    the directory is not preserved and same-basename docs surface as an ambiguity),
    UID prefix (≥ 6 chars), or display-name fragment (case-insensitive). A frozen
    slug is validated against the actual rendered filename, so a mistyped slug base
    is a not-found, not a wrong-document match. Ambiguous targets list candidates;
    unknown targets give a clear not-found error with a count of `kind=document`
    rows checked.
  - **Scope selection** — with `--scope` omitted, every configured scope's mirror
    is tried in name order and the first that resolves the target wins, so a
    document living in a non-first scope is still found.
  - **Graceful degradation** — a document that predates body rendering (pre-P4
    push) or lives in an encrypted scope that pruned `rendered/` produces a clear
    actionable error, not a panic. A mistyped git revision is reported as a
    resolution error rather than a silently empty diff.

### Context injection (P5)

A session shouldn't start cold. P5 assembles the working set into a single
**token-budgeted markdown brief**, deterministically and with no LLM in the hot
path (`internal/contextbrief/`).

#### hot.md vs index.md — two distinct artifacts

P5 produces **two vault documents** with different purposes:

| Document | Content | Budget-gated? |
|----------|---------|---------------|
| `hot.md` | Token-budgeted BRIEF: pinned → recent → entities → query-relevant. The "working-set cache" an agent loads at session open to warm up context. | Yes — never exceeds `budgetTokens` |
| `index.md` | Master vault DIRECTORY: folder-grouped listing of ALL vault documents, a recently-updated section, and a key-entity roll-up. Human-navigable navigation aid. | No — always complete enumeration |

Both are written as real, versioned vault documents via `writeDocument`
(content-hash idempotent — no new revision when content is unchanged).

#### Assembly (hot.md / getContext)

- **Priority order**: pinned documents → recently-updated documents → key
  entities → (when a `query` is given) the most query-relevant recall hits,
  co-ranked by the same `HybridQuery` that backs `recall`/`ask`. Each document
  contributes a linked bullet plus a byte-capped body excerpt; each hit a capped
  excerpt with its `path › heading` (or citation) source.
- **Token budgeting** — `tokens ≈ bytes/4`; blocks are greedily packed in
  priority order and the lowest-priority tail is dropped behind a visible marker.
  The brief **never exceeds the budget** — even the title and the marker are
  budget-gated, so an absurdly small budget degrades cleanly rather than
  overflowing. Per-section counts of what was included are returned alongside.

- **Key-entities ranking** — the "Key entities" section orders entities by the
  same signal the `entities` tool exposes as `sortBy=recallCount`: the daemon's
  in-process usage-stats store (`stats.Store.Top`, `Last7Days` window by
  default). At the `registerContextTools` wiring site a `contextbrief.StatsRanker`
  is composed from the stats store and the navigator, then installed on the
  assembler via `WithEntityRanker`. `StatsRanker.TopEntities` hydrates the
  recall-ranked UIDs from the stats store via `Navigator.GetEntity`; forgotten
  entities (ErrNotFound) are silently skipped, and the fetch is grown (deduping
  by UID, capped) so dropped rows do not underfill the section while live
  lower-ranked entities still exist. The usage signal is produced by the recall
  path itself: `HybridQuery.Recall` calls `stats.Store.ObserveTopEntity` on each
  hit's primary entity anchor, so the same counter feeds both the `entities`
  tool's recallCount sort and this ranking. Two deterministic fallbacks keep
  the section useful: (1) if the stats store is empty (cold start — no recalls
  recorded yet) it falls back to `ListEntities` (createTime DESC), and (2) if
  every stats-ranked entity has been forgotten it does the same. The
  `EntityRanker` interface is a single-method seam (`TopEntities(ctx, n)`) so
  test fakes stay trivial; `StatsRanker` is the only production implementation.
  The brief remains byte-deterministic because `stats.Top` returns items in a
  stable order (recallCount DESC, then UID ASC within a tier) and the
  fallback path inherits Navigator's `create_time DESC` ordering.

#### Assembly (index.md / getIndex)

- **Folder grouping** — all live vault documents are grouped by their leading
  path segment ("folder"). Each folder gets a `### folder` heading; documents at
  the root (no slash in path) go under `### (root)`. Per-folder lists are capped
  at 50 to stay scannable. Documents without a title fall back to their path.
- **Pinned star** — pinned documents get a ★ marker in all listings so they are
  visually prominent.
- **Deterministic** — `groupByFolder` sorts folder names with `sort.Strings` so
  two runs of an identical vault produce byte-identical output (enabling the
  content-hash idempotency check at the `writeDocument` level).
- Implemented in `internal/contextbrief/index.go` (`Assembler.BuildIndex`).

#### MCP tools

| Tool | Listed? | Shape |
|------|---------|-------|
| `getContext` | listed | `{query?, budgetTokens?}` → `{markdown, tokens, budget, dropped, sections}` — the budgeted brief (hot.md content) |
| `getIndex` | **unlisted** | `{}` → `{markdown, documents, folders}` — the vault directory (index.md content); driven by `maz context index`, not an agent navigation verb |

#### CLI

- `maz context build [--query --budget -o file]` — prints or writes the brief to
  stdout or a local file (does not persist as a vault document).
- `maz context index [--hot-path hot.md --index-path index.md --query --budget --pin]`
  — regenerates BOTH hot.md (the budgeted brief) AND index.md (the directory
  listing) as real versioned vault documents. Content-hash idempotency skips a
  no-op refresh.
- `maz context pin <path|documentUid> [--unpin]` — toggles the `pinned` flag on
  an existing vault document so it leads the brief. **Idempotent**: pinning an
  already-pinned document (or unpinning an unpinned one) returns `changed=false`
  without appending a spurious revision to the event log. The mechanism is a
  read-then-write: the body is preserved VERBATIM; when the body has its own
  frontmatter the `pinned:` key is patched in-place, otherwise `writeDocument`
  synthesizes a frontmatter block. Outputs `{path, pinned, changed, documentUid}`
  in JSON mode (default in piped/scripted contexts).

Cold-start MCP *resources* (`mazarbul://context/hot|index|pinned` and
`mazarbul://documents/<slug>`) are wired via the MCP resource capability
(`internal/mcp/resources.go`); the brief and index are also available through
the `getContext`/`getIndex` tools and the `maz context` CLI for clients that do
not auto-attach resources.

### Typed wikilink relations, observations→facts, and health lint (P6)

P6 enriches two write-path features and adds a read-only health command.

#### Typed wikilink predicates

The parser already emits `WikiLink.RelType` and `WikiLink.Confidence` for
typed prefix syntax (`[relType:0.8] [[Target]]` / `[[relType::Target]]`).
P6 wires these into the importer. A closed set of eight non-functional
predicates is registered in migration 0017:

| predicate    | meaning                                |
|--------------|----------------------------------------|
| `mentions`   | generic reference (default, doc→any)   |
| `links_to`   | hyperlink to another document          |
| `relates_to` | loose semantic relation                |
| `contradicts`| explicit contradiction signal          |
| `supports`   | corroborating/affirming relation       |
| `blocks`     | task or flow dependency                |
| `depends_on` | soft prerequisite                      |
| `part_of`    | containment / membership               |

When a wikilink carries a `RelType` that is in this set, the edge is stored
with that predicate and the stated confidence (falling back to 1.0). Unknown
`RelType` values fall back to the default `mentions`/`links_to` and are logged
at DEBUG. The forget and dedup passes cover all eight predicates, so a typed
edge is correctly superseded when the link is removed or re-typed.

#### Observations → fact events

`Parsed.Observations` (the `- [category] text #tag (context)` bullet syntax)
are projected into `KindFact` events in the same atomic import batch, with the
**document itself** as the fact subject (a vault note is an entity, and its
observations are that entity's facts — the basic-memory model). Provenance is
preserved: `source = "document:import"` and `sourceEventUid = <revisionUid>`.

Each observation produces one `Fact` keyed by a three-segment state-key,
`timeless.<category>.h<8-hex>`, where the trailing segment is a hash of the
value. The discriminator is essential: an observation is a **set-valued**
statement — a note routinely carries several `[fact]` lines — so every line of
the same category must land in its own fact slot. Sharing a single
`timeless.<category>` slot would make the supersede pipeline
(`reconcileFactSlot`) close all but the last, silently dropping every earlier
observation. Hashing the value also keeps the projection idempotent: an
identical re-assert maps to the same slot (a no-op via `writeFactRow`'s
open-slot content-hash check), while a distinct value maps to a distinct slot,
independent of source order. (The hex segment is prefixed with `h` because a
`Fact` state-key segment must start with a letter.)

The projection is visible at the read surface: `readDocument` returns an
`observations[]` array (`{category, text, tags, context}`) alongside the body,
and its description points agents at `get(entities/{documentUid})` /
`maz fact get <documentUid>` to query the projected facts.

**Known limitation (deferred):** removing an observation from a note on a later
revision does *not* close its fact slot — the supersede model only closes a slot
when a *successor* fact lands in it, and there is no fact-forget event today.
The verbatim body revision remains the source of truth; stale observation facts
are an additive projection, mirroring how prose-extraction edges reconcile
asynchronously rather than synchronously on every edit.

#### `maz doc lint`

`maz doc lint` is a read-only health command that opens the SQLite projection
and reports three categories:

- **orphans** — documents with no active inbound or outbound relation edges
  (isolated from the rest of the graph).
- **dead links** — documents whose write-time `unresolved_links_json` snapshot
  is still non-empty (wikilinks that named nothing at import time and have not
  since been reconciled).
- **contradictions** — document pairs connected by an active `contradicts`
  edge (explicit signals that two notes disagree; may need human resolution).

Exit codes: 0 = no issues, 1 = issues found, 2 = fatal error.
`--output json|ndjson` emits machine-readable arrays.

### Bidirectional file-watch (P7)

P7 makes the vault and the event log a bidirectional, loop-free fixpoint: editing
a vault file in `$EDITOR` or Obsidian appends exactly one `documentRevision`; a
revision appended externally (MCP `writeDocument`, `importDocuments`, sync)
re-materializes the file on disk — neither path triggers the other in a cycle.

#### Loop-safety invariants

| # | Name | Enforcement |
|---|------|-------------|
| I1 | Content-hash idempotency (cycle break) | `Importer.ImportContent` skips the ingest when the on-disk SHA-256 equals the stored `ContentHash`. `Projector.writeDocLocked` skips the write when on-disk hash equals the stored revision hash. Bodies are verbatim (never normalized), so the round-trip is byte-stable and the equal-hash check terminates the loop. |
| I2 | Ingest-before-render | `Projector.writeDocLocked` reads on-disk bytes first; if they do not match the projector's last-written hash (`.mazarbul/state.json`) the file is classified as a pending edit and skipped — the watcher ingests it first. |
| I3 | Single serialized writer | All `Projector` writes serialize through `Projector.mu`. The watcher's `ReconcilePath` calls `Importer.ImportContent` (which serializes through `FileEventLog`'s single-writer lock); the writeback calls `Projector.SyncPath` (which acquires `Projector.mu`). |
| I4 | Atomic writes only | `Projector.writeDocLocked` uses `writeFileAtomic` (temp-file + rename). `Watcher` watches the parent directory and filters by basename, so atomic-rename writes survive. |

#### Files

- `internal/sync/vault/watcher.go` — `Watcher` with fsnotify directory watch (the root **and** every existing subdirectory, walked at startup via `addTree`; fsnotify is not recursive), per-path debounce (200 ms), and `ReconcilePath` as the testable seam. A newly-created directory is added to the watch set **and** its already-present markdown files are reconciled (a synced/pulled dir can arrive populated before its watch installs). Debounced paths land in a path-keyed `pendingSet` drained on a non-blocking wake signal — lossless under a burst across many distinct files (the set grows by unique key, never dropping a distinct path). `ReconcilePath` self-validates `rel` (`filepath.IsLocal` + `.mazarbul` guard) before any `os.ReadFile`.
- `internal/sync/vault/writeback.go` — `Writeback` implements `RevisionNotifier`. A path-keyed pending set behind a 1-buffered wake signal feeds a coalescing ticker loop that calls `Projector.SyncPath`. `Notify` is non-blocking and lossless across distinct paths, and is a shutdown-safe no-op after `Stop` (the keyed set removes the closed-channel-send hazard entirely). `Stop` signals via a `done` channel.
- `internal/document/importer.go` — `Importer.newUID` guards the `ulid.Monotonic` entropy with a mutex; `ImportContent` can be driven concurrently (watcher fsnotify goroutine + a direct `ReconcilePath`) and two new-document imports both reach UID minting before the event-log writer lock serializes them.
- `internal/mcp/tools/document.go` — `importDocuments` re-materializes each **changed** imported document via `Projector.SyncPath` after the import, so a directory import from OUTSIDE the vault lands on disk (the non-watcher append path's editable-loop guarantee). Best-effort: materialization failures never fail the import (revisions stay durable in the event log; a later `materializeVault`/rebuild re-projects them).
- `internal/config/userconfig.go` — `VaultTable.Watch bool` (`[vault] watch = true`), **OFF by default**.
- `internal/daemon/wiring.go` — `startVaultWatcher` wired in `OpenWiring` when `user.Vault.Watch` is true; skipped for non-production dispatcher (test fakes). Watcher and `Writeback` share one `w.Projector` (one mutex, one `.mazarbul/state.json` sidecar owner: I3 holds). Closers are registered writeback-then-watcher so reverse-order (LIFO) shutdown tears the watcher down first.

#### Opt-in mechanism

Set `[vault] watch = true` in `${data_dir}/config.toml`. The watcher is off by
default — a misfiring watcher must never corrupt the vault or loop. After enabling,
the daemon starts `Watcher.Run` and `Writeback.Run` as goroutines; both are stopped
LIFO via `w.closers` on daemon shutdown.

#### Tests (`internal/sync/vault/watcher_test.go`, all `-race`)

Four loop-safety cases plus two supporting race tests, all driven via
`ReconcilePath` (not OS fsnotify events) for full determinism:

| Case | What is asserted |
|------|-----------------|
| (a) `TestWatcher_SingleFileWriteProducesExactlyOneRevision` | Single external file write → exactly one `documentRevision`; writeback SyncPath is a no-op (I1 hash-equal skip). |
| (b) `TestWatcher_ExternalRevisionRematerializesFileWithoutReIngest` | Externally-appended revision → file re-materializes byte-identically; watcher's `ReconcilePath` is a no-op (I1). |
| (c) `TestWatcher_ConcurrentAgentWriteAndFileEditLastULIDWins` | Agent write + file edit before writeback fires → I2 (SyncPath skips un-ingested edit); watcher ingests editor version as revision 2; head body is the editor version. |
| (d) `TestWatcher_CrashRestartDoesNotDoubleIngest` | Replaying `ReconcilePath` on an already-ingested file is a no-op (I1 content-hash dedup). |
| (race) `TestWatcher_RaceReconcileAndWriteback` | 8 concurrent `ReconcilePath` + 8 concurrent `SyncPath` — no data races, exactly 1 revision. |
| (queue) `TestWriteback_NotifyDoesNotBlock` | 512 `Notify` calls under a burst — never blocks. |
| (shutdown) `TestWriteback_NotifyAfterStopIsNoOp` | `Notify` after `Stop` (and double `Stop`) — no panic, no block. |
| (lossless) `TestWriteback_DrainsAllDistinctPathsUnderBurst` | 400 distinct paths (> any former buffer) all reach `SyncPath` — no drops. |
| (live, `loop_test.go`) `TestLiveLoop_SingleWriteProducesExactlyOneRevision` | Real `Watcher.Run`+`Writeback.Run` goroutines over a shared projector → exactly one revision, no rewrite cycle. |
| (live, `loop_test.go`) `TestSharedProjector_SidecarRetainsBothPaths` | Concurrent `SyncPath` for two paths through ONE projector — both survive the sidecar (two-projector regression). |
| (live, `loop_test.go`) `TestLiveLoop_IngestInExistingNestedSubdir` | A save under a pre-existing nested folder is ingested via real fsnotify (existing-subdir watch regression). |

`importDocuments` materialization is covered in `internal/mcp/tools/document_test.go`:
`TestImportDocuments_MaterializesChangedRevisions` (changed docs synced, unchanged skipped) and
`TestImportDocuments_NilProjectorSkipsMaterialize` (safe no-op without a vault).

## P2 follow-up: delete and UID-preserving move (Gap B)

Two **UNLISTED** MCP tools that close the write surface:

### `deleteDocument`

Appends a `documentDeleted` tombstone event plus `relationForgotten` events for
every outgoing wikilink edge (all 8 document predicates), in **one atomic batch**
via `Importer.Delete`. The dispatcher's `applyDocumentDeleted` handler:

- sets `documents.deleted = 1` (soft-delete; row kept for rebuild idempotency)
- strips FTS5 coverage via the AFTER UPDATE trigger
- deletes vec sidecar rows

**Edge semantics (explicit choice)**:
- **Outgoing edges** (doc → entity/doc): forgotten in the same batch as the
  tombstone. A deleted document's wikilink references are stale; removing them
  keeps `getBacklinks` and `neighbors` consistent.
- **Backlinks pointing AT the deleted doc**: left intact. They become dead links
  surfaced by `maz doc lint --dead-links`. The human authored those references
  and the system must not silently rewrite them on delete.

**Idempotency**: deleting an already-deleted doc returns `navigate.ErrNotFound`
(documents with `deleted=1` are invisible to `GetDocumentByPath/UID`).

**Vault cleanup**: `DeleteDocument.Invoke` calls `proj.SyncAll` after the
tombstone lands; `removeTombstoneLocked` removes the file conservatively (I2:
only when on-disk bytes still match the last projected hash).

CLI: `maz doc rm <path> --yes`

### `moveDocument`

Appends a new `documentRevision` at `newPath` with the **same `documentUID`**
and verbatim body, chaining `previousRevisionUid` to the pre-move revision.
Implemented in `Importer.Move`.

**UID-preserving property**: all wikilink/mention edges survive the rename
because they resolve to the document's entity UID (not the path). No tombstone
is emitted for the old path.

**Metadata preservation** (the move re-emits a revision): the dispatcher's
`applyDocumentRevision` UPSERT refreshes `frontmatter_raw`, `tags`, and
`unresolved_links_json` from the new revision. `navigate.Document` does not
expose `Wikilinks`, `Observations`, `FrontmatterRaw`, or `Tags`, so `Move`
**re-parses the verbatim body** (`document.Parse`) to recover them and
recomputes the dangling-link snapshot exactly as the importer does (resolve, keep
the unresolved remainder). Omitting these would silently blank the moved
document's dead-link snapshot — it would drop out of `DocumentsWithUnresolvedLinks`
and `maz doc lint` would stop reporting its dead links purely because the file
moved.

**Old-path vault cleanup** (the critical edge case): a plain `SyncPath(newPath)`
would write the new file but leave the stale file at the old path orphaned
forever. The solution is `Projector.SyncMove(ctx, oldPath, newPath)`:
1. Calls `writeDocLocked` for `newPath` (writes the file, records hash in sidecar).
2. **Same-path guard**: if `oldPath == newPath` (an idempotent move-by-uid
   retry), SyncMove writes/adopts the file and returns WITHOUT the removal step —
   removing the path it just wrote would delete the document. The tool layer
   still always calls SyncMove on a retry (it never skips): a prior move could
   have appended the revision and then failed during SyncMove (projection errors
   are non-fatal), so the retry re-materializes the new file as a repair.
3. **Pending-edit guard**: if `writeDocLocked` SKIPPED `newPath` because an
   un-ingested manual file already occupies it (I2 — bytes the projector never
   wrote), the moved document was NOT materialized there. SyncMove keeps the old
   file and returns without removing it, so the only projected copy of the
   document is never lost.
4. Otherwise reads the old-path file; if its bytes still match what we projected
   (I2 check), removes it and drops the sidecar entry. If bytes differ (human
   edit after the last projection), leaves the file untouched.
5. Saves the updated sidecar atomically.

**Watcher loop safety**: `SyncMove` removes the old file before the watcher can
see it; the watcher tracks the new path as a known sidecar entry (I1 hash-equal
skip on the next `ReconcilePath`). A regression test (`TestMove_VaultOldFileRemovedNewFilePresent`)
verifies old-path gone + new-path present after one `SyncMove` call.

**Rendered git tree (slug note)**: the dispatcher UPSERT in `applyDocumentRevision`
updates `documents.slug` only when no row yet exists (slug is frozen at first
revision). A move resets the slug via the UPSERT `ON CONFLICT DO UPDATE` path
(slug is NOT excluded from the update set). The rendered git tree re-derives the
slug from `newPath` on the next render pass; old rendered-path history stays in
git.

**Validation**:
- `newPath` must pass `CleanVaultPath` (vault-local, `.md` extension). All
  `CleanVaultPath` failures (absolute path, non-markdown extension, vault escape,
  empty) map to E_VALIDATION, not E_INTERNAL.
- `newPath` must not be occupied by a different document (E_VALIDATION).
- **Same path**: addressed by `path` → E_VALIDATION (a degenerate self-move).
  Addressed by `documentUid` and the UID already resolves to `newPath` →
  **idempotent success** (a retry of a move that already landed), not an error.

CLI: `maz doc mv <old> <new>`

### Test coverage

| Test | What is asserted |
|------|-----------------|
| `TestDelete_TombstoneLandsAndVaultFileRemoved` | Tombstone in log; row invisible to navigator; SyncAll removes vault file; re-delete returns ErrNotFound. |
| `TestDelete_EdgesForgotten` | After delete, active outgoing mentions edges are gone. |
| `TestDelete_ByUID` | Delete via `documentUid` resolves correctly. |
| `TestDelete_NotFoundReturnsError` | Ghost path → ErrNotFound. |
| `TestMove_UIDPreservedEdgesSurvive` | UID unchanged; edges still navigate; old path gone; new path live. |
| `TestMove_VaultOldFileRemovedNewFilePresent` | SyncMove: new file present, old file gone. |
| `TestMove_RevisionChainIntact` | New revision UID differs from original; projected `revision_uid` is the move revision. |
| `TestMove_PathCollisionRejected` | Move to occupied path → error mentioning "already exists at". |
| `TestMove_SamePathRejected` | Move to same path (by path) → error. |
| `TestMove_ByUIDIdempotentRetry` | Re-issuing a move-by-`documentUid` after it landed → idempotent success, not an error. |
| `TestMove_PreservesUnresolvedLinkSnapshot` | Moved doc stays in `DocumentsWithUnresolvedLinks` (dead-link state survives the rename). |
| `TestSyncMove_KeepsOldFileWhenTargetIsManualEdit` | Un-ingested manual file at target → old file kept, target left untouched, nothing deleted. |
| `TestSyncMove_SamePathKeepsFile` | `SyncMove(p, p)` writes/adopts but never removes → idempotent retry can't delete the doc. |
| `TestDelete_ForgetsProseExtractedEdges` | Delete forgets prose-extracted edges too (no future extraction pass to reconcile them). |
| `TestDeleteDocument_*` (tools pkg) | E_NOT_FOUND, E_VALIDATION paths + happy path + Unlisted() + reason passthrough. |
| `TestMapDeleteMoveErr_PathValidation` (tools pkg) | All `CleanVaultPath` + same-path errors → E_VALIDATION; internal errors → E_INTERNAL. |
| `TestMoveDocument_NoOpRetryStillMaterializes` (tools pkg) | Idempotent retry still calls SyncMove (repairs a vault left stale by a prior failed SyncMove). |
| `TestMoveDocument_*` (tools pkg) | E_NOT_FOUND, E_VALIDATION, collision→E_VALIDATION, Unlisted(). |

## P4: Template support and semantic near-duplicate lint

### `maz doc new --template <name>`

Extends `maz doc new` with a `--template` flag that bootstraps a new document
from an existing vault template document:

- `--template foo` resolves to the vault path `templates/foo.md`. A value
  containing `/` is treated as a literal vault-relative path (e.g.
  `--template project/weekly.md`). The `.md` extension is appended when absent.
- **Default path**: when `--template` is set and no positional `<path>` is
  given, the path defaults to `journal/YYYY-MM-DD.md` (UTC date from the
  injected clock).
- **Placeholder substitution** applied to the full template body (frontmatter +
  markdown):
  - `{{date}}` → YYYY-MM-DD (UTC)
  - `{{time}}` → HH:MM (UTC)
  - `{{title}}` → title resolved from `--title`, template frontmatter, or
    destination path basename
- **Frontmatter defaults**: the template's own `title`, `pinned`, and `tags`
  fields are parsed and used as defaults; explicit CLI flags override them.
- **Frontmatter vs. metadata reconciliation**: `writeDocument` refuses separate
  `title`/`pinned`/`tags` metadata when the body already carries a frontmatter
  block. So when the (substituted) template body has its own frontmatter, the CLI
  folds any explicit `--title`/`--pin`/`--tag` override **into that block** and
  sends the body alone (with no overrides it forwards the body verbatim); when the
  body has no frontmatter, the CLI sends the resolved `title`/`pinned`/`tags` as
  metadata for `writeDocument` to synthesize. Either way a templated write never
  trips the "body already has frontmatter" validation.
- The template body (with placeholders substituted) is passed verbatim as the
  new document's initial body. Templates live in the same vault as other
  documents — they are just regular documents under `templates/`.

Implementation: `internal/cli/doc_template.go` (helpers: `resolveTemplatePath`,
`defaultJournalPath`, `applyTemplatePlaceholders`, `readTemplateBody`,
`parseTemplateFrontmatter`, `titleFromPath`, `hasLeadingFrontmatter`,
`splitLeadingFrontmatter`, `mergeTemplateFrontmatter`). Clock is injected via
`nowFn` package variable (test seam). Tests: `internal/cli/doc_template_test.go`.

### Semantic near-duplicate detection in `lintDocuments`

`lintDocuments` MCP tool and `maz doc lint` CLI gain a `nearDuplicates`
category that detects pairs of live vault documents whose most-similar chunk
embeddings exceed a cosine-similarity threshold (default 0.92).

This is **lint-only and non-blocking**: it never prevents writes or imports.

**Query design** (`Navigator.NearDuplicateDocuments`):

1. Fetch all live documents with at least one chunk embedding
   (`documents_vec_ids` JOIN).
2. For each document, collect the chunk rowids that **actually have a stored
   `documents_vec` vector** (a JOIN, not just the sidecar) — a chunk whose
   background embedding is still pending has a sidecar row but no vector, and
   using it as the KNN query vector would make sqlite-vec reject a NULL query
   vector and abort the lint. For each embedded chunk, run a sqlite-vec KNN query
   and keep the nearest chunks from OTHER documents (own-document chunks are
   filtered in Go, not in SQL, so the window boundary is observable). Any-chunk-
   pair: if any two chunks from two documents are near-duplicates, the documents
   are near-duplicates.
3. Aggregate the minimum distance across all chunks per candidate document.
4. Convert to cosine similarity via `1 - d²/2` (valid for L2-normalised vectors).
5. Deduplicate symmetric pairs by keeping `(min(UID-A, UID-B), max(UID-A, UID-B))`
   ordering.

**Adaptive KNN window.** The per-chunk window starts at `k=50` and doubles (up to
a hard cap `nearDupMaxK=2048`) until its farthest chunk falls below the
similarity threshold — meaning no above-threshold neighbor can lie beyond the
window — or the corpus is exhausted. A fixed window silently dropped pairs once a
near-duplicate cluster exceeded it (e.g. 60 identical docs lost 45 of 1770 pairs);
the adaptive window finds every above-threshold pair in any realistic vault, with
the only bound being `nearDupMaxK` near-identical copies of a single passage.
If no embeddings have been written (no embedder configured, or all still pending),
the function returns an empty slice without error.

Implementation: `Navigator.NearDuplicateDocuments` + helpers in
`internal/kernel/navigate/documents.go`. Interface extended in
`internal/mcp/tools/document_lint.go` (`lintNavigator`). CLI output extended
in `internal/cli/doc_lint.go`. Tests: `internal/kernel/navigate/near_duplicate_test.go`.

## JSON Canvas export (#198)

`maz doc canvas <seed> [--hops N] [-o file.canvas]` exports an N-hop subgraph
rooted at `seed` as a [JSON Canvas](https://jsoncanvas.org) 1.0 `.canvas` file,
the open format Obsidian's canvas UI reads. It bridges the real graph into the
Obsidian ecosystem for free: a visual map with no TUI/web renderer to build.

- **N-hop traversal, made first-class.** The walk reuses the kernel's
  scope/validity-gated BFS. `clampHops` was generalized from `1..2` to
  `1..navigate.MaxHops` (currently 6), so the `neighbors` MCP tool, `maz
  traverse`, and `maz neighbors` all expand deeper now too (additive; depth 1-2
  still valid). `(*Navigator).Subgraph(seed, hops, opts)` rides that walk: it
  collects the node set (seed at hop 0 + reached neighbors, capped by `limit`),
  then unions **every** edge among those nodes via `EntityRelations` (sibling
  cross-links, not just the tree edge each node was reached through), deduping by
  UID and dropping self-loops and edges to out-of-set endpoints (the latter
  doubles as a scope-leak guard). Nodes/edges are sorted deterministically.
- **Serializer** (`internal/canvas`, pure, no I/O): maps a `Subgraph` DTO to JSON
  Canvas nodes/edges with a deterministic concentric radial layout (seed at the
  origin, hop rings outward, integer geometry via `math.Round`). Node IDs are the
  entity/relation ULIDs and field/sort order is fixed, so the same subgraph always
  serializes byte-identically, so the `.canvas` is git-diffable. Documents render as
  `file` nodes (the live vault `.md`); entities and unmaterialized documents render
  as `text` cards. The seed is colored distinctly.
- **Surface.** Engine is the Unlisted `exportCanvas` MCP tool (CLI-only, like
  `materializeVault`); it returns `{canvas, documents}`. `maz doc canvas`
  materializes the referenced `documents` so their file nodes resolve, then writes
  the canvas to `<vault>/canvases/<slug>.canvas` (or `-o`). Default scope is
  `{project, personal}`; `--scope project` produces a shareable canvas.

Implementation: `internal/canvas/{canvas,layout}.go`,
`internal/mcp/tools/canvas.go`, `internal/cli/doc_canvas.go`, and
`Navigator.Subgraph` + `MaxHops`/`clampHops` in `internal/kernel/navigate/navigate.go`.

## Coming next

- **P3 shipped** — semantic wikilink-resolution lane: `SemanticNameResolver`
  interface + `NewNameEmbeddingResolver` backed by `entities_vec
  WHERE vector_type='name'`; threshold 0.92, margin 0.05, synchronous,
  non-fatal, wired by `wireMemoryLayer` after the embedder is available.
  `LiveDeadLinkDocs` and the `reconcile` pass also use the semantic lane so
  previously-dangling links resolve without re-import.
- **P5 follow-up** — MCP `WithResourceCapabilities` + `mazarbul://context/*`
  resources and `maz context pin` (additive; the `getContext` tool already
  serves the brief).
- **P8** — distributed sync integration (vault files round-trip through the
  Moria chunk sealer).
