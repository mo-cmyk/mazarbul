package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// GitHub anchor constants. The SSHSIG namespace and the statement
// domain-separation tag are frozen: a verifier must use the identical
// strings, so changing them is a v2 break.
const (
	githubDefaultBaseURL   = "https://github.com"
	anchorNamespace        = "mazarbul-identity"
	anchorStatementVersion = "mazarbul-identity-anchor-v1"
	anchorSignerPrincipal  = "mazarbul" // allowed_signers principal label
)

// GitHubAnchorKind enumerates how strongly a principal is bound to its
// GitHub identity. "ssh" means an SSHSIG attestation by a GitHub-published
// key verified; "none" means the binding is absent or unverified (offline,
// no `gh`, or no published signing key).
const (
	GitHubAnchorSSH  = "ssh"
	GitHubAnchorNone = "none"
)

// ErrNoSigningKey is returned when no local SSH key matching a published
// GitHub key can be found to produce the attestation. The genesis ceremony
// downgrades this to GitHubAnchorNone rather than failing.
var ErrNoSigningKey = errors.New("identity: no local SSH key matches the account's published keys")

// GitHubIdentity is the resolved account identity. UserID is the immutable
// numeric id (logins are renameable, so it is the durable anchor).
type GitHubIdentity struct {
	Login  string
	UserID int64
}

// Attestation is the verified cross-anchor: an SSHSIG over the anchor
// statement, produced by a key the account publishes on GitHub. Stored on
// the principalRegistered event.
type Attestation struct {
	Login     string `json:"login"`
	UserID    int64  `json:"userId"`
	Signature string `json:"signature"` // armored SSHSIG (PEM text)
	SignerKey string `json:"signerKey"` // authorized_keys line of the vouching key
}

// commandRunner runs an external command with optional stdin and returns
// stdout. It is the test seam for the ssh-keygen / gh / git shell-outs.
type commandRunner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// GitHubAnchorer resolves the operator's GitHub identity, fetches their
// published SSH keys, and produces/verifies an SSHSIG attestation binding
// the local ed25519 identity to that account. All external dependencies
// (HTTP host, ssh-keygen path, command execution, SSH dir) are injectable
// so the genesis path is testable with httptest + a generated key.
type GitHubAnchorer struct {
	baseURL string
	http    *http.Client
	keygen  string
	run     commandRunner
	sshDir  string
}

// GitHubOption configures a GitHubAnchorer.
type GitHubOption func(*GitHubAnchorer)

// WithGitHubBaseURL overrides the GitHub host (default https://github.com;
// tests point this at an httptest server, GHES users at their host).
func WithGitHubBaseURL(u string) GitHubOption {
	return func(a *GitHubAnchorer) {
		if u != "" {
			a.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithCommandRunner overrides external command execution (test seam).
func WithCommandRunner(r commandRunner) GitHubOption {
	return func(a *GitHubAnchorer) {
		if r != nil {
			a.run = r
		}
	}
}

// WithSSHDir overrides the directory scanned for a fallback signing key.
func WithSSHDir(dir string) GitHubOption {
	return func(a *GitHubAnchorer) {
		if dir != "" {
			a.sshDir = dir
		}
	}
}

// NewGitHubAnchorer builds an anchorer with production defaults.
func NewGitHubAnchorer(opts ...GitHubOption) *GitHubAnchorer {
	a := &GitHubAnchorer{
		baseURL: githubDefaultBaseURL,
		http:    &http.Client{Timeout: 8 * time.Second},
		keygen:  "ssh-keygen",
		run:     execRunner,
		sshDir:  defaultSSHDir(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// ResolveIdentity discovers the operator's GitHub login + immutable id.
// It tries `gh api user` first (authoritative), then `git config
// github.user` (login only, id 0). Returns ok=false when neither yields a
// login, which the caller treats as "no GitHub anchor available".
func (a *GitHubAnchorer) ResolveIdentity(ctx context.Context) (GitHubIdentity, bool) {
	if out, err := a.run(ctx, nil, "gh", "api", "user"); err == nil {
		var u struct {
			Login string `json:"login"`
			ID    int64  `json:"id"`
		}
		if jerr := json.Unmarshal(out, &u); jerr == nil && u.Login != "" {
			return GitHubIdentity{Login: u.Login, UserID: u.ID}, true
		}
	}
	if out, err := a.run(ctx, nil, "git", "config", "--get", "github.user"); err == nil {
		if login := strings.TrimSpace(string(out)); login != "" {
			return GitHubIdentity{Login: login}, true
		}
	}
	return GitHubIdentity{}, false
}

// FetchPublishedKeys returns the SSH public keys the account publishes at
// {baseURL}/{login}.keys (public, unauthenticated). An empty list with a
// nil error means the account publishes no SSH keys.
func (a *GitHubAnchorer) FetchPublishedKeys(ctx context.Context, login string) ([]ssh.PublicKey, error) {
	url := a.baseURL + "/" + login + ".keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: build keys request: %w", err)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: fetch published keys: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close of a read-only response body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity: fetch published keys: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("identity: read published keys: %w", err)
	}
	lines := strings.Split(string(body), "\n")
	keys := make([]ssh.PublicKey, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		pk, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
		if perr != nil {
			continue // skip unparseable lines rather than fail the whole fetch
		}
		keys = append(keys, pk)
	}
	return keys, nil
}

// AnchorStatement builds the deterministic bytes the SSHSIG attestation
// covers: a domain-separated, length-prefixed binding of the principal,
// its ed25519 identity key, and the claimed GitHub account. The verifier
// reconstructs identical bytes.
func AnchorStatement(principalUID string, edPub ed25519.PublicKey, login string, userID int64) []byte {
	buf := make([]byte, 0, 128)
	buf = append(buf, anchorStatementVersion...)
	buf = append(buf, 0)
	buf = appendField(buf, []byte(principalUID))
	buf = appendField(buf, []byte(base64.StdEncoding.EncodeToString(edPub)))
	buf = appendField(buf, []byte("github:"+login))
	buf = appendField(buf, []byte(strconv.FormatInt(userID, 10)))
	return buf
}

func appendField(buf, b []byte) []byte {
	n := len(b)
	if n > math.MaxUint32 {
		n = math.MaxUint32
	}
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(n))
	buf = append(buf, lp[:]...)
	return append(buf, b[:n]...)
}

// Anchor produces a verified GitHub attestation binding principalUID to the
// given GitHub identity. It locates a local SSH key whose public half the
// account publishes, signs the anchor statement (which length-prefixes the
// principal UID, the ed25519 identity key, and the GitHub account) with it
// via ssh-keygen, and verifies the signature against the published keys.
// Returns ErrNoSigningKey when no suitable local key exists; the caller
// downgrades to GitHubAnchorNone.
func (a *GitHubAnchorer) Anchor(ctx context.Context, principalUID string, id GitHubIdentity, edPub ed25519.PublicKey) (Attestation, error) {
	published, err := a.FetchPublishedKeys(ctx, id.Login)
	if err != nil {
		return Attestation{}, err
	}
	if len(published) == 0 {
		return Attestation{}, ErrNoSigningKey
	}
	keyPath, signerPub, err := a.resolveLocalSigningKey(ctx, published)
	if err != nil {
		return Attestation{}, err
	}
	statement := AnchorStatement(principalUID, edPub, id.Login, id.UserID)
	sig, err := a.sign(ctx, statement, keyPath)
	if err != nil {
		return Attestation{}, err
	}
	// Self-check: the attestation must verify against the very key that
	// signed it (which is, by construction, a published key).
	if verr := a.verify(ctx, statement, sig, []ssh.PublicKey{signerPub}); verr != nil {
		return Attestation{}, fmt.Errorf("identity: attestation self-verify: %w", verr)
	}
	return Attestation{
		Login:     id.Login,
		UserID:    id.UserID,
		Signature: string(sig),
		SignerKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signerPub))),
	}, nil
}

// VerifyAttestation re-checks a stored attestation against the account's
// currently-published keys (the teams-replay trust path). It fetches the
// live keys, confirms the recorded signer key is still published, and
// verifies the SSHSIG over the statement reconstructed for principalUID —
// so an attestation cannot be replayed to vouch for a different principal.
func (a *GitHubAnchorer) VerifyAttestation(ctx context.Context, principalUID string, att Attestation, edPub ed25519.PublicKey) error {
	published, err := a.FetchPublishedKeys(ctx, att.Login)
	if err != nil {
		return err
	}
	signerPub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(att.SignerKey))
	if err != nil {
		return fmt.Errorf("identity: parse recorded signer key: %w", err)
	}
	if !keyInSet(signerPub, published) {
		return fmt.Errorf("identity: recorded signer key is no longer published by %q", att.Login)
	}
	statement := AnchorStatement(principalUID, edPub, att.Login, att.UserID)
	return a.verify(ctx, statement, []byte(att.Signature), []ssh.PublicKey{signerPub})
}

// sign produces an armored SSHSIG over statement using the private key at
// keyPath: `ssh-keygen -Y sign -n <namespace> -f <keyPath>` with the
// statement on stdin.
func (a *GitHubAnchorer) sign(ctx context.Context, statement []byte, keyPath string) ([]byte, error) {
	out, err := a.run(ctx, statement, a.keygen, "-Y", "sign", "-n", anchorNamespace, "-f", keyPath)
	if err != nil {
		return nil, fmt.Errorf("identity: ssh-keygen sign: %w", err)
	}
	return out, nil
}

// verify checks an armored SSHSIG over statement against the given allowed
// keys via `ssh-keygen -Y verify`. It writes a temporary allowed_signers
// file + signature file (ssh-keygen requires real paths) and feeds the
// statement on stdin.
func (a *GitHubAnchorer) verify(ctx context.Context, statement, sig []byte, allowed []ssh.PublicKey) error {
	dir, err := os.MkdirTemp("", "maz-anchor-*")
	if err != nil {
		return fmt.Errorf("identity: temp dir: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a temp dir

	var allowedSigners bytes.Buffer
	for _, k := range allowed {
		fmt.Fprintf(&allowedSigners, "%s %s\n", anchorSignerPrincipal, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))))
	}
	allowedPath := filepath.Join(dir, "allowed_signers")
	if werr := os.WriteFile(allowedPath, allowedSigners.Bytes(), 0o600); werr != nil {
		return fmt.Errorf("identity: write allowed_signers: %w", werr)
	}
	sigPath := filepath.Join(dir, "sig")
	if werr := os.WriteFile(sigPath, sig, 0o600); werr != nil {
		return fmt.Errorf("identity: write signature: %w", werr)
	}
	if _, verr := a.run(ctx, statement, a.keygen,
		"-Y", "verify", "-f", allowedPath, "-I", anchorSignerPrincipal,
		"-n", anchorNamespace, "-s", sigPath); verr != nil {
		return fmt.Errorf("identity: ssh-keygen verify: %w", verr)
	}
	return nil
}

// resolveLocalSigningKey finds a local SSH private key whose public half is
// among published. It prefers the git-configured commit-signing key
// (`user.signingkey` when `gpg.format=ssh`), then scans sshDir for
// id_ed25519 / id_rsa. Returns the private-key path and the matching
// public key.
func (a *GitHubAnchorer) resolveLocalSigningKey(ctx context.Context, published []ssh.PublicKey) (string, ssh.PublicKey, error) {
	// At most one git-config signing key plus the three default key names.
	candidates := make([]string, 0, 4)
	if out, err := a.run(ctx, nil, "git", "config", "--get", "gpg.format"); err == nil && strings.TrimSpace(string(out)) == "ssh" {
		if sk, serr := a.run(ctx, nil, "git", "config", "--get", "user.signingkey"); serr == nil {
			if p := strings.TrimSpace(string(sk)); p != "" {
				candidates = append(candidates, expandHome(p, a.sshDir))
			}
		}
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		candidates = append(candidates, filepath.Join(a.sshDir, name))
	}

	for _, priv := range candidates {
		pub, ok := a.publicForPrivate(ctx, priv)
		if !ok {
			continue
		}
		if keyInSet(pub, published) {
			return priv, pub, nil
		}
	}
	return "", nil, ErrNoSigningKey
}

// publicForPrivate derives the public key for a private-key path. It reads
// {path}.pub when present, else runs `ssh-keygen -y -f {path}`.
func (a *GitHubAnchorer) publicForPrivate(ctx context.Context, privPath string) (ssh.PublicKey, bool) {
	if data, err := os.ReadFile(privPath + ".pub"); err == nil { //nolint:gosec // path from git config / fixed ~/.ssh names
		if pk, _, _, _, perr := ssh.ParseAuthorizedKey(data); perr == nil {
			return pk, true
		}
	}
	if out, err := a.run(ctx, nil, a.keygen, "-y", "-f", privPath); err == nil {
		if pk, _, _, _, perr := ssh.ParseAuthorizedKey(out); perr == nil {
			return pk, true
		}
	}
	return nil, false
}

// keyInSet reports whether pub matches any key in set (by marshalled bytes).
func keyInSet(pub ssh.PublicKey, set []ssh.PublicKey) bool {
	want := pub.Marshal()
	for _, k := range set {
		if bytes.Equal(k.Marshal(), want) {
			return true
		}
	}
	return false
}

// expandHome resolves a leading ~ against the parent of sshDir (i.e. the
// home dir), leaving absolute and relative paths untouched.
func expandHome(p, sshDir string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(filepath.Dir(sshDir), p[2:])
	}
	return p
}

// defaultSSHDir returns ~/.ssh, or "" when the home dir is unknown.
func defaultSSHDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh")
}

// execRunner is the production commandRunner.
func execRunner(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
