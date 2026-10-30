# Renderer

Sibling to the sync transport (see [sync-transport.md](sync-transport.md)). Where the transport handles raw chunk bytes, the **renderer** turns the event stream into a human-readable view of the entity graph and writes it into the same git repo. Result: a reader who lands on github.com sees not just JSONL events but a styled "Rosie Cotton — works_at Erebor Inc., role CFO" page — and `git diff` between two pushes reads as semantic change ("+ entity Rosie Cotton", "⤳ role CEO → CFO"), not opaque byte diffs.

## Why a renderer, and why now

Plaintext-default sync made the chunks layer the default. That layer was already enough to give a power user a usable on-github view: read the JSONL, grep for names, run `git log` over the manifest. But three things were missing:

1. **Diffs were uninterpretable.** Two pushes are two chunk files. `git diff` shows new lines, not "the entity Rosie Cotton gained an alias" or "the relation between Rosie and Erebor was superseded".
2. **Pulls were expensive.** A receiving device replaying the sender's chunks would re-run the LLM extractor over each Episode. Extraction is non-deterministic — same Episode, different prompts, divergent Entities. The receiver's graph slowly drifted from the sender's.
3. **Casual readers had no entry point.** Someone opening the sync repo on github.com saw raw event JSONL, not a person-shaped page they could navigate.

The renderer is a small, pure package — `internal/sync/render/` — that solves all three with one mechanism: render the graph as files in the repo, then teach the receiver to recognise "the sender already extracted this" and skip.

## Architecture

```
events ─────────► render.Render(Input) ─────► View{Files, Cursor, Stats}
                       │
                       │ pure, deterministic, single-threaded
                       │ no time.Now / math.rand / goroutines
                       ▼
                  GraphState (event-driven state machine)
```

The renderer is a **pure function**. Same input always yields byte-identical output. This is the load-bearing invariant: it's what lets the receiving device re-derive the same `rendered/` tree the sender produced without coordination. It's also what makes diffing meaningful: two devices that pulled the same chunks render to the same bytes, so `git diff` over `rendered/` between two of your machines should be empty.

Determinism rules, enforced and tested:

1. The caller passes events in canonical order (`CreateTime` asc, ULID tiebreak). The renderer aborts on out-of-order ULIDs.
2. Every map iteration goes through a `sortedKeys` helper.
3. YAML frontmatter is hand-written with a fixed key order — no `yaml.Marshal`.
4. Times: `t.UTC().Format(time.RFC3339Nano)`.
5. Floats: `strconv.FormatFloat(f, 'f', 3, 64)`.
6. Slugs frozen on first sight of an entity. A rename adds to `aliases:` and updates `display_name:` but never changes the file path.
7. JSONL lines end `\n`; markdown files end `\n`.
8. No goroutines, no `time.Now()`, no `math/rand`. The `determinism_test.go` suite walks the package and refuses to compile if any leak in.

## Output layout

```
<scope-repo>/
  chunks/                                 # canonical JSONL stream, unchanged
  MANIFEST.json                           # canonical manifest, unchanged
  rendered/
    .cursor.json                          # last-rendered ULID
    entities.jsonl                        # one line per current entity, ULID-sorted
    relations.jsonl                       # one line per current edge
    superseded-relations.jsonl            # supersede history, append-only
    forgotten-relations.jsonl             # user-initiated forgets, append-only
    facts.jsonl                           # free-text facts not bound to an entity
    predicates.md                         # predicate index
    by-uid/01H/AB/01HABCDEFG...K.md       # ULID-sharded entity stubs
    by-kind/person/INDEX.md               # auto-regenerated per push
  CHANGELOG/
    INDEX.md                              # rollup, last 50 entries
    2026/06/<headULID>__<deviceSlug>.md   # per-push fragments, ULID-orderable
```

### File-layout strategy

- **Per-entity stubs** live under `rendered/by-uid/<3-char>/<2-char>/<full-ulid>.md`. The 3-2 sharding keeps each leaf dir under ~25 entities at 100 k entity scale — avoids the flat-100 k-files-in-one-dir problem (slow `git status`, GitHub UI truncation).
- **`entities.jsonl`** is the canonical machine-readable view. One line per current entity, sorted by UID. Line-level git diffs work out of the box; the renderer rewrites the file every push via the standard merge-and-emit pattern.
- **`relations.jsonl`** is similar, but mutations go through **tombstone-and-compact**: appends are O(1); supersedes mark the old line's offset as tombstoned in `.relations-index.bin` (a small binary index lazily rebuilt per daemon session). When tombstones exceed 10% of total lines, a compaction sweep emits a fresh file.
- **CHANGELOG fragments** are per-push files, named after the push's head ULID. Two devices pushing concurrently emit two different fragment files, so a fast-forward merge takes neither side's content as ground truth — no merge conflict on a single appending `CHANGELOG.md`.

## Event-kind handling

The 12 event kinds and the renderer's response:

| Event kind | Renderer effect |
|---|---|
| `episode` | Tracks provenance only; not rendered as its own file. |
| `episodeForgotten` | Removes the episode from every entity's provenance. If an entity loses all mentions → marked `status: orphaned`. The stub is kept (forgotten history matters). |
| `mention` | Adds an entry to the entity's "Mentions" section (capped to last 20). |
| `entity` | Creates the stub + emits a line in `entities.jsonl`. Slug frozen here. |
| `entityIdentifierAdded` | Updates frontmatter aliases/identifiers and the row in `entities.jsonl`. |
| `relation` | Appends to `relations.jsonl`, updates both endpoints' stubs. |
| `relationCandidatesProposed` | Not rendered (transient — resolver decides). Doctor warns if these accumulate without promotion (stuck pipeline). |
| `relationSuperseded` | Old line tombstoned in `relations.jsonl`; copied to `superseded-relations.jsonl`; per-entity stub moves old edge to "Supersede history" with strikethrough. |
| `relationForgotten` | Tombstones the line; copies to `forgotten-relations.jsonl` with reason and `forgottenAt`. Stub gets a "Forgotten" section with ⊗ glyph. |
| `predicateRegistered` | Adds to `predicates.md`; CHANGELOG row `+ predicate <name>`. |
| `supersedeConflict` | Both losing arms kept in `relations.jsonl` with `conflict_with` cross-references. Stub gains "⚠ Unresolved conflicts". Surfaces as `maz diff`'s highest-priority entry. |
| `fact` | Appended to `rendered/facts.jsonl`. |

## Extraction-skip on pull

The receiver-side dedup happens at `internal/kernel/file_eventlog.go::applyExtraction`. The mechanism:

1. The receiver records, in an in-memory set, every episode UID it sees referenced by a `Mention` event.
2. Before invoking the extractor for any event, it checks the set. If the event's UID is already there (= some Mention already attributed itself to this Episode), extraction is skipped.

This is enough because chunks are sealed with their events together on the sender side, so an Episode and its Mentions typically arrive in the same Append batch (or in consecutive batches inside the same daemon process). On the rare path where an Episode is originated locally and never gets a Mention, the extractor runs normally — the absence of Mentions is itself the signal that nothing has yet extracted this Episode.

Alternatives considered and rejected:

- **A payload flag on every Episode** — pollutes the schema, requires the resolver to set it, leaves room for divergence.
- **A chunk-header hint** — too coarse; one chunk can carry events at different extraction stages.
- **A persistent on-disk set** — the in-memory set rebuilds naturally from each batch; persistence buys nothing and adds a corruption surface.

## Transport integration

`internal/sync/transport/git/render_view.go` is the seam. After the manifest write in `writeRepoMetadata`, the transport:

1. Checks whether any blob in the current push is encrypted. If yes → skip the renderer entirely. Encrypted scopes never get a `rendered/` tree (rendered files would defeat the encryption).
2. Reads every plaintext chunk in `chunks/`, decodes events, sorts by UID.
3. Calls `render.Render(in)`.
4. Reconciles `rendered/` against `view.Files`: writes/updates/deletes files via atomic temp+rename, prunes stale files no longer in the view.

The reconcile is full — every push re-renders from the union of chunks in the working tree, not from a delta. The renderer's benchmark budget (~30 s for 100 k entities + 500 k relations cold) is the push-latency ceiling we're sizing to; PriorState-driven incremental rendering exists in the API but isn't yet wired through Transport.

### Concurrent-write story

Two devices pushing simultaneously: the transport already does `fastForward → commit → push → rollback-and-retry-on-non-ff`. The renderer extends this naturally — after `fastForward` pulls in the other device's events and chunks, the renderer re-runs against the merged event set. Output is byte-deterministic by event ULID order, so the second device produces the same bytes the first device would have produced from the merged set. No new merge-conflict surface.

## Scale envelope

| Workload | Wall-clock budget | Disk budget |
|---|---|---|
| 10 k entities + 50 k relations (cold) | < 500 ms | — |
| 50 k entities + 250 k relations (cold) | < 5 s | — |
| 100 k entities + 500 k relations (cold) | < 30 s | < 200 MB |
| 1 k changed against 100 k base (incremental) | < 200 ms | — |
| Relation tombstone compaction (500 k base, 50 k tombstones) | < 200 ms | — |

The doctor row `sync.render_size` warns at **1 GiB** total rendered tree or **200 MiB** for `entities.jsonl` — above those, github.com starts truncating inline renders and `git diff` slows noticeably. Past that, the operational path is scope rotation (start a new remote for the next slice of memory) rather than further sharding.

## What's out of scope (filed for follow-up)

- **JSON Patch (RFC 6902) output for `maz diff`** — derivable from the structured fragments; ship when a tooling consumer exists.
- **Persistent PriorState cache** for incremental push-latency — would shave most of the 30 s ceiling but adds a corruption surface; defer until benchmarks against real 100 k-entity scopes show the cost.
- **Merge-conflict UI for concurrent renames/extractions** — when two devices independently extract divergent entities for overlapping episodes, the per-push fragments compose cleanly but the materialized graph may carry parallel entities for a future resolver merge. Building a TUI for "review this concurrent rename" belongs in the M2.8 review loop.
- **Signed rendered files** — view is derived; truth lives in events; signatures on derived data add ceremony without trust gains.

## Cross-references

- [sync-transport.md](sync-transport.md) — the git transport that hosts the rendered tree.
- [event-log.md](event-log.md) — the kernel.FileEventLog that powers `applyExtraction`.
- [ontology.md](ontology.md) — the event-kind canon the renderer dispatches over.
- [storage.md](storage.md) — the four-layer storage the dispatcher hydrates from chunks (independent of rendering).
