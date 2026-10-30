//go:build integration

package git_test

// history_rewrite_test.go — integration test for C7: force-push detection and
// repair. Uses a real bare git remote (file://) to exercise the full path:
// fetch → marker read → merge-base ancestry check → ErrHistoryRewritten.
//
// Run with: mise exec -- go test -tags='sqlite_fts5 integration' -count=1 \
//   -timeout=60s -run TestHistoryRewrite ./internal/sync/transport/git/...

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// TestHistoryRewrite_FastForwardReturnsErrHistoryRewritten is the end-to-end
// integration test for C7:
//
//  1. Bare remote is created.
//  2. Device A pushes a chunk (establishes root commit C1).
//  3. Device B clones via List — marker file written with C1's SHA.
//  4. The bare remote is force-reset to a fresh orphan root (no common ancestor).
//  5. Device A pushes another chunk on top of the rewritten history.
//  6. Device B's next List triggers fastForward → returns ErrHistoryRewritten.
//  7. Simulate `maz sync repair`: delete B's mirror.
//  8. Device B lists again without error and sees A's latest chunk.
func TestHistoryRewrite_FastForwardReturnsErrHistoryRewritten(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)

	// Step 1: A pushes the initial commit so the remote has history.
	aDir := t.TempDir()
	a := newTransport(t, aDir, remote, "device-a")
	body1, hdr1 := makePlaintextChunk(t, "01FFFFFFFFFFFFFFFFFFFF0A1")
	if _, err := a.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: hdr1, Body: io.NopCloser(bytes.NewReader(body1))},
	}); err != nil {
		t.Fatalf("A Push (setup): %v", err)
	}

	// Step 2: B clones via List — this runs fastForward which writes the marker.
	bDir := t.TempDir()
	b := newTransport(t, bDir, remote, "device-b")
	if _, err := b.List(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("B List (clone): %v", err)
	}

	// Verify marker was written.
	bMirror := filepath.Join(bDir, "sync", "personal", "repo")
	markerPath := filepath.Join(bMirror, ".git", "maz-last-remote-head")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("marker file missing after B.List: %v", err)
	}

	// Step 3: Force-reset the remote to an orphan root — simulates force-push
	// that rewrites history (C1 is no longer an ancestor of origin/main).
	forcePushOrphan(t, remote)

	// Step 4: A fresh device (C) pushes on top of the orphan history.
	// Device A's mirror still has C1 and cannot fast-forward onto an unrelated
	// root, so we use a new transport with no prior mirror to represent a
	// legitimate pusher that started after the force-push.
	cDir := t.TempDir()
	c := newTransport(t, cDir, remote, "device-c")
	body2, hdr2 := makePlaintextChunk(t, "01FFFFFFFFFFFFFFFFFFFF0A2")
	if _, err := c.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: hdr2, Body: io.NopCloser(bytes.NewReader(body2))},
	}); err != nil {
		t.Fatalf("C Push after force-reset: %v", err)
	}

	// Step 5: B's List must detect the rewrite.
	_, listErr := b.List(ctx, kernel.ScopePersonal)
	if !errors.Is(listErr, transport.ErrHistoryRewritten) {
		t.Errorf("B.List after force-push: want ErrHistoryRewritten, got %v", listErr)
	}

	// Step 6: Simulate `maz sync repair --scope personal --yes`:
	// delete B's mirror so the next List re-clones cleanly.
	if err := os.RemoveAll(bMirror); err != nil {
		t.Fatalf("remove B mirror (repair simulation): %v", err)
	}

	// Step 7: B lists again — should succeed and see A's new chunk.
	bRepaired := newTransport(t, bDir, remote, "device-b")
	listing, err := bRepaired.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("B.List after repair: %v", err)
	}
	found := false
	for _, h := range listing.Chunks {
		if h.ChunkID == hdr2.ChunkID {
			found = true
		}
	}
	if !found {
		t.Errorf("B after repair: chunk %s not in listing (chunks: %v)", hdr2.ChunkID, listing.Chunks)
	}
}

// forcePushOrphan clones remoteURL into a temp dir, creates a fresh orphan
// root commit with no common ancestor, and force-pushes it onto main.
// This simulates a destructive `git push --force` that rewrites history.
func forcePushOrphan(t *testing.T, remoteURL string) {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "force-push-clone")

	gitRun(t, "", "clone", "--quiet", remoteURL, workDir)
	gitRun(t, workDir, "checkout", "--quiet", "--orphan", "rewrite-orphan")
	gitRun(t, workDir, "rm", "-rf", "--quiet", ".")
	// Write a placeholder file — git refuses to commit an empty tree.
	placeholder := filepath.Join(workDir, ".force-rewritten")
	if err := os.WriteFile(placeholder, []byte("force-rewritten\n"), 0o644); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}
	gitRun(t, workDir, "add", ".force-rewritten")
	gitRun(t, workDir, "commit", "--quiet", "-m", "force-rewritten root (test fixture)")
	gitRun(t, workDir, "push", "--force", "--quiet", "origin", "HEAD:main")
}

// gitRun executes `git <args>` in dir (empty dir = inherit working directory)
// with a test-safe author identity and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test-fixture",
		"GIT_AUTHOR_EMAIL=test@fixture.local",
		"GIT_COMMITTER_NAME=test-fixture",
		"GIT_COMMITTER_EMAIL=test@fixture.local",
		"GIT_TERMINAL_PROMPT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
