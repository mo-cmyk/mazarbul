# Privacy

Privacy in Mazarbul is enforced by **scope**, not by ACL inside a single store. Each scope is its own event log and its own git remote. Sharing happens by pushing a scope to a different remote; revoking happens by rotating the scope's keys.

## Three scopes

| Scope | Where | Default mode | Who can read |
|---|---|---|---|
| `project` | In-repo at `.mazarbul/` (or a project-bound remote) | plaintext | Anyone with read access to the repo |
| `personal` | Private GitHub repo per user | plaintext (encryption opt-in) | The user, on any device they paired (+ git host) |
| `team` | Org-owned GitHub repo | plaintext (encryption opt-in) | The collaborator list on that repo |

A fact lives in exactly one scope. Cross-scope reads are explicit (queries declare which scopes to traverse).

## Read-side scope isolation (the local projection)

At-rest isolation is owned by the per-scope event logs and encryption above. The
local SQLite **projection** — a cache rebuilt from those logs — flattens all
scopes into one store for fast hybrid recall, so the read spine enforces scope at
query time instead (#206).

- **Default read span is `{project, personal}`.** Recall, `getContext`, and graph
  traversal no longer span every scope: `team` (and any future scope) requires
  explicit opt-in. Pass the `scopes` array param on the MCP read tools (`recall`,
  `getContext`, `get`, `neighbors`, `relations`, `entities`) or the repeatable
  `--scope` CLI flag; an unknown value is rejected with `E_VALIDATION`.
- **An unset or empty scope set fails closed** — it returns nothing, never
  everything. Two layers guarantee this: (1) the read spine carries a `ScopeSet`
  capability you cannot construct without naming scopes (`navigate.Navigator.WithScopes`,
  per-request scope on the recall lane), so an empty set emits a `scope IN (NULL)`
  predicate that matches no rows; (2) a build-gating trace test
  (`internal/kernel/scope_trace_test.go`) records every SQL statement the spine
  submits and fails if any read of a shared derived table
  (`entities`/`relations`/`facts`/`episodes`/`mentions`/`documents`) runs without a
  `scope` predicate. SQLite has no row-level security; this is how it is emulated.
  **A new read path must be exercised by that trace test or it can leak silently.**
- **Scope lives on statements.** Each relation / fact / episode / mention /
  document row carries its source-event scope (migration 0027 adds the `scope`
  column, written from `event.Scope` at projection). An entity surfaces in a scope
  when it has an in-scope statement. The entity's own `scope` column is a backstop
  only for the bare-entity listing path; a multi-scope bare entity is
  last-writer-wins there — a documented limitation (the per-`(entity, scope)`
  junction table shipped with team scopes in #213; the last-writer-wins caveat
  for the bare-entity listing path remains).
- **Documents are scope-aware on the write surface too.** `writeDocument` (and
  `maz doc new`) take a `scope` (`project`|`personal`|`team`|`draft`, default
  personal, `team` needs membership else `E_PERMISSION`); a document IS an
  entity, so the import stamps that one scope on the document row, its companion
  entity row, and every wikilink/observation edge alike. Document uniqueness is
  the composite **`(path, scope)`** (migration 0055): the same vault-relative
  path names one document per scope, and scope is **immutable at creation** —
  `editDocument` preserves it, and re-writing a path under a different scope
  mints a separate document rather than re-homing the old one. The editable
  vault mirrors this on disk, **partitioned as `<vault-root>/<scope>/<path>`**,
  so two same-path documents in different scopes can never clobber each other
  (and `draft` documents stay physically separate). The cross-document reconcile
  pass stamps each reconciled edge with *that document's* own scope, never the
  triggering batch's — a personal write can make a project doc's link resolvable
  without leaking a personal edge onto it.
- **The `query` SQL tool is scope-enforced.** It accepts a `scopes` parameter
  (default `{project, personal}`; pass `scopes:["team"]` to include team rows).
  Before the caller's SQL runs, the tool creates scope-filtered TEMPORARY views
  that shadow `v_entities`/`v_relations`/`v_facts`, so out-of-scope rows are
  removed *before* the query sees them — no `WHERE scope IN (...)` clause is
  needed or sufficient (a `WHERE scope='team'` against the default views returns
  nothing because the shadow already excluded team). An empty scope set filters
  to `NULL` (fail-closed), and the SQL validator rejects schema-qualified
  (`main.v_entities`), base-table, and `sqlite_master` reachouts. `count` takes
  the same `scopes` parameter.
- **Maintenance reads opt into all scopes explicitly.** `navigate.New` now
  defaults to `DefaultReadSet` (`{project, personal}`); the audited wide paths —
  rebuild verification, self/identity resolution, the doctor checks, the
  nav-tool base, `getEntityContext`, the hybrid anchor, and the vault watcher —
  call `navigate.NewAllScopes` and are each intent-commented. Full reads are
  named, never defaulted.
- **Quality caveat (follow-up, not fixed here):** the vec / FTS candidate lanes
  generate top-K unfiltered and the out-of-scope hits are dropped at the scoped
  hydration chokepoint. An out-of-scope-heavy top-K can therefore starve in-scope
  hits; the follow-up is to push scope into candidate generation. The spatial
  `Near` lane (`geo.NearbyEntities`) rides the same scoped chokepoint and is not
  yet covered by the trace test.

## Local daemon trust boundary (loopback TCP, unauthenticated)

The on-device boundary is **single-user, not single-process**. The daemon's primary transport is **unauthenticated loopback TCP `127.0.0.1:7842`** (`internal/cli/preflight.go`) — the CLI and most MCP clients dial it, and any local process running as the same user can too. The Unix-socket transport is chmod'd `0600` (see `agent-docs/tech/mcp-go.md`), but that mode bounds only that transport; it is not the boundary the CLI uses. So the local trust boundary is the OS user account, not the daemon.

This matters most for **Channel A ambient context** (#214): the MCP `initialize` instructions land in the agent's **system prompt** — the highest-trust sink. Because untrusted content (installed-preset bodies, document excerpts, recall-hit text) can reach that sink, it is markdown-escaped and wrapped in a non-spoofable untrusted-content fence inside `contextbrief.Assembler` before composition. Ambient context refreshes **per session** (a new connection re-reads current state); mid-session push is structurally impossible over MCP. See the channel map in `agent-docs/tech/mcp-go.md`.

## Encryption at rest — opt-in (default: plaintext)

**Plaintext by default:** chunks pushed to a sync remote are **plaintext JSONL**. This is the explicit, deliberate default: it means your Mazarbul GitHub repo works like any other repo — line-level `git diff`, `git blame` on individual events, full GitHub web-UI inspection, `git log -S "some text"`. The Mintlify analogy: editing memory on your local device is your "editor," pushing to git is your "preview," and the repo *is* the source of truth.

**Encryption is the opt-in.** Per-scope, off by default. Turn it on (Settings → Sync, or `encrypted = true` under `[sync.<scope>]` in `state.toml`) when you cannot trust your git host with the plaintext contents — a third-party host you're skeptical of, a shared org repo where you want only paired devices to read, etc. The cost is real: once a scope is encrypted, GitHub renders the chunks as opaque binary blobs. No diffs, no blame, no web-UI search.

The toggle is **forward-only either way**:

- Plaintext → encrypted: future seals are XChaCha20-Poly1305 sealed; **previously-pushed plaintext stays in git history**. You cannot un-publish a leak by re-encrypting.
- Encrypted → plaintext: future seals are JSONL; old `.maz` chunks remain encrypted on disk and continue to be readable by the daemon (it carries both modes).

When at least one scope is encrypted, the on-the-wire object is the **sealed chunk**: ~10 000 events from one scope, gzip-compressed and XChaCha20-Poly1305-encrypted under that scope's key. The transport — git in v0.x — sees ciphertext only. The open JSONL log on the local device always remains plaintext for fast reads. Keys are held in the OS keychain (Keychain on macOS, Secret Service on Linux, Credential Manager on Windows).

**Cipher:** XChaCha20-Poly1305 AEAD via `golang.org/x/crypto/chacha20poly1305`. 24-byte random nonce per seal; 16-byte authentication tag. Implemented in `internal/watchword`.

**KDF:** Argon2id at OWASP-2025 parameters (`m=64 MiB, t=3, p=1`). Inputs: the BIP-39 mnemonic seed (root) and a domain-separated `info` string per scope (`mazarbul:scope:<scope>:v1`) so a leaked personal-scope key cannot decrypt team-scope chunks.

**Derivation tree:**

```
Watchword (24 words, 256-bit entropy)
    └─ Argon2id (m=64MiB, t=3, p=1, salt="mazarbul:root:v1")
        └─ root key (32 bytes)
            └─ HKDF-SHA-256 (info="mazarbul:scope:<scope>:v1")
                └─ scope key (32 bytes, used directly as XChaCha20 key)
```

**Key rotation:** rotating a scope means generating a new scope key, sealing all future chunks under it, and instructing collaborators to re-pair. The old key is kept in the keychain so historical chunks remain readable. Full forward-secrecy rotation (re-sealing the history) is roadmap work.

## `maz mellon` pairing

`mellon` is the Tolkien-flavoured pairing command — air-gapped on purpose, no TCP listener required. The flow:

1. On the existing device: `maz mellon code --watchword <mn> --device-id <id> [--personal-remote <url>] ...` prints a 6-digit pair code and a one-shot sealed bundle (~1 KB base64).
2. The operator transmits the code and bundle through **two different channels** (one read aloud, one pasted; Signal + AirDrop; etc.). A single-channel transcript exposes both to interceptors.
3. On the new device: `maz mellon pair --code <c> --bundle <b>` decrypts and prints the recovered Watchword, RemoteURLs, and DeviceID.
4. The new device then runs `maz init --restore --watchword <mn>` to materialise scope keys into its keychain and join the sync mesh.

**Bundle cryptography:** 16-byte random `bundleID` + sealed payload. The pairing key is Argon2id(code, salt=`mellon:v1:`+bundleID, m=64MiB, t=3, p=1). The payload is XChaCha20-Poly1305-sealed via the same `watchword.Seal` primitive used for chunks. Six-digit codes give 10^6 brute-force candidates; the Argon2id cost (~250 ms/guess on a 2024 laptop) keeps offline attacks slow without harming UX.

**Single-use semantics:** codes are not network-bound, so the "60-second TTL" is enforced by the producing device deleting the bundle file after a successful pair. The salt randomness (bundleID) ensures two bundles produced with the same code derive distinct keys.

## If you regret pushing plaintext

Once a plaintext chunk has been pushed to a third-party git host (github.com, etc.), **treat it as published**. A force-push that rewrites history does not actually un-publish: the host may keep dangling commit objects, GitHub's `/events` API exposes recent pushes, and any clone fetched between push and rewrite still holds the data.

The recommended forward path:

1. **Stop the bleed.** Flip the leaking scope to `encrypted = true` (Settings → Sync, or `state.toml`). Future seals will not add more plaintext to the repo.
2. **Rotate the scope.** Create a fresh remote URL for the scope (a new GitHub repo). Configure `state.toml` to point at the new URL. The next push goes there; the old leaked repo remains in whatever state the host's history allows.
3. **Optionally, delete the leaked repo** through the host's UI. This is the most realistic "un-publish" you can get — the host removes the public surface; copies in clones / archives obviously persist.

Mazarbul does not ship a `redact-plaintext` destructive command. The design decision is deliberate: a "delete it from history" button reads as a privacy guarantee it cannot deliver, and the scope-rotation path above is both safer (no `git filter-repo` dependency) and more honest about what the threat model actually is.

### Reviewing what's already published

`maz sync inspect [--scope NAME]` is the non-destructive companion: it walks each scope's local mirror, separates plaintext from encrypted chunks, reports the plaintext ULID range and on-disk byte footprint, and lists the last few plaintext commits in the local mirror with their SHAs. The command never mutates the remote — it answers "what does my git host actually see?" so the operator can make an informed rotate-vs-tolerate call.

Use `maz sync inspect` before you escalate to scope rotation. If the only plaintext chunks the inspector finds are recent and the threat model tolerates them, the cheapest fix is to flip the scope to `encrypted = true` for future seals. If the inspector surfaces plaintext that was never meant to leave the device, scope rotation as above remains the canonical path.

## What `rendered/` adds to the leak surface

The renderer introduces a `rendered/` directory in plaintext scopes that materialises the entity graph as human-readable markdown and JSONL. The aggregate leak shape grows: a reader who lands on github.com no longer has to parse JSONL events to recognise people, organisations, and relationships — they get a styled "Rosie Cotton — works_at Erebor Inc." page rendered inline by github.com.

Encrypted scopes do not produce a `rendered/` tree; the transport bypasses the renderer entirely for those scopes (a doctor row, `sync.encryption_posture`, surfaces the per-scope mode). For plaintext scopes, treat the rendered tree the same way as the chunks: published. The `maz sync inspect` advisory already names "force-push does NOT un-publish historical data" as the operational constraint; that applies equally to rendered output.

## Commit authorship is plaintext, even when encrypted

By default sync commits are authored as the user (a GitHub no-reply identity — see [sync.md](sync.md#commit-authorship)) rather than a generic bot, so `git blame` / `git shortlog` work on the memory repo. Two consequences for the threat model:

- **The author, committer, and `Principal:`/`Device:` trailers are git metadata, so they are plaintext even when the scope's chunk payloads are encrypted.** Enabling encryption hides *what* you remembered, not *who* committed or *which device* pushed. This is deliberate (the author line is the point of bring-your-own-git attribution) and matches how every git host works, but a reader of an encrypted repo still learns the participant set and push cadence. The no-reply default keeps the email un-leaked; the `Agent:` and `Co-authored-by:` trailers are suppressed on encrypted pushes so the decoded source/peer set never leaks.
- **The git author is not the trust root.** Anyone can forge a git author line; it is a UX/attribution surface, not an authentication one. Trust is anchored in `EventAuthor.Sig` — the ed25519 signature over each event, verified on replay (`verifyTeamEvent` for team scope). A commit "authored by Frodo" with events whose signatures do not verify is rejected regardless of the git author. Read the author line as "who this repo says pushed", and the event signatures as "who actually authored each fact".

## Two views of one truth

The same memory exists in two shapes, and only one is editable:

- **`vault/*.md` (local) is the round-trip edit surface.** Edit it locally and the change flows back into the event log (SOURCE) and re-renders everywhere.
- **`rendered/by-uid/*.md` (synced repo) is a read-only mirror.** It renders beautifully on github.com and *looks* editable, but it is regenerated and pruned on every push — edits there are a dead end. To change a fact, use `maz` locally; never edit the remote's rendered tree or `chunks/`.

`chunks/` is the source of truth and is integrity-checked on pull (a content-hash mismatch is refused, not grafted — see [sync.md](sync.md#what-you-may-edit-and-what-you-must-not)). The honest framing: this catches accidental edits loudly; it is not a defense against the repo owner, who holds all the keys.

## The sensitive-payload guard (fail-closed)

Some events must never be written in cleartext — connector pulls (external transcripts, messages, mail) and anything a writer explicitly marks. The guard refuses to write such an event into a **plaintext** chunk, the git commit message, or the rendered tree. It is **fail-closed**: blocked, never silently dropped, never partially rendered.

**The predicate.** `internal/sync/privacy.IsSensitivePayload(event)` is the single source of truth, backed by a marker registry. Two seed markers ship today:

- `source.connector` — any event whose `Source` is `connector:*` / `connector/*` (the external-connector convention; see the MCP `source` field).
- `payload.explicit_flag` — any event whose payload carries a top-level `{"sensitive": true}`.

Downstream features extend the guard by calling `privacy.RegisterMarker(name, fn)` once from their own wiring and shipping one byte-level regression test. They never add a second gate. (The typed `source_type` taxonomy registers its marker the same way and can retire the `source.connector` seed without touching any gate.)

**Where it fires (and where it does not).** The load-bearing gate is at the **seal boundary** (`chunk.EncodeWithOpts`): a sensitive event bound for a plaintext, non-project scope refuses the **whole seal batch**, so no plaintext chunk is ever written and the sealer's cursor does not advance — the events stay pending for a later encrypted seal rather than stranding (no stuck-push loop). Because nothing is sealed, the commit-message builder and renderer never see the payload either. The git transport (`writeBlob`) and the renderer (`collectPlaintextEvents`) carry the same check as **defence-in-depth mirrors** for a plaintext chunk that arrived by a non-sealer path (pulled from a remote, or sealed before a marker existed); they abort, never skip. The transport runs its working-tree scan as a **push pre-flight** — before any blob is materialized — so a blocked push writes nothing and a retry re-runs the same check, rather than stranding already-written safe blobs behind a later abort. The pre-flight runs for **every non-project push, encrypted or plaintext**: a cleartext sensitive chunk already in the working tree is staged by the commit's `git add .` regardless of the new blobs' mode, so enabling encryption stops *new* plaintext seals but does not neutralise an existing plaintext leak — that chunk must be removed, and until it is, every push to the scope is refused.

Two exemptions, both at the **seal boundary**: **encrypted scopes** seal sensitive payloads normally (encryption is the correct destination), and the **`project` scope** is exempt because it is the intentionally-public, unencryptable in-repo tree. The project exemption holds at the transport and render mirrors too; the encrypted-scope allowance does **not** — see the pre-flight note above.

**Doctor.** `maz doctor` runs `sync.sensitive_payload_requires_encryption`, which **FAILs** (non-zero exit) when a plaintext, non-project scope already holds a chunk carrying a sensitive event. Remedy: enable encryption for the scope, or unbind the sensitive source. Like force-push, the guard is forward-only — it prevents *new* leaks; a chunk already pushed plaintext is published and must be remediated (history rewrite / key rotation), which is out of the guard's scope.

**Remote-source connectors (#212) inherit the guard for free.** Granola transcripts and Claude-chats conversations are pulled by sources whose `Name()` tags events `connector:granola` / `connector:claude-chats`, so the existing `source.connector` marker already flags them sensitive — there is **no new marker** for the remote sources. Two operator-facing additions reinforce the standing guard: (1) the ingest verbs (`maz source connect/ingest/import`) **refuse before any pull** when the target scope syncs as plaintext, so the operator gets a clear error instead of events stuck pending behind the seal gate; (2) the per-scope config-load check `sync.connector_scope_requires_encryption` (above) covers the local default-plaintext device the runtime scan cannot see. A connector's **API key** is held in the OS keychain only — never config, the event log, `rendered/`, sync chunks, or logs — and `maz source revoke <connector>` deletes it and then re-reads the chain to **prove it is gone**, failing loudly if a `MAZ_SECRET_*` environment override still shadows the deleted key.

## Watchword recovery

A **Watchword** is a 24-word BIP-39 English mnemonic printed once at first init. It encodes 256 bits of entropy from which all scope keys derive. Lose every paired device, restore via `maz init --restore --watchword "..."` and type the Watchword. The KDF + HKDF chain regenerates every scope key deterministically.

The Watchword is the only way to recover from total device loss. The user is responsible for storing it safely (paper in a safe, password manager). Mazarbul never writes the Watchword to disk; it is held in the keychain by ID, not by value.

**Wordlist:** BIP-39 English (2048 words, well-known, broadly tooled). 24 words is the BIP-39 max-entropy slot — 12/15/18/21 are accepted by BIP-39 but rejected by Mazarbul's `watchword.Validate`, which only accepts the canonical 24-word length.

**Versioning:** the salt and info strings carry a `:v1` suffix so a future cipher swap can derive parallel `:v2` keys from the same mnemonic without breaking historical chunks.

## Capability tokens

Sharing a slice of the graph without sharing the whole scope uses capability tokens: short-lived signed tokens scoped to a query predicate. A capability token can grant read access to `entities/01HNX...` and its 2-hop neighborhood for 24 hours, no more.

<!-- TODO(m2): expand: token format (Macaroons or biscuit-auth likely), caveat language, revocation, audit trail. -->

## Audit log

Every read of a non-public scope writes an entry to the local audit log (separate from the event log). Audit entries record: scope, principal (device fingerprint), query predicate, timestamp, result count. The audit log is append-only and stays local; it never syncs.

<!-- TODO(m2): expand: retention policy, audit log schema, query interface, compliance exports. -->

## Per-file privacy tags

Connectors and parsers can tag a file's resulting events with a privacy class (`public`, `internal`, `confidential`, `secret`). Tags act as filters in `recall`; the user can configure default class behaviour per provider.

<!-- TODO(m2): expand: tag taxonomy, propagation through derived facts, UI for class management. -->
