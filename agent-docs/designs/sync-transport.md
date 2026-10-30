# Sync transport (Moria)

Mazarbul's sync layer — codename **Moria** — moves event chunks between devices through a pluggable transport. The append-only event log was [designed as a CRDT](../synthesis.md#the-event-log-is-a-crdt-by-construction): the merge model is structurally free; the work is transport, encryption, and key management. This doc fixes the contracts for the plaintext-default sync implementation.

## Layering

```
kernel events  (JSONL, plaintext, append-only)
    └─ sealer      → chunks  (plaintext .jsonl by default; encrypted .maz when opt-in)
        └─ Transport.Push(chunks)  → remote
            └─ Transport.Pull → []Chunk
                └─ dispatcher.Apply (idempotent replay)
```

Each layer has one job. The sealer never touches the network; the transport never touches event semantics; the dispatcher does not care about chunk mode.

## Chunk formats

One chunk = one batch of events from a single scope. The unit of transport is the chunk file, not the individual event. Sync ships two formats; the choice is **per-scope, opt-in to encryption**.

### Plaintext JSONL (`<chunkID>.jsonl`, default)

```
line 1   : {"_meta": true, "chunkId": "...", "scope": "...", "count": N,
            "firstUlid": "...", "lastUlid": "...", "createTime": "...",
            "device": "...", "encrypted": false}
lines 2..N : one event JSON object per line, ULID-ordered
```

No framing, no compression, no magic bytes. github.com renders this as syntax-highlighted JSONL with line-level `git diff` and per-event `git blame`. Batch cap defaults to **2 000 events** so each `.jsonl` stays under github.com's ~1 MiB web-UI ceiling.

### Encrypted MAZ1 (`<chunkID>.maz`, opt-in)

```
[4 bytes]  magic: "MAZ1"
[1 byte]   format version (currently 0x01)
[2 bytes]  header length (big-endian uint16, max 64 KiB)
[N bytes]  header (JSON; plaintext; same field set as JSONL `_meta`)
[remainder] sealed payload: XChaCha20-Poly1305(gzip(JSONL events))
```

The header is plaintext on purpose: an operator with a chunk file but no scope key can still see which scope it belongs to and when it was minted; only the events themselves stay encrypted. Header rejection cases (bad magic, unsupported version, length > 64 KiB, JSON syntax error) all surface as typed `ErrCorruptHeader` / `ErrBadMagic` / `ErrBadVersion` so the transport can distinguish a stray file from a corrupted chunk. Batch cap defaults to **10 000 events** (compression amortises the size cost).

`PeekHeader` sniffs the first 4 bytes: `MAZ1` routes to the framed decoder, `{"_m` routes to JSONL. Both paths return the same `chunk.Header` struct; the boolean `Header.Encrypted` lets the transport, MANIFEST writer, and doctor distinguish the two without re-sniffing.

### ChunkID generation

ChunkID is a ULID minted at seal time using `ulid.Monotonic(crypto/rand)`. The default `ulid.New(ms, nil)` path leaves the 80 entropy bits zeroed — two seals in the same millisecond would collide on disk. The chunk package owns a package-level monotonic entropy source and serialises calls under a mutex.

### Sealing cadence

The `PeriodicSealer` triggers on a ticker (default 30 minutes) and on graceful shutdown via `SealNow`. Empty seal windows are no-ops. The cursor (last-sealed UID per scope) is persisted after every successful seal so a daemon restart resumes where it left off rather than re-sealing the full open log.

Batch caps: 10 000 events per chunk by default (configurable via `SealerOptions.MaxEventsPerChunk`). A scope with 500 000 events thus produces ~50 chunks; at GitHub's recommended 5 GB ceiling and a typical chunk size of ~1 MB compressed-and-sealed, the headroom is in the tens of millions of events per scope.

### Layout on disk

```
<dataDir>/sync/<scope>/chunks/<chunkID>.maz
```

`scope` is one of `project|personal|team` (see [privacy.md](../privacy.md)). The chunks directory is `0o700`; individual files are `0o600`. Atomic rename writes (temp + sync + chmod + rename) ensure a crash during `Put` leaves either the old state or the new — never a half-written chunk.

## Cryptography

Cipher, KDF, and key derivation are spelled out in [privacy.md](../privacy.md#encryption-at-rest). Summary:

- **Watchword** = BIP-39 English, 24 words, 256 bits of entropy.
- **Root key** = Argon2id(mnemonic, salt=`mazarbul:root:v1`, m=64 MiB, t=3, p=1), 32 bytes.
- **Scope key** = HKDF-SHA-256(root, info=`mazarbul:scope:<scope>:v1`), 32 bytes.
- **Seal** = XChaCha20-Poly1305(scopeKey, plaintext) with a fresh 24-byte random nonce per seal.

## Transport contract

The transport never decrypts a chunk; it moves bytes only.

```go
type Transport interface {
    Push(ctx, scope, []Blob) (int, error)
    List(ctx, scope) (Listing, error)
    Get(ctx, scope, chunkID) (Blob, error)
    Presence(ctx, scope) ([]Device, error)
    Name() string
}
```

A `Blob` is `(chunk.Header, io.ReadCloser)`. The Header is the same plaintext struct that lives in the chunk file's framing; the transport reads it cheaply via `chunk.PeekHeader` without holding the scope key.

### Errors

Two sentinel errors form the contract:

- `transport.ErrTransport` — the remote misbehaved (network failure, auth rejection, malformed response). Wrapped at every call site so `errors.Is(err, ErrTransport)` is the reachability check.
- `transport.ErrNotConfigured` — the scope has no remote configured. Distinct so the CLI can print "run `maz sync configure`" instead of a stack trace.

### Push semantics

- **Idempotent on ChunkID.** A blob whose ChunkID already exists at the remote MUST be skipped, not errored. The git impl materialises the blob locally first; if the working tree already has the file it skips the commit. The return value (`accepted int`) reports how many blobs were *newly* added.
- **Empty pushes are no-ops.** Zero accepted blobs → no commit → no network round-trip.
- **One commit per Push call.** The commit message records `device-id + scope + +N chunks` so the audit trail tells you which device pushed what.

### Pull / List / Get semantics

`List` returns the remote's view of the scope as a slice of `chunk.Header`, sorted by ChunkID (ULID lex = seal-time order). The caller computes the diff against its local chunk store and calls `Get` per missing ChunkID. `Get` returns the raw bytes; the caller passes them to `chunk.Decode` with the scope key.

Fast-forward only: the git impl uses `git fetch && git merge --ff-only`. A non-fast-forward situation (two devices both pushed concurrently from the same starting commit) is rare in practice because pushes are serialised per device, but when it does happen the remote's HEAD wins on the next pull and the local device re-pushes its blobs on the next cadence — git's commit-once-per-blob model makes this a no-op for already-uploaded files.

## Git transport (v0.x default)

Shells out to the system `git` binary. Existing credential helpers (SSH agents, HTTPS PATs in keychain, gh-cli's git-credential-osxkeychain) work without re-implementation. Bare-repo mirrors live at `<dataDir>/sync/<scope>/repo/`; chunks live inside the working tree at `chunks/<chunkID>.maz`.

**Empty-remote bootstrap.** The first push against a brand-new bare repo falls back to `git init && git remote add origin` instead of failing on `git clone` of an empty remote.

**Credential prompts.** `GIT_TERMINAL_PROMPT=0` and `GIT_ASKPASS=/bin/true` are set on every git invocation so a missing credential surfaces as an error rather than pinning the daemon goroutine on a stalled interactive prompt.

**Author identity.** Commit identity is forced via `GIT_AUTHOR_*` and `GIT_COMMITTER_*` env vars on the `git commit` invocation rather than per-repo `user.name` config. The env vars are evaluated *after* config — if the operator's global gitconfig is `[user] name = Moritz`, a per-repo `git config user.name "device-b"` is silently overridden by the global. Env vars take precedence.

**Operational ceilings.** GitHub recommends ≤ 5 GB per repo and accepts ~6 pushes/min. The sealer's 30-minute tick keeps push rate well below the cap; the chunk-store ceiling is reached only after tens of millions of events per scope.

## Pairing (Mellon)

See [privacy.md](../privacy.md#maz-mellon-pairing) for the cryptography. Pairing is air-gapped: producing device runs `maz mellon code`, transmits (code, bundle) through two independent channels, receiving device runs `maz mellon pair`. No network listener, no firewall hole.

## Concurrent-supersede materialisation

Cross-device sync can leave a functional slot — `(subject_uid, predicate)` where the predicate registry marks predicate as functional — with more than one open relation if two devices independently superseded the same prior relation along incompatible paths. The supersede resolver assumes session-serial supersede order and was silently leaving both arms open before this milestone.

After every post-pull replay pass, the resolver runs `DetectConcurrentSupersede` against the relations table. For each functional slot with N > 1 open arms, it emits N-1 `relationSupersedeConflict` events. The primary arm is the lowest-ULID open relation (deterministic tiebreak, not a quality judgement). The dispatcher logs a `WARN` and writes nothing to derived stores — the event itself is the durable conflict record. The TUI's `/review conflicts` command reads these events to drive operator-led resolution; the operator closes the losing arm via a normal `relationForgotten`.

## Kernel compatibility

The existing event log, dispatcher, and resolver are pull-merge-ready as-is:

- **Rebuild** already sorts events by `(CreateTime, UID lexicographic)`. Replaying chunks from a different device produces deterministic interleaving.
- **Dispatcher idempotency:** every layer is already `ON CONFLICT` / `MERGE`-based. Re-applying an event the local log already saw is a no-op.
- **ULID merge order:** per-device monotonic, globally lexicographic-stable sort. Two interleaved device streams sort deterministically.

The only schema-level addition for scoped sync is `Event.Scope`. A pre-scope event with no `scope` field reloads as the zero value and `FileEventLog.prepare()` upgrades it to `personal` on append, so existing JSONL files round-trip without modification.
