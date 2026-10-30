# Event Log: Append-Only JSONL Writer + Random-Access Lookup

**Status**: Implemented
**Issues**: [#20](https://github.com/mo-cmyk/mazarbul/issues/20) (writer), [#26](https://github.com/mo-cmyk/mazarbul/issues/26) (random-access lookup)
**Epic**: [#10](https://github.com/mo-cmyk/mazarbul/issues/10)
**Milestone**: M1 (Walking skeleton)
**Owner**: `@golang-developer`
**Created**: 2026-05-24
**Updated**: 2026-05-24 (added `At` + bbolt sidecar)

## Goal

Land the writer half of `kernel.EventLog`. Every fact in Mazarbul becomes a line in `events/YYYY/MM/DD.jsonl` under the user's data dir; the file is the source of truth and the only durable artifact. Graph, vector, and FTS indexes are derivatives that get rebuilt from this log.

This doc covers the **writer**, the **streaming reader** (issue #25), the **random-access lookup** (`At`, issue #26), and **`Rebuild`** (issue #27, this page).

## Interface

```go
// internal/kernel/eventlog.go
type EventLog interface {
    Append(ctx context.Context, events []Event) ([]Receipt, error)
    Read(ctx context.Context, opts ReadOpts) iter.Seq2[Event, error]
    At(ctx context.Context, uid string) (Event, error)
    Rebuild(ctx context.Context, into Storage, opts ...RebuildOption) ([]ApplyError, error)
}
```

`FileEventLog` implements all four methods. `Rebuild` accepts functional options (`WithProgress(io.Writer)`, `WithProgressEvery(int)`, `WithStrict()`) and returns a slice of per-event `ApplyError` together with a top-level error that is non-nil only on Rebuild-level failures (reader I/O, context cancellation, strict-mode abort wrapping the offending `ApplyError`).

## On-disk format

```
$MAZ_DATA_DIR/
  events/
    2026/
      05/
        24.jsonl    <- one event per line, JSON
        25.jsonl
```

Each line is `json.Marshal(Event) + "\n"`. The Event struct is defined in `internal/kernel/eventlog.go` and uses AIP-compliant field naming (`createTime`, not `createdAt`). Field order on the wire is determined by Go's `encoding/json` (struct field declaration order).

Sample line:

```json
{"name":"events/01J0NX1234567890ABCDEFGHJK","uid":"01J0NX1234567890ABCDEFGHJK","createTime":"2026-05-24T12:00:00Z","kind":"remember","source":"providers/anthropic","confidence":1,"payload":{"text":"hello"}}
```

The path bucket is computed from `event.CreateTime.UTC()`. A batch that straddles midnight UTC writes into two files (one fsync per file).

## Algorithm

```
Append(ctx, events) -> []Receipt, error:
    if len(events) == 0:
        return nil, nil
    mu.Lock(); defer mu.Unlock()
    for each event in events:
        if event.UID == "":
            event.UID = newULID()
        else:
            if !ulid.Parse(event.UID): return ErrInvalidEvent
        if event.CreateTime.IsZero():
            event.CreateTime = clock().UTC()
        else:
            event.CreateTime = event.CreateTime.UTC()
            if event.CreateTime.After(clock().Add(maxSkew)): return ErrInvalidEvent
        event.Name = "events/" + event.UID
        if err := ontology.Validate(ctx, event.Kind, event.Payload); err != nil:
            return wrap(ErrInvalidEvent)   // strict: unknown kind also rejects
    bucket events by CreateTime.UTC() date
    for each (date, batch) in chronological order:
        path = dataDir / events / YYYY / MM / DD.jsonl
        MkdirAll(parent, 0o755)
        f = OpenFile(path, O_APPEND|O_CREATE|O_WRONLY, 0o644)
        oldSize = stat(f).Size
        buf = concat(json.Marshal(e) + "\n" for e in batch)
        n, err = f.Write(buf)
        if err or n < len(buf):
            f.Truncate(oldSize); f.Close(); return wrap(err)
        if err = f.Sync():
            f.Truncate(oldSize); f.Sync(); f.Close(); return wrap(err)
        f.Close()
    return receipts in original input order
```

Properties:

- **Append-only**: `O_APPEND|O_CREATE|O_WRONLY|O_EXCL` first, falling back to `O_APPEND|O_WRONLY` when the file already exists. Truncate is reachable only on a failed write or fsync, and only shrinks to the previously committed size.
- **Single fsync per touched file**: one syscall per file per batch.
- **Torn-line free**: if the write or fsync errors, the file is rolled back to `oldSize`. Any later reader sees a clean tail.
- **Post-write rollback**: when `writeOne` errors AFTER content has been written + fsynced (parent-dir fsync failure, or close failure in defer), the current bucket is still included in the rollback set so durable content does not survive a reported failure. Without this, a caller's retry could duplicate events.
- **Ownership-aware rollback**: rollback decisions consult only the runtime signals from `writeOne` (`createdByUs`, proven by `O_EXCL` succeeding, and `observed`, set after Stat). The plan-time `isNew` hint is deliberately ignored: between `planBuckets` and `writeOne`, another writer can create or delete the file. Trusting the stale hint would cause `os.Remove` to delete another writer's durable JSONL file, or `Truncate(0)` to wipe a pre-existing day. When neither signal is set, `writeOne` failed before claiming or observing the file, so the bucket is intentionally not added to the rollback set.
- **Race-clean**: in-process callers are serialized by `mu`. Cross-process append is out of scope for M1 (single-daemon design).
- **Monotonic ULIDs**: `ulid.MonotonicEntropy` so receipts within the same millisecond sort in append order.
- **Chronological persistence + chronological Read**: the `EventLog.Read` contract at `eventlog.go:94` requires `CreateTime` order with lexicographic `UID` tiebreak. The writer cooperates: `bucketByDate` sorts events into day buckets in chronological order, then stable-sorts each day's events by `(CreateTime, UID)` so a single backfill batch like `Append([{t=11:00}, {t=10:00}])` lands on disk as `[10:00, 11:00]`. For cross-batch backfills (the file already holds `[t=11:00]` and a later `Append([{t=10:00}])` lands), on-disk order is `[11:00, 10:00]` because the log is append-only — the writer cannot rewrite history. The reader closes the gap by loading each day file into memory, sorting `(CreateTime, UID)` ascending, and yielding chronologically. Memory cost is bounded by one day's events. Receipts continue to mirror caller input order (the public `Append` contract); only the persisted and yielded orders are chronological.

## At — random-access lookup by UID

Recall ranks `Hit`s and then needs to materialize the full `Event` behind each one. Scanning every JSONL file is O(n) and slow on a large log; the writer therefore maintains a UID -> (file, byte offset) sidecar that `At` consults in O(log n).

### Sidecar layout

- File: `<dataDir>/events/index.bolt` (bbolt v1.4).
- Bucket: `events` (single bucket).
- Key: 16-byte binary ULID. Sorts chronologically, mirroring JSONL append order, so prefix scans line up with chronological ranges.
- Value: fixed 12-byte little-endian record:

```
[0..2)  uint16  year   (e.g. 2026)
[2..3)  uint8   month  (1..12)
[3..4)  uint8   day    (1..31)
[4..12) int64   byte offset of the event line within the day file
```

The path is reconstructed via `dateKey.path(dataDir)`, the same helper Append uses. Storing date components rather than a path string keeps the value compact and lets both code paths share one date helper.

### Algorithm

```
At(ctx, uid) -> Event, error:
    if ctx.Err(): return wrap
    if !ulid.Parse(uid): return ErrInvalidEvent

    if !stat(<dataDir>/events/index.bolt):
        return fallbackScanAndRebuild(ctx, uid)

    db = getIndex()   // lazy bolt.Open under indexMu
    key = ulid.Bytes(uid)
    view: tx -> v = bucket.Get(key)
    if v == nil: return ErrNotFound
    (day, offset) = decodeIndexValue(v)
    return readEventAt(day, offset)

readEventAt(day, offset):
    path = dataDir / events / YYYY / MM / DD.jsonl
    f = os.Open(path); seek(offset, SeekStart)
    line = bufio.ReadBytes('\n')
    return json.Unmarshal(trim(line))

putIndex(entries):           // called from Append, after JSONL fsync
    db.Update: for each entry: bucket.Put(ulidKey, encodeValue(day, oldSize+offset))

fallbackScanAndRebuild(ctx, uid):   // called only when index.bolt is missing
    db = getIndex()
    db.Update:
        bucket = ensureIndexBucket(tx)
        walk events/**/*.jsonl in chronological order:
            for each line at offset off:
                bucket.Put(ulidKey, encodeValue(day, off))
                if uid matches wantUID: capture event
    return captured event or ErrNotFound
```

### Concurrency

- `Append` already holds `l.mu` while it stages, fsyncs the JSONL, and then calls `putIndex`. The bbolt `Update` is the last step before `mu.Unlock`.
- `At` never touches `l.mu`. It opens the bbolt handle through a `withIndex` helper that takes `indexMu.RLock()` for the duration of the bbolt transaction.
- `Close` takes `indexMu.Lock()`, so it cannot tear down the handle underneath an in-flight `At` or `putIndex`. The lock is a `sync.RWMutex`: many concurrent `At` calls run in parallel; `Close` waits for all of them.
- The JSONL files are append-only: bytes at any committed offset are immutable, so `os.Open` + `Seek` + `ReadBytes('\n')` is safe to run concurrently with an in-flight `Append`.
- **Duplicate UIDs are rejected before commit, with the JSONL as source of truth.** `Append` runs a `checkUIDsUnique` pass that errors with `ErrInvalidEvent` on (a) duplicate UIDs within the same batch, and (b) any UID already present in the log. The check ensures the bbolt sidecar reflects the JSONL before reading: if the sidecar is missing (operator deletion, first run, or a previous `putIndex` failure invalidated it), `Append` rebuilds it from JSONL first. Two compounding safeguards keep this honest: `putIndex` failures call `invalidateIndex` to unlink the sidecar so the next `Append` lands in the rebuild branch, and the rebuild happens before any new JSONL bytes are written. The log therefore never holds two events with the same UID — `At(uid)` always resolves to a single, well-defined event.

### Failure model

| Failure | Behavior |
|---|---|
| `At` called with malformed UID | `ErrInvalidEvent` |
| Index sidecar missing | Fallback scan over JSONL, rebuild sidecar, return the event or `ErrNotFound` |
| Index sidecar present, UID absent | `ErrNotFound` (index is authoritative when present) |
| `Append` batch contains duplicate UIDs (within batch or against the JSONL log) | `ErrInvalidEvent` before any JSONL write. The check rebuilds the sidecar from JSONL first if the sidecar is missing, so the verdict is always against the source of truth. |
| `Append` fsyncs JSONL but bbolt `Update` fails | Append returns the error. JSONL bytes are durable; the sidecar is invalidated (closed + unlinked) so the next Append's uniqueness check rebuilds from JSONL before accepting any new events. |
| Operator removes `events/index.bolt` while daemon holds handle | `At` notices via `os.Stat`, closes the unlinked handle, falls back to JSONL scan + rebuild |
| JSONL line at indexed offset is malformed | `At` returns the wrapped JSON decode error and the zero `Event{}` so callers never see a partial event |

### Lifecycle

- `NewFileEventLog` does not open the sidecar; the open is lazy on first `Append` / `At` so a read-only inspection of an empty data dir does not create a stray bolt file.
- **Single writer per data dir.** The bbolt sidecar holds an exclusive OS file lock; a second `FileEventLog` over the same `dataDir` will time out on its first `Append`/`At`. The daemon owns the data dir.
- `Close()` releases the bbolt handle and flushes pending writes. **Precondition**: no `Append` or `At` in flight on this `FileEventLog` — daemon shutdown owns this contract (stop accepting work, then `Close`). After `Close`, subsequent `Append`/`At` calls lazily reopen the handle.
- **Running-deletion recovery**: when an operator removes `events/index.bolt` while the daemon still holds an open handle (Unix keeps the inode alive through the open fd, but the path is gone), `At` detects the missing path via `os.Stat`, closes the cached handle, and falls back to the JSONL scan that rebuilds a fresh path-backed index.

### Tests

- `file_eventlog_at_test.go`: round-trip single + 200 events, unknown-UID `ErrNotFound`, malformed UID, sidecar-deleted rebuild, cross-day batch, context cancellation.
- `file_eventlog_at_integration_test.go` (`//go:build integration`): concurrent `At` + `Append` race-clean, lookup across process restart (new `FileEventLog` over the same dataDir).
- `BenchmarkAt_HotIndex` in `file_eventlog_bench_test.go`: 10k pre-populated events, random UID lookup. Apple M1 Pro baseline: ~24 µs/op, 31 allocs/op (target was sub-100 µs).

### Append cost note

`Append` now performs two fsyncs per batch (JSONL + bbolt), where #20 only performed one. Apple M1 Pro baseline shifts:

| Benchmark | #20 baseline | After #26 |
|---|---|---|
| `BenchmarkAppend_Single` | ~4.7 ms/op | ~13.8 ms/op |
| `BenchmarkAppend_BatchOf100` | ~5.0 ms/op | ~14.3 ms/op |

Per-event amortized cost is ~140 µs in the 100-event batch case. For M1 (human-paced single-user writes) this is acceptable; if Recall traffic ramps up, M2 can batch index updates with a periodic flush instead of fsyncing per Append.

## Reader (issue #25)

`FileEventLog.Read(ctx, opts)` returns an `iter.Seq2[Event, error]` that streams events in chronological `(CreateTime, UID)` order from the JSONL day buckets. Lives in `internal/kernel/file_eventlog_read.go`. Each day file is loaded fully into memory, sorted, and yielded chronologically; the iterator is single-pass across the entire log (each file is opened, read, and closed exactly once).

```
Read(ctx, opts) -> iter.Seq2[Event, error]:
    files = listEventFiles(dataDir)   # walks dataDir/events, sorts YYYY/MM/DD chronologically
    emitted = 0
    for ef in files:
        if opts.Limit > 0 and emitted >= opts.Limit: stop
        if not dayOverlaps(ef.day, opts.Since, opts.Until): skip
        if ctx canceled: yield (Event{}, ctx err); stop

        # Load + sort + yield one day file
        f, committedSize = openCommitted(ef.path)    # RLock to snapshot size
        events = []
        for line in bufio.NewReader(io.LimitReader(f, committedSize)).ReadBytes('\n'):
            if not line ends with '\n':
                # past the committed boundary; ignore trailing partial
                break
            ev = json.Unmarshal(line)
            if unmarshal error:
                yield (Event{}, wrapped parse err); stop entire iterator
            events.append(ev)
        sort events by (CreateTime, UID) ascending
        for ev in events:
            if ctx canceled: yield ctx err; stop
            if not passesFilters(ev, opts): continue
            if not yield(ev, nil): stop (caller broke)
            emitted++
            if opts.Limit > 0 and emitted >= opts.Limit: stop
```

Properties:

- **Concurrent reads are lock-free with respect to each other**: `Read` uses `l.mu.RLock` (a read lock on the writer's `sync.RWMutex`). Multiple readers can hold RLock simultaneously and proceed in parallel.
- **Reads see fully-committed events only**: the writer's `Append` takes `l.mu.Lock` (exclusive) for the whole batch — validation, write, fsync, rollback. While that's held, no reader can acquire RLock. The reader's `openCommitted` helper takes RLock for the duration of `os.Open + Stat` only, snapshots the file's then-current size, and releases RLock before iterating. Because no writer can mutate the file while RLock is held, the snapshotted size is by definition a fully-committed boundary — never the page-cache window between `f.Write` and a successful `f.Sync`, and never a size that includes about-to-be-rolled-back bytes. The reader then wraps the file in `io.LimitReader(f, committedSize)` so any subsequent Append's bytes (or a later rollback) cannot enter this iteration.
- **Trailing partial line**: with the RWMutex contract above, in-flight Append bytes are never observable. A trailing non-terminated line in a day file is therefore evidence of crash recovery (a prior writer process died mid-write without rolling back) or an externally modified file, not an in-process race. The reader treats it as the natural stop boundary and ends the file cleanly.
- **Lazy file open**: day files are opened only as the iterator advances. Date-pruning (via `Since`/`Until`) skips whole files before any file descriptor is allocated.
- **Filter discipline**: `Since` is inclusive, `Until` is exclusive, `Kinds` is an allow-list (empty means all), `Limit` is a hard cap counted post-filter (matches the godoc contract "caps the number of events returned").
- **Typed parse errors**: malformed JSON yields `(Event{}, fmt.Errorf("eventlog: <path> line N: %w", parseErr))` and stops. Per the spec, bad lines are never silently skipped.
- **Lexicographic == chronological**: the writer zero-pads YYYY/MM/DD so `sort.Strings` order matches calendar order. The reader relies on this; if the writer's path layout ever diverges, the reader must change in lockstep.
- **Non-conforming files ignored**: any file under `events/` whose path is not `YYYY/MM/DD.jsonl` (including directories named `*.jsonl`) is silently skipped. The writer never produces such entries; they are externally introduced and not the reader's concern.

The iterator is single-pass per call; resuming after `break` requires a fresh `Read` call (with a `Since` chosen by the caller to avoid re-yielding consumed events). M2+ may add cursor-style resume; M1 does not.

## Decisions

| Question | Decision | Rationale |
|---|---|---|
| `Event.ContentHash` field | Defer | Goal narrative mentions hashing but no AC requires it; struct has no field. Content addressing lives with the chunk store (M4+). |
| Unknown-kind validation | **Strict** | When `Ontology.Validate` returns `ErrNotFound` (no schema for `kind`), reject the event with `ErrInvalidEvent`. Permissive bypass made the writer silently accept typo'd or unregistered kinds — including malformed Episode payloads — defeating the validation contract. Extension-registered kinds must register before first append. Flipped from permissive after Greptile review of the post-merge state ([PR #69 confidence 3/5](https://github.com/mo-cmyk/mazarbul/pull/69)). |
| Clock-skew tolerance | 5 minutes | Configurable via `WithMaxSkew(d)`. Tolerates NTP drift across providers without letting clients fabricate far-future events. |
| Cross-midnight batches | One fsync per touched file | Faithful to the spirit of "single fsync per batch" while handling the edge cleanly. Rare in practice. |
| ULID library | `github.com/oklog/ulid/v2` with `ulid.MonotonicEntropy` | Canonical Go ULID. Monotonic entropy preserves append order within a millisecond. |

## Failure modes

| Failure | Behavior |
|---|---|
| Caller supplies malformed UID | Return `ErrInvalidEvent`, no write |
| Caller supplies future CreateTime > 5m skew | Return `ErrInvalidEvent`, no write |
| Ontology rejects payload (schema mismatch) | Return `ErrInvalidEvent`, no write |
| Ontology has no schema for kind | Return `ErrInvalidEvent` (strict); wrapped cause is `ErrNotFound` |
| `MkdirAll` fails | Return wrapped error, no write |
| `OpenFile` fails | Return wrapped error, no write |
| `Write` returns short or errors | Truncate to `oldSize`, return wrapped error. File is clean. |
| `Sync` errors | Truncate to `oldSize` (best-effort second Sync), return wrapped error |
| Truncate itself fails after a failed write | Wrapped error returned. Reader (separate issue) must tolerate a single trailing partial line. |

## Tests

Unit tests live in `file_eventlog_test.go` (writer) and `file_eventlog_read_test.go` (reader), no build tag. Integration tests in `file_eventlog_integration_test.go` use `//go:build integration` and cover writer concurrency, midnight crossing, torn-line recovery, durability across writer close, and `TestIntegration_ConcurrentReadAndAppend` (#25) — multiple readers drain the log while writers append, asserting every yielded event is complete and every UID is drawn from the appended set.

Benchmarks in `file_eventlog_bench_test.go` measure single-event Append, 100-event batch Append, sequential Read drain (`BenchmarkRead_Sequential`), and filtered Read drain (`BenchmarkRead_Filtered`). All use `-benchmem`. Baseline numbers belong in the PR description.

## Acceptance criteria (mapped from #20)

- [x] AC1: Writes to `events/YYYY/MM/DD.jsonl`, single fsync per batch
- [x] AC2: Read-after-fsync visibility, no partial reads
- [x] AC3: ULID assignment, malformed-ULID rejection
- [x] AC4: `CreateTime` defaulting and future-skew rejection
- [x] AC5: `Ontology.Validate` payload validation
- [x] AC6: `[]Receipt` in input order
- [x] AC7: Race-clean under `go test -race`
- [x] AC8: Typed error on disk-full, no torn line

## Acceptance criteria (mapped from #25)

- [x] Iterates `events/YYYY/MM/DD.jsonl` in lexicographic order; opens each file lazily
- [x] Yields each event in append order; iterator stops cleanly when callers break
- [x] Applies `Since` (inclusive), `Until` (exclusive), `Kinds` (allow-list), and `Limit` (hard cap) before yielding
- [x] Returns a typed error via `yield(Event{}, err)` on JSONL parse failure; never silently skipped
- [x] Concurrent `Read` calls do not block each other; concurrent with `Append` is safe (readers see fully-written events only)
- [x] Unit + integration tests cover ordering, filters, caller-break, parse errors, torn last line, context cancel, and concurrent reader/writer

## Acceptance criteria (mapped from #26)

- [x] AC1: UID -> (file, byte offset) sidecar at `events/index.bolt` populated post-fsync inside `Append`
- [x] AC2: Index rebuilt on `Append`; fallback scan + rebuild when the sidecar is missing
- [x] AC3: `ErrNotFound` on miss; never a partial `Event` (decode errors return zero value + wrapped err)
- [x] AC4: Race-clean concurrent `At` + `Append` under `go test -race`
- [x] AC5: Unit test appends N events and looks up each by UID

## Rebuild (issue #27)

`Rebuild(ctx, into, opts...)` replays every event in the log into `into.Apply`, reconstructing the four derived stores from the JSONL source of truth. Use it on a fresh checkout, after a corruption (`rm -rf data/index.db`), or to reconcile the soft-inconsistency window where a SQLite commit succeeded but the LadybugDB MERGE failed (or vice versa).

### Algorithm

1. Iterate `l.Read(ctx, ReadOpts{})`. The Read contract yields events in chronological order with ULID monotonic tie-breaking, so Rebuild is single-pass and needs no buffering.
2. For each event:
   - `into.HasEvent(uid)` — true ⇒ skip (already in derived store).
   - Otherwise `into.Apply(ctx, []Event{ev})` — one event per Apply call so per-event errors carry the exact UID.
3. Best-effort by default: per-event Apply failures are collected into `[]ApplyError`, warn-logged, and Rebuild continues. `WithStrict()` aborts on the first failure and wraps the offending `ApplyError` in the returned error (callable via `errors.As`).
4. Optional progress streaming via `WithProgress(io.Writer)` at `WithProgressEvery(N)` cadence (default 100). Format: `rebuilt {n} events  skipped={s}  failed={f}  elapsed={d}  rate={r}/s`. A terminal summary always fires on every exit path (normal completion, strict abort, ctx cancel, reader I/O error) via `defer`, so short rebuilds and aborted rebuilds both print a final count. Writer errors on the progress sink are discarded — a closed pipe must not abort a rebuild.
5. Context cancellation is honoured between every event. **Apply runs under a detached context** (`context.WithoutCancel(ctx)` + `WithTimeout(defaultApplyTimeout)`), mirroring `Append.applyToStorage`. A tight caller deadline cannot rip a half-finished dispatcher tx out mid-write; outer cancellation halts the next event's dispatch, not the in-flight one.

### Idempotency

Two layers compose:
- **Cheap skip** via `Storage.HasEvent(uid)`. The dispatcher implementation checks **every derived layer it would have written**: the `episodes` row (always), the vector layer (`episodes_vec_ids` JOIN `episodes_vec` ON rowid — both the sidecar mapping AND the actual vec0 row, since recall depends on the joined pair), and the LadybugDB `Episode {uid}` node. Returning `true` means "Apply has no work left for this UID"; returning `false` for any missing layer triggers a re-Apply so partial corruption (e.g. `rm -rf data/graph.ladybug` while `index.db` survives, or vec0 rows wiped while the sidecar mapping persists) self-heals on the next Rebuild.
- **Safe re-apply** via the dispatcher's `INSERT … ON CONFLICT(uid) DO UPDATE` upsert in SQLite, vec0 DELETE-then-INSERT for the vector layer, and `MERGE (e:Episode {uid:$uid})` in LadybugDB. A false negative from `HasEvent` costs a redundant tx, not correctness, at any layer.

### Failure model

| Failure | Behavior |
|---|---|
| `into == nil` | Return `(nil, error)` immediately. |
| `Read` yields a parse error | Return `(applyErrsSoFar, wrappedErr)`. |
| `HasEvent` fails | Return `(applyErrsSoFar, wrappedErr)`. The store cannot be queried; falling back to Apply would risk inconsistency. |
| `Apply` fails, non-strict | Append `ApplyError{UID, Err}` to slice; warn-log; continue. |
| `Apply` fails, strict | Return `(applyErrsSoFar, fmt.Errorf("...strict abort: %w", ae))`. |
| `ctx.Done()` | Return `(applyErrsSoFar, ctx.Err())` at the next loop iteration. |

### Tests

- `file_eventlog_rebuild_test.go` — unit tests with an in-memory `kernel.Storage` fake: empty log, all-succeed, partial-skip (HasEvent returns true for half), non-strict error collection (failed counter visible in progress), strict abort (terminal flush still fires), HasEvent driver fault, ctx cancellation mid-rebuild (terminal flush still fires), Apply-context-is-detached observation, progress writer cadence, nil storage rejection, two-pass idempotency, terminal-summary flush for short logs, `ApplyError`'s `errors.Is`/`Unwrap` contract.
- `internal/kernel/storage/dispatcher/file_eventlog_rebuild_integration_test.go` (`//go:build integration`) — 1000 episode events appended through the real four-layer dispatcher, derived stores deleted, fresh dispatcher opened at the same data dir, `Rebuild` called, row counts and FTS hits asserted equal to baseline, second `Rebuild` asserts `skipped=1000` and zero new applies. Two companion partial-corruption tests pin the multi-layer `HasEvent` contract: `TestFileEventLog_Rebuild_RestoresGraphAfterPartialCorruption` deletes **only** `graph.ladybug` and asserts re-MERGE of every Episode node; `TestFileEventLog_Rebuild_RestoresVec0AfterCorruption` wipes **only** the `episodes_vec` rows (keeping the `episodes_vec_ids` sidecar mapping) and asserts re-INSERT into the vec0 layer.

## Acceptance criteria (mapped from #27)

- [x] AC1: Walks `Read(ctx, ReadOpts{})` in append order; calls `storage.Apply` per event
- [x] AC2: Per-event errors surface as `[]ApplyError` with UID + error; continues by default, aborts under `WithStrict()`
- [x] AC3: Idempotent — `HasEvent` skips already-applied events; the dispatcher's upsert backstops false negatives
- [x] AC4: Progress streams to a caller-provided `io.Writer` (event count + elapsed time + rate); terminal summary always fires
- [x] AC5: Integration test writes 1000 events, drops derived stores, calls `Rebuild`, asserts row counts + FTS recall match the baseline; second `Rebuild` is a full skip
