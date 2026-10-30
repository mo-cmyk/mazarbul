package config_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// TestStateWatcher_FiresOnAtomicWrite verifies the parent-dir + basename
// pattern survives the temp-file + rename dance WriteUserState performs.
// Naive watcher-on-the-file would lose the subscription after the first
// rename; this test pins that the parent-dir + basename-filter trick
// (fsnotify issue #17) holds.
func TestStateWatcher_FiresOnAtomicWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := config.NewStateWatcher(dir)
	if err != nil {
		t.Fatalf("NewStateWatcher: %v", err)
	}

	got := make(chan config.StateChange, 4)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := w.Run(ctx, func(c config.StateChange) {
			got <- c
		}); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	// First write should fire.
	first := config.UserState{Defaults: config.DefaultsStateTable{Chat: "ollama"}}
	if err := config.WriteUserState(dir, first); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}
	change := waitForChange(t, got, 2*time.Second)
	if change.NewState.Defaults.Chat != "ollama" {
		t.Errorf("first change NewState.Defaults.Chat = %q, want %q",
			change.NewState.Defaults.Chat, "ollama")
	}

	// Second atomic write — would silently no-op under file-level fsnotify.
	second := config.UserState{Defaults: config.DefaultsStateTable{Chat: "anthropic"}}
	if err := config.WriteUserState(dir, second); err != nil {
		t.Fatalf("second WriteUserState: %v", err)
	}
	change = waitForChange(t, got, 2*time.Second)
	if change.OldState.Defaults.Chat != "ollama" {
		t.Errorf("second change OldState.Defaults.Chat = %q, want %q",
			change.OldState.Defaults.Chat, "ollama")
	}
	if change.NewState.Defaults.Chat != "anthropic" {
		t.Errorf("second change NewState.Defaults.Chat = %q, want %q",
			change.NewState.Defaults.Chat, "anthropic")
	}
}

func TestStateWatcher_CurrentSeededFromExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Pre-populate state.toml before starting the watcher.
	want := config.UserState{Defaults: config.DefaultsStateTable{Chat: "anthropic"}}
	if err := config.WriteUserState(dir, want); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	w, err := config.NewStateWatcher(dir)
	if err != nil {
		t.Fatalf("NewStateWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := w.Run(ctx, nil); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	// Run loads the file synchronously before entering the event loop;
	// a short poll is sufficient to see Current() populated.
	if !waitForCurrent(t, w, 1*time.Second) {
		t.Fatalf("Current() never populated despite existing state.toml")
	}
	cur, _ := w.Current()
	if cur.Defaults.Chat != want.Defaults.Chat {
		t.Errorf("Current().Defaults.Chat = %q, want %q",
			cur.Defaults.Chat, want.Defaults.Chat)
	}
}

func TestStateWatcher_NilCallbackKeepsCurrentFresh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := config.NewStateWatcher(dir)
	if err != nil {
		t.Fatalf("NewStateWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := w.Run(ctx, nil); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	want := config.UserState{Defaults: config.DefaultsStateTable{Chat: "ollama"}}
	if err := config.WriteUserState(dir, want); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cur, ok := w.Current(); ok && cur.Defaults.Chat == "ollama" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Current() never refreshed without a callback")
}

// TestStateWatcher_FiresOnRemove pins Greptile review-2 finding: a pure
// `rm state.toml` fires fsnotify.Remove without a Create/Write/Rename
// companion, so the event filter must include Remove or the daemon
// keeps stale overrides indefinitely. The reload path's
// ErrUserStateNotFound branch then fires onChange with the zero state.
func TestStateWatcher_FiresOnRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Pre-populate so the watcher seeds a non-zero Current().
	initial := config.UserState{Defaults: config.DefaultsStateTable{Chat: "anthropic"}}
	if err := config.WriteUserState(dir, initial); err != nil {
		t.Fatalf("WriteUserState: %v", err)
	}

	w, err := config.NewStateWatcher(dir)
	if err != nil {
		t.Fatalf("NewStateWatcher: %v", err)
	}

	got := make(chan config.StateChange, 4)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := w.Run(ctx, func(c config.StateChange) {
			got <- c
		}); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	// Wait for the initial seed before mutating.
	if !waitForCurrent(t, w, 1*time.Second) {
		t.Fatalf("Current() never seeded from existing state.toml")
	}

	// Direct os.Remove — fires only fsnotify.Remove on every platform.
	if err := os.Remove(filepath.Join(dir, "state.toml")); err != nil {
		t.Fatalf("os.Remove: %v", err)
	}

	change := waitForChange(t, got, 2*time.Second)
	if change.OldState.Defaults.Chat != "anthropic" {
		t.Errorf("OldState.Defaults.Chat = %q, want %q",
			change.OldState.Defaults.Chat, "anthropic")
	}
	if change.NewState.Defaults.Chat != "" {
		t.Errorf("NewState.Defaults.Chat = %q, want empty after delete",
			change.NewState.Defaults.Chat)
	}
}

func TestStateWatcher_ContextCancelStops(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := config.NewStateWatcher(dir)
	if err != nil {
		t.Fatalf("NewStateWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, nil) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s of cancel")
	}
}

func waitForChange(t *testing.T, ch <-chan config.StateChange, d time.Duration) config.StateChange {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(d):
		t.Fatalf("no StateChange within %s", d)
	}
	return config.StateChange{}
}

func waitForCurrent(t *testing.T, w *config.StateWatcher, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, ok := w.Current(); ok {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
