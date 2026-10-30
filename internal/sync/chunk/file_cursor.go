package chunk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// FileCursor is the production CursorStore: one append-only JSONL
// file per scope at <dataDir>/sync/<scope>/sealed.jsonl, each line a
// single sealed UID. The in-memory set is hydrated lazily on first
// Sealed() per scope. MarkSealed appends to the file, fsyncs, then
// updates the in-memory set so a crash between the two leaves the
// next process with an authoritative on-disk record.
//
// Format choice: JSONL of raw ULIDs (one per line, no JSON wrapper)
// keeps the file forensic-friendly (`wc -l`, `grep`) and the parse
// cost negligible. The file's monotone append-only property lets a
// crash mid-write leave at most a partial last line — handled by
// the loader (skip empty + skip non-ULID).
type FileCursor struct {
	root string

	mu    sync.Mutex
	cache map[kernel.Scope]map[string]struct{}
}

// NewFileCursor returns a cursor rooted at <dataDir>/sync. The
// directory is NOT created here; per-scope dirs land lazily in
// MarkSealed so a daemon that never seals leaves no on-disk
// footprint for that subsystem.
func NewFileCursor(dataDir string) (*FileCursor, error) {
	if dataDir == "" {
		return nil, errors.New("chunk: data dir is required")
	}
	return &FileCursor{
		root:  filepath.Join(dataDir, "sync"),
		cache: map[kernel.Scope]map[string]struct{}{},
	}, nil
}

// Sealed returns the set of UIDs already sealed for scope. The first
// call per scope reads <root>/<scope>/sealed.jsonl into memory; later
// calls return the cached set. A missing file is treated as an empty
// set (the scope has never sealed).
func (c *FileCursor) Sealed(_ context.Context, scope kernel.Scope) (map[string]struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.cache[scope]; ok {
		return existing, nil
	}
	set, err := c.loadLocked(scope)
	if err != nil {
		return nil, err
	}
	c.cache[scope] = set
	return set, nil
}

// MarkSealed appends uids to <root>/<scope>/sealed.jsonl atomically
// (open with O_APPEND, write all lines, fsync, close) and updates the
// in-memory set. A partial failure (e.g. ENOSPC mid-write) leaves the
// file at the pre-call length thanks to the single-Write call; the
// in-memory cache is updated only on success.
func (c *FileCursor) MarkSealed(_ context.Context, scope kernel.Scope, uids []string) error {
	if len(uids) == 0 {
		return nil
	}
	if !scope.Valid() {
		return fmt.Errorf("chunk: invalid scope %q", scope)
	}
	dir := filepath.Join(c.root, string(scope))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("chunk: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "sealed.jsonl")

	// Build the payload in one buffer so the kernel sees a single
	// write call — short writes on local FS are rare but the single
	// Write avoids any chance of an interleaved torn line if multiple
	// daemons ever race the file.
	buf := make([]byte, 0, len(uids)*27)
	for _, uid := range uids {
		buf = append(buf, uid...)
		buf = append(buf, '\n')
	}

	// #nosec G304 -- path is filepath.Join(validated dataDir, validated
	// scope, fixed basename "sealed.jsonl"); no operator-supplied path
	// component reaches OpenFile.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("chunk: open %s: %w", path, err)
	}
	if _, werr := f.Write(buf); werr != nil {
		_ = f.Close()
		return fmt.Errorf("chunk: append %s: %w", path, werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return fmt.Errorf("chunk: fsync %s: %w", path, serr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("chunk: close %s: %w", path, cerr)
	}

	c.mu.Lock()
	set, ok := c.cache[scope]
	if !ok {
		set = map[string]struct{}{}
		c.cache[scope] = set
	}
	for _, uid := range uids {
		set[uid] = struct{}{}
	}
	c.mu.Unlock()
	return nil
}

func (c *FileCursor) loadLocked(scope kernel.Scope) (map[string]struct{}, error) {
	set := map[string]struct{}{}
	if !scope.Valid() {
		return set, fmt.Errorf("chunk: invalid scope %q", scope)
	}
	path := filepath.Join(c.root, string(scope), "sealed.jsonl")
	// #nosec G304 -- path components are validated (dataDir, scope.Valid)
	// plus a fixed basename; no operator-supplied path piece reaches Open.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return set, nil
		}
		return nil, fmt.Errorf("chunk: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// ULIDs are 26 bytes; budget for line-length is small but
	// allow a generous buffer in case a future schema bumps it.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		uid := scanner.Text()
		if uid == "" {
			continue
		}
		// A torn last line at a crash boundary will not match the
		// ULID alphabet; ignore it rather than fail-startup.
		if !looksLikeULID(uid) {
			continue
		}
		set[uid] = struct{}{}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("chunk: scan %s: %w", path, err)
	}
	return set, nil
}

// looksLikeULID is a fast filter for a Crockford-base32 26-char
// upper-case token. Sufficient for skipping torn writes; we don't
// re-validate the strict ULID alphabet here because every entry was
// produced by a ulid.MustParse-validated UID upstream.
func looksLikeULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'H':
		case c == 'J' || c == 'K':
		case c >= 'M' && c <= 'N':
		case c >= 'P' && c <= 'T':
		case c >= 'V' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}
