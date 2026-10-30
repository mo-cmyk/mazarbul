package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// sshKeygenAvailable skips a test when ssh-keygen is absent (e.g. a
// minimal CI image). The anchor path degrades to GitHubAnchorNone in that
// case, which the genesis ceremony handles; the crypto round-trip is only
// asserted where the tool exists.
func sshKeygenAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
}

// genSSHKey writes an ed25519 SSH keypair to dir/name and returns the
// public-key authorized_keys line.
func genSSHKey(t *testing.T, dir, name string) []byte {
	t.Helper()
	path := filepath.Join(dir, name)
	if _, err := execRunner(context.Background(), nil, "ssh-keygen", "-t", "ed25519", "-f", path, "-N", "", "-C", name); err != nil {
		t.Fatalf("ssh-keygen generate: %v", err)
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatalf("read pub: %v", err)
	}
	return pub
}

// keysServer serves the given authorized_keys bytes at /{login}.keys.
func keysServer(t *testing.T, login string, keys []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+login+".keys" {
			_, _ = w.Write(keys)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stubGitGh routes ssh-keygen to the real binary and fails git/gh so the
// signing-key resolution falls through to the injected SSH dir.
func stubGitGh(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	if name == "ssh-keygen" {
		return execRunner(ctx, stdin, name, args...)
	}
	return nil, errors.New("stubbed: " + name)
}

func TestGitHubAnchorRoundTrip(t *testing.T) {
	sshKeygenAvailable(t)
	ctx := context.Background()
	dir := t.TempDir()
	pub := genSSHKey(t, dir, "id_ed25519")
	srv := keysServer(t, "octocat", pub)

	a := NewGitHubAnchorer(
		WithGitHubBaseURL(srv.URL),
		WithSSHDir(dir),
		WithCommandRunner(stubGitGh),
	)
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed25519: %v", err)
	}
	id := GitHubIdentity{Login: "octocat", UserID: 583231}
	const principalUID = "01J0PRINCIPALULIDABCDEF1234"

	att, err := a.Anchor(ctx, principalUID, id, edPub)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if att.Signature == "" || att.SignerKey == "" {
		t.Fatalf("incomplete attestation: %+v", att)
	}

	// A valid attestation re-verifies against the published keys.
	if err := a.VerifyAttestation(ctx, principalUID, att, edPub); err != nil {
		t.Fatalf("verify valid attestation: %v", err)
	}

	// A different ed25519 identity key must fail: the statement no longer
	// matches what was signed.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen other ed25519: %v", err)
	}
	if err := a.VerifyAttestation(ctx, principalUID, att, otherPub); err == nil {
		t.Fatal("expected verification failure for a mismatched identity key")
	}

	// A different principal UID must fail: the attestation binds the local
	// principal, so it cannot be replayed to vouch for another principal
	// even with the same GitHub account and identity key.
	if err := a.VerifyAttestation(ctx, "01J0OTHERPRINCIPALULID98765", att, edPub); err == nil {
		t.Fatal("expected verification failure for a mismatched principal UID")
	}
}

func TestGitHubAnchorNoMatchingLocalKey(t *testing.T) {
	sshKeygenAvailable(t)
	ctx := context.Background()

	// The account publishes a DIFFERENT key than the one in our SSH dir,
	// so no local key can produce a vouching attestation.
	pubDir := t.TempDir()
	publishedPub := genSSHKey(t, pubDir, "published")
	localDir := t.TempDir()
	genSSHKey(t, localDir, "id_ed25519") // unrelated local key

	srv := keysServer(t, "octocat", publishedPub)
	a := NewGitHubAnchorer(
		WithGitHubBaseURL(srv.URL),
		WithSSHDir(localDir),
		WithCommandRunner(stubGitGh),
	)
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)

	_, err := a.Anchor(ctx, "01J0PRINCIPALULIDABCDEF1234", GitHubIdentity{Login: "octocat", UserID: 1}, edPub)
	if !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("want ErrNoSigningKey, got %v", err)
	}
}

func TestFetchPublishedKeysParsesLines(t *testing.T) {
	sshKeygenAvailable(t)
	ctx := context.Background()
	dir := t.TempDir()
	pub := genSSHKey(t, dir, "id_ed25519")
	srv := keysServer(t, "octocat", pub)

	a := NewGitHubAnchorer(WithGitHubBaseURL(srv.URL), WithCommandRunner(stubGitGh))
	keys, err := a.FetchPublishedKeys(ctx, "octocat")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("want 1 published key, got %d", len(keys))
	}
}
