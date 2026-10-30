package cli

// init_lock_test.go pins the data-dir lock UX the user hit: re-running `maz
// init` while a daemon is already up must NOT contend for the LadybugDB lock,
// and a held-lock message must name the real holder rather than misdirecting
// the user to `maz daemon stop` (which cannot release a `maz mcp serve` lock).

import (
	"context"
	"strings"
	"testing"
)

func TestDataDirLockedHint(t *testing.T) {
	t.Run("running daemon points at daemon stop", func(t *testing.T) {
		prevProbe := probeDaemonListening
		probeDaemonListening = func(string) bool { return true }
		t.Cleanup(func() { probeDaemonListening = prevProbe })

		got := dataDirLockedHint("/data/dir")
		if !strings.Contains(got, "maz daemon stop") {
			t.Errorf("hint %q should suggest `maz daemon stop` when a daemon is up", got)
		}
		if strings.Contains(got, "will NOT help") {
			t.Errorf("hint %q must not say daemon stop won't help when a daemon IS the holder", got)
		}
	})

	t.Run("no daemon names the holding process", func(t *testing.T) {
		prevProbe := probeDaemonListening
		probeDaemonListening = func(string) bool { return false }
		t.Cleanup(func() { probeDaemonListening = prevProbe })

		prevHolder := lockHolderDescriber
		lockHolderDescriber = func(string) string { return "PID 65992 (maz mcp serve)" }
		t.Cleanup(func() { lockHolderDescriber = prevHolder })

		got := dataDirLockedHint("/data/dir")
		for _, want := range []string{"PID 65992", "maz mcp serve", "only helps if the holder is the background daemon"} {
			if !strings.Contains(got, want) {
				t.Errorf("hint %q missing %q", got, want)
			}
		}
	})

	t.Run("no daemon and unknown holder still steers away from daemon stop", func(t *testing.T) {
		prevProbe := probeDaemonListening
		probeDaemonListening = func(string) bool { return false }
		t.Cleanup(func() { probeDaemonListening = prevProbe })

		prevHolder := lockHolderDescriber
		lockHolderDescriber = func(string) string { return "" }
		t.Cleanup(func() { lockHolderDescriber = prevHolder })

		got := dataDirLockedHint("/data/dir")
		for _, want := range []string{"no daemon socket is responding", "mcp serve", "only helps if the holder is the background daemon"} {
			if !strings.Contains(got, want) {
				t.Errorf("hint %q missing %q", got, want)
			}
		}
	})
}

// TestPrewarmStorage_SkipsWhenDaemonListening proves init no longer races a
// healthy running daemon for the storage lock: with a daemon listening it must
// return without ever opening the dispatcher (so a bogus data dir never errors).
func TestPrewarmStorage_SkipsWhenDaemonListening(t *testing.T) {
	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return true }
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	// A path that would fail hard if dispatcher.Open were actually attempted.
	if err := prewarmStorage(context.Background(), "/nonexistent/locked/dir"); err != nil {
		t.Fatalf("prewarmStorage should no-op when a daemon is listening, got %v", err)
	}
}
