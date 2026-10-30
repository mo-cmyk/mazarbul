package config

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// StateChange is delivered to a watcher callback when state.toml has
// landed atomically on disk. The OldState / NewState pair lets callers
// surface diffs (which fields changed, which require a restart).
type StateChange struct {
	OldState UserState
	NewState UserState
}

// StateWatcher is a long-running fsnotify subscriber that fires a
// callback whenever ${dataDir}/state.toml is modified. It works against
// the parent directory because atomic-rename writes (the only ones
// WriteUserState performs) replace the inode — fsnotify on the path
// itself would lose the subscription on the first save, while the
// parent-dir + basename-filter pattern survives every atomic rename.
//
// Calls to the callback are coalesced within debounceWindow so an
// atomic write that fires multiple inotify events (CREATE + RENAME +
// CHMOD on the temp file plus the rename onto the final path) lands as
// one logical change.
//
// The current StateWatcher does not live-mutate any subsystem; it
// simply re-parses state.toml and hands the diff to the callback. Most
// runtime knobs (provider endpoint, embedder model, data-dir) require a
// process restart to take effect — callbacks should log the change and
// rely on `maz doctor` / the Settings status row to surface the
// restart-required gap.
type StateWatcher struct {
	w        *fsnotify.Watcher
	dataDir  string
	basename string
	debounce time.Duration

	current atomic.Pointer[UserState]
}

// DefaultStateDebounce is the coalescing window for atomic-write bursts
// on state.toml. Tuned conservatively: editors (vim's :w) emit several
// events in quick succession; 200 ms is well below human-perceptible
// latency and above every burst observed in fsnotify integration tests.
const DefaultStateDebounce = 200 * time.Millisecond

// NewStateWatcher prepares a watcher rooted at dataDir. The actual file
// (state.toml) need not exist yet — the watcher subscribes to its
// parent directory and filters events by basename. The returned
// watcher is dormant until Run is called.
func NewStateWatcher(dataDir string) (*StateWatcher, error) {
	if dataDir == "" {
		return nil, errors.New("config: data dir is required")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("config: fsnotify init: %w", err)
	}
	if err := w.Add(dataDir); err != nil {
		if cerr := w.Close(); cerr != nil {
			return nil, fmt.Errorf("config: watch %s: %w (close failed: %w)", dataDir, err, cerr)
		}
		return nil, fmt.Errorf("config: watch %s: %w", dataDir, err)
	}
	return &StateWatcher{
		w:        w,
		dataDir:  dataDir,
		basename: stateFile,
		debounce: DefaultStateDebounce,
	}, nil
}

// Current returns the most recently observed UserState. Returns a zero
// value (and false) until Run has loaded the file at least once. Safe
// for concurrent reads from multiple goroutines.
func (sw *StateWatcher) Current() (UserState, bool) {
	if p := sw.current.Load(); p != nil {
		return *p, true
	}
	return UserState{}, false
}

// Run drives the watcher loop until ctx is cancelled, the underlying
// fsnotify watcher closes, or an unrecoverable error is observed.
// OnChange is called with the diff (old, new) every time state.toml
// is modified. A nil onChange disables callbacks but leaves the
// watcher's internal Current() snapshot fresh — useful for subsystems
// that poll rather than subscribe.
//
// Run loads state.toml once on entry so Current() is populated before
// any event fires; a missing file leaves Current() at the zero value
// until the first write.
func (sw *StateWatcher) Run(ctx context.Context, onChange func(StateChange)) error {
	defer sw.closeWatcher()

	if err := sw.seedCurrent(); err != nil {
		return err
	}

	debounced := make(chan struct{}, 1)
	var debounce *time.Timer
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-sw.w.Errors:
			if !ok {
				return nil
			}
			if err != nil {
				return fmt.Errorf("config: watcher: %w", err)
			}
		case ev, ok := <-sw.w.Events:
			if !ok {
				return nil
			}
			sw.handleFSEvent(ev, &debounce, debounced)
		case <-debounced:
			sw.reload(onChange)
		}
	}
}

func (sw *StateWatcher) closeWatcher() {
	if err := sw.w.Close(); err != nil {
		// Best-effort: closing a watcher that has already errored or
		// drained is harmless. The daemon log already records the
		// underlying failure; this close error would be redundant.
		_ = err
	}
}

func (sw *StateWatcher) seedCurrent() error {
	state, err := LoadUserState(sw.dataDir)
	if err != nil {
		if errors.Is(err, ErrUserStateNotFound) {
			return nil
		}
		return fmt.Errorf("config: initial load: %w", err)
	}
	sw.current.Store(&state)
	return nil
}

// handleFSEvent filters an fsnotify event by basename and op kind, then
// (re)arms the debounce timer. The timer's callback drops a signal onto
// `debounced` if the channel is empty; a buffered channel + non-blocking
// send guarantees no goroutine leaks even if Run is mid-iteration.
func (sw *StateWatcher) handleFSEvent(ev fsnotify.Event, debounce **time.Timer, debounced chan<- struct{}) {
	if filepath.Base(ev.Name) != sw.basename {
		return
	}
	// Only react to events that materially change file contents:
	// Create / Write / Rename / Remove. Atomic-write lands as Rename
	// on most platforms; Linux fsnotify reports Create instead. A pure
	// `rm state.toml` fires Remove without any companion event, so it
	// must be in the mask for the reload path's ErrUserStateNotFound
	// branch to drop overrides back to the file-resident defaults.
	// Chmod alone is uninteresting.
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
		return
	}
	if *debounce != nil {
		(*debounce).Stop()
	}
	*debounce = time.AfterFunc(sw.debounce, func() {
		select {
		case debounced <- struct{}{}:
		default:
		}
	})
}

func (sw *StateWatcher) reload(onChange func(StateChange)) {
	next, err := LoadUserState(sw.dataDir)
	if err != nil {
		if errors.Is(err, ErrUserStateNotFound) {
			// Deletion is a valid state transition — the operator wiped
			// state.toml to reset every override back to the file-resident
			// config defaults. Treat it as a transition to the zero
			// UserState and fire onChange so subscribers (log level, recall
			// tuning) can drop their override and fall back to the base
			// config. Without this branch a `rm state.toml` would leave
			// the daemon serving stale overrides until restart.
			var prev UserState
			if p := sw.current.Load(); p != nil {
				prev = *p
			}
			zero := UserState{}
			sw.current.Store(&zero)
			if onChange != nil {
				onChange(StateChange{OldState: prev, NewState: zero})
			}
			return
		}
		// A parse failure (corrupt state.toml mid-write, future schema)
		// must not crash the daemon; the previous Current() snapshot is
		// preserved so subsystems keep working with the last good state.
		// Operators see the failure via the daemon log.
		return
	}
	var prev UserState
	if p := sw.current.Load(); p != nil {
		prev = *p
	}
	sw.current.Store(&next)
	if onChange != nil {
		onChange(StateChange{OldState: prev, NewState: next})
	}
}
