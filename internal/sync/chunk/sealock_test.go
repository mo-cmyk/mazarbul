package chunk_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// TestSealLock_ExclusiveAcquisition pins the invariant the lock
// exists to enforce: while one process holds the seal lock, every other
// non-blocking acquirer must see ErrSealBusy. The CLI's
// sealBeforePush and the daemon's sealOnce both rely on this to decide
// "skip this pass" without aborting the push.
func TestSealLock_ExclusiveAcquisition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := chunk.TryAcquireSealLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() {
		if cerr := first.Close(); cerr != nil {
			t.Errorf("first close: %v", cerr)
		}
	})

	second, err := chunk.TryAcquireSealLock(dir)
	if !errors.Is(err, chunk.ErrSealBusy) {
		t.Fatalf("second acquire: want ErrSealBusy, got err=%v second=%v", err, second)
	}
	if second != nil {
		t.Fatalf("second acquire returned non-nil lock alongside ErrSealBusy: %v", second)
	}
}

// TestSealLock_ReleaseAllowsReacquisition guards the supervisor's
// pattern: take the lock, seal, release, let the next tick take it
// again. A release-then-reacquire must succeed in the same process.
func TestSealLock_ReleaseAllowsReacquisition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := chunk.TryAcquireSealLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	second, err := chunk.TryAcquireSealLock(dir)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestSealLock_CloseIdempotent guards the defer-Close + explicit-Close
// pattern at sealOnce / sealBeforePush call sites. Double-Close must
// not panic and must not return an error on the second call.
func TestSealLock_CloseIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	lock, err := chunk.TryAcquireSealLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("second close: want nil, got %v", err)
	}
	// Nil-receiver Close is also part of the contract — call sites
	// guard with `if lock != nil` but the receiver must tolerate nil
	// for the deferred-cleanup-after-error-path shape.
	var nilLock *chunk.SealLock
	if err := nilLock.Close(); err != nil {
		t.Errorf("nil-receiver close: want nil, got %v", err)
	}
}

// TestSealLock_CreatesParentDir verifies the helper's mkdir step works
// when <dataDir>/sync/ does not exist yet (first-ever seal on a fresh
// data dir). The directory must be created and the lockfile must land
// inside it.
func TestSealLock_CreatesParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	lock, err := chunk.TryAcquireSealLock(dir)
	if err != nil {
		t.Fatalf("acquire on fresh dataDir: %v", err)
	}
	t.Cleanup(func() { _ = lock.Close() })

	expected := filepath.Join(dir, "sync", ".seal.lock")
	if _, statErr := os.Stat(expected); statErr != nil {
		t.Errorf("lockfile not at %s: %v", expected, statErr)
	}
}
