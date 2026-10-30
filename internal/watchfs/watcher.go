package watchfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// pendingBuffer bounds the channel of debounce-fired paths waiting to be
// turned into Changes. Generous so a burst across many distinct files
// rarely makes a timer callback block on the send; when the buffer is
// full a callback blocks until Run drains a slot or ctx is cancelled.
const pendingBuffer = 256

// debounceTimer pairs a per-path debounce timer with a generation number.
// The generation lets the Run loop reject a stale fire: when a path is
// re-armed while an earlier timer's send is still queued in pending, the
// queued (older) generation no longer matches the map entry, so it is
// skipped and the burst collapses to exactly one dispatch.
type debounceTimer struct {
	timer *time.Timer
	gen   uint64
}

// pendingFire is what a fired debounce timer sends to the Run loop: the
// path plus the generation that fired, so the loop can drop a superseded
// fire instead of dispatching it (and instead of deleting a newer timer's
// map entry).
type pendingFire struct {
	path string
	gen  uint64
}

// EmitFunc consumes a debounced, filtered Change. Returning an error is
// logged and does not stop the watcher: passive capture must survive a
// transient write failure.
type EmitFunc func(ctx context.Context, c *Change) error

// Watcher observes Options.Roots and invokes an EmitFunc for every
// text-file change. Because fsnotify is not recursive, the watcher walks
// each root up front and adds newly-created directories as it observes
// them.
type Watcher struct {
	opts Options
	ig   *ignorer
	log  *zap.Logger
}

// New constructs a Watcher. It validates and defaults opts and compiles
// the ignore rules; it does not touch the filesystem watch descriptors
// until Run.
func New(opts Options) (*Watcher, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	roots, err := absRoots(opts.Roots)
	if err != nil {
		return nil, err
	}
	opts.Roots = roots
	opts = opts.withDefaults()
	return &Watcher{
		opts: opts,
		ig:   newIgnorer(roots, opts.ExtraIgnores, opts.Logger),
		log:  opts.Logger,
	}, nil
}

// Roots returns the resolved absolute roots being watched.
func (w *Watcher) Roots() []string { return append([]string(nil), w.opts.Roots...) }

// Run watches until ctx is cancelled or the underlying fsnotify watcher
// fails unrecoverably. Each debounced, non-ignored, text-file change is
// passed to emit on a single goroutine, so emit need not be safe for
// concurrent use.
func (w *Watcher) Run(ctx context.Context, emit EmitFunc) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watchfs: fsnotify init: %w", err)
	}
	defer func() {
		if cerr := fw.Close(); cerr != nil {
			w.log.Debug("watchfs: close watcher", zap.Error(cerr))
		}
	}()

	for _, root := range w.opts.Roots {
		w.addTree(fw, root)
	}

	pending := make(chan pendingFire, pendingBuffer)
	timers := make(map[string]*debounceTimer)
	defer stopTimers(timers)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			// A watcher-level error (e.g. inotify queue overflow) is
			// logged, not fatal: the next event re-establishes flow.
			w.log.Warn("watchfs: watcher error", zap.Error(err))
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ctx, fw, ev, timers, pending)
		case pf := <-pending:
			// Dispatch only when this fire is the current generation for the
			// path. A superseded fire (the path was re-armed before this
			// send drained) is dropped, so it neither double-dispatches nor
			// deletes the newer timer's map entry.
			if d := timers[pf.path]; d != nil && d.gen == pf.gen {
				delete(timers, pf.path)
				w.dispatch(ctx, pf.path, emit)
			}
		}
	}
}

// handleEvent reacts to one fsnotify event: it extends watch coverage to
// newly-created directories and (re)arms the per-path debounce timer for
// content-changing file events. Runs on the Run goroutine, so the timers
// map needs no lock.
func (w *Watcher) handleEvent(ctx context.Context, fw *fsnotify.Watcher, ev fsnotify.Event, timers map[string]*debounceTimer, pending chan<- pendingFire) {
	if ev.Op&fsnotify.Create != 0 {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			if !w.ig.ignoreDir(ev.Name) {
				w.addTree(fw, ev.Name)
			}
			return
		}
	}
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
		return // chmod-only and friends carry no content change
	}
	// A rename within the tree fires Rename on the old path (→ file_delete)
	// and Create on the new path (→ file_change with the moved content).
	// Capturing both is intentional: a moved file is a delete plus an
	// arrival, and the arrival carries the latest content.
	if w.ig.ignoreFile(ev.Name) {
		return
	}
	w.arm(ctx, timers, pending, ev.Name)
}

// arm starts or restarts the debounce timer for path, bumping its
// generation so a previously-fired-but-not-yet-drained timer is recognised
// as stale. When the new timer fires it sends its path and generation onto
// pending unless ctx is already done.
func (w *Watcher) arm(ctx context.Context, timers map[string]*debounceTimer, pending chan<- pendingFire, path string) {
	gen := uint64(1)
	if d := timers[path]; d != nil {
		d.timer.Stop()
		gen = d.gen + 1
	}
	timers[path] = &debounceTimer{
		gen: gen,
		timer: time.AfterFunc(w.opts.Debounce, func() {
			select {
			case pending <- pendingFire{path: path, gen: gen}:
			case <-ctx.Done():
			}
		}),
	}
}

// dispatch builds a Change for a debounced path and hands it to emit,
// logging (but not propagating) any emit error.
func (w *Watcher) dispatch(ctx context.Context, path string, emit EmitFunc) {
	c, ok := w.buildChange(path)
	if !ok {
		return
	}
	if err := emit(ctx, &c); err != nil {
		w.log.Warn("watchfs: emit change",
			zap.String("path", path), zap.String("kind", string(c.Kind)), zap.Error(err))
	}
}

// buildChange resolves the kind of a debounced path and, for a
// modification, reads its capped, text-only content. The ok result is
// false when the path is outside every root, is a directory, is
// unreadable, or is binary.
func (w *Watcher) buildChange(path string) (Change, bool) {
	root, rel, ok := rootFor(w.opts.Roots, path)
	if !ok {
		return Change{}, false
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Change{Root: root, Path: path, RelPath: rel, Kind: ChangeDeleted}, true
	}
	if err != nil || fi.IsDir() {
		return Change{}, false
	}
	content, truncated, rerr := readCapped(path, w.opts.SizeCap)
	if rerr != nil {
		// Deleted between the stat above and this read: emit the delete we
		// would otherwise drop, rather than losing the event entirely.
		if errors.Is(rerr, fs.ErrNotExist) {
			return Change{Root: root, Path: path, RelPath: rel, Kind: ChangeDeleted}, true
		}
		w.log.Debug("watchfs: read file", zap.String("path", path), zap.Error(rerr))
		return Change{}, false
	}
	if !isProbablyText(content) {
		return Change{}, false
	}
	return Change{
		Root:      root,
		Path:      path,
		RelPath:   rel,
		Kind:      ChangeModified,
		Content:   content,
		Truncated: truncated,
	}, true
}

// addTree walks dir and adds every non-ignored directory to the fsnotify
// watcher. Walk errors and per-directory Add failures are logged and
// skipped so one unreadable subtree never aborts coverage of the rest.
func (w *Watcher) addTree(fw *fsnotify.Watcher, dir string) {
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entry, keep walking
		}
		if !d.IsDir() {
			return nil
		}
		if w.ig.ignoreDir(path) {
			return filepath.SkipDir
		}
		if aerr := fw.Add(path); aerr != nil {
			w.log.Debug("watchfs: add watch", zap.String("dir", path), zap.Error(aerr))
		}
		return nil
	})
	if err != nil {
		w.log.Debug("watchfs: walk tree", zap.String("dir", dir), zap.Error(err))
	}
}

// readCapped reads up to limit bytes of path, reporting whether the file
// exceeded the limit. It reads limit+1 bytes to distinguish an
// exactly-limit file from a truncated one.
func readCapped(path string, limit int) (content []byte, truncated bool, err error) {
	f, err := os.Open(path) //nolint:gosec // path is a watched file under an operator-chosen root
	if err != nil {
		return nil, false, fmt.Errorf("watchfs: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, limit+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, false, fmt.Errorf("watchfs: read %s: %w", path, err)
	}
	if n > limit {
		return buf[:limit], true, nil
	}
	return buf[:n], false, nil
}

// absRoots cleans each root to an absolute path, failing on the first
// that cannot be resolved.
func absRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("watchfs: resolve root %q: %w", r, err)
		}
		out = append(out, abs)
	}
	return out, nil
}

// stopTimers halts every outstanding debounce timer during shutdown.
func stopTimers(timers map[string]*debounceTimer) {
	for _, d := range timers {
		d.timer.Stop()
	}
}
