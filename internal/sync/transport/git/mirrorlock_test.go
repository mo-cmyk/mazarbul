package git

// mirrorlock_test.go — the cross-process mirror lock (F7 review Finding B).
//
// The daemon's push loop and a `maz sync push` CLI invocation share one
// on-disk mirror per scope but each hold only their own in-process
// mirrorLock (a different *sync.Mutex instance per process). Without an
// interprocess lock, one process's resetMirrorToRemote (git clean -fd
// chunks/) or rollbackForRetry (git checkout -- .) can delete the other's
// staged-but-uncommitted chunk mid-push. These tests pin the flock that
// serializes the two.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// TestTryAcquireMirrorFileLock_MutualExclusion proves the primitive: a second
// acquire while the first is held reports errMirrorBusy (not a hard error), and
// releasing the first lets the next acquire succeed.
func TestTryAcquireMirrorFileLock_MutualExclusion(t *testing.T) {
	dataDir := t.TempDir()
	tr, err := New(Options{
		DataDir:    dataDir,
		RemoteURLs: map[kernel.Scope]string{kernel.ScopeTeam: "file:///unused"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := tr.tryAcquireMirrorFileLock(kernel.ScopeTeam)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second acquire (a distinct fd, as a separate process would open) must
	// report contention rather than block or corrupt.
	if _, err := tr.tryAcquireMirrorFileLock(kernel.ScopeTeam); !errors.Is(err, errMirrorBusy) {
		t.Fatalf("second acquire while held: want errMirrorBusy, got %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("release first: %v", err)
	}

	// After release the lock is re-acquirable.
	again, err := tr.tryAcquireMirrorFileLock(kernel.ScopeTeam)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("release again: %v", err)
	}

	// A different scope is a different lock — never contends.
	other, err := tr.tryAcquireMirrorFileLock(kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("acquire other scope: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("release other: %v", err)
	}
}

// TestPush_YieldsToConcurrentMirrorLockHolder proves Push wires the flock in:
// when another holder owns the scope's mirror lock, Push returns ErrPushContention
// and never proceeds to mutate the mirror working tree (the repo dir is not even
// created). The daemon re-offers the chunk on its next tick, so no data is lost.
func TestPush_YieldsToConcurrentMirrorLockHolder(t *testing.T) {
	dataDir := t.TempDir()
	tr, err := New(Options{
		DataDir:    dataDir,
		RemoteURLs: map[kernel.Scope]string{kernel.ScopeTeam: "file:///unused"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Simulate a concurrent process (daemon or CLI) mid-push holding the lock.
	held, err := tr.tryAcquireMirrorFileLock(kernel.ScopeTeam)
	if err != nil {
		t.Fatalf("hold mirror lock: %v", err)
	}
	defer func() { _ = held.Close() }()

	ev := kernel.Event{
		UID:        "01JBQZ0000000000000000TEAM",
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    []byte(`{"text":"lock test"}`),
	}
	body, hdr, err := chunk.EncodePlaintext([]kernel.Event{ev}, kernel.ScopeTeam, "test-device")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	blob := transport.Blob{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}

	_, perr := tr.Push(context.Background(), kernel.ScopeTeam, []transport.Blob{blob})
	if !errors.Is(perr, transport.ErrPushContention) {
		t.Fatalf("Push while mirror lock held: want ErrPushContention, got %v", perr)
	}

	// Push must have yielded before touching the working tree.
	repoDir := filepath.Join(dataDir, "sync", string(kernel.ScopeTeam), "repo")
	if _, statErr := os.Stat(repoDir); !os.IsNotExist(statErr) {
		t.Fatalf("Push mutated the mirror despite contention: repo dir exists (%v)", statErr)
	}
}
