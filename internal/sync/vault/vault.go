// Package vault materializes event-sourced documents as an editable markdown
// directory — the human surface of the document layer. The vault is DERIVED
// state: every file is reproducible from documentRevision events, so the
// directory can be deleted and rebuilt (`maz rebuild`) without data loss.
//
// Invariants (plan §E):
//
//   - I1 content-hash idempotency: a file is (re)written only when its on-disk
//     SHA-256 differs from the document's ContentHash, and the write is the
//     verbatim body — so projecting twice is byte-stable and a no-op.
//   - I2 never clobber an un-ingested edit: a file is overwritten only when its
//     on-disk bytes are something the projector itself wrote (tracked in a
//     state sidecar, .mazarbul/state.json — path → last-projected hash). Bytes
//     we did not write are a manual edit not yet in the event log: the file is
//     SKIPPED and reported as a pending edit (`maz doc import <vault>` or
//     `maz doc edit` ingests it).
//   - I3 single serialized writer: all writes go through one mutex.
//   - I4 atomic writes only: temp file + rename, never partial content.
//
// Deletion is tombstone-driven and conservative: a file is removed only when
// its document was soft-deleted AND the on-disk bytes still match the last
// known revision; anything else (operator's own files, edited-after-delete
// files) is left untouched.
package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// stateRelPath is the projector's sidecar inside the vault root: a map of
// document path → the content hash this projector last wrote there. It is how
// the projector tells a stale projection of an older revision (bytes WE wrote
// — safe to advance) apart from a manual edit (bytes we did not write — never
// clobber, I2), across process restarts.
const stateRelPath = ".mazarbul/state.json"

// vaultState is the persisted shape of the state sidecar.
type vaultState struct {
	Projected map[string]string `json:"projected"`
}

// syncState wraps vaultState with dirty tracking so an unchanged pass never
// rewrites the sidecar.
type syncState struct {
	vaultState
	dirty bool
}

func (s *syncState) set(path, hash string) {
	if s.Projected[path] == hash {
		return
	}
	s.Projected[path] = hash
	s.dirty = true
}

func (s *syncState) drop(path string) {
	if _, ok := s.Projected[path]; !ok {
		return
	}
	delete(s.Projected, path)
	s.dirty = true
}

// Reader is the navigate surface the projector needs. Satisfied by
// *navigate.Navigator. The projector is wired with a navigator spanning ALL
// four scopes (navigate.NewVaultScopes) so SyncAll materializes every scope —
// including draft — onto disk; the draft-visible exception is confined to this
// one navigator and never widens the recall/navigation read set.
type Reader interface {
	ListDocuments(ctx context.Context, opts navigate.DocumentFilter) ([]navigate.Document, error)
	GetDocumentByPathScope(ctx context.Context, path, scope string) (navigate.Document, error)
	DeletedDocuments(ctx context.Context) ([]navigate.Document, error)
}

// Projector materializes documents under a vault root directory.
type Projector struct {
	reader Reader
	root   string
	log    *zap.Logger

	// mu is the I3 single-writer lock: SyncAll and SyncPath serialize so two
	// concurrent writes can never interleave on the same path.
	mu sync.Mutex
}

// NewProjector builds a Projector rooted at root. Log may be nil.
func NewProjector(reader Reader, root string, log *zap.Logger) *Projector {
	if log == nil {
		log = zap.NewNop()
	}
	return &Projector{reader: reader, root: root, log: log}
}

// Root returns the vault root directory.
func (p *Projector) Root() string { return p.root }

// FilePath returns the absolute on-disk path for a vault-relative document path
// in scope. The vault is partitioned on disk as <root>/<scope>/<path>, so two
// documents that share a relative path in different scopes materialize to
// distinct files and can never clobber each other. It errors when the scope is
// invalid or the path would escape the scope partition.
func (p *Projector) FilePath(rel, scope string) (string, error) {
	if !kernel.Scope(scope).Valid() {
		return "", fmt.Errorf("vault: document %q has invalid scope %q", rel, scope)
	}
	native := filepath.FromSlash(rel)
	if !filepath.IsLocal(native) {
		return "", fmt.Errorf("vault: document path %q escapes the vault root", rel)
	}
	return filepath.Join(p.root, scope, native), nil
}

// stateKey is the sidecar key for a (scope, path) pair. The state sidecar lives
// once at the vault root (one owner, one mutex — I3) and keys every entry by
// "<scope>/<rel>" so the same relative path in two scopes tracks two distinct
// projected-hash entries.
func stateKey(scope, rel string) string {
	return scope + "/" + rel
}

// Stats summarises one projection pass.
type Stats struct {
	// Written counts files created or updated to the latest revision body.
	Written int `json:"written"`
	// Unchanged counts files whose on-disk hash already matched (I1 skip).
	Unchanged int `json:"unchanged"`
	// PendingEdits counts files SKIPPED because their on-disk bytes differ
	// from the latest revision — a manual edit not yet ingested (I2). Run
	// `maz doc import <vault>` (or save through `maz doc edit`) to ingest.
	PendingEdits int `json:"pendingEdits"`
	// Deleted counts tombstoned documents whose file was removed.
	Deleted int `json:"deleted"`
	// Kept counts tombstoned documents whose file was preserved because the
	// on-disk bytes no longer match the deleted revision.
	Kept int `json:"kept"`
}

// SyncAll materializes every live document under the vault root and removes
// the files of tombstoned documents (conservatively; see package comment).
func (p *Projector) SyncAll(ctx context.Context) (Stats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st := &syncState{vaultState: p.loadStateLocked()}
	stats, err := p.syncAllLocked(ctx, st)
	// Persist the sidecar even when a later document failed: the files already
	// written this pass must be remembered as ours, or a retry would misread
	// them as manual edits.
	if saveErr := p.saveStateLocked(st); saveErr != nil && err == nil {
		err = saveErr
	}
	return stats, err
}

func (p *Projector) syncAllLocked(ctx context.Context, st *syncState) (Stats, error) {
	var stats Stats
	docs, err := p.reader.ListDocuments(ctx, navigate.DocumentFilter{Limit: -1, IncludeBody: true})
	if err != nil {
		return Stats{}, fmt.Errorf("vault: list documents: %w", err)
	}
	for i := range docs {
		if err := p.writeDocLocked(&docs[i], st, &stats); err != nil {
			return stats, err
		}
	}

	tombstones, err := p.reader.DeletedDocuments(ctx)
	if err != nil {
		return stats, fmt.Errorf("vault: list tombstones: %w", err)
	}
	for i := range tombstones {
		if err := p.removeTombstoneLocked(&tombstones[i], st, &stats); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// SyncPath materializes the single live document at the vault-relative path in
// scope. The lookup is scope-exact: with composite (path, scope) uniqueness a
// path can name one document per scope, so the projector addresses exactly the
// row that belongs on <root>/<scope>/<path>. Returns navigate.ErrNotFound
// (wrapped) when no live document exists there.
func (p *Projector) SyncPath(ctx context.Context, rel, scope string) (Stats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	doc, err := p.reader.GetDocumentByPathScope(ctx, rel, scope)
	if err != nil {
		return Stats{}, fmt.Errorf("vault: sync %q@%s: %w", rel, scope, err)
	}
	st := &syncState{vaultState: p.loadStateLocked()}
	var stats Stats
	writeErr := p.writeDocLocked(&doc, st, &stats)
	if saveErr := p.saveStateLocked(st); saveErr != nil && writeErr == nil {
		writeErr = saveErr
	}
	return stats, writeErr
}

// SyncMove writes the document at newPath and removes the stale file at
// oldPath (only when oldPath's on-disk bytes are still those we projected —
// I2 is not violated). This is the vault-side cleanup for moveDocument: a
// plain SyncPath(newPath) writes the new location but leaves the old file
// orphaned until the next SyncAll. SyncMove is loop-safe: the watcher
// triggers on the new file (which is already known to the sidecar after this
// call) and is a no-op re-import; the removed old file produces no watcher
// event because we remove it ourselves before the watcher sees it.
// A move is within one scope (a cross-scope re-home is a delete+create, not
// modeled here), so both oldPath and newPath live under <root>/<scope>/.
func (p *Projector) SyncMove(ctx context.Context, oldPath, newPath, scope string) (Stats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	doc, err := p.reader.GetDocumentByPathScope(ctx, newPath, scope)
	if err != nil {
		return Stats{}, fmt.Errorf("vault: sync move new path %q@%s: %w", newPath, scope, err)
	}
	st := &syncState{vaultState: p.loadStateLocked()}
	var stats Stats

	// Write the new-path file.
	if werr := p.writeDocLocked(&doc, st, &stats); werr != nil {
		return stats, werr
	}

	// Defense in depth: if oldPath == newPath this is a no-op materialization
	// (e.g. an idempotent move-by-uid retry the tool layer should already have
	// skipped). The old-path removal below would otherwise delete the file we
	// just wrote, since it now hashes to doc.ContentHash. Write only; never
	// remove the path we just materialized.
	if oldPath == newPath {
		if saveErr := p.saveStateLocked(st); saveErr != nil {
			return stats, saveErr
		}
		return stats, nil
	}

	// If writeDocLocked SKIPPED the new path (I2: an untracked manual file
	// already occupies newPath with bytes we did not write), the moved document
	// was NOT materialized there. Removing the old file now would lose the only
	// projected copy of the document and leave the unrelated target file in its
	// place. Keep the old file; the next SyncAll re-reports the pending edit.
	if stats.PendingEdits > 0 {
		p.log.Warn("vault: move target holds an un-ingested manual edit; keeping the old-path file",
			zap.String("oldPath", oldPath), zap.String("newPath", newPath))
		if saveErr := p.saveStateLocked(st); saveErr != nil {
			return stats, saveErr
		}
		return stats, nil
	}

	// Remove the old-path file conservatively: only when we projected it AND
	// the on-disk bytes still match what we wrote (I2). If they differ, the
	// operator edited the file between the last projection and the move — leave
	// it untouched and let `maz doc lint` report it as a pending edit.
	oldKey := stateKey(scope, oldPath)
	oldTarget, pathErr := p.FilePath(oldPath, scope)
	if pathErr == nil {
		onDisk, readErr := os.ReadFile(oldTarget) //nolint:gosec // path validated by FilePath
		if readErr == nil {
			got := hashHex(onDisk)
			if got == st.Projected[oldKey] || got == doc.ContentHash {
				if rmErr := os.Remove(oldTarget); rmErr == nil {
					stats.Deleted++
				}
			}
			// else: edited after projection — leave it (not our bytes any more).
		}
		// else: file already gone — nothing to do.
	}
	st.drop(oldKey)

	if saveErr := p.saveStateLocked(st); saveErr != nil {
		return stats, saveErr
	}
	return stats, nil
}

// loadStateLocked reads the state sidecar. A missing file is a fresh vault; an
// unreadable or corrupt file degrades to an empty state, which only errs in
// the SAFE direction (unrecognized on-disk bytes become pending edits, never
// overwrites). Caller holds p.mu.
func (p *Projector) loadStateLocked() vaultState {
	empty := vaultState{Projected: map[string]string{}}
	raw, err := os.ReadFile(filepath.Join(p.root, filepath.FromSlash(stateRelPath)))
	if errors.Is(err, os.ErrNotExist) {
		return empty
	}
	if err != nil {
		p.log.Warn("vault: cannot read projection state; treating files as untracked", zap.Error(err))
		return empty
	}
	var st vaultState
	if err := json.Unmarshal(raw, &st); err != nil || st.Projected == nil {
		p.log.Warn("vault: corrupt projection state; treating files as untracked", zap.Error(err))
		return empty
	}
	return st
}

// saveStateLocked persists the state sidecar when it changed. Caller holds p.mu.
func (p *Projector) saveStateLocked(st *syncState) error {
	if !st.dirty {
		return nil
	}
	target := filepath.Join(p.root, filepath.FromSlash(stateRelPath))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("vault: mkdir state dir: %w", err)
	}
	raw, err := json.MarshalIndent(st.vaultState, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: encode state: %w", err)
	}
	if err := writeFileAtomic(target, raw); err != nil {
		return fmt.Errorf("vault: write state: %w", err)
	}
	st.dirty = false
	return nil
}

// writeDocLocked materializes one document under its scope partition
// (<root>/<scope>/<path>). The sidecar entry is keyed by (scope, path) so the
// same relative path in two scopes tracks independently. Caller holds p.mu.
func (p *Projector) writeDocLocked(doc *navigate.Document, st *syncState, stats *Stats) error {
	target, err := p.FilePath(doc.Path, doc.Scope)
	if err != nil {
		// Defense in depth: CleanVaultPath rejects these at ingest time, so a
		// hit here means a hand-crafted event; refuse to write outside root.
		p.log.Warn("vault: refusing to materialize document outside the vault root",
			zap.String("path", doc.Path), zap.String("scope", doc.Scope))
		return nil //nolint:nilerr // one bad path must not abort the whole projection pass
	}
	key := stateKey(doc.Scope, doc.Path)

	onDisk, readErr := os.ReadFile(target) //nolint:gosec // path validated by FilePath
	switch {
	case readErr == nil:
		got := hashHex(onDisk)
		if got == doc.ContentHash {
			stats.Unchanged++ // I1: byte-identical, nothing to do
			// Adopt: the file already holds the latest revision (e.g. it was
			// just saved through `maz doc edit` and ingested), so future
			// passes may treat these bytes as projector-written.
			st.set(key, doc.ContentHash)
			return nil
		}
		if got != st.Projected[key] {
			// I2: bytes the projector did not write — a manual edit that has
			// not been ingested yet. Never clobber it.
			stats.PendingEdits++
			p.log.Warn("vault: on-disk file differs from the latest revision; skipping (ingest it with `maz doc import` or save via `maz doc edit`)",
				zap.String("path", doc.Path), zap.String("scope", doc.Scope))
			return nil
		}
		// Stale projection of an older revision that we wrote: safe to advance.
	case errors.Is(readErr, os.ErrNotExist):
		// fresh materialization
	default:
		return fmt.Errorf("vault: read %q: %w", target, readErr)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("vault: mkdir for %q: %w", doc.Path, err)
	}
	if err := writeFileAtomic(target, []byte(doc.Body)); err != nil {
		return fmt.Errorf("vault: write %q: %w", doc.Path, err)
	}
	stats.Written++
	st.set(key, doc.ContentHash)
	return nil
}

// removeTombstoneLocked deletes the file of a tombstoned document iff the
// on-disk bytes still match the deleted revision (or a projection this
// projector wrote). Caller holds p.mu.
func (p *Projector) removeTombstoneLocked(doc *navigate.Document, st *syncState, stats *Stats) error {
	target, err := p.FilePath(doc.Path, doc.Scope)
	if err != nil {
		p.log.Warn("vault: tombstoned document path escapes the vault root; ignoring",
			zap.String("path", doc.Path), zap.String("scope", doc.Scope))
		return nil //nolint:nilerr // one bad path must not abort the whole projection pass
	}
	key := stateKey(doc.Scope, doc.Path)
	onDisk, readErr := os.ReadFile(target) //nolint:gosec // path validated by FilePath
	if errors.Is(readErr, os.ErrNotExist) {
		st.drop(key)
		return nil // already gone
	}
	if readErr != nil {
		return fmt.Errorf("vault: read tombstoned %q: %w", target, readErr)
	}
	if got := hashHex(onDisk); got != doc.ContentHash && got != st.Projected[key] {
		// Edited after the tombstone (or never ours): preserve the bytes. Drop
		// the state entry too — the document is gone, so whatever lives at
		// this path now belongs to the human.
		stats.Kept++
		st.drop(key)
		p.log.Warn("vault: tombstoned document's file was modified on disk; keeping it",
			zap.String("path", doc.Path), zap.String("scope", doc.Scope))
		return nil
	}
	if err := os.Remove(target); err != nil {
		return fmt.Errorf("vault: remove tombstoned %q: %w", target, err)
	}
	stats.Deleted++
	st.drop(key)
	return nil
}

// hashHex returns the lowercase SHA-256 hex digest of b — the same digest
// document.Parse stores as ContentHash, so on-disk bytes compare directly.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic writes content to path via a temp file + rename in the same
// directory (I4): readers and editors never observe a partial file. Files are
// 0600 to match the data dir's owner-only stance.
func writeFileAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
			_ = rmErr // best-effort: a leftover temp file is harmless
		}
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}
