package git

// commit_authorship_e2e_test.go drives a real plaintext push through a file://
// remote and asserts the resulting commit is authored as the resolved user and
// carries the authorship trailers — the end-to-end proof for Workstream A that
// the wiring (applyAuthorship → buildCommitMessage → commitAndPush env) holds
// together, not just the unit-level pieces.

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// fakeCoAuthorResolver maps peer principal UIDs to author lines for the test.
type fakeCoAuthorResolver struct{ byUID map[string]string }

func (f fakeCoAuthorResolver) CoAuthor(_ context.Context, principalUID string) (string, bool) {
	a, ok := f.byUID[principalUID]
	return a, ok
}

func TestPush_StampsAuthorAndTrailers(t *testing.T) {
	t.Parallel()
	requireGitBin(t)
	ctx := context.Background()

	tr, err := New(Options{
		DataDir:            t.TempDir(),
		RemoteURLs:         map[kernel.Scope]string{kernel.ScopePersonal: newBareRemoteRepo(t)},
		AuthorName:         "Frodo Baggins",
		AuthorMail:         "12345+frodo@users.noreply.github.com",
		DeviceID:           "mac-1",
		CommitIdentityMode: "noreply",
		SelfPrincipalUID:   "self-uid",
		CoAuthors: fakeCoAuthorResolver{byUID: map[string]string{
			"peer-uid": "Sam Gamgee <67890+sam@users.noreply.github.com>",
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Two plaintext events: one authored by self, one by a peer, with distinct
	// agent sources (neither a connector:, which would trip the sensitivity
	// gate and refuse the plaintext render).
	self := guardEvent("01HQ8Z0000000000000000000A", "mcp", kernel.ScopePersonal)
	self.Author = kernel.EventAuthor{PrincipalUID: "self-uid"}
	peer := guardEvent("01HQ8Z0000000000000000000B", "cli:remember", kernel.ScopePersonal)
	peer.Author = kernel.EventAuthor{PrincipalUID: "peer-uid"}

	raw, hdr, encErr := chunk.EncodePlaintext([]kernel.Event{self, peer}, kernel.ScopePersonal, "mac-1")
	if encErr != nil {
		t.Fatalf("EncodePlaintext: %v", encErr)
	}
	if _, pErr := tr.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: hdr, Body: io.NopCloser(bytes.NewReader(raw))},
	}); pErr != nil {
		t.Fatalf("Push: %v", pErr)
	}

	author, body := lastCommit(t, tr.scopeRepo(kernel.ScopePersonal))

	if author != "Frodo Baggins|12345+frodo@users.noreply.github.com" {
		t.Errorf("author = %q; want Frodo Baggins|12345+frodo@users.noreply.github.com", author)
	}
	for _, want := range []string{
		"Principal: Frodo Baggins <12345+frodo@users.noreply.github.com>",
		"Device: mac-1",
		"Co-authored-by: Sam Gamgee <67890+sam@users.noreply.github.com>",
		"Agent: cli:remember, mcp",
		"Mazarbul-Identity: noreply",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commit body missing %q\n---\n%s", want, body)
		}
	}
}

// lastCommit returns the author "name|email" and full body of HEAD in repo.
func lastCommit(t *testing.T, repo string) (author, body string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "log", "-1", "--format=%an|%ae").Output()
	if err != nil {
		t.Fatalf("git log author: %v", err)
	}
	bodyOut, err := exec.CommandContext(t.Context(), "git", "-C", repo, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log body: %v", err)
	}
	return strings.TrimSpace(string(out)), string(bodyOut)
}
