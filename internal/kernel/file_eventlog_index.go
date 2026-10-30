package kernel

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
)

// Sidecar UID -> (file, byte offset) index. The event log is the source
// of truth; this index is a derived random-access shortcut populated
// after every successful Append fsync. At looks an event up in O(log n)
// instead of scanning the JSONL files.
//
// File: <dataDir>/events/index.bolt
// Bucket: "events"
// Key: 16-byte binary ULID (sorts chronologically).
// Value: 12-byte fixed record:
//
//	[0..2)  uint16 LE  year   (e.g. 2026)
//	[2..3)  uint8      month  (1..12)
//	[3..4)  uint8      day    (1..31)
//	[4..12) int64 LE   byte offset of the line within the day file
//
// The path is reconstructed via dateKey.path(dataDir). Encoding the date
// directly keeps the value compact and lets the writer reuse the same
// helper for both Append and At.

// indexBucketName is the single bucket under which UID -> location
// records live.
const indexBucketName = "events"

// hlcBucketName is the bbolt bucket that holds the last-seen HLC stamp.
// Key: "last" (hlcLastKeyName). Value: the 16-char hex HLC string.
const hlcBucketName = "hlc"

// hlcLastKeyName is the single key inside the "hlc" bucket.
const hlcLastKeyName = "last"

// hlcBucket returns the HLC bucket-name slice.
func hlcBucket() []byte { return []byte(hlcBucketName) }

// hlcLastKey returns the key slice for the last-HLC record.
func hlcLastKey() []byte { return []byte(hlcLastKeyName) }

// indexValueSize is the on-disk size of one bbolt value record.
const indexValueSize = 12

// indexBucket returns the bucket-name slice. A function (not a var)
// dodges gochecknoglobals while costing only a tiny allocation, and
// the slice is read-only by bbolt anyway.
func indexBucket() []byte { return []byte(indexBucketName) }

// indexEntry is what commitPlans returns: the UID together with the
// absolute file location written to the JSONL bucket.
type indexEntry struct {
	uid    string
	day    dateKey
	offset int64
}

// indexPath is the absolute path to the bbolt sidecar.
func (l *FileEventLog) indexPath() string {
	return filepath.Join(l.dataDir, "events", "index.bolt")
}

// openIndexLocked opens the bbolt sidecar and assigns it to l.index.
// The caller must already hold indexMu for write. The parent events/
// directory is created here so callers do not need to MkdirAll twice.
func (l *FileEventLog) openIndexLocked() error {
	if l.index != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(l.dataDir, "events"), 0o750); err != nil {
		return fmt.Errorf("eventlog: mkdir index parent: %w", err)
	}
	db, err := bolt.Open(l.indexPath(), 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return fmt.Errorf("eventlog: open index %s: %w", l.indexPath(), err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, berr := tx.CreateBucketIfNotExists(indexBucket()); berr != nil {
			return fmt.Errorf("create events bucket: %w", berr)
		}
		if _, berr := tx.CreateBucketIfNotExists(hlcBucket()); berr != nil {
			return fmt.Errorf("create hlc bucket: %w", berr)
		}
		return nil
	}); err != nil {
		if cerr := db.Close(); cerr != nil {
			return fmt.Errorf("eventlog: init index bucket: %w (close also failed: %w)", err, cerr)
		}
		return fmt.Errorf("eventlog: init index bucket: %w", err)
	}
	l.index = db
	return nil
}

// loadHLCFromIndex reads the last-seen HLC stamp from the bbolt sidecar.
// Returns (stamp, true, nil) when the value is present and non-empty,
// ("", false, nil) when the bucket/key is absent, and ("", false, err) on
// any bbolt error. Callers use the not-found result as the signal to fall
// back to the day-file scan.
func (l *FileEventLog) loadHLCFromIndex() (string, bool, error) {
	var stamp string
	err := l.withIndex(func(db *bolt.DB) error {
		return db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(hlcBucket())
			if b == nil {
				return nil
			}
			v := b.Get(hlcLastKey())
			if len(v) > 0 {
				stamp = string(v)
			}
			return nil
		})
	})
	if err != nil {
		return "", false, err
	}
	return stamp, stamp != "", nil
}

// putHLCLast persists the last-seen HLC stamp to the bbolt sidecar in its own
// transaction. Called best-effort after each successful Append batch and after
// GraftSealed advances the clock; a failure is non-fatal because the fallback
// day-file scan re-derives the stamp on the next open.
func (l *FileEventLog) putHLCLast(hlcStr string) error {
	if hlcStr == "" {
		return nil
	}
	return l.withIndex(func(db *bolt.DB) error {
		return db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(hlcBucket())
			if b == nil {
				return errors.New("hlc bucket missing")
			}
			return b.Put(hlcLastKey(), []byte(hlcStr))
		})
	})
}

// closeIndexLocked closes the cached handle, if any. The caller must
// already hold indexMu for write. Used both by Close and by the
// recovery path when At detects that the sidecar file has been
// unlinked underneath a still-live handle.
func (l *FileEventLog) closeIndexLocked() error {
	if l.index == nil {
		return nil
	}
	db := l.index
	l.index = nil
	if err := db.Close(); err != nil {
		return fmt.Errorf("eventlog: close index: %w", err)
	}
	return nil
}

// withIndex runs fn against the bbolt handle. It holds indexMu for
// read for the duration of fn, so the handle cannot be closed by a
// concurrent Close while fn is mid-transaction. The handle is opened
// lazily under the write lock; the two-iteration loop handles the
// (rare) race where Close fires between our open and our read-lock.
func (l *FileEventLog) withIndex(fn func(*bolt.DB) error) error {
	for range 2 {
		l.indexMu.RLock()
		if db := l.index; db != nil {
			err := fn(db)
			l.indexMu.RUnlock()
			return err
		}
		l.indexMu.RUnlock()

		l.indexMu.Lock()
		oerr := l.openIndexLocked()
		l.indexMu.Unlock()
		if oerr != nil {
			return oerr
		}
	}
	return errors.New("eventlog: index repeatedly closed during open")
}

// Close releases the bbolt handle. Safe to call multiple times.
//
// PRECONDITION: callers must ensure no Append or At is in flight on
// this FileEventLog. Daemon shutdown owns this contract: stop
// accepting work, then Close. Mid-lifetime Close (e.g., for an offline
// backup) is supported only when the caller can guarantee quiescence.
// Subsequent Append/At calls lazily reopen the handle.
//
// Close waits for in-flight extraction + resolution goroutines spawned by
// Append to complete before releasing the index handle. That serialisation
// keeps the four-layer dispatcher and prompt registry pools alive long
// enough for the extractor to write its derived events, and lets tests
// observe post-extraction state by calling Close.
func (l *FileEventLog) Close() error {
	// Shut the bounded worker pool down BEFORE waiting on bgWork. The
	// stop channel signals scheduleBackgroundEnrichment to take its
	// fallback path on new Appends (rare during Close, but possible).
	// Closing extractionQueue then lets each worker drain remaining
	// jobs, fire bgWork.Done() per job, and exit. extractionWG.Wait
	// confirms the workers themselves have exited; bgWork.Wait then
	// covers any fallback goroutines that lost the enqueue race.
	if l.extractionQueue != nil {
		// Take the schedule write-lock so any in-flight
		// scheduleBackgroundEnrichment (typically the inner Append from
		// a worker's applyExtraction) completes its send-or-spill before
		// we close the channels. Without this barrier the producer's
		// select can fire on the closed extractionQueue and panic with
		// "send on closed channel". The RLock holders are short-lived
		// (one select + bookkeeping) so the wait here is bounded.
		l.scheduleMu.Lock()
		l.shuttingDown = true
		close(l.extractionStop)
		close(l.extractionQueue)
		l.scheduleMu.Unlock()
		l.extractionWG.Wait()
	}
	l.bgWork.Wait()
	l.indexMu.Lock()
	defer l.indexMu.Unlock()
	return l.closeIndexLocked()
}

// dropIndexIfMissing closes the cached handle when the sidecar file
// no longer exists on disk. Used by At before falling back to a full
// scan: a running daemon may still hold an open fd that points at an
// unlinked inode, and writes through that fd would leave the rebuilt
// index invisible on disk. By forcing a close, we guarantee that the
// subsequent open in withIndex creates a fresh path-backed file.
func (l *FileEventLog) dropIndexIfMissing() error {
	if _, err := os.Stat(l.indexPath()); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("eventlog: stat index: %w", err)
	}
	l.indexMu.Lock()
	defer l.indexMu.Unlock()
	return l.closeIndexLocked()
}

// putIndex writes a batch of UID -> location entries in a single bbolt
// transaction. Called from Append after the JSONL fsync. A failure here
// leaves the JSONL durable on disk but the index stale; the caller
// surfaces the error so an operator notices, and the fallback scan in
// At reconciles the next time the bolt file is recreated.
func (l *FileEventLog) putIndex(entries []indexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return l.withIndex(func(db *bolt.DB) error {
		if uerr := db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(indexBucket())
			if b == nil {
				return fmt.Errorf("bucket %q missing", indexBucketName)
			}
			for _, e := range entries {
				key, kerr := encodeIndexKey(e.uid)
				if kerr != nil {
					return kerr
				}
				val := encodeIndexValue(e.day, e.offset)
				if perr := b.Put(key[:], val[:]); perr != nil {
					return fmt.Errorf("put %s: %w", e.uid, perr)
				}
			}
			return nil
		}); uerr != nil {
			return fmt.Errorf("index update: %w", uerr)
		}
		return nil
	})
}

// checkUIDsUnique rejects an Append whose batch contains duplicate
// caller-supplied UIDs, either within the batch itself or against
// events already in the log. Returning before commitPlans keeps the
// JSONL files clean: the moment a duplicate is detected no bytes have
// been written, so the caller can fix the UID and retry without
// reconciling.
//
// Newly-generated UIDs are guaranteed unique by the monotonic ULID
// entropy, so checking them against the index is harmless and lets
// this helper stay single-pass over the batch.
func (l *FileEventLog) checkUIDsUnique(ctx context.Context, events []Event) error {
	if err := rejectDuplicatesWithinBatch(events); err != nil {
		return err
	}

	// The JSONL files are the source of truth. If the sidecar is
	// missing or known stale, rebuild it from JSONL before trusting
	// the index for a uniqueness verdict. Without this, an operator-
	// deleted sidecar (or a previous putIndex failure that left the
	// index behind the log) would let a duplicate caller-supplied UID
	// slip past the check and create a second JSONL line for an
	// already-issued receipt.
	if err := l.ensureIndexReflectsLog(ctx); err != nil {
		return err
	}

	return l.withIndex(func(db *bolt.DB) error {
		if verr := db.View(func(tx *bolt.Tx) error {
			return rejectUIDsInIndex(tx, events)
		}); verr != nil {
			return fmt.Errorf("uid uniqueness check: %w", verr)
		}
		return nil
	})
}

// ensureIndexReflectsLog guarantees the bbolt sidecar mirrors every
// committed JSONL event before the caller reads from it. When the
// sidecar file is missing, any cached open handle is closed (its
// inode is unlinked and would not produce a path-backed rebuild) and
// a fresh sidecar is built by walking events/**/*.jsonl. When the
// sidecar is present, this is a no-op: putIndex keeps it up to date
// for the happy path, and a putIndex failure invalidates the sidecar
// so the next call lands in the rebuild branch above.
func (l *FileEventLog) ensureIndexReflectsLog(ctx context.Context) error {
	if _, err := os.Stat(l.indexPath()); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("eventlog: stat index: %w", err)
	}
	if cerr := l.dropIndexIfMissing(); cerr != nil {
		return cerr
	}
	return l.rebuildIndexFromLog(ctx)
}

// rebuildIndexFromLog walks events/**/*.jsonl and writes every UID's
// location into a fresh bolt sidecar. Called from
// ensureIndexReflectsLog and as the recovery half of At's fallback.
// A missing events/ directory (first run, no Appends yet) is a no-op.
func (l *FileEventLog) rebuildIndexFromLog(ctx context.Context) error {
	root := filepath.Join(l.dataDir, "events")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("eventlog: stat events dir: %w", err)
	}
	return l.withIndex(func(db *bolt.DB) error {
		if uerr := db.Update(func(tx *bolt.Tx) error {
			b, berr := ensureIndexBucket(tx)
			if berr != nil {
				return berr
			}
			var found Event
			var foundOK bool
			return l.walkAndIndex(ctx, root, b, "", &found, &foundOK)
		}); uerr != nil {
			return fmt.Errorf("rebuild index: %w", uerr)
		}
		return nil
	})
}

// invalidateIndex unlinks the bolt sidecar so the next
// ensureIndexReflectsLog rebuilds from JSONL. Called from Append after
// a putIndex failure: the JSONL bytes are durable but the index is
// behind, and accepting more Appends without reconciling would let
// duplicate caller-supplied UIDs slip past the uniqueness check.
//
// Best-effort: any error closing the handle or unlinking the file is
// returned but the caller already has a primary error in flight.
func (l *FileEventLog) invalidateIndex() error {
	if err := l.Close(); err != nil {
		return err
	}
	if err := os.Remove(l.indexPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("eventlog: remove stale index: %w", err)
	}
	return nil
}

// rejectDuplicatesWithinBatch returns ErrInvalidEvent when two events
// in the same Append batch share a UID. Caller-supplied UIDs are the
// only realistic source of this; generator UIDs are unique by
// construction.
func rejectDuplicatesWithinBatch(events []Event) error {
	seen := make(map[string]struct{}, len(events))
	for _, ev := range events {
		if _, dup := seen[ev.UID]; dup {
			return fmt.Errorf("%w: duplicate uid %q within batch", ErrInvalidEvent, ev.UID)
		}
		seen[ev.UID] = struct{}{}
	}
	return nil
}

// rejectUIDsInIndex returns ErrInvalidEvent on the first batch UID
// already present in the index. Run inside a bolt View transaction by
// the caller so the check sees a single consistent snapshot.
func rejectUIDsInIndex(tx *bolt.Tx, events []Event) error {
	b := tx.Bucket(indexBucket())
	if b == nil {
		return nil
	}
	for _, ev := range events {
		key, kerr := encodeIndexKey(ev.UID)
		if kerr != nil {
			return kerr
		}
		if b.Get(key[:]) != nil {
			return fmt.Errorf("%w: uid %q already in event log", ErrInvalidEvent, ev.UID)
		}
	}
	return nil
}

// encodeIndexKey converts a ULID string to its 16-byte binary form.
// A malformed UID escaping prepare's validation is a writer bug, not a
// caller error; return it so the Append surfaces a fatal index error
// rather than silently corrupting the index.
func encodeIndexKey(uid string) ([16]byte, error) {
	id, err := ulid.Parse(uid)
	if err != nil {
		return [16]byte{}, fmt.Errorf("encode index key %q: %w", uid, err)
	}
	return id, nil
}

// encodeIndexValue packs the day bucket and absolute byte offset into
// the fixed 12-byte record format documented at the top of this file.
func encodeIndexValue(k dateKey, offset int64) [indexValueSize]byte {
	var v [indexValueSize]byte
	binary.LittleEndian.PutUint16(v[0:2], uint16(k.year))
	v[2] = byte(k.month)
	v[3] = byte(k.day)
	binary.LittleEndian.PutUint64(v[4:12], uint64(offset))
	return v
}

// decodeIndexValue is the inverse of encodeIndexValue. Returns an error
// when the length is wrong so a partially-written or future-format
// record fails loudly instead of returning a junk location.
func decodeIndexValue(b []byte) (dateKey, int64, error) {
	if len(b) != indexValueSize {
		return dateKey{}, 0, fmt.Errorf("index value length %d, want %d", len(b), indexValueSize)
	}
	year := int(binary.LittleEndian.Uint16(b[0:2]))
	month := time.Month(b[2])
	day := int(b[3])
	offset := int64(binary.LittleEndian.Uint64(b[4:12]))
	if offset < 0 {
		return dateKey{}, 0, fmt.Errorf("index value offset negative: %d", offset)
	}
	return dateKey{year: year, month: month, day: day}, offset, nil
}

// At looks up an event by UID. It consults the bbolt index first; if
// the sidecar file is missing the writer has not yet been initialized
// (or an operator removed it) and At falls back to a full JSONL scan
// that also rebuilds the index. When the index exists and the UID is
// not in it, At returns ErrNotFound: the index is authoritative when
// present.
func (l *FileEventLog) At(ctx context.Context, uid string) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, fmt.Errorf("eventlog: context: %w", err)
	}
	if _, err := ulid.Parse(uid); err != nil {
		return Event{}, fmt.Errorf("%w: uid %q: %w", ErrInvalidEvent, uid, err)
	}

	// Detect operator-deleted sidecar. dropIndexIfMissing closes any
	// cached handle that would otherwise still write through to the
	// unlinked inode, ensuring the subsequent open in withIndex
	// recreates events/index.bolt at its path.
	if derr := l.dropIndexIfMissing(); derr != nil {
		return Event{}, derr
	}
	if _, err := os.Stat(l.indexPath()); errors.Is(err, fs.ErrNotExist) {
		return l.fallbackScanAndRebuild(ctx, uid)
	} else if err != nil {
		return Event{}, fmt.Errorf("eventlog: stat index: %w", err)
	}

	key, err := encodeIndexKey(uid)
	if err != nil {
		return Event{}, err
	}

	var (
		day    dateKey
		offset int64
		found  bool
	)
	if verr := l.withIndex(func(db *bolt.DB) error {
		return db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(indexBucket())
			if b == nil {
				return nil
			}
			v := b.Get(key[:])
			if v == nil {
				return nil
			}
			d, o, derr := decodeIndexValue(v)
			if derr != nil {
				return derr
			}
			day = d
			offset = o
			found = true
			return nil
		})
	}); verr != nil {
		return Event{}, fmt.Errorf("eventlog: index lookup: %w", verr)
	}
	if !found {
		return Event{}, ErrNotFound
	}

	return l.readEventAt(ctx, day, offset)
}

// readEventAt reads the JSONL line at the given byte offset and
// unmarshals it. Any decode or path mismatch returns the zero Event so
// callers never see a partial result.
func (l *FileEventLog) readEventAt(ctx context.Context, day dateKey, offset int64) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, fmt.Errorf("eventlog: context: %w", err)
	}
	path := day.path(l.dataDir)
	// #nosec G304 -- path is composed from the writer's dataDir and a
	// dateKey decoded from the index this writer populated.
	f, err := os.Open(path)
	if err != nil {
		return Event{}, fmt.Errorf("eventlog: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return Event{}, fmt.Errorf("eventlog: seek %s @%d: %w", path, offset, err)
	}
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Event{}, fmt.Errorf("eventlog: read %s @%d: %w", path, offset, err)
	}
	line = trimTrailingNewline(line)
	if len(line) == 0 {
		return Event{}, fmt.Errorf("eventlog: empty line at %s @%d", path, offset)
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, fmt.Errorf("eventlog: unmarshal %s @%d: %w", path, offset, err)
	}
	return ev, nil
}

func trimTrailingNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		return b[:n-1]
	}
	return b
}

// fallbackScanAndRebuild walks events/**/*.jsonl, records every UID's
// location into the bolt sidecar, and returns the event matching uid
// (or ErrNotFound). It is the recovery path when the sidecar file is
// missing entirely (first run, operator deleted it, or a corrupt
// sidecar was removed). Cost is O(log size); acceptable for a rare
// recovery, and the rebuild leaves a fresh index behind so subsequent
// At calls are fast again.
//
// The scan and the rebuild Put happen inside the same bolt Update so
// either both land or neither does. If the UID isn't found after the
// full walk, the index is still rebuilt and ErrNotFound is returned.
func (l *FileEventLog) fallbackScanAndRebuild(ctx context.Context, uid string) (Event, error) {
	root := filepath.Join(l.dataDir, "events")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return Event{}, ErrNotFound
	} else if err != nil {
		return Event{}, fmt.Errorf("eventlog: stat events dir: %w", err)
	}

	var found Event
	var foundOK bool

	if uerr := l.withIndex(func(db *bolt.DB) error {
		return db.Update(func(tx *bolt.Tx) error {
			b, berr := ensureIndexBucket(tx)
			if berr != nil {
				return berr
			}
			return l.walkAndIndex(ctx, root, b, uid, &found, &foundOK)
		})
	}); uerr != nil {
		return Event{}, fmt.Errorf("eventlog: fallback scan: %w", uerr)
	}
	if !foundOK {
		return Event{}, ErrNotFound
	}
	return found, nil
}

// ensureIndexBucket returns the events bucket inside tx, creating it
// when missing. Pulled out of fallbackScanAndRebuild so the gocognit
// budget for the caller stays low.
func ensureIndexBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	if b := tx.Bucket(indexBucket()); b != nil {
		return b, nil
	}
	created, err := tx.CreateBucket(indexBucket())
	if err != nil {
		return nil, fmt.Errorf("create bucket: %w", err)
	}
	return created, nil
}

// walkAndIndex iterates every .jsonl under root and feeds each file to
// scanJSONLForIndex. Returning a *fmt-wrapped* ctx error keeps the
// linter happy without changing semantics.
func (l *FileEventLog) walkAndIndex(
	ctx context.Context,
	root string,
	b *bolt.Bucket,
	wantUID string,
	found *Event,
	foundOK *bool,
) error {
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("ctx: %w", cerr)
		}
		return scanJSONLForIndex(ctx, path, l.dataDir, b, wantUID, found, foundOK)
	})
	if walkErr != nil {
		return fmt.Errorf("walk events: %w", walkErr)
	}
	return nil
}

// scanJSONLForIndex reads one JSONL file line by line, recording each
// event's offset into b. When an event's UID matches the requested uid,
// the decoded event is captured for return to the caller. The day key
// is recovered from the file's path so we do not have to trust the
// event payload for path reconstruction.
func scanJSONLForIndex(
	ctx context.Context,
	path string,
	dataDir string,
	b *bolt.Bucket,
	wantUID string,
	found *Event,
	foundOK *bool,
) error {
	day, ok := dateKeyFromPath(dataDir, path)
	if !ok {
		// File is under events/ but not in the YYYY/MM/DD.jsonl
		// layout. Skip rather than fail: an operator may have left
		// auxiliary files in the directory.
		return nil
	}

	// #nosec G304 -- path comes from WalkDir over the writer's dataDir.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	var offset int64
	for {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("ctx: %w", cerr)
		}
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if perr := indexLine(b, line, day, offset, path, wantUID, found, foundOK); perr != nil {
				return perr
			}
			offset += int64(len(line))
		}
		if rerr == nil {
			continue
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		return fmt.Errorf("read %s @%d: %w", path, offset, rerr)
	}
}

// indexLine decodes one JSONL line, writes its (uid -> location) record
// to b, and captures the event if its UID matches wantUID. Lifted out
// of scanJSONLForIndex so each function stays under the gocognit
// threshold.
func indexLine(
	b *bolt.Bucket,
	line []byte,
	day dateKey,
	offset int64,
	path string,
	wantUID string,
	found *Event,
	foundOK *bool,
) error {
	trimmed := trimTrailingNewline(line)
	if len(trimmed) == 0 {
		return nil
	}
	var ev Event
	if uerr := json.Unmarshal(trimmed, &ev); uerr != nil {
		return fmt.Errorf("unmarshal %s @%d: %w", path, offset, uerr)
	}
	key, kerr := encodeIndexKey(ev.UID)
	if kerr != nil {
		return kerr
	}
	val := encodeIndexValue(day, offset)
	if perr := b.Put(key[:], val[:]); perr != nil {
		return fmt.Errorf("put %s: %w", ev.UID, perr)
	}
	if ev.UID == wantUID {
		*found = ev
		*foundOK = true
	}
	return nil
}

// dateKeyFromPath recovers the day bucket from a file path of the form
// <dataDir>/events/YYYY/MM/DD.jsonl. Returns false on any structural
// mismatch so the walker skips unknown layouts instead of erroring.
func dateKeyFromPath(dataDir, path string) (dateKey, bool) {
	rel, err := filepath.Rel(filepath.Join(dataDir, "events"), path)
	if err != nil {
		return dateKey{}, false
	}
	parts := splitPath(rel)
	if len(parts) != 3 {
		return dateKey{}, false
	}
	year, yerr := parseIntStrict(parts[0])
	if yerr != nil {
		return dateKey{}, false
	}
	month, merr := parseIntStrict(parts[1])
	if merr != nil || month < 1 || month > 12 {
		return dateKey{}, false
	}
	dayName := parts[2]
	if filepath.Ext(dayName) != ".jsonl" {
		return dateKey{}, false
	}
	day, derr := parseIntStrict(dayName[:len(dayName)-len(".jsonl")])
	if derr != nil || day < 1 || day > 31 {
		return dateKey{}, false
	}
	return dateKey{year: year, month: time.Month(month), day: day}, true
}

// splitPath splits a filepath.Rel result into its forward components on
// the host separator. Filepath.SplitList would be wrong (that splits on
// the PATH separator); strings.Split with filepath.Separator is right
// but unwieldy, hence this tiny helper.
func splitPath(rel string) []string {
	var parts []string
	for {
		dir, name := filepath.Split(rel)
		if name != "" {
			parts = append([]string{name}, parts...)
		}
		if dir == "" || dir == string(filepath.Separator) {
			break
		}
		rel = filepath.Clean(dir)
		if rel == "." {
			break
		}
	}
	return parts
}

// parseIntStrict accepts only base-10 unsigned integers. The dateKey
// path components are zero-padded fixed-width fields, so anything
// outside that shape should fail and skip the file rather than panic.
func parseIntStrict(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit %q", r)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
