package kernel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// eventFile is a single day-bucket JSONL file paired with the UTC midnight
// of the day it covers. Read sorts these chronologically before opening.
type eventFile struct {
	path string
	day  time.Time // UTC midnight; the file covers [day, day+24h).
}

// Read streams events in CreateTime order (with lexicographic UID
// tiebreak) from the JSONL day buckets under dataDir, honoring the
// EventLog.Read contract at eventlog.go:94. Each call performs a fresh
// directory walk and opens each day file lazily as the iterator
// advances. The iterator is single-pass across the entire log: each
// day file is read once into memory, sorted, and yielded; the iterator
// never revisits a file.
//
// Concurrency: Read acquires l.mu.RLock briefly per file to open the
// handle and snapshot a fully-committed byte boundary, then releases
// the lock before iterating. Two guarantees follow:
//
//   - Concurrent Read calls do not block each other (multiple RLocks
//     coexist).
//   - Reads see fully-written events only. An RLock cannot be held
//     while a writer holds the Lock, so a Read never observes the
//     page-cache window between the writer's f.Write and a successful
//     f.Sync. Bytes past the snapshot are clamped by io.LimitReader,
//     so a concurrent Append's in-flight bytes (or a later rollback)
//     never enter this iteration.
//
// Filters in ReadOpts apply in-iterator: Since (inclusive lower on
// CreateTime), Until (exclusive upper), Kinds (allow-list), and Limit
// (hard cap on yielded events). Parse errors surface typed via
// yield(Event{}, err) and stop the iterator; the spec is explicit
// that bad lines must not be skipped silently.
func (l *FileEventLog) Read(ctx context.Context, opts ReadOpts) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		files, err := listEventFiles(l.dataDir)
		if err != nil {
			yield(Event{}, fmt.Errorf("eventlog: list files: %w", err))
			return
		}
		emitted := 0
		for _, ef := range files {
			if opts.Limit > 0 && emitted >= opts.Limit {
				return
			}
			if !dayOverlaps(ef.day, opts.Since, opts.Until) {
				continue
			}
			if err := ctx.Err(); err != nil {
				yield(Event{}, fmt.Errorf("eventlog: context: %w", err))
				return
			}
			if !l.readEventFile(ctx, ef.path, opts, &emitted, yield) {
				return
			}
		}
	}
}

// listEventFiles walks dataDir/events and returns every YYYY/MM/DD.jsonl
// file with its parsed UTC date, sorted chronologically. Entries that do
// not match the writer's layout are skipped silently: the writer never
// emits them, so they are externally introduced and not our concern.
// Returns (nil, nil) when events/ does not exist.
func listEventFiles(dataDir string) ([]eventFile, error) {
	root := filepath.Join(dataDir, "events")
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}

	var files []eventFile
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if ef, ok := parseEventPath(root, p); ok {
			files = append(files, ef)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", root, walkErr)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].day.Before(files[j].day)
	})
	return files, nil
}

// parseEventPath turns a YYYY/MM/DD.jsonl path under root into an
// eventFile. Returns ok=false for any path that does not match the
// writer's layout.
func parseEventPath(root, fullPath string) (eventFile, bool) {
	rel, err := filepath.Rel(root, fullPath)
	if err != nil {
		return eventFile{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 {
		return eventFile{}, false
	}
	if !strings.HasSuffix(parts[2], ".jsonl") {
		return eventFile{}, false
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 1 {
		return eventFile{}, false
	}
	month, err := strconv.Atoi(parts[1])
	if err != nil || month < 1 || month > 12 {
		return eventFile{}, false
	}
	day, err := strconv.Atoi(strings.TrimSuffix(parts[2], ".jsonl"))
	if err != nil || day < 1 || day > 31 {
		return eventFile{}, false
	}
	return eventFile{
		path: fullPath,
		day:  time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC),
	}, true
}

// dayOverlaps reports whether a UTC day [midnight, midnight+24h) could
// contain events that satisfy [since, until). A zero Since means
// open-on-the-left; a zero Until means open-on-the-right.
func dayOverlaps(midnight, since, until time.Time) bool {
	dayEnd := midnight.Add(24 * time.Hour)
	if !since.IsZero() && !dayEnd.After(since) {
		return false
	}
	if !until.IsZero() && !midnight.Before(until) {
		return false
	}
	return true
}

// passesFilters applies the Since / Until / Kinds filters. Limit is
// applied by the caller, which tracks the emitted count across files.
func passesFilters(ev Event, opts ReadOpts) bool {
	if !opts.Since.IsZero() && ev.CreateTime.Before(opts.Since) {
		return false
	}
	if !opts.Until.IsZero() && !ev.CreateTime.Before(opts.Until) {
		return false
	}
	if len(opts.Kinds) > 0 {
		match := false
		for _, k := range opts.Kinds {
			if k == ev.Kind {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// readEventFile loads one day-bucket JSONL, sorts its events by
// (CreateTime, UID), and yields them through the iterator. Returns
// false when iteration must stop entirely (caller broke, ctx canceled,
// hard read error, or Limit reached); returns true to continue with
// the next file.
//
// Chronological sort: the EventLog.Read contract (eventlog.go:94)
// requires events in CreateTime order with ULID tiebreak. The writer
// sorts within a single Append batch, but cross-batch backfills that
// land on the same day still arrive in append order on disk. Sorting
// the day's events in memory before yielding is the simplest way to
// honor the contract; the cost is bounded by one day's worth of events.
//
// Parse-error visibility (Greptile PR #91 findings 6 + 7):
//
//   - Errors at file lines BEFORE any yielded event must surface; the
//     caller cannot receive an event from past a corrupt prefix without
//     knowing the prefix is corrupt.
//   - Errors at file lines AFTER all yielded events can be masked: a
//     bounded consumer (Limit, caller break) that took only a prefix
//     of pre-error events should not have its read fail because of
//     downstream corruption.
//
// readEventFile splits the loaded events into pre-error (load-line
// < error-line) and discards post-error events. Pre-error events are
// sorted chronologically and yielded; the deferred error is surfaced
// only after pre-error events are exhausted, never before.
//
// Concurrency: openCommitted acquires l.mu.RLock for the duration of
// the open + stat call so the snapshotted size is a fully-committed
// boundary (no writer can hold l.mu.Lock simultaneously). The lock is
// then released before the load, and io.LimitReader bounds the read to
// the snapshot.
func (l *FileEventLog) readEventFile(ctx context.Context, path string, opts ReadOpts, emitted *int, yield func(Event, error) bool) bool {
	recs, ok, deferredErrLine, deferredErr := l.loadEventFile(ctx, path, yield)
	if !ok {
		return false
	}

	pre := preErrorEvents(recs, deferredErrLine)
	sortDayEvents(pre)
	for _, ev := range pre {
		if err := ctx.Err(); err != nil {
			yield(Event{}, fmt.Errorf("eventlog: context: %w", err))
			return false
		}
		if !passesFilters(ev, opts) {
			continue
		}
		if !yield(ev, nil) {
			return false
		}
		*emitted++
		if opts.Limit > 0 && *emitted >= opts.Limit {
			return false
		}
	}
	// Pre-error events drained. If a parse error was deferred, surface
	// it now so the caller learns about the corruption before any
	// post-error event could ever be yielded.
	if deferredErr != nil {
		yield(Event{}, deferredErr)
		return false
	}
	return true
}

// eventRec pairs a parsed Event with the load-order line number it
// came from so the reader can decide whether a deferred parse error
// happens before or after it in the file.
type eventRec struct {
	ev   Event
	line int
}

// preErrorEvents returns the events whose load-line is strictly less
// than errLine, i.e. events that appear in the file BEFORE the first
// parse error. When errLine is 0 (no error), every event is returned.
// Post-error events are deliberately dropped: yielding them would mean
// returning data from past a corrupt section without first telling the
// caller about the corruption.
func preErrorEvents(recs []eventRec, errLine int) []Event {
	out := make([]Event, 0, len(recs))
	for _, r := range recs {
		if errLine == 0 || r.line < errLine {
			out = append(out, r.ev)
		}
	}
	return out
}

// loadEventFile reads every fully-terminated JSONL line up to the
// committed-size snapshot. Each successful parse is recorded with its
// source line; the first parse failure is captured as a deferred
// error along with its line. Subsequent lines are still scanned so
// readEventFile can split events into pre-error vs post-error.
//
// Returns (recs, true, deferredErrLine, deferredErr) on success. The
// deferred fields are zero-valued when no parse error was seen. Returns
// (nil, false, 0, nil) when a hard read or context error is yielded.
func (l *FileEventLog) loadEventFile(ctx context.Context, path string, yield func(Event, error) bool) ([]eventRec, bool, int, error) {
	f, committedSize, err := l.openCommitted(path)
	if err != nil {
		yield(Event{}, fmt.Errorf("eventlog: open %s: %w", path, err))
		return nil, false, 0, nil
	}
	defer f.Close()

	r := bufio.NewReader(io.LimitReader(f, committedSize))
	recs := make([]eventRec, 0, 64)
	var deferredErr error
	var deferredErrLine int
	var lineNum int
	for {
		if err := ctx.Err(); err != nil {
			yield(Event{}, fmt.Errorf("eventlog: context: %w", err))
			return nil, false, 0, nil
		}

		line, readErr := r.ReadBytes('\n')
		if isCompleteLine(line) {
			lineNum++
			var ev Event
			if perr := json.Unmarshal(line[:len(line)-1], &ev); perr != nil {
				if deferredErr == nil {
					deferredErr = fmt.Errorf("eventlog: %s line %d: %w", path, lineNum, perr)
					deferredErrLine = lineNum
				}
				continue
			}
			recs = append(recs, eventRec{ev: ev, line: lineNum})
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return recs, true, deferredErrLine, deferredErr
			}
			yield(Event{}, fmt.Errorf("eventlog: read %s: %w", path, readErr))
			return nil, false, 0, nil
		}
	}
}

// sortDayEvents sorts in-place by (EffectiveHLC ascending, UID ascending).
//
// EffectiveHLC returns the real HLC for post-wave events and a derived implicit
// HLC (Encode(CreateTime.UnixMilli(), 0)) for legacy events that predate the HLC
// wave. Because the implicit HLC is a pure function of the immutable CreateTime,
// legacy-only logs keep their exact current (CreateTime, UID) ordering: same wall
// millisecond means the same implicit HLC, and the UID tiebreaker is unchanged.
//
// Stable sort isn't required because UID is unique by ULID invariant.
func sortDayEvents(events []Event) {
	sort.Slice(events, func(i, j int) bool {
		hi := events[i].EffectiveHLC()
		hj := events[j].EffectiveHLC()
		if hi != hj {
			return hi < hj
		}
		return events[i].UID < events[j].UID
	})
}

// openCommitted opens path and snapshots its size while holding l.mu's
// RLock. Holding the RLock blocks any writer (which needs l.mu.Lock to
// run Append), so the size we observe is by definition the end of the
// last fully-committed batch — never the page-cache view between a
// writer's f.Write and a successful f.Sync, and never a soon-to-be-
// rolled-back size. After this call returns, the caller can read up to
// committedSize lock-free; concurrent Appends can extend the file or
// roll back later writes without affecting the snapshot.
func (l *FileEventLog) openCommitted(path string) (*os.File, int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	// #nosec G304 -- path comes from listEventFiles, which only emits
	// files under the writer's internal dataDir.
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open: %w", err)
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat: %w", statErr)
	}
	return f, info.Size(), nil
}

// isCompleteLine reports whether line ends with a newline, the writer's
// signal that the line was fully committed before any reader observed
// the file.
func isCompleteLine(line []byte) bool {
	return len(line) > 0 && line[len(line)-1] == '\n'
}
