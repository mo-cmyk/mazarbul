package git_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
	git "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
)

// pushSeq monotonically increments so two pushes inside the same test
// produce strictly ordered ULIDs, which the chunk header sorter and the
// renderer's HeadULID logic both rely on.
var pushSeq atomic.Uint64

func nextEventULID(t *testing.T) string {
	t.Helper()
	now := time.Now().Add(time.Duration(pushSeq.Add(1)) * time.Second)
	id, err := ulid.New(ulid.Timestamp(now), rand.Reader)
	if err != nil {
		t.Fatalf("ulid.New: %v", err)
	}
	return id.String()
}

// lsTreeAtHead returns every file path tracked at HEAD in the bare repo
// at remoteURL (a file:// URL). The paths are POSIX-style and sorted.
func lsTreeAtHead(t *testing.T, remoteURL string) []string {
	t.Helper()
	dir := strings.TrimPrefix(remoteURL, "file://")
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "ls-tree", "-r", "HEAD", "--name-only")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		paths = append(paths, line)
	}
	return paths
}

// gitLogCount returns the number of commits at HEAD.
func gitLogCount(t *testing.T, remoteURL string) int {
	t.Helper()
	dir := strings.TrimPrefix(remoteURL, "file://")
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-list", "--count", "HEAD")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("rev-list: %v\n%s", err, out.String())
	}
	var n int
	for _, r := range strings.TrimSpace(out.String()) {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	return n
}

func pushOneEpisodeChunk(t *testing.T, tr transport.Transport, scope kernel.Scope, payload string) chunk.Header {
	t.Helper()
	ev := kernel.Event{
		UID:        nextEventULID(t),
		Kind:       "episode",
		Source:     "test",
		Confidence: 1,
		Scope:      scope,
		Payload:    []byte(`{"text":` + jsonQuote(payload) + `,"source":"test","confidence":1.0}`),
	}
	body, hdr, err := chunk.Encode([]kernel.Event{ev}, scope, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	blobs := []transport.Blob{{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}}
	if _, err := tr.Push(t.Context(), scope, blobs); err != nil {
		t.Fatalf("push: %v", err)
	}
	return hdr
}

func jsonQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// TestRenderedLayout_NoDoubleRenderedPrefix pins the bug found during the
// CLI verification: the renderer-emitted paths (`rendered/entities.jsonl`)
// were being prepended with another `rendered/` by the transport, landing at
// `rendered/rendered/entities.jsonl` on the remote. That broke `maz blame`,
// `maz show`, and `maz sync log` which all read from the plan-specified
// path. This test fails until the transport stops double-wrapping.
func TestRenderedLayout_NoDoubleRenderedPrefix(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote := newBareRemote(t)
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "dev",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pushOneEpisodeChunk(t, tr, kernel.ScopePersonal, "hello world")

	paths := lsTreeAtHead(t, remote)
	for _, p := range paths {
		if strings.Contains(p, "rendered/rendered/") {
			t.Errorf("found double-prefixed path %q in remote tree", p)
		}
	}
}

// TestRenderedLayout_PathsMatchPlan pins the plan-specified locations:
//   - rendered/entities.jsonl       (sibling of chunks/)
//   - rendered/relations.jsonl
//   - rendered/superseded-relations.jsonl
//   - rendered/forgotten-relations.jsonl
//   - rendered/by-kind/<kind>/INDEX.md
//   - CHANGELOG/<YYYY>/<MM>/<ULID>__<device>.md   (TOP-LEVEL, sibling of rendered/)
//   - rendered/.cursor.json
//
// Fails until both the path-duplication bug and the missing cursor bug are
// fixed. Plus locks CHANGELOG/ at repo-root, not inside rendered/.
func TestRenderedLayout_PathsMatchPlan(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote := newBareRemote(t)
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "dev",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pushOneEpisodeChunk(t, tr, kernel.ScopePersonal, "plan-layout-probe")
	paths := lsTreeAtHead(t, remote)
	pathSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		pathSet[p] = struct{}{}
	}

	mustExist := []string{
		"rendered/entities.jsonl",
		"rendered/relations.jsonl",
		"rendered/superseded-relations.jsonl",
		"rendered/forgotten-relations.jsonl",
		"rendered/.cursor.json",
		"rendered/by-kind/person/INDEX.md",
		"rendered/by-kind/organization/INDEX.md",
	}
	for _, want := range mustExist {
		if _, ok := pathSet[want]; !ok {
			t.Errorf("expected path %q missing from remote tree; have:\n%s", want, strings.Join(paths, "\n"))
		}
	}

	// CHANGELOG must be a sibling of rendered/, not nested inside it.
	var changelogPaths []string
	for _, p := range paths {
		if strings.HasPrefix(p, "CHANGELOG/") {
			changelogPaths = append(changelogPaths, p)
		}
		if strings.HasPrefix(p, "rendered/CHANGELOG/") {
			t.Errorf("CHANGELOG must live at repo root, not inside rendered/: %s", p)
		}
	}
	if len(changelogPaths) == 0 {
		t.Errorf("no CHANGELOG fragment at repo root; have:\n%s", strings.Join(paths, "\n"))
	}
}

// TestChangelog_FragmentsAccumulate_NotRenamed pins bug #2: the second
// push must ADD a new CHANGELOG fragment file (per-push audit trail), not
// rename the previous push's fragment to the new HEAD ULID. Verified via
// git history: the second commit must show an A-type entry for the new
// fragment AND the prior fragment must still be present at HEAD.
func TestChangelog_FragmentsAccumulate_NotRenamed(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote := newBareRemote(t)
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "dev",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pushOneEpisodeChunk(t, tr, kernel.ScopePersonal, "fragment one")
	firstPaths := lsTreeAtHead(t, remote)
	var firstFragment string
	for _, p := range firstPaths {
		if strings.HasPrefix(p, "CHANGELOG/") && strings.HasSuffix(p, ".md") {
			firstFragment = p
			break
		}
	}
	if firstFragment == "" {
		t.Fatalf("first push produced no CHANGELOG fragment; tree:\n%s", strings.Join(firstPaths, "\n"))
	}

	pushOneEpisodeChunk(t, tr, kernel.ScopePersonal, "fragment two")
	if n := gitLogCount(t, remote); n != 2 {
		t.Fatalf("commit count = %d, want 2", n)
	}
	secondPaths := lsTreeAtHead(t, remote)
	var fragments []string
	for _, p := range secondPaths {
		if strings.HasPrefix(p, "CHANGELOG/") && strings.HasSuffix(p, ".md") {
			fragments = append(fragments, p)
		}
	}
	if len(fragments) < 2 {
		t.Errorf("expected at least 2 CHANGELOG fragments after two pushes, got %d: %v", len(fragments), fragments)
	}
	found := false
	for _, f := range fragments {
		if f == firstFragment {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("first push's CHANGELOG fragment %q disappeared on second push; tree:\n%s",
			firstFragment, strings.Join(secondPaths, "\n"))
	}
}

// TestRenderedCursor_WrittenAtPlannedPath ensures rendered/.cursor.json is
// produced by the transport after a push. The cursor records the last
// applied event ULID so an incremental render on the next push can pick
// up where the prior one left off (plan §B). The doctor check
// `sync.render_cursor_behind` relies on this file existing.
func TestRenderedCursor_WrittenAtPlannedPath(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote := newBareRemote(t)
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "dev",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hdr := pushOneEpisodeChunk(t, tr, kernel.ScopePersonal, "cursor probe")
	paths := lsTreeAtHead(t, remote)
	const wantPath = "rendered/.cursor.json"
	for _, p := range paths {
		if p == wantPath {
			return // ok
		}
	}
	t.Errorf("rendered/.cursor.json missing from remote tree (HeadULID=%s); have:\n%s",
		hdr.LastULID, strings.Join(paths, "\n"))
	_ = filepath.Separator // keep import
}

// TestPruneRenderedTree_FlipPlaintextToEncrypted pins Greptile finding #2
// on PR #142: when a scope flips from plaintext to encrypted, the
// transport must prune BOTH the rendered/ tree AND the top-level
// CHANGELOG/ tree. Otherwise the encrypted snapshot still publishes the
// previously-rendered changelog fragments at HEAD because `git add .`
// doesn't remove files unless they're deleted from the working tree.
func TestPruneRenderedTree_FlipPlaintextToEncrypted(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote := newBareRemote(t)

	// Push one plaintext chunk first so rendered/ + CHANGELOG/ are
	// populated on the remote.
	trPlain, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "dev",
	})
	if err != nil {
		t.Fatalf("New plaintext: %v", err)
	}
	pushOneEpisodeChunk(t, trPlain, kernel.ScopePersonal, "plaintext seed")

	first := lsTreeAtHead(t, remote)
	hasRendered, hasChangelog := false, false
	for _, p := range first {
		if strings.HasPrefix(p, "rendered/") {
			hasRendered = true
		}
		if strings.HasPrefix(p, "CHANGELOG/") {
			hasChangelog = true
		}
	}
	if !hasRendered || !hasChangelog {
		t.Fatalf("seed push did not populate both rendered/ and CHANGELOG/; tree:\n%s", strings.Join(first, "\n"))
	}

	// Now push an encrypted chunk. The transport must prune both
	// subtrees so the encrypted snapshot is rendered-free.
	trEnc, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "dev",
		AuthorMail: "dev@test",
		DeviceID:   "dev",
	})
	if err != nil {
		t.Fatalf("New encrypted: %v", err)
	}
	key := scopeKey(t)
	ev := fakeEvent(kernel.ScopePersonal)
	ev.UID = nextEventULID(t)
	body, hdr, err := chunk.Encode([]kernel.Event{ev}, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("encode encrypted: %v", err)
	}
	blobs := []transport.Blob{{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}}
	if _, err := trEnc.Push(t.Context(), kernel.ScopePersonal, blobs); err != nil {
		t.Fatalf("push encrypted: %v", err)
	}

	second := lsTreeAtHead(t, remote)
	for _, p := range second {
		if strings.HasPrefix(p, "rendered/") {
			t.Errorf("rendered/ leaked into encrypted snapshot: %s", p)
		}
		if strings.HasPrefix(p, "CHANGELOG/") {
			t.Errorf("CHANGELOG/ leaked into encrypted snapshot: %s", p)
		}
	}
}
