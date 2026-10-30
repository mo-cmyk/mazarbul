package git

// mirrorlock.go — interprocess mutex around a scope's local git mirror.
//
// Why: mirrorLock (the in-process *sync.Mutex per scope) serializes the
// daemon's own push and consume loops, but it cannot reach across
// processes. A `maz sync push` / `maz push` CLI invocation opens its own
// Transport against the SAME on-disk mirror (`<dataDir>/sync/<scope>/repo`)
// with its own, unrelated mirrorLock. Two processes then run Push
// concurrently on one working tree — and Push's recovery paths mutate that
// tree destructively: resetMirrorToRemote runs `git clean -fd chunks/` and
// rollbackForRetry runs `git checkout -- .`, either of which can delete the
// other process's staged-but-uncommitted chunk mid-push.
//
// Fix: a per-scope lockfile (`<dataDir>/sync/<scope>/.mirror.lock`, a
// sibling of repo/ so git clean/reset never touch it) that Push
// try-acquires (LOCK_EX | LOCK_NB) before it opens the working tree. The
// loser reports ErrPushContention and yields; its chunks stay in the
// FSStore and the daemon's push loop re-offers them on the next tick, so
// nothing is lost. Reads (List/Get/Presence) are left unguarded: git's own
// ref locks already serialize concurrent `git fetch` ref updates, and a
// read that races a clean degrades to a retryable error, not data loss.
//
// This mirrors internal/sync/chunk/sealock.go (the seal-step equivalent).
// Cross-platform note: syscall.Flock is POSIX (darwin + linux), the repo's
// only targets today.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// mirrorLockFileName is the basename of the per-scope interprocess lock.
const mirrorLockFileName = ".mirror.lock"

// errMirrorBusy reports that another process holds the scope's mirror lock.
// Push maps it to ErrPushContention: the caller yields this pass and the
// daemon re-offers the chunk on its next tick. It is NOT a hard error.
var errMirrorBusy = errors.New("git transport: mirror lock busy (another process is pushing)")

// mirrorFileLock is an acquired interprocess mirror lock. Release with Close;
// safe to Close more than once. The lock lives on the fd, so two processes
// (or two fds within one process) sharing the path race against each other.
type mirrorFileLock struct {
	f      *os.File
	closed bool
}

// tryAcquireMirrorFileLock attempts a non-blocking exclusive flock on
// <dataDir>/sync/<scope>/.mirror.lock. Returns (nil, errMirrorBusy) when
// another holder owns it; any other non-nil error is structural (mkdir
// failure, fd exhaustion) and the caller should escalate. The lockfile is
// left in place across runs — the lock is on the fd, not the inode.
func (t *Transport) tryAcquireMirrorFileLock(scope kernel.Scope) (*mirrorFileLock, error) {
	dir := filepath.Join(t.dataDir, "sync", string(scope))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("git transport: mkdir mirror lock parent: %w", err)
	}
	path := filepath.Join(dir, mirrorLockFileName)
	// gosec G304: path is composed from dataDir (operator-controlled) plus a
	// validated scope segment and a hard-coded basename. No untrusted input.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("git transport: open mirror lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if cerr := f.Close(); cerr != nil {
			return nil, fmt.Errorf("git transport: flock mirror lock: %w (close also failed: %w)", err, cerr)
		}
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errMirrorBusy
		}
		return nil, fmt.Errorf("git transport: flock mirror lock: %w", err)
	}
	return &mirrorFileLock{f: f}, nil
}

// Close releases the flock and closes the descriptor. Idempotent so a
// deferred-Close + explicit-Close pattern stays safe.
func (l *mirrorFileLock) Close() error {
	if l == nil || l.closed {
		return nil
	}
	l.closed = true
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		// The OS drops the lock on close regardless; nothing actionable here.
		_ = err
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("git transport: close mirror lock: %w", err)
	}
	return nil
}
