package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ReStampAuthors retro-fits a signed EventAuthor onto every legacy (unsigned)
// event in the JSONL log under dataDir, signing each with signer. It is the
// single sanctioned exception to the append-only "never rewrite an event row"
// rule (AGENTS.md §3): semantically lossless (only the author block is added;
// UID, CreateTime, Scope, Kind, Source, Confidence, and the verbatim Payload
// are preserved), crash-safe (per-file tmp + fsync + atomic rename + parent
// fsync), and idempotent (a file whose every line already carries a signature
// is left byte-for-byte untouched).
//
// It MUST run with the daemon stopped — it is not concurrency-safe against a
// live writer. After rewriting it removes the bbolt offset sidecar
// (events/index.bolt): line byte-lengths changed, so the next event-log open
// rebuilds the offset index from the rewritten JSONL. Returns the number of
// events re-stamped.
func ReStampAuthors(dataDir string, signer Signer) (int, error) {
	if signer == nil {
		return 0, errors.New("kernel: ReStampAuthors requires a signer")
	}
	eventsDir := filepath.Join(dataDir, "events")
	files, err := collectEventFiles(eventsDir)
	if err != nil {
		return 0, err
	}

	// Invalidate the offset sidecar BEFORE rewriting any file. The first
	// rewrite shifts every subsequent byte offset, so the sidecar must not
	// outlive it. Removing it up front means a crash mid-walk can only leave
	// the index absent (a fresh rebuild on next open), never stale-but-present
	// — which would mmap wrong offsets against the rewritten JSONL once the
	// log reads as fully signed. The one-time rebuild cost on an already-signed
	// log (where nothing changes) is negligible for this rare migration.
	if rerr := os.Remove(filepath.Join(eventsDir, "index.bolt")); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return 0, fmt.Errorf("kernel: invalidate offset index: %w", rerr)
	}

	total := 0
	for _, path := range files {
		n, rerr := reStampFile(path, signer)
		if rerr != nil {
			return total, fmt.Errorf("kernel: re-stamp %s: %w", path, rerr)
		}
		total += n
	}
	return total, nil
}

// LogNeedsSigning reports whether the event log under dataDir contains any
// unsigned event — used by daemon boot to refuse startup on a legacy log and
// direct the operator to `maz migrate sign-events`. It scans every event line
// of every daily bucket, returning true at the first unsigned line. Scanning
// the whole bucket (not just its first line) is what makes a *mixed* bucket —
// e.g. legacy events followed by signed ones after an interrupted migration,
// or a signed file an older binary later appended unsigned events to — fail
// the boot check rather than slip through. An empty or absent log needs no
// signing.
func LogNeedsSigning(dataDir string) (bool, error) {
	files, err := collectEventFiles(filepath.Join(dataDir, "events"))
	if err != nil {
		return false, err
	}
	for _, path := range files {
		signed, perr := fileFullySigned(path)
		if perr != nil {
			return false, fmt.Errorf("kernel: inspect %s: %w", path, perr)
		}
		if !signed {
			return true, nil
		}
	}
	return false, nil
}

// collectEventFiles returns every YYYY/MM/DD.jsonl bucket under eventsDir,
// sorted, skipping the bbolt sidecar and any non-JSONL entry. A missing events
// directory yields an empty list (a fresh data dir has no log yet).
func collectEventFiles(eventsDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(eventsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("kernel: walk event log: %w", err)
	}
	return files, nil
}

// reStampFile rewrites one daily bucket. Already-signed lines are copied
// verbatim; unsigned lines are signed and re-encoded. The file is only
// rewritten when at least one line changed, so a fully-signed bucket is a
// no-op (idempotent). Returns the number of lines re-stamped in this file.
func reStampFile(path string, signer Signer) (int, error) {
	// #nosec G304 -- path comes from WalkDir over the writer's own dataDir.
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read: %w", err)
	}

	var out bytes.Buffer
	out.Grow(len(raw) + len(raw)/8)
	changed := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue // skip blank lines (notably the trailing newline split)
		}
		var ev Event
		if uerr := json.Unmarshal([]byte(line), &ev); uerr != nil {
			return 0, fmt.Errorf("decode line: %w", uerr)
		}
		if len(ev.Author.Sig) > 0 {
			out.WriteString(line) // already signed: preserve exact bytes
			out.WriteByte('\n')
			continue
		}
		if serr := SignEventAuthor(&ev, signer); serr != nil {
			return 0, fmt.Errorf("sign event %s: %w", ev.UID, serr)
		}
		encoded, merr := json.Marshal(ev)
		if merr != nil {
			return 0, fmt.Errorf("encode event %s: %w", ev.UID, merr)
		}
		out.Write(encoded)
		out.WriteByte('\n')
		changed++
	}

	if changed == 0 {
		return 0, nil
	}
	if werr := writeFileAtomic(path, out.Bytes()); werr != nil {
		return 0, werr
	}
	return changed, nil
}

// fileFullySigned reports whether EVERY non-blank event line of path carries a
// signature. It scans all lines and returns false as soon as it finds an
// unsigned one, so a mixed bucket (some signed, some legacy) is reported as not
// fully signed rather than passing on the strength of its first line. An empty
// file reports signed=true (nothing to sign).
func fileFullySigned(path string) (bool, error) {
	// #nosec G304 -- path comes from WalkDir over the writer's own dataDir.
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev Event
		if uerr := json.Unmarshal([]byte(line), &ev); uerr != nil {
			return false, fmt.Errorf("decode line: %w", uerr)
		}
		if len(ev.Author.Sig) == 0 {
			return false, nil
		}
	}
	return true, nil
}

// writeFileAtomic writes data to path crash-safely: write to a sibling .tmp,
// fsync it, rename over the target (atomic on POSIX), then fsync the parent
// directory so the rename itself is durable.
func writeFileAtomic(path string, data []byte) (retErr error) {
	tmp := path + ".tmp"
	// #nosec G304 -- tmp is derived from a WalkDir path under the writer's dataDir.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup of a failed temp write
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	if err := fsyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("fsync dir: %w", err)
	}
	return nil
}
