package chunk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// Store is the chunk-persistence boundary the sealer and the
// transport layer share. FS is the on-disk impl; tests use the
// in-memory MemStore.
type Store interface {
	// Put writes a chunk to the store. blob is the Encode output; the
	// caller has already sealed + framed it.
	Put(ctx context.Context, scope kernel.Scope, hdr Header, blob []byte) error

	// Get returns the raw chunk bytes for a known chunk ID, suitable
	// for passing to Decode. Returns ErrNotFound if the chunk is absent.
	Get(ctx context.Context, scope kernel.Scope, chunkID string) ([]byte, error)

	// List returns refs for every chunk in scope, sorted by chunkID
	// (ULID lex = seal-time order).
	List(ctx context.Context, scope kernel.Scope) ([]Ref, error)
}

// ErrNotFound is returned by Store.Get when the chunk does not exist.
var ErrNotFound = errors.New("chunk: not found")

// FSStore is the on-disk Store implementation. Chunks live at
// PathFor(dataDir, scope, chunkID); the chunks directory is created
// with 0o700 owner-only mode (defence in depth on macOS where /tmp
// is multi-user).
type FSStore struct {
	dataDir string
}

// NewFSStore returns a filesystem-backed Store rooted at dataDir.
// DataDir must already exist; per-scope chunk subdirectories are
// created lazily on first Put.
func NewFSStore(dataDir string) (*FSStore, error) {
	if dataDir == "" {
		return nil, errors.New("chunk: data dir is required")
	}
	return &FSStore{dataDir: dataDir}, nil
}

// Put writes blob atomically: temp file + rename onto the final
// path. The chunks subdirectory is created with 0o700 if absent.
// The on-disk filename extension is selected from hdr.Encrypted:
// encrypted chunks land at <chunkID>.maz, plaintext chunks at
// <chunkID>.jsonl. Callers that already chose a mode via Encode
// just pass the returned Header back through.
func (s *FSStore) Put(_ context.Context, scope kernel.Scope, hdr Header, blob []byte) error {
	if hdr.ChunkID == "" {
		return errors.New("chunk: header missing ChunkID")
	}
	dir := DirFor(s.dataDir, scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("chunk: mkdir %s: %w", dir, err)
	}
	hdr.Scope = scope // ensure path derivation honours the caller's scope arg
	final := PathForHeader(s.dataDir, hdr)
	tmp, err := os.CreateTemp(dir, ".tmp-chunk-*")
	if err != nil {
		return fmt.Errorf("chunk: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			_ = rmErr
		}
	}
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chunk: write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chunk: chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chunk: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("chunk: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		cleanup()
		return fmt.Errorf("chunk: rename: %w", err)
	}
	return nil
}

// Get reads chunk bytes from disk. A missing chunk returns
// ErrNotFound. Tries the encrypted extension first, then falls back
// to the plaintext extension so callers do not need to know which
// mode produced a given chunkID — useful for mixed-mode scopes that
// accumulated chunks under both modes during an opt-in transition.
func (s *FSStore) Get(_ context.Context, scope kernel.Scope, chunkID string) ([]byte, error) {
	for _, ext := range []string{Extension, ExtensionJSONL} {
		path := filepath.Join(DirFor(s.dataDir, scope), chunkID+ext)
		blob, err := os.ReadFile(path) //nolint:gosec // path is constructed from internal layout + caller-controlled scope/id
		if err == nil {
			return blob, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("chunk: read %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("%w: scope=%s id=%s", ErrNotFound, scope, chunkID)
}

// List walks the scope's chunks directory and returns Refs for every
// chunk file regardless of mode (.maz encrypted, .jsonl plaintext).
// Each Ref carries the plaintext header (no decryption performed).
// Missing scope directory returns an empty slice. The returned slice
// is sorted by ChunkID, which is ULID lex == seal-time order across
// both modes.
func (s *FSStore) List(_ context.Context, scope kernel.Scope) ([]Ref, error) {
	dir := DirFor(s.dataDir, scope)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("chunk: readdir %s: %w", dir, err)
	}
	out := make([]Ref, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isChunkFilename(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		blob, rerr := os.ReadFile(path) //nolint:gosec // path is bounded by ReadDir of an internal layout dir
		if rerr != nil {
			return nil, fmt.Errorf("chunk: read %s: %w", path, rerr)
		}
		hdr, perr := PeekHeader(blob)
		if perr != nil {
			// A single unreadable file must never brick the whole listing —
			// that turns one stale artifact into a total sync outage (the user
			// hit `maz sync push/pull` aborting on one leftover 0x01 chunk).
			// Two classes are safe to skip because they carry no data we are
			// obliged to transport: a pre-0x02 MAZ1 chunk (ErrBadVersion — a
			// deliberate clean-cut, and its events remain durable in the event
			// log) and a stray file with a chunk extension that is not a chunk
			// at all (ErrBadMagic). Genuine corruption of an otherwise-valid
			// chunk (truncated/garbled header) still surfaces, so an operator
			// sees real damage rather than us silently dropping live data.
			if errors.Is(perr, ErrBadVersion) || errors.Is(perr, ErrBadMagic) {
				continue
			}
			return nil, fmt.Errorf("chunk: peek %s: %w", path, perr)
		}
		out = append(out, Ref{Header: hdr, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkID < out[j].ChunkID })
	return out, nil
}

// isChunkFilename returns true when name has a recognised chunk
// extension (.maz encrypted, .jsonl plaintext). Centralises the
// extension allow-list so the router only has one source of truth.
func isChunkFilename(name string) bool {
	return strings.HasSuffix(name, Extension) || strings.HasSuffix(name, ExtensionJSONL)
}

// MemStore is an in-memory Store for tests. Not safe for concurrent
// use; tests that need parallel access should wrap with a sync.Mutex.
type MemStore struct {
	chunks map[string]map[string]memEntry // scope -> chunkID -> entry
}

type memEntry struct {
	hdr  Header
	blob []byte
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{chunks: map[string]map[string]memEntry{}}
}

// Put records the chunk in memory; existing chunks with the same ID
// are overwritten (matches the FS rename semantics).
func (m *MemStore) Put(_ context.Context, scope kernel.Scope, hdr Header, blob []byte) error {
	if m.chunks[string(scope)] == nil {
		m.chunks[string(scope)] = map[string]memEntry{}
	}
	cp := make([]byte, len(blob))
	copy(cp, blob)
	m.chunks[string(scope)][hdr.ChunkID] = memEntry{hdr: hdr, blob: cp}
	return nil
}

// Get returns the chunk bytes for a known ID, or ErrNotFound.
func (m *MemStore) Get(_ context.Context, scope kernel.Scope, chunkID string) ([]byte, error) {
	if scopeMap, ok := m.chunks[string(scope)]; ok {
		if entry, ok := scopeMap[chunkID]; ok {
			cp := make([]byte, len(entry.blob))
			copy(cp, entry.blob)
			return cp, nil
		}
	}
	return nil, fmt.Errorf("%w: scope=%s id=%s", ErrNotFound, scope, chunkID)
}

// List returns refs sorted by chunkID for the named scope.
func (m *MemStore) List(_ context.Context, scope kernel.Scope) ([]Ref, error) {
	scopeMap, ok := m.chunks[string(scope)]
	if !ok {
		return nil, nil
	}
	out := make([]Ref, 0, len(scopeMap))
	for _, entry := range scopeMap {
		out = append(out, Ref{Header: entry.hdr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkID < out[j].ChunkID })
	return out, nil
}
