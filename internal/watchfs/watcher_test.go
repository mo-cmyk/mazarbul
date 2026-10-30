package watchfs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// collector accumulates emitted Changes under a mutex so the test
// goroutine can read them while the watcher goroutine appends.
type collector struct {
	mu      sync.Mutex
	changes []Change
}

func (c *collector) emit(_ context.Context, ch *Change) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changes = append(c.changes, *ch)
	return nil
}

func (c *collector) snapshot() []Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Change(nil), c.changes...)
}

// startWatcher runs a watcher over root with a short debounce and returns
// the collector plus a cancel func. It waits briefly for the initial tree
// walk to register watches before returning.
func startWatcher(t *testing.T, root string, debounce time.Duration, extra ...string) (*collector, context.CancelFunc) {
	t.Helper()
	w, err := New(Options{Roots: []string{root}, Debounce: debounce, ExtraIgnores: extra})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	col := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if rerr := w.Run(ctx, col.emit); rerr != nil {
			t.Errorf("Run: %v", rerr)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	time.Sleep(100 * time.Millisecond) // let the initial walk add watches
	return col, cancel
}

// waitFor polls until cond returns true or the deadline elapses.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestWatcherCapturesModifyAndDelete(t *testing.T) {
	root := t.TempDir()
	col, _ := startWatcher(t, root, 30*time.Millisecond)

	target := filepath.Join(root, "note.md")
	if err := os.WriteFile(target, []byte("Alice prefers dark mode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool { return len(col.snapshot()) >= 1 }) {
		t.Fatalf("no change captured after write; got %d", len(col.snapshot()))
	}
	got := col.snapshot()[0]
	if got.Kind != ChangeModified {
		t.Errorf("kind = %q, want %q", got.Kind, ChangeModified)
	}
	if got.RelPath != "note.md" {
		t.Errorf("relPath = %q, want note.md", got.RelPath)
	}
	if string(got.Content) != "Alice prefers dark mode" {
		t.Errorf("content = %q", got.Content)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool {
		for _, c := range col.snapshot() {
			if c.Kind == ChangeDeleted {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("delete not captured; changes=%+v", col.snapshot())
	}
}

func TestWatcherDebounceCollapsesBurst(t *testing.T) {
	root := t.TempDir()
	col, _ := startWatcher(t, root, 200*time.Millisecond)

	target := filepath.Join(root, "burst.txt")
	// Ten rapid writes well within the debounce window.
	for i := range 10 {
		if err := os.WriteFile(target, []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waitFor(t, func() bool { return len(col.snapshot()) >= 1 }) {
		t.Fatal("burst produced no change")
	}
	// Allow any stragglers to flush, then assert collapse to a single emit.
	time.Sleep(400 * time.Millisecond)
	if n := len(col.snapshot()); n != 1 {
		t.Errorf("debounce should collapse burst to 1 change, got %d", n)
	}
}

func TestWatcherHonoursIgnoreRules(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	col, _ := startWatcher(t, root, 30*time.Millisecond)

	// These must all be ignored.
	mustWrite(t, filepath.Join(root, "server.log"), "noise")
	mustWrite(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "dep")
	mustWriteBinary(t, filepath.Join(root, "image.png"), []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01})
	// This one must be captured.
	mustWrite(t, filepath.Join(root, "keep.go"), "package main")

	if !waitFor(t, func() bool {
		for _, c := range col.snapshot() {
			if c.RelPath == "keep.go" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("kept file never captured; changes=%+v", col.snapshot())
	}
	time.Sleep(150 * time.Millisecond)
	for _, c := range col.snapshot() {
		if c.RelPath != "keep.go" {
			t.Errorf("captured an ignored path: %q", c.RelPath)
		}
	}
}

func TestWatcherCapturesNewSubdir(t *testing.T) {
	root := t.TempDir()
	col, _ := startWatcher(t, root, 30*time.Millisecond)

	// Create a brand-new subdir after the watcher started, then a file in it.
	sub := filepath.Join(root, "newpkg")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the Create-dir handler add the watch
	mustWrite(t, filepath.Join(sub, "deep.go"), "package newpkg")

	if !waitFor(t, func() bool {
		for _, c := range col.snapshot() {
			if c.RelPath == "newpkg/deep.go" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("file in new subdir not captured; changes=%+v", col.snapshot())
	}
}

func TestWatcherTruncatesLargeFile(t *testing.T) {
	root := t.TempDir()
	w, err := New(Options{Roots: []string{root}, Debounce: 30 * time.Millisecond, SizeCap: 16})
	if err != nil {
		t.Fatal(err)
	}
	col := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx, col.emit) }()
	time.Sleep(100 * time.Millisecond)

	mustWrite(t, filepath.Join(root, "big.txt"), "0123456789ABCDEFGHIJKLMNOP")
	if !waitFor(t, func() bool { return len(col.snapshot()) >= 1 }) {
		t.Fatal("large file not captured")
	}
	got := col.snapshot()[0]
	if !got.Truncated {
		t.Error("expected truncated flag")
	}
	if len(got.Content) != 16 {
		t.Errorf("content len = %d, want 16", len(got.Content))
	}
}

func TestNewRejectsNoRoots(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error for empty roots")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustWriteBinary(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
