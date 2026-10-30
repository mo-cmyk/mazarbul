package git

// recipients_test.go pins the copy-up contract for the recipients.jsonl
// bootstrap artifact: an encrypted push lands the daemon-staged file at
// the repo root, a plaintext push does not (and prunes a stale copy), and
// the encrypted-flip prune leaves a repo-root recipients.jsonl intact.

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// encryptedBlob is a minimal transport.Blob flagged encrypted so
// repoHasEncryptedChunks reports true. The body is never read by
// reconcileRecipients, so a header is enough.
func encryptedBlob() transport.Blob {
	return transport.Blob{
		Header: chunk.Header{ChunkID: "01HQ8ZENC00000000000000001", Scope: kernel.ScopePersonal, Encrypted: true},
		Body:   io.NopCloser(nil),
	}
}

// plaintextHeaderBlob is a minimal plaintext-flagged blob.
func plaintextHeaderBlob() transport.Blob {
	return transport.Blob{
		Header: chunk.Header{ChunkID: "01HQ8ZPLN00000000000000001", Scope: kernel.ScopePersonal, Encrypted: false},
		Body:   io.NopCloser(nil),
	}
}

// stageRecipients writes a staged recipients.jsonl at the daemon path
// <dataDir>/sync/<scope>/recipients.jsonl and returns its bytes.
func stageRecipients(t *testing.T, tr *Transport, scope kernel.Scope) []byte {
	t.Helper()
	staged := filepath.Join(tr.dataDir, "sync", string(scope), recipients.Filename)
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatalf("mkdir staged: %v", err)
	}
	body := []byte(`{"principalUid":"01HZZZZ0000000000000000001","wrapPublicKey":"cHViLTE=",` +
		`"dekEpoch":1,"wrappedDek":"d3JhcC0x","createTime":"2026-06-16T12:00:00Z"}` + "\n")
	if err := os.WriteFile(staged, body, 0o600); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	return body
}

// mkRepoDir creates and returns the scope repo dir for tr.
func mkRepoDir(t *testing.T, tr *Transport, scope kernel.Scope) string {
	t.Helper()
	repoDir := tr.scopeRepo(scope)
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	return repoDir
}

// TestReconcileRecipients_EncryptedLandsAtRoot confirms an encrypted push
// copies the staged recipients.jsonl into the repo root verbatim.
func TestReconcileRecipients_EncryptedLandsAtRoot(t *testing.T) {
	t.Parallel()
	tr := newGuardTransport(t)
	scope := kernel.ScopePersonal
	repoDir := mkRepoDir(t, tr, scope)
	want := stageRecipients(t, tr, scope)

	if err := tr.reconcileRecipients(scope, []transport.Blob{encryptedBlob()}, repoDir); err != nil {
		t.Fatalf("reconcileRecipients: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoDir, recipients.Filename))
	if err != nil {
		t.Fatalf("read repo-root recipients: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("repo-root recipients mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestReconcileRecipients_EncryptedNoStagedFileIsNoOp confirms that when
// the daemon has not staged a recipients file, an encrypted push neither
// errors nor creates a repo-root copy.
func TestReconcileRecipients_EncryptedNoStagedFileIsNoOp(t *testing.T) {
	t.Parallel()
	tr := newGuardTransport(t)
	scope := kernel.ScopePersonal
	repoDir := mkRepoDir(t, tr, scope)

	if err := tr.reconcileRecipients(scope, []transport.Blob{encryptedBlob()}, repoDir); err != nil {
		t.Fatalf("reconcileRecipients with no staged file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, recipients.Filename)); !os.IsNotExist(err) {
		t.Errorf("repo-root recipients should be absent (err=%v)", err)
	}
}

// TestReconcileRecipients_PlaintextDoesNotCopy confirms a plaintext push
// never copies the staged recipients file (recipients is envelope-mode).
func TestReconcileRecipients_PlaintextDoesNotCopy(t *testing.T) {
	t.Parallel()
	tr := newGuardTransport(t)
	scope := kernel.ScopePersonal
	repoDir := mkRepoDir(t, tr, scope)
	stageRecipients(t, tr, scope) // staged, but a plaintext push must ignore it

	if err := tr.reconcileRecipients(scope, []transport.Blob{plaintextHeaderBlob()}, repoDir); err != nil {
		t.Fatalf("reconcileRecipients (plaintext): %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, recipients.Filename)); !os.IsNotExist(err) {
		t.Errorf("plaintext push must not copy recipients into repo root (err=%v)", err)
	}
}

// TestReconcileRecipients_PlaintextPrunesStale confirms that a scope which
// flipped from encrypted to plaintext drops a lingering repo-root
// recipients.jsonl so the old wrapped-DEK roster is not re-committed.
func TestReconcileRecipients_PlaintextPrunesStale(t *testing.T) {
	t.Parallel()
	tr := newGuardTransport(t)
	scope := kernel.ScopePersonal
	repoDir := mkRepoDir(t, tr, scope)
	stale := filepath.Join(repoDir, recipients.Filename)
	if err := os.WriteFile(stale, []byte("stale-roster\n"), 0o600); err != nil {
		t.Fatalf("seed stale recipients: %v", err)
	}

	if err := tr.reconcileRecipients(scope, []transport.Blob{plaintextHeaderBlob()}, repoDir); err != nil {
		t.Fatalf("reconcileRecipients (plaintext prune): %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("plaintext push must prune a stale repo-root recipients.jsonl (err=%v)", err)
	}
}

// TestPruneRenderedTree_LeavesRecipientsIntact confirms the encrypted-flip
// prune of the renderer-owned subtrees does NOT delete a repo-root
// recipients.jsonl: it is a transport-owned bootstrap artifact, not part
// of rendered/, CHANGELOG/, or ontology/.
func TestPruneRenderedTree_LeavesRecipientsIntact(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	recipientsPath := filepath.Join(repoDir, recipients.Filename)
	if err := os.WriteFile(recipientsPath, []byte("roster\n"), 0o600); err != nil {
		t.Fatalf("seed recipients: %v", err)
	}
	// Also seed a renderer-owned file so the prune has something to remove.
	rendered := filepath.Join(repoDir, "rendered", "entities.jsonl")
	if err := os.MkdirAll(filepath.Dir(rendered), 0o750); err != nil {
		t.Fatalf("mkdir rendered: %v", err)
	}
	if err := os.WriteFile(rendered, []byte("graph"), 0o600); err != nil {
		t.Fatalf("seed rendered: %v", err)
	}

	if err := pruneRenderedTree(repoDir); err != nil {
		t.Fatalf("pruneRenderedTree: %v", err)
	}
	if _, err := os.Stat(recipientsPath); err != nil {
		t.Errorf("pruneRenderedTree wrongly removed repo-root recipients.jsonl: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "rendered")); !os.IsNotExist(err) {
		t.Errorf("rendered/ should have been pruned (err=%v)", err)
	}
}
