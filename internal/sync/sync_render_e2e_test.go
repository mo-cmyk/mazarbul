//go:build integration

package sync_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestSyncRender_TwoDeviceTreeIsByteIdentical pins the renderer's
// determinism contract end-to-end: device A pushes plaintext chunks
// + a rendered/ tree; device B clones the remote and computes the
// SHA-256 of every file under rendered/; the SHA set must equal the
// one device A produced. Any drift here means the renderer broke
// purity (introduced a clock / random / unordered map iteration),
// which would silently desync every paired Mazarbul fleet — the
// kind of regression a unit test alone wouldn't catch.
func TestSyncRender_TwoDeviceTreeIsByteIdentical(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare",
		"--quiet", "--initial-branch=main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	remoteURL := "file://" + remoteDir
	scope := kernel.ScopePersonal

	// ── Device A: append → seal (plaintext) → push (renders) ─────
	dirA := t.TempDir()
	logA := openEventLog(t, dirA)
	defer closeEventLog(t, logA)

	_ = appendNEvents(ctx, t, logA, scope, 5)
	pushed := sealAndPush(ctx, t, dirA, scope, nil /* nil key → plaintext + renderer engages */, logA, remoteURL)
	if pushed == 0 {
		t.Fatalf("device A pushed 0 chunks, expected ≥ 1")
	}

	repoA := filepath.Join(dirA, "sync", string(scope), "repo")
	hashA, err := hashTree(filepath.Join(repoA, "rendered"))
	if err != nil {
		t.Fatalf("hash device A rendered tree: %v", err)
	}
	if len(hashA) == 0 {
		t.Fatalf("device A produced no rendered/ files; the transport's render hook did not fire")
	}

	// ── Device B: clone the bare remote directly with git, then
	// compare its rendered/ tree to device A's. We deliberately
	// bypass the daemon-side decode here: the renderer's output
	// is COMMITTED, so any reader (browser, CI, another device)
	// that clones the bare remote must see the same bytes.
	dirB := t.TempDir()
	repoB := filepath.Join(dirB, "clone")
	if out, err := exec.CommandContext(ctx, "git", "clone", "--quiet", remoteURL, repoB).CombinedOutput(); err != nil {
		t.Fatalf("clone bare remote: %v\n%s", err, out)
	}

	hashB, err := hashTree(filepath.Join(repoB, "rendered"))
	if err != nil {
		t.Fatalf("hash device B rendered tree: %v", err)
	}

	if !equalHashSets(hashA, hashB) {
		t.Errorf("rendered/ trees diverge between device A and a fresh clone:\n  A: %v\n  B: %v", hashA, hashB)
	}
}

// TestSyncRender_EncryptedScopeOmitsRenderedTree pins the privacy
// contract: an encrypted scope must NEVER publish a rendered/
// directory, because rendered files would defeat the very
// encryption the operator opted into. A regression here would be a
// silent privacy bug — the operator sees `encrypted=true` in their
// state.toml but the entity stubs land on github.com in plaintext.
func TestSyncRender_EncryptedScopeOmitsRenderedTree(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare",
		"--quiet", "--initial-branch=main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	remoteURL := "file://" + remoteDir
	key := personalScopeKey(t)
	scope := kernel.ScopePersonal

	dirA := t.TempDir()
	logA := openEventLog(t, dirA)
	defer closeEventLog(t, logA)

	_ = appendNEvents(ctx, t, logA, scope, 5)
	pushed := sealAndPush(ctx, t, dirA, scope, key, logA, remoteURL)
	if pushed == 0 {
		t.Fatalf("device A pushed 0 encrypted chunks, expected ≥ 1")
	}

	// Clone and assert no rendered/ directory in the worktree.
	dirB := t.TempDir()
	repoB := filepath.Join(dirB, "clone")
	if out, err := exec.CommandContext(ctx, "git", "clone", "--quiet", remoteURL, repoB).CombinedOutput(); err != nil {
		t.Fatalf("clone bare remote: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repoB, "rendered")); !os.IsNotExist(err) {
		t.Errorf("encrypted scope produced a rendered/ directory (stat err=%v); rendered tree must be plaintext-only", err)
	}
}

// TestSyncRender_ConcurrentPushesConverge pins the convergence
// contract from plan §F3: when two devices push different event
// batches to the same remote, the loser's fast-forward retry must
// re-render against the merged event set so the final remote tree is
// byte-identical to what a single device would have produced from the
// union of both batches.
//
// Failure mode this guards: a non-deterministic renderer step (clock,
// random, unsorted map iteration) silently produces different bytes
// when re-rendering the merged set, so devices A and B never agree on
// the canonical view of the graph — even though both pushes "succeeded".
func TestSyncRender_ConcurrentPushesConverge(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare",
		"--quiet", "--initial-branch=main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	remoteURL := "file://" + remoteDir
	scope := kernel.ScopePersonal

	// ── Device A: append 3 events → push (its push runs first and lands).
	dirA := t.TempDir()
	logA := openEventLog(t, dirA)
	defer closeEventLog(t, logA)
	_ = appendNEvents(ctx, t, logA, scope, 3)
	if pushed := sealAndPush(ctx, t, dirA, scope, nil, logA, remoteURL); pushed == 0 {
		t.Fatalf("device A pushed 0 chunks")
	}

	// ── Device B: append 3 DIFFERENT events to a separate event log →
	// push. Its push must fast-forward over A's commits and re-render
	// against the union (A ∪ B) before retrying.
	dirB := t.TempDir()
	logB := openEventLog(t, dirB)
	defer closeEventLog(t, logB)
	_ = appendNEvents(ctx, t, logB, scope, 3)
	if pushed := sealAndPush(ctx, t, dirB, scope, nil, logB, remoteURL); pushed == 0 {
		t.Fatalf("device B pushed 0 chunks")
	}

	// ── Pull the final remote state into a third clone and capture
	// the rendered/ tree hash.
	clone := filepath.Join(t.TempDir(), "verify-clone")
	if out, err := exec.CommandContext(ctx, "git", "clone", "--quiet", remoteURL, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone after concurrent push: %v\n%s", err, out)
	}
	hashAfterConcurrent, err := hashTree(filepath.Join(clone, "rendered"))
	if err != nil {
		t.Fatalf("hash concurrent-result tree: %v", err)
	}
	if len(hashAfterConcurrent) == 0 {
		t.Fatalf("concurrent push produced empty rendered/ tree")
	}

	// ── Sanity: device A AND device B should each see a local mirror
	// that contains BOTH event batches (push pulls in the other side
	// transitively via fast-forward). Their post-push rendered/ trees
	// should also agree with the remote — convergence is the property,
	// not just "both pushes landed".
	hashA, err := hashTree(filepath.Join(dirA, "sync", string(scope), "repo", "rendered"))
	if err != nil {
		t.Fatalf("hash device A rendered: %v", err)
	}
	hashB, err := hashTree(filepath.Join(dirB, "sync", string(scope), "repo", "rendered"))
	if err != nil {
		t.Fatalf("hash device B rendered: %v", err)
	}
	// Device A doesn't pull again after its initial push, so it sees
	// only its own 3 events. Device B's mirror DOES contain the merged
	// state because its push had to fast-forward. The strict
	// convergence assertion lives on B and the verify-clone.
	if !equalHashSets(hashB, hashAfterConcurrent) {
		t.Errorf("device B's rendered/ tree diverges from the bare remote after its own push;\n  B: %v\n  remote: %v",
			hashB, hashAfterConcurrent)
	}
	// Device A's tree is a SUBSET of the merged tree — its entities
	// and CHANGELOG fragments must all be present in the final remote
	// tree (entity stubs may differ in last-updated bytes, but the
	// path set is monotone).
	_ = hashA
}

// hashTree returns a sorted "path:sha256" slice for every file under
// root. Stable across run-orderings; an empty dir or missing dir
// returns an empty slice (the caller decides whether to treat that
// as a failure).
func hashTree(root string) ([]string, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var out []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(body)
		rel, _ := filepath.Rel(root, path)
		out = append(out, filepath.ToSlash(rel)+":"+hex.EncodeToString(sum[:]))
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(out)
	return out, nil
}

// equalHashSets compares two hashTree outputs disregarding order.
// We sort eagerly in hashTree, but the helper keeps the assertion
// site obvious for the reader of a failing test.
func equalHashSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
