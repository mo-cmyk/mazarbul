package vault

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// DefaultWatchDebounce is the coalescing window for atomic-write bursts in
// the vault. Editors (vim :w, Obsidian autosave) emit several fsnotify events
// in rapid succession; 200 ms collapses them into one logical change and is
// well below human-perceptible latency. Mirrors config.DefaultStateDebounce.
const DefaultWatchDebounce = 200 * time.Millisecond

// ContentIngester is the write surface the watcher uses to ingest a changed
// vault file. Satisfied by *document.Importer. The scope is inferred from the
// file's top-level partition directory (<root>/<scope>/<path>).
type ContentIngester interface {
	ImportContent(ctx context.Context, scope kernel.Scope, path string, raw []byte) (document.Result, error)
}

// RevisionNotifier is the write surface that lets the watcher signal the
// writeback worker after a successful ingest. The writeback re-materializes
// the updated document so other clients (Obsidian sync, another device's
// vault) see the canonical body. Any component that appends a new
// documentRevision via a route other than the watcher (e.g. writeDocument
// MCP tool) should call Notify to trigger the same writeback.
type RevisionNotifier interface {
	Notify(scope, vaultRelPath string)
}

// Watcher is the fsnotify subscriber for the vault directory. It watches the
// vault root and every subdirectory (parent-dir + markdown-extension filter)
// and, for each debounced change, reads the file, computes its SHA-256, and
// calls ImportContent only when the hash differs from the latest stored
// revision (I1). The watcher is the ingest half of the bidirectional loop; the
// writeback half lives in Writeback.
//
// Loop-safety invariants:
//
//   - I1 (content-hash idempotency): ImportContent already deduplicates by
//     ContentHash. A file the watcher just materialised via the writeback
//     path has the same hash as the stored revision, so ImportContent returns
//     Skipped=1 and no new revision is appended. The cycle breaks here.
//   - I3 (serialized writer): ImportContent serializes inside the
//     FileEventLog's single-writer lock. Concurrent fsnotify deliveries for
//     the same path are coalesced by the per-path debounce.
//   - I4 (atomic writes): the writeback uses writeFileAtomic; the watcher
//     watches the parent dir so temp-file→rename survives.
//
// ReconcilePath is the testable seam: tests drive it directly without relying
// on real OS fsnotify events, making the loop-safety assertions deterministic.
type Watcher struct {
	root     string
	ingester ContentIngester
	notifier RevisionNotifier
	debounce time.Duration
	log      *zap.Logger

	fsw *fsnotify.Watcher
}

// WatcherConfig parameterises NewWatcher.
type WatcherConfig struct {
	// Root is the vault root directory. Required.
	Root string
	// Ingester ingests changed files. Required.
	Ingester ContentIngester
	// Notifier is called after a successful ingest so the writeback worker
	// can re-materialize the document. May be nil (writeback disabled).
	Notifier RevisionNotifier
	// Debounce overrides DefaultWatchDebounce. Zero uses the default.
	Debounce time.Duration
	// Log may be nil (nop logger substituted).
	Log *zap.Logger
}

// NewWatcher creates a Watcher. The underlying fsnotify watcher is opened and
// the vault root plus every existing subdirectory is registered, but the watch
// loop does not start until Run is called.
func NewWatcher(cfg WatcherConfig) (*Watcher, error) {
	if cfg.Root == "" {
		return nil, errors.New("vault: watcher root is required")
	}
	if cfg.Ingester == nil {
		return nil, errors.New("vault: watcher ingester is required")
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	deb := cfg.Debounce
	if deb == 0 {
		deb = DefaultWatchDebounce
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("vault: fsnotify init: %w", err)
	}
	w := &Watcher{
		root:     cfg.Root,
		ingester: cfg.Ingester,
		notifier: cfg.Notifier,
		debounce: deb,
		log:      log,
		fsw:      fsw,
	}
	// Watch the vault root AND every existing subdirectory. fsnotify is not
	// recursive: an event only fires for files in directories explicitly added.
	// Without this, saves under an existing nested folder (notes/, daily/, …)
	// would never be ingested, breaking the editable loop for common layouts.
	// Watching directories (not individual files) survives atomic-rename writes
	// (temp→target): fsnotify on the file itself loses the subscription on the
	// first rename.
	if err := w.addTree(cfg.Root); err != nil {
		if cerr := fsw.Close(); cerr != nil {
			return nil, fmt.Errorf("vault: watch %q: %w (close: %w)", cfg.Root, err, cerr)
		}
		return nil, fmt.Errorf("vault: watch %q: %w", cfg.Root, err)
	}
	return w, nil
}

// addTree registers dir and every subdirectory beneath it with fsnotify,
// skipping the .mazarbul state sidecar directory. A failure to add the root
// itself is fatal; failures to add a descendant are logged and tolerated (the
// daemon's next SyncAll pass recovers any missed events).
func (w *Watcher) addTree(dir string) error {
	if err := w.fsw.Add(dir); err != nil {
		return fmt.Errorf("vault: fsnotify add %q: %w", dir, err)
	}
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Tolerate transient stat errors on descendants; never abort the walk.
			w.log.Debug("Vault watcher: walk error", zap.String("path", path), zap.Error(err))
			return nil
		}
		if !d.IsDir() || path == dir {
			return nil
		}
		if d.Name() == mazarbulStateDir {
			return filepath.SkipDir
		}
		if addErr := w.fsw.Add(path); addErr != nil {
			w.log.Debug("Vault watcher: could not watch existing subdirectory",
				zap.String("dir", path), zap.Error(addErr))
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("vault: walk %q: %w", dir, walkErr)
	}
	return nil
}

// pendingRef is one enqueued reconcile target: a document's vault-relative path
// plus the scope partition it was found under (<root>/<scope>/<rel>).
type pendingRef struct {
	scope string
	rel   string
}

// pendingSet is a (scope,path)-keyed work queue for the watcher's reconcile
// loop. Keying by the pair makes it lossless under a burst across many distinct
// files (the set grows by unique key, not by event count), unlike a bounded
// channel that would drop on overflow, and keeps the same relative path in two
// scopes as two distinct work items. A 1-buffered wake channel signals the
// consumer without the producer ever blocking.
type pendingSet struct {
	mu   sync.Mutex
	set  map[pendingRef]struct{}
	wake chan struct{}
}

func newPendingSet() *pendingSet {
	return &pendingSet{set: map[pendingRef]struct{}{}, wake: make(chan struct{}, 1)}
}

// add records (scope, rel) and signals the consumer. Never blocks.
func (p *pendingSet) add(scope, rel string) {
	p.mu.Lock()
	p.set[pendingRef{scope: scope, rel: rel}] = struct{}{}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// take atomically snapshots and clears the set.
func (p *pendingSet) take() []pendingRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.set) == 0 {
		return nil
	}
	batch := make([]pendingRef, 0, len(p.set))
	for ref := range p.set {
		batch = append(batch, ref)
		delete(p.set, ref)
	}
	return batch
}

// Run drives the watcher event loop until ctx is cancelled or the underlying
// fsnotify watcher closes. It blocks; callers should run it in a goroutine.
func (w *Watcher) Run(ctx context.Context) error {
	defer func() {
		if err := w.fsw.Close(); err != nil {
			w.log.Warn("Vault watcher: close fsnotify", zap.Error(err))
		}
	}()

	// debounceTimers maps vault-relative path to the active debounce timer.
	// We keep per-path timers so edits to two different files do not coalesce.
	debounceTimers := map[string]*time.Timer{}
	defer func() {
		for _, t := range debounceTimers {
			t.Stop()
		}
	}()

	pending := newPendingSet()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			if err != nil {
				w.log.Warn("Vault watcher: fsnotify error", zap.Error(err))
			}
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			w.handleFSEvent(ev, debounceTimers, pending.add)
		case <-pending.wake:
			for _, ref := range pending.take() {
				w.ReconcilePath(ctx, ref.scope, ref.rel)
			}
		}
	}
}

// handleFSEvent filters an fsnotify event and (re)arms the per-path debounce
// timer. Only markdown files whose op includes content-changing events are
// processed. New subdirectories are added to the watch and their existing
// markdown files reconciled (a synced/pulled directory can arrive already
// populated before its watch is installed).
func (w *Watcher) handleFSEvent(ev fsnotify.Event, timers map[string]*time.Timer, enqueue func(scope, rel string)) {
	// Add new directories to the watcher so nested markdown files are tracked,
	// and reconcile any markdown files already present (e.g. a git pull or sync
	// tool created a populated directory before this watch was installed; those
	// inner files may never emit their own create events).
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			if addErr := w.addTree(ev.Name); addErr != nil {
				// Best-effort: missing events in a newly-created subdirectory are
				// recoverable (the daemon's next SyncAll pass will catch them).
				w.log.Debug("Vault watcher: could not watch new subdirectory",
					zap.String("dir", ev.Name), zap.Error(addErr))
			}
			w.reconcileExistingFiles(ev.Name, enqueue)
			return
		}
	}

	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
		return
	}
	if !isMarkdownFile(ev.Name) {
		return
	}

	scope, rel, ok := w.vaultRel(ev.Name)
	if !ok {
		return
	}

	// Timers are keyed by the full scope/path so the same relative path in two
	// scopes debounces independently.
	key := stateKey(scope, rel)
	if t, ok := timers[key]; ok {
		t.Stop()
	}
	timers[key] = time.AfterFunc(w.debounce, func() {
		enqueue(scope, rel)
	})
}

// reconcileExistingFiles enqueues every markdown file already present under dir
// for reconciliation. Used when a populated directory appears (initial nested
// trees are handled by addTree at startup; this covers directories created
// after Run begins).
func (w *Watcher) reconcileExistingFiles(dir string, enqueue func(scope, rel string)) {
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort scan; skip unreadable entries
		}
		if d.IsDir() {
			if d.Name() == mazarbulStateDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !isMarkdownFile(path) {
			return nil
		}
		if scope, rel, ok := w.vaultRel(path); ok {
			enqueue(scope, rel)
		}
		return nil
	})
	if walkErr != nil {
		// Best-effort: a partial scan still recovers on the next SyncAll pass.
		w.log.Debug("Vault watcher: scan new directory", zap.String("dir", dir), zap.Error(walkErr))
	}
}

// vaultRel converts an absolute path into the (scope, vault-relative path) pair
// its partitioned location encodes: the vault is laid out as
// <root>/<scope>/<path>, so the first path segment is the scope and the
// remainder is the document's vault-relative path. The bool is false when the
// path should be ignored: it escapes the vault root, lives under the .mazarbul
// state sidecar, sits directly at the vault root (no scope partition), or names
// an unknown top-level directory that is not a valid scope. Ignored non-sidecar
// files are logged so a stray markdown file that will never be ingested is
// diagnosable.
func (w *Watcher) vaultRel(abs string) (scope, rel string, ok bool) {
	full, err := filepath.Rel(w.root, abs)
	if err != nil || !filepath.IsLocal(full) {
		return "", "", false
	}
	full = filepath.ToSlash(full)
	if full == mazarbulStateDir || strings.HasPrefix(full, mazarbulStateDir+"/") {
		return "", "", false
	}
	seg, remainder, found := strings.Cut(full, "/")
	if !found || remainder == "" {
		// A markdown file directly under the vault root: the vault is partitioned
		// by scope, so a file with no <scope>/ prefix has no home and is skipped.
		w.log.Warn("Vault watcher: ignoring markdown file at the vault root (expected <scope>/<path>)",
			zap.String("path", full))
		return "", "", false
	}
	if !kernel.Scope(seg).Valid() {
		w.log.Warn("Vault watcher: ignoring file under unknown top-level directory (not a valid scope)",
			zap.String("path", full), zap.String("segment", seg))
		return "", "", false
	}
	return seg, remainder, true
}

// ReconcilePath is the testable seam: given a vault-relative path, it reads
// the on-disk file, computes its hash, and calls ImportContent only when the
// hash differs from the latest stored revision (I1 cycle break). After a
// successful ingest it notifies the writeback worker (if configured).
//
// This method is exported so tests can drive it directly without depending on
// real OS fsnotify events. It validates rel and scope itself (rather than
// trusting the caller) so a path that escapes the scope partition — addresses
// the state sidecar, or carries an invalid scope — is rejected before any
// os.ReadFile, independent of the fsnotify filter in handleFSEvent. The
// document is ingested into its partition's scope; rel is the document's
// vault-relative path (no scope prefix) and the file lives at
// <root>/<scope>/<rel>.
func (w *Watcher) ReconcilePath(ctx context.Context, scope, rel string) {
	if !kernel.Scope(scope).Valid() {
		w.log.Warn("Vault watcher: refusing to reconcile invalid scope", zap.String("scope", scope), zap.String("path", rel))
		return
	}
	native := filepath.FromSlash(rel)
	if !filepath.IsLocal(native) {
		w.log.Warn("Vault watcher: refusing to reconcile non-local path", zap.String("path", rel))
		return
	}
	slashed := filepath.ToSlash(native)
	if slashed == mazarbulStateDir || strings.HasPrefix(slashed, mazarbulStateDir+"/") {
		return
	}

	abs := filepath.Join(w.root, scope, native)
	raw, err := os.ReadFile(abs) //nolint:gosec // path validated above: filepath.IsLocal rejects partition escapes
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.log.Warn("Vault watcher: read file for reconcile", zap.String("path", rel), zap.String("scope", scope), zap.Error(err))
		}
		return
	}

	_, err = w.ingester.ImportContent(ctx, kernel.Scope(scope), slashed, raw)
	if err != nil {
		w.log.Warn("Vault watcher: ingest failed", zap.String("path", rel), zap.String("scope", scope), zap.Error(err))
		return
	}
	// After a successful ingest (new revision or content-hash skip), tell the
	// writeback worker to reconcile. If the ingest was a skip (hash equal), the
	// writeback will also be a no-op (I1 idempotency). If the ingest produced a
	// new revision, the writeback verifies the on-disk bytes now match (they
	// will, since we just read and ingested them) and skips the write.
	if w.notifier != nil {
		w.notifier.Notify(scope, slashed)
	}
}

// mazarbulStateDir is the vault subdirectory holding the projection state
// sidecar; the watcher never ingests or watches files beneath it.
const mazarbulStateDir = ".mazarbul"

// isMarkdownFile reports whether the absolute path has a markdown extension.
func isMarkdownFile(abs string) bool {
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".md", ".markdown":
		return true
	}
	return false
}
