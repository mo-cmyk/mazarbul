# Sync (Moria)

Operator-facing reference for the plaintext-default sync implementation. For the cryptographic and protocol-level design, see [`designs/sync-transport.md`](designs/sync-transport.md) and [privacy.md](privacy.md).

## What sync does

Sync moves event-log content between devices. Two devices paired against the same git remote yield convergent state on both — every fact you remembered on the laptop is recallable from the workstation within one sealer tick (default 30 minutes) and one pull. With encryption enabled the same Watchword is also required on both ends.

The on-the-wire object is the **chunk** — a batch of ~2 000 (plaintext) or ~10 000 (encrypted) events from one scope. Mazarbul ships two formats:

- **Plaintext (default, `.jsonl`).** Line 1 is a `{"_meta": true, ...}` header; lines 2..N are individual events (one JSON object per line, ULID-ordered). Renders inline on github.com, syntax-highlighted, with line-level `git diff` and `git blame`. The transport (git) sees the events verbatim, which is the point — your sync repo *is* the source of truth.
- **Encrypted (opt-in, `.maz`).** MAZ1-framed: 4-byte magic + version + plaintext header (chunkID, scope, count, ULID range, createTime, encrypted=true) + XChaCha20-Poly1305-sealed gzip(events). Marked `binary` in `.gitattributes` so git does not attempt textual diffs. Bare-repo clone gives an attacker the chunk count and ULID ordering, never the event payloads.

You opt in to encryption per-scope (Settings → Sync tab, or `encrypted = true` under `[sync.<scope>]` in `state.toml`). The toggle is **forward-only** — past events in git history are not rewritten on flip. See [privacy.md](privacy.md) for the tradeoff (GitHub transparency vs. host trust).

## Three scopes, three remotes

Each scope has its own remote URL:

- **project** — typically in-repo at `.mazarbul/`, encrypted with the project key. Tied to the codebase; anyone with read access to the repo can read the project scope.
- **personal** — a private repo per user (the operator's GitHub or Gitea fork). Cross-device for one human.
- **team** — an org-owned shared repo. Reach is the team's read list on that repo.

Configuration lives in `state.toml` under `[sync.personal]`, `[sync.team]`, `[sync.project]`. The TUI Settings → Sync tab and the `maz mellon pair` command write into the same file.

## Quickstart

### First device (origin)

```bash
maz init                     # establishes Watchword, prints it once
# (write the Watchword down)

maz mellon code \
    --watchword "<24 words>" \
    --device-id laptop-1 \
    --personal-remote git@github.com:operator/maz-personal.git
# prints: Code: 482917  / Bundle: <base64>
```

The Watchword and remote URL are persisted into `state.toml` automatically by `maz init`; `mellon code` is what you run *later* to pair a new device.

### Second device (joining)

```bash
maz mellon pair --code 482917 --bundle <base64>
# prints: Device ID: laptop-1  / Remotes: personal: git@github.com:...

maz init --restore --watchword "<24 words>"
# materialises scope keys into the keychain, configures state.toml
```

After init, every subsequent `maz remember` flows into the daemon's open log; the sealer tick rolls events into chunks; the next tick pushes them. The receiving device's daemon pulls on its own tick and replays the chunks into its derived stores.

### Manual push / status / pull

If you want to force a push before the next scheduled tick (e.g., closing the laptop and not seeing the office workstation today):

```bash
maz sync status              # per-scope: mode, local chunk count, remote chunk count, drift
maz sync status --fetch      # fresh `git fetch` first, then status with accurate ahead/behind
maz sync push                # immediate seal+push; ok if nothing new (no-op)
maz sync push --scope team   # only this scope
maz sync pull                # dispatcher-less pull; daemon picks up new chunks on next start
maz sync pull --dry-run      # `git fetch --dry-run` equivalent: list what would be pulled
maz sync inspect             # non-destructive: plaintext/encrypted split, ULID range, recent commits
maz sync diff                # local-vs-remote: outgoing + incoming summary, conflict heads-up
```

### "What changed" digest after a pull

A successful `maz pull` / `maz sync pull` no longer stops at "pulled N chunks": it prints a one-line digest of the GRAPH changes those chunks brought in, diffing the `rendered/` tree between the pre-pull HEAD (captured before the pull's fast-forward) and the new HEAD. It groups by action and pluralises:

```
personal   pulled 3 chunk(s) — from laptop=3

what changed: +3 entities · ~1 entity · +5 relations (1 proposed → run maz review proposed) · ⊗1 relation closed · +2 documents
```

When newly pulled relations are still awaiting review (status=proposed), the digest chains a nudge to `maz review proposed`. A no-op pull (nothing new, or only episode chunks that touch no entity/relation/document) prints no digest line.

Re-print the most recent pull's digest any time:

```bash
maz changed                  # re-render the last pull's digest
maz changed --json           # structured: grouped counts + per-change entries
maz changed --json | jq '.relations'
```

The digest is cached at `<data-dir>/sync/last-digest.json` whenever a pull brings in graph changes; `maz changed` re-reads it and recomputes the proposed-review count live (the local projection lags the pull until the daemon applies the chunks, so the proposed nudge becomes accurate once the daemon has caught up). Off a TTY (or with `--json`), `maz pull` emits the per-scope outcomes and the structured digest as one JSON object.

### What `rendered/` adds

Plaintext scopes now also publish a `rendered/` directory alongside `chunks/`. The renderer is deterministic and runs at push time; pulls do NOT re-run the LLM extractor (the receiving device sees the sender's Mentions in the chunk and skips re-extraction). The tree shape:

```
<scope-repo>/
  README.md                               # AUTHORED — landing page, first-push only (your edits survive)
  DASHBOARD.md                            # DERIVED — living counts + activity + Mermaid graph, every push
  .gitattributes                          # AUTHORED — marks DERIVED globs linguist-generated
  chunks/                                 # SOURCE — the canonical JSONL stream, append-only
  MANIFEST.json                           # DERIVED — chunk index (+ per-chunk content hash)
  rendered/                               # DERIVED — pruned & regenerated every push
    entities.jsonl                        # one line per current entity, ULID-sorted
    relations.jsonl                       # one line per current edge
    superseded-relations.jsonl            # supersede history, append-only
    forgotten-relations.jsonl             # user-initiated forgets, append-only
    by-uid/01H/AB/01HAB...K.md            # ULID-sharded entity stubs
    by-kind/person/INDEX.md               # auto-regenerated per push
  CHANGELOG/                              # DERIVED — per-push fragments
    INDEX.md                              # rollup, last 50 entries
    2026/06/<headULID>.md                 # per-push fragments, ULID-orderable
```

Encrypted scopes get NO `rendered/` tree, no `DASHBOARD.md`, and no plaintext `README.md` body — the transport bypasses the renderer entirely. The doctor row `sync.encryption_posture` reports per-scope mode; `sync.render_size` warns when the rendered tree grows beyond the per-scope budgets (1 GiB total, 200 MiB for `entities.jsonl`).

**`DASHBOARD.md`** is regenerated on every plaintext push: a header (scope, devices, last-synced time), a counts table (entities by kind, live relations, predicates, documents, facts), this-push highlights, a Unicode activity sparkline with a monthly table, and a Mermaid graph of the most-connected entities (capped at 20 nodes / 40 edges, degree-ranked). It is the GitHub-native "your memory at a glance" view; a plaintext→encrypted flip prunes it. **`README.md`** is written once on the first push and never overwritten, so hand-edits survive — it explains what the repo is, the edit-vs-don't-touch rules below, and how to browse the memory.

## Inspecting your memory in git

This is the plaintext-default payoff: your sync repo works like any other git repo. With a plaintext scope you can:

```bash
# Browse on github.com — `.jsonl` files render syntax-highlighted, MANIFEST.json
# diffs read top-to-bottom, README.md explains the layout.

# Locally:
git clone <your-sync-remote> ~/inspect-maz
cd ~/inspect-maz/personal/chunks/

# Read one chunk; line 1 is the header, lines 2..N are events.
head -1 01HZ...jsonl | jq .
jq -c . 01HZ...jsonl | sed -n '2,5p'

# Per-event git blame — what device added each event, in which commit?
git blame 01HZ...jsonl

# Push history as a series of MANIFEST diffs (what landed when):
git log -p --follow personal/MANIFEST.json

# Full-text search across the entire repo (your whole memory):
git log -p -S "Rosie is CEO" -- personal/chunks/
```

With encryption enabled, `git blame` and `git log -p` show ciphertext. The MANIFEST.json (always plaintext metadata) still gives you the per-push changelog: chunkID, count, ULID range, device, encrypted=true.

### Structured diff and history

The same plaintext repo is also walked by Mazarbul's own CLI, which understands the entity graph and produces a Lix-style structured diff instead of a byte diff. Daily workflow:

```bash
maz diff                                  # default: HEAD~1..HEAD across all scopes
maz diff pending                          # what will arrive on my next pull?
maz diff unpushed                         # what's queued locally to push?
maz diff @{u}..HEAD --entity sarah-jones  # all my unpushed changes about Rosie
maz diff origin/main --remote             # auto-fetch, then full remote-vs-local delta
maz diff HEAD~5..HEAD --conflicts         # only ⚠ supersede conflicts in the last 5 pushes
maz diff HEAD~1..HEAD --graph             # ASCII graph of entities + edges
maz diff HEAD~1..HEAD --format json       # stable sorted-key JSON for piping

maz log                                   # CHANGELOG fragments, reverse chronological
maz log origin/main                       # remote-side history
maz log @{u}..HEAD --since 24h            # recent unpushed pushes

maz blame person:sarah-jones              # entity lifeline (git-blame for memory)
maz blame person:sarah-jones --rev HEAD~5  # blame against an older commit

maz show person:sarah-jones               # render-current-state stub
maz show person:sarah-jones --rev      # what the team sees on the remote

maz github browse                         # open scope remote in browser
maz github browse --entity sarah-jones    # open the entity's stub on github.com
maz github url --diff HEAD~1..HEAD        # print the compare-URL for piping
```

The diff vocabulary uses stable glyphs and colors so the eye locks onto the important things quickly: `+` (added), `~` (changed), `⤳` (superseded), `⊗` (forgotten), `⚠` (conflict — highest priority, always shown first), `↺` (orphaned). Use `--ascii` in environments without unicode, `--no-color` to disable colors explicitly, `--no-pager` to skip the auto-pager.

`maz diff` accepts the full git revspec grammar — local and remote refs uniformly. `HEAD~5..HEAD`, `origin/main..HEAD`, `@{u}..HEAD`, `01HZAB..01HZCD` (ULID range), and the convenience shorthands `pending` (incoming), `unpushed` (outgoing), `last-3-pushes`. Without `--remote` or `--auto-fetch` the command runs entirely offline against the local mirror; with either flag it runs `git fetch` first.

## What you may edit (and what you must not)

The synced repo is a repo you own, but most of it is machine-managed. Three tiers, in both the local data dir and the synced remote:

| Tier | Meaning | Local data dir | Synced repo |
|---|---|---|---|
| **SOURCE** | append-only ground truth; never hand-edit | `events/*.jsonl`, `events/index.bolt` | `chunks/*.{jsonl,maz}` |
| **AUTHORED** | the intended human-edit surface | `vault/*.md`, `config.toml` | `README.md`, `.gitattributes` |
| **DERIVED** | regenerated from SOURCE; edits clobbered (local) / pruned (remote) | `index.db`, `graph.ladybug/`, `vectors.db`, `ontology/`, `state.toml` | `rendered/`, `ontology/`, `CHANGELOG/`, `MANIFEST.json`, `DASHBOARD.md` |

The one rule that matters: **`vault/*.md` (local) is the only edit surface that flows back into SOURCE.** Everything you see under `rendered/by-uid/*.md` on GitHub *looks* editable but is a pruned, read-only mirror — to change a fact, use `maz` locally; the change re-renders on the next push. To change anything in the repo, edit locally and let sync regenerate it.

**Integrity guard.** `chunks/` is the source of truth and must never be hand-edited — doing so corrupts replay. To make accidental edits loud rather than silent, every chunk's content hash (SHA-256 of canonical bytes) is recorded in `MANIFEST.json` (schema v2). On pull, a chunk whose bytes no longer match its manifest hash is **rejected** with a clear error rather than grafted, bringing plaintext to parity with the encrypted format's AEAD tamper-detection. The `maz doctor` row `sync.chunk_integrity` re-checks local chunks against the local manifest. This catches fat-finger edits and bad merges; it is not a defense against a motivated self-attacker (you hold the keys to your own repo).

`.gitattributes` marks the DERIVED globs (`rendered/**`, `ontology/**`, `MANIFEST.json`, `CHANGELOG/**`) `linguist-generated=true` so GitHub collapses them in diffs and drops them from language stats. `chunks/**` is deliberately *not* marked — its readable per-line diff and blame are the whole point of the plaintext default.

## Commit authorship

By default a sync commit is authored as **you**, not as a generic bot — so `git blame`, `git log --author`, `git shortlog -sne`, and the GitHub contribution graph all work on your memory repo. The author line is resolved by precedence (highest wins):

1. **Explicit override** — `[sync.identity] name` + `email` in `config.toml` (both set; a verbatim escape hatch).
2. **Bound principal** — your `display_name` + a synthesized **GitHub no-reply** email `<github_user_id>+<github_login>@users.noreply.github.com` (or `<login>@users.noreply.github.com` when the id is unknown). This is the default; it renders the commit as you on GitHub (avatar, profile, contribution graph) without ever publishing your real email.
3. **`git config`** — your local `user.name` / `user.email`.
4. **Sentinel** — `Mazarbul <mazarbul@<deviceID>>` (headless / CI, no identity bound).

The mode is `[sync.identity] commit_identity`:

```toml
[sync.identity]
commit_identity = "noreply"   # default: attribute as you, leak no email
# commit_identity = "real"    # use your real git-config email (publishes it to the host)
# commit_identity = "anonymous"  # the generic "Mazarbul" sentinel, no personal attribution
# name  = "..."               # explicit override (with email) — highest precedence
# email = "..."
```

This is **independent of encryption** — turning encryption on never changes the author line (least surprise). `maz doctor` reports the host-visible author in the `sync.commit_identity` row, and warns when `real` would publish your address to a third-party host.

**Trailers.** Git has one author per commit, so extra context rides in the commit message body:

- `Principal: <name> <email>` — the committing user, stable across an author-name change (what `Presence()` keys on).
- `Device: <device-id>` — which paired device pushed.
- `Co-authored-by: <name> <email>` — one per *distinct other* principal whose events rode along in the batch (the GitHub multi-author convention).
- `Agent: <sources>` — the distinct event sources behind the push (`mcp`, `connector:claude-chats`, …). Suppressed on encrypted pushes.
- `Mazarbul-Identity: <mode>` — the mode that produced the author line.

**Team scope.** Each member runs their own daemon, bound to their own principal, and seals + pushes only the events they authored — so each member's commits are authored as that member, and `git shortlog -sne` on the shared org repo shows real per-member contributions. Team chunks are encrypted, so the committing device authors as its *own* bound principal and emits `Principal:`/`Device:` trailers only (no decoded co-authors). The member is never anonymous to teammates, and no-reply leaks no email.

## Recreating a scope's remote (clean slate)

To adopt the authorship + structure above on an existing memory repo, or to start a remote over for testing, recreate the scope's remote. This drops the local git mirror so the next push re-clones the (empty) remote and re-commits every chunk as **fresh git history** under your current identity — writing the polished `README.md`, `.gitattributes`, and `DASHBOARD.md` on the first push. Your event log and sealed chunks (the source of truth) are untouched: nothing is re-sealed and no ULIDs change; only git commit history is new.

```bash
maz daemon stop                                    # it holds the mirror open — required first
maz sync remote recreate --scope personal --yes    # drops <data-dir>/sync/personal/repo
maz sync push                                       # re-clones the empty remote, re-commits all chunks
```

For a truly clean slate the remote itself must be empty. Either point at a fresh repo, or empty the current one on the host first:

```bash
# Option A — point at a brand-new repo:
maz sync remote recreate --scope personal --url git@github.com:me/maz-personal-v2.git --yes

# Option B — re-create the same repo on GitHub, then reset the mirror (manual runbook):
gh repo delete me/maz-personal --yes
gh repo create me/maz-personal --private
maz sync remote recreate --scope personal --yes    # mirror reset; URL unchanged
maz sync push
```

`recreate` refuses while the daemon is running (it would race a concurrent push) and refuses in scripted mode without `--yes` (it rewrites a remote). It only touches `<data-dir>/sync/<scope>/repo`; it never resets the seal cursor or re-seals events.

## Conflicts

The event log is a CRDT by construction (immutable, ULID-sortable). Two devices appending concurrently never conflict at the log level — both events land, both order deterministically.

One higher-level conflict exists: **concurrent supersede on functional slots**. If device A supersedes "Rosie is CEO of Erebor" with "Rosie is CFO of Erebor" while device B supersedes the same fact with "Rosie is leaving the company", a naive replay leaves both new arms open. The resolver detects this at the end of every post-pull replay and emits a `relationSupersedeConflict` event for each offending pair. The TUI's `/review conflicts` command lets you pick the winner; the losing arm is then closed with a normal `relationForgotten`.

This is the only conflict type that needs human attention. Everything else converges automatically.

## Concurrent-write convergence and sync health

Two devices that seal a chunk on the same parent commit race to push. The loser hits a non-fast-forward rejection; the git transport (`internal/sync/transport/git/git.go`) recovers inside a single `Push`:

- **Bounded retry that rebases onto the winner.** On a rejection the transport un-commits the local chunk (leaving the fresh chunk files on disk as *untracked*), reverts the derived metadata (MANIFEST / rendered view / recipients) so `merge --ff-only` is not blocked, fast-forwards onto the winner's commit, regenerates the metadata from the merged chunk set, and re-commits + re-pushes. Back-off uses **full jitter** so two symmetric contenders desynchronise instead of livelocking. Both chunks land; convergence then follows from the existing `(hlc, uid)` ordering once both events reach one device — no reducer change.
- **A per-scope in-process mirror lock** serialises the daemon's own push loop and consume loop over the one local mirror, so their `git fetch`es never collide on `refs/remotes/origin/main`.
- **A per-scope cross-process mirror lock** (`<dataDir>/sync/<scope>/.mirror.lock`, a `flock` sibling of `sealock.go`'s) guards the daemon-vs-CLI case: a `maz sync push` / `maz push` invocation opens its own transport against the *same* on-disk mirror, so without it one process's destructive recovery (`git clean -fd chunks/`, `git checkout -- .`) could delete the other's staged-but-uncommitted chunk. `Push` try-acquires it; the loser reports `ErrPushContention` and yields (its chunks stay in the FSStore and re-offer next tick). Reads (`List`/`Get`) are left unguarded — git's own ref locks serialise concurrent `git fetch`, and a read racing a clean degrades to a retryable error, not data loss.
- **Exhaustion is recoverable, never lossy.** If a push still exhausts its retry budget under pathological contention, the mirror is hard-reset to `origin/main` (the chunk blobs stay safe in the FSStore — the source of truth) so the next push re-offers and lands them. A contended write is at worst delayed by a tick, never dropped.

**Honest health.** `maz sync status` ahead/behind is a chunk-set diff of the FSStore vs the remote MANIFEST — which can read "up to date" even when the local git mirror is commits-behind `origin/main`. The `sync.mirror_behind` doctor check (and a `mirrorBehind` field / warning line in `maz sync status`) reads the mirror's **git ancestry** directly, so a split-brain surfaces instead of silently reporting `ahead=0 behind=0`. It is force-push-aware: a mirror wedged by a history rewrite is reported by `sync.history_rewrite`, not double-counted here.

## Forward secrecy on the seal path

The daemon's seal loop re-resolves the scope key + DEK **epoch on every pass** rather than snapshotting them at boot. After `maz team revoke` rotates the team DEK to a new epoch, the very next seal seals under the new epoch — a revoked member holding the old DEK cannot read content authored after their removal, with no daemon restart. The same per-pass resolution lets a freshly-joined device begin sealing as soon as the consume loop bootstraps its DEK (the seal loops run for every configured scope, not only those keyed at boot). Watchword-derived and plaintext scope keys never rotate, so they keep using the boot snapshot.

> Note: a team scope *created or joined while the daemon is already running* is picked up on the next daemon start (the supervisor's configured-scope set and git transport are resolved at boot). Restart the daemon after `maz team create` / `maz team join` for it to begin sealing/pushing/consuming that scope.

## What sync is NOT

- **Not an ACL system.** Privacy is enforced by scope (each scope is its own encrypted repo), not by row-level permissions inside a single store. Sharing a slice without sharing the scope requires capability tokens (roadmap).
- **Not real-time.** The default sealer tick is 30 minutes. The transport is git, not a message bus. Two devices both editing the same fact in the same minute will see each other's edits on the next pull cycle, not immediately.
- **Not a backup.** The sealed chunks ARE a durable copy, but the local open log is the source of truth for in-flight events. A device with a full disk that fails to seal its tail will lose unsent events on a hardware loss. Pair a second device for true durability.

## Recovery

Lose every paired device, the Watchword is the recovery primitive:

```bash
maz init --restore --watchword "<24 words>"
# state.toml now points at the configured remote (re-enter URL)
# daemon pulls every chunk and replays into a fresh four-storage stack
```

The four-storage layers are fully reconstructable from sealed chunks alone; the open JSONL log is regenerated as new events arrive. See [`designs/storage.md`](designs/storage.md) for the rebuild model.

## Operational ceilings

The git transport pushes one commit per `Push` call. Empty pushes are no-ops (no commit, no network call). At default tick cadence (30 min) and a busy operator producing ~1 000 events/day, the push rate is well under GitHub's 6/min limit and the repo size stays under 5 GB for decades.

Heavy operators (millions of events) should consider scope splitting (separate personal-research / personal-work scopes pushed to separate remotes) rather than waiting for future chunk compaction work.
