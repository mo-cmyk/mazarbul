package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// guardChunkID is a valid 26-char Crockford ULID used for hand-built
// fixtures (it must pass validateChunkID in writeBlob).
const guardChunkID = "01HQ8ZAAAAAAAAAAAAAAAAAAAA"

func guardEvent(uid, source string, scope kernel.Scope) kernel.Event {
	return kernel.Event{
		Name:       "events/" + uid,
		UID:        uid,
		Kind:       "episode",
		Source:     source,
		Confidence: 1,
		Scope:      scope,
		Payload:    []byte(`{"text":"body"}`),
	}
}

// manualPlaintextChunk hand-builds a plaintext (.jsonl) chunk so a test
// can stage a SENSITIVE chunk that bypassed the seal-time gate — exactly
// the threat the transport/render mirrors defend against (a chunk pulled
// from a remote, or sealed before a marker existed). chunk.EncodePlaintext
// would (correctly) refuse to build one.
func manualPlaintextChunk(t *testing.T, chunkID string, scope kernel.Scope, events []kernel.Event) []byte {
	t.Helper()
	meta := map[string]any{
		"_meta":      true,
		"chunkId":    chunkID,
		"scope":      string(scope),
		"count":      len(events),
		"firstUlid":  events[0].UID,
		"lastUlid":   events[len(events)-1].UID,
		"createTime": "2026-06-15T00:00:00Z",
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(meta); err != nil {
		t.Fatalf("encode meta: %v", err)
	}
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode event: %v", err)
		}
	}
	return buf.Bytes()
}

func plaintextBlob(t *testing.T, e kernel.Event) transport.Blob {
	t.Helper()
	raw, hdr, err := chunk.EncodePlaintext([]kernel.Event{e}, e.Scope, "device-test")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	return transport.Blob{Header: hdr, Body: io.NopCloser(bytes.NewReader(raw))}
}

// TestWriteBlob_PlaintextSensitive_BlocksAndWritesNoFile pins the
// transport mirror: a sensitive plaintext blob is refused and no file
// lands in the chunks dir, so materializeBlobs aborts the push before
// any commit.
func TestWriteBlob_PlaintextSensitive_BlocksAndWritesNoFile(t *testing.T) {
	t.Parallel()
	chunksDir := t.TempDir()
	ev := guardEvent("01HQ8Z0000000000000000000A", "connector:granola", kernel.ScopePersonal)
	raw := manualPlaintextChunk(t, guardChunkID, kernel.ScopePersonal, []kernel.Event{ev})
	b := transport.Blob{
		Header: chunk.Header{ChunkID: guardChunkID, Scope: kernel.ScopePersonal, Count: 1, FirstULID: ev.UID, LastULID: ev.UID},
		Body:   io.NopCloser(bytes.NewReader(raw)),
	}

	err := writeBlob(b, chunksDir)
	if !errors.Is(err, errSensitiveBlobPlaintext) {
		t.Fatalf("writeBlob err = %v, want errSensitiveBlobPlaintext", err)
	}
	if !errors.Is(err, transport.ErrTransport) {
		t.Errorf("err does not wrap transport.ErrTransport: %v", err)
	}
	entries, rerr := os.ReadDir(chunksDir)
	if rerr != nil {
		t.Fatalf("ReadDir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("chunks dir has %d entr(ies), want 0 (blocked write must leave no file)", len(entries))
	}
}

// TestWriteBlob_PlaintextNonSensitive_Writes confirms the gate does not
// disturb the normal plaintext path.
func TestWriteBlob_PlaintextNonSensitive_Writes(t *testing.T) {
	t.Parallel()
	chunksDir := t.TempDir()
	b := plaintextBlob(t, guardEvent("01HQ8Z0000000000000000000B", "cli:remember", kernel.ScopePersonal))

	if err := writeBlob(b, chunksDir); err != nil {
		t.Fatalf("writeBlob: %v", err)
	}
	final := filepath.Join(chunksDir, b.Header.ChunkID+chunk.ExtensionJSONL)
	if _, err := os.Stat(final); err != nil {
		t.Errorf("expected plaintext chunk at %s: %v", final, err)
	}
}

// TestWriteBlob_EncryptedSensitive_Unaffected confirms encrypted blobs
// are streamed unchanged — the transport cannot (and must not) decode
// them, and encryption is the correct destination for sensitive data.
func TestWriteBlob_EncryptedSensitive_Unaffected(t *testing.T) {
	t.Parallel()
	chunksDir := t.TempDir()
	key := bytes.Repeat([]byte{0x42}, 32)
	raw, hdr, err := chunk.Encode(
		[]kernel.Event{guardEvent("01HQ8Z0000000000000000000C", "connector:granola", kernel.ScopePersonal)},
		kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	b := transport.Blob{Header: hdr, Body: io.NopCloser(bytes.NewReader(raw))}
	if err := writeBlob(b, chunksDir); err != nil {
		t.Fatalf("writeBlob (encrypted): %v", err)
	}
	final := filepath.Join(chunksDir, b.Header.ChunkID+chunk.Extension)
	if _, statErr := os.Stat(final); statErr != nil {
		t.Errorf("expected encrypted chunk at %s: %v", final, statErr)
	}
}

// TestCollectPlaintextEvents_Sensitive_Aborts pins the render mirror: a
// sensitive plaintext chunk in the working tree (e.g. pulled from a
// remote) aborts the render rather than partially rendering.
func TestCollectPlaintextEvents_Sensitive_Aborts(t *testing.T) {
	t.Parallel()
	chunksDir := t.TempDir()
	ev := guardEvent("01HQ8Z0000000000000000000D", "connector:granola", kernel.ScopePersonal)
	raw := manualPlaintextChunk(t, guardChunkID, kernel.ScopePersonal, []kernel.Event{ev})
	if werr := os.WriteFile(filepath.Join(chunksDir, guardChunkID+chunk.ExtensionJSONL), raw, 0o600); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	_, _, _, cerr := collectPlaintextEvents(kernel.ScopePersonal, chunksDir)
	if !errors.Is(cerr, errSensitiveRenderPlaintext) {
		t.Fatalf("collectPlaintextEvents err = %v, want errSensitiveRenderPlaintext", cerr)
	}
}

// TestCollectPlaintextEvents_ProjectScope_Exempt confirms the project
// exemption at the render boundary.
func TestCollectPlaintextEvents_ProjectScope_Exempt(t *testing.T) {
	t.Parallel()
	chunksDir := t.TempDir()
	ev := guardEvent("01HQ8Z0000000000000000000E", "connector:granola", kernel.ScopeProject)
	raw := manualPlaintextChunk(t, guardChunkID, kernel.ScopeProject, []kernel.Event{ev})
	if werr := os.WriteFile(filepath.Join(chunksDir, guardChunkID+chunk.ExtensionJSONL), raw, 0o600); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	events, _, _, cerr := collectPlaintextEvents(kernel.ScopeProject, chunksDir)
	if cerr != nil {
		t.Fatalf("project scope must be exempt, got err = %v", cerr)
	}
	if len(events) != 1 {
		t.Errorf("collected %d events, want 1 (project exempt, event rendered)", len(events))
	}
}

// stageWorkingTreeChunk hand-writes a plaintext chunk into chunksDir,
// modelling a chunk that arrived by a non-sealer path (pulled from a
// remote, or sealed before a marker existed).
func stageWorkingTreeChunk(t *testing.T, chunksDir, chunkID string, scope kernel.Scope, ev kernel.Event) {
	t.Helper()
	raw := manualPlaintextChunk(t, chunkID, scope, []kernel.Event{ev})
	if werr := os.WriteFile(filepath.Join(chunksDir, chunkID+chunk.ExtensionJSONL), raw, 0o600); werr != nil {
		t.Fatalf("stage chunk: %v", werr)
	}
}

// newGuardTransport builds a Transport rooted at a temp data dir with a
// single personal remote. The remote URL is a dummy: these tests exercise
// only prepareScopeWorkingTree / the pre-flight scan, which never touch
// the remote.
func newGuardTransport(t *testing.T) *Transport {
	t.Helper()
	tr, err := New(Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: "file:///nope"},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "device-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

// TestRefuseSensitiveWorkingTree pins the Push pre-flight predicate. The
// scan is mode-agnostic: it has no encrypted-vs-plaintext branch, so an
// encrypted-mode push cannot bypass it (see
// TestPush_EncryptedPushBlockedBySensitivePlaintextChunk). Only the
// project scope is exempt.
func TestRefuseSensitiveWorkingTree(t *testing.T) {
	t.Parallel()
	sensitive := guardEvent("01HQ8Z00000000000000000R01", "connector:granola", kernel.ScopePersonal)
	safe := guardEvent("01HQ8Z00000000000000000R02", "cli:remember", kernel.ScopePersonal)

	t.Run("sensitive plaintext chunk is refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		stageWorkingTreeChunk(t, dir, "01HQ8ZSENS0000000000000001", kernel.ScopePersonal, sensitive)
		if err := refuseSensitiveWorkingTree(kernel.ScopePersonal, dir); !errors.Is(err, errSensitiveRenderPlaintext) {
			t.Fatalf("err = %v, want errSensitiveRenderPlaintext", err)
		}
	})

	t.Run("clean plaintext tree passes", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		stageWorkingTreeChunk(t, dir, "01HQ8ZSAFE0000000000000001", kernel.ScopePersonal, safe)
		if err := refuseSensitiveWorkingTree(kernel.ScopePersonal, dir); err != nil {
			t.Fatalf("clean tree err = %v, want nil", err)
		}
	})

	t.Run("project scope is exempt", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		ev := guardEvent("01HQ8Z00000000000000000R03", "connector:granola", kernel.ScopeProject)
		stageWorkingTreeChunk(t, dir, "01HQ8ZSENS0000000000000002", kernel.ScopeProject, ev)
		if err := refuseSensitiveWorkingTree(kernel.ScopeProject, dir); err != nil {
			t.Fatalf("project exempt err = %v, want nil", err)
		}
	})

	t.Run("missing chunks dir is clean", func(t *testing.T) {
		t.Parallel()
		if err := refuseSensitiveWorkingTree(kernel.ScopePersonal, filepath.Join(t.TempDir(), "absent")); err != nil {
			t.Fatalf("missing dir err = %v, want nil", err)
		}
	})
}

// TestPrepareScopeWorkingTree_SensitiveAbortsBeforeMaterialize is the
// direct regression for the retry-strands-chunks finding: Push calls
// prepareScopeWorkingTree BEFORE materializeBlobs, so when the working
// tree already holds a sensitive plaintext chunk the push aborts without
// writing the fresh safe blob — and a retry aborts identically rather than
// idempotently skipping a stranded blob and falsely reporting success.
func TestPrepareScopeWorkingTree_SensitiveAbortsBeforeMaterialize(t *testing.T) {
	t.Parallel()
	tr := newGuardTransport(t)
	chunksDir := tr.scopeChunksDir(kernel.ScopePersonal)
	if err := os.MkdirAll(chunksDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stageWorkingTreeChunk(t, chunksDir, "01HQ8ZSENS0000000000000010", kernel.ScopePersonal,
		guardEvent("01HQ8Z00000000000000000P01", "connector:granola", kernel.ScopePersonal))

	// A fresh safe blob that Push would materialize into this scope.
	safe := plaintextBlob(t, guardEvent("01HQ8Z00000000000000000P02", "cli:remember", kernel.ScopePersonal))

	// Twice: the retry must refuse identically (never falsely succeed).
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := tr.prepareScopeWorkingTree(kernel.ScopePersonal)
		if !errors.Is(err, errSensitiveRenderPlaintext) {
			t.Fatalf("attempt %d: err = %v, want errSensitiveRenderPlaintext", attempt, err)
		}
	}

	// The safe blob must never have been written (abort before materialize).
	safePath := filepath.Join(chunksDir, safe.Header.ChunkID+chunk.ExtensionJSONL)
	if _, statErr := os.Stat(safePath); !os.IsNotExist(statErr) {
		t.Errorf("safe blob %s materialized despite the abort (stranding regression)", safe.Header.ChunkID)
	}
}

// TestPrepareScopeWorkingTree_CleanTreeReturnsDir is the positive control:
// a clean working tree yields the chunks dir (created) and no error.
func TestPrepareScopeWorkingTree_CleanTreeReturnsDir(t *testing.T) {
	t.Parallel()
	tr := newGuardTransport(t)
	dir, err := tr.prepareScopeWorkingTree(kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("prepareScopeWorkingTree: %v", err)
	}
	if want := tr.scopeChunksDir(kernel.ScopePersonal); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Errorf("chunks dir not created: %v", statErr)
	}
}

// requireGitBin skips when git is unavailable. Duplicated from the
// external git_test package, which is invisible to this internal file.
func requireGitBin(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not in PATH: %v", err)
	}
}

// newBareRemoteRepo initialises an empty bare git repo and returns its
// file:// URL (duplicated from git_test.go for the same package reason).
func newBareRemoteRepo(t *testing.T) string {
	t.Helper()
	requireGitBin(t)
	dir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.CommandContext(t.Context(), "git", "init", "--bare", "--quiet", "--initial-branch=main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	return "file://" + dir
}

// TestPush_EncryptedPushBlockedBySensitivePlaintextChunk is the end-to-end
// regression for the encrypted-push-bypass finding: a sensitive plaintext
// chunk already in the working tree (pulled from a remote, or sealed
// before a marker existed) blocks the push EVEN when the new blob is
// encrypted — because commitAndPush's `git add .` would otherwise stage
// the cleartext chunk regardless of the new blob's mode. Enabling
// encryption stops new plaintext seals; it does not neutralise an existing
// plaintext leak, which must be removed.
func TestPush_EncryptedPushBlockedBySensitivePlaintextChunk(t *testing.T) {
	t.Parallel()
	requireGitBin(t)
	ctx := context.Background()
	tr, err := New(Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: newBareRemoteRepo(t)},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "device-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First push establishes the scope repo with a normal plaintext blob.
	safe := plaintextBlob(t, guardEvent("01HQ8Z00000000000000000Q01", "cli:remember", kernel.ScopePersonal))
	if _, pErr := tr.Push(ctx, kernel.ScopePersonal, []transport.Blob{safe}); pErr != nil {
		t.Fatalf("first (plaintext) push: %v", pErr)
	}

	// A sensitive plaintext chunk then arrives by a non-sealer path.
	chunksDir := tr.scopeChunksDir(kernel.ScopePersonal)
	stageWorkingTreeChunk(t, chunksDir, "01HQ8ZSENS0000000000000020", kernel.ScopePersonal,
		guardEvent("01HQ8Z00000000000000000Q02", "connector:granola", kernel.ScopePersonal))

	// The remediation "stop the bleed" push is ENCRYPTED — and must still
	// be refused while the cleartext chunk sits in the working tree.
	key := bytes.Repeat([]byte{0x42}, 32)
	raw, hdr, encErr := chunk.Encode(
		[]kernel.Event{guardEvent("01HQ8Z00000000000000000Q03", "cli:remember", kernel.ScopePersonal)},
		kernel.ScopePersonal, key)
	if encErr != nil {
		t.Fatalf("Encode: %v", encErr)
	}
	encBlob := transport.Blob{Header: hdr, Body: io.NopCloser(bytes.NewReader(raw))}
	if _, pErr := tr.Push(ctx, kernel.ScopePersonal, []transport.Blob{encBlob}); !errors.Is(pErr, errSensitiveRenderPlaintext) {
		t.Fatalf("encrypted Push err = %v, want errSensitiveRenderPlaintext (must not bypass)", pErr)
	}
	// The encrypted blob must not have been materialized (abort before write).
	if _, statErr := os.Stat(filepath.Join(chunksDir, hdr.ChunkID+chunk.Extension)); !os.IsNotExist(statErr) {
		t.Errorf("encrypted blob %s materialized despite the abort", hdr.ChunkID)
	}
}
