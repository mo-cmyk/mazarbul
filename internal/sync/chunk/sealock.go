package chunk

// sealock.go — interprocess mutex around the seal step.
//
// Why: the chunk cursor uses an in-process mutex (FileCursor.mu) which
// serialises seals inside one daemon, but the CLI gained a
// pre-push seal in `maz sync push`. Two processes (daemon supervisor
// + CLI) can read the same sealed.jsonl set, sort the same event tail,
// and write divergent chunks with different ChunkIDs (the device tag
// differs). That ships the same logical events twice to the remote.
//
// Fix: a per-data-dir lockfile (`<dataDir>/sync/.seal.lock`) that both
// the supervisor and the CLI try-acquire (LOCK_EX | LOCK_NB) before
// every seal pass. Owner seals; the other side skips and falls back to
// "push whatever's currently sealed" without a duplicate.
//
// Cross-platform note: syscall.Flock is POSIX (darwin + linux). The
// repo's only target platforms today; we'd surface a Windows fallback
// here if that ever changed.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// sealLockFileName is the basename of the per-scope lock. Lives under
// the same parent as the chunk store and sealed.jsonl so a directory
// snapshot covers it cleanly.
const sealLockFileName = ".seal.lock"

// SealLock is an acquired interprocess seal lock. Release with Close;
// safe to Close more than once. Locks are held for the lifetime of the
// returned handle; concurrent acquirers see ErrSealBusy from
// TryAcquireSealLock and must skip the seal step.
type SealLock struct {
	f      *os.File
	closed bool
}

// ErrSealBusy reports that another process owns the seal lock.
// Callers treat this as "the other side is sealing right now, skip
// this pass" — it is NOT a hard error and the caller must not abort.
var ErrSealBusy = errors.New("chunk: seal lock busy (another process is sealing)")

// TryAcquireSealLock attempts a non-blocking exclusive flock on
// <dataDir>/sync/.seal.lock. Returns (nil, ErrSealBusy) when another
// process owns it. Any other non-nil error is structural (mkdir
// failure, fd exhaustion) and the caller should escalate.
//
// The lockfile is left in place across runs — the lock is on the fd,
// not the inode — so two processes sharing dataDir always race against
// the same path.
func TryAcquireSealLock(dataDir string) (*SealLock, error) {
	parent := filepath.Join(dataDir, "sync")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("chunk: mkdir seal lock parent: %w", err)
	}
	path := filepath.Join(parent, sealLockFileName)
	// gosec G304: path is composed from dataDir (operator-controlled,
	// validated by the caller) and a hard-coded basename. No untrusted
	// component flows here.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("chunk: open seal lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if cerr := f.Close(); cerr != nil {
			return nil, fmt.Errorf("chunk: flock seal lock: %w (close also failed: %w)", err, cerr)
		}
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrSealBusy
		}
		return nil, fmt.Errorf("chunk: flock seal lock: %w", err)
	}
	return &SealLock{f: f}, nil
}

// Close releases the flock and closes the descriptor. Idempotent; a
// double-Close is a no-op so deferred-Close + explicit-Close patterns
// stay safe. Errors from the flock release are swallowed (the OS
// drops the lock on close regardless); errors from Close are returned
// so the caller can surface them.
func (l *SealLock) Close() error {
	if l == nil || l.closed {
		return nil
	}
	l.closed = true
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		// The OS releases the lock on close regardless. Log-only
		// diagnostic; the caller cannot do anything useful with this.
		_ = err
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("chunk: close seal lock: %w", err)
	}
	return nil
}
