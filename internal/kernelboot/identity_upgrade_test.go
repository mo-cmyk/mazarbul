package kernelboot_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// anchorCommandRunner fakes the external commands the GitHub anchor shells out
// to: `gh api user` returns a fixed octocat identity, ssh-keygen routes to the
// real binary (so the SSHSIG sign/verify round-trip is genuine), and git (the
// gpg.format / signingkey probe) fails so signing-key resolution falls through
// to the injected SSH dir.
func anchorCommandRunner(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	switch name {
	case "gh":
		return []byte(`{"login":"octocat","id":583231}`), nil
	case "ssh-keygen":
		cmd := exec.CommandContext(ctx, name, args...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), nil
	default:
		return nil, errors.New("stubbed: " + name)
	}
}

// genAnchorSSHKey writes an ed25519 SSH keypair to dir/id_ed25519 and returns
// the authorized_keys public line, used both as the local signing key and as
// the account's published key so the attestation verifies.
func genAnchorSSHKey(t *testing.T, dir string) []byte {
	t.Helper()
	path := filepath.Join(dir, "id_ed25519")
	if _, err := anchorCommandRunner(context.Background(), nil, "ssh-keygen", "-t", "ed25519", "-f", path, "-N", "", "-C", "octocat"); err != nil {
		t.Fatalf("generate ssh key: %v", err)
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatalf("read pub: %v", err)
	}
	return pub
}

// countEventKind walks the JSONL log under dataDir and counts events of kind.
func countEventKind(t *testing.T, dataDir, kind string) int {
	t.Helper()
	needle := `"kind":"` + kind + `"`
	n := 0
	root := filepath.Join(dataDir, "events")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, needle) {
				n++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk events: %v", err)
	}
	return n
}

func principalAnchorOf(t *testing.T, ctx context.Context, db *sql.DB, principalUID string) string {
	t.Helper()
	var anchor string
	if err := db.QueryRowContext(ctx,
		`SELECT github_anchor FROM principals WHERE principal_uid = ?`, principalUID).Scan(&anchor); err != nil {
		t.Fatalf("query github_anchor: %v", err)
	}
	return anchor
}

// TestUpgradeGitHubAnchor_AnchorsPreviouslyUnanchoredPrincipal is the D2
// acceptance: a principal minted offline (githubAnchor:"none") is upgraded to a
// verified SSH anchor on a later boot when connectivity + a published key are
// available, by appending a new principalRegistered revision. The upgrade is
// idempotent: a second run, now that the principal is anchored, appends nothing.
func TestUpgradeGitHubAnchor_AnchorsPreviouslyUnanchoredPrincipal(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	idStore := identity.New(providerWithWatchword(t))

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	// Genesis with a nil anchorer → the principal is registered unanchored.
	if serr := kernelboot.SeedIdentity(ctx, log, db, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity: %v", serr)
	}
	principalUID, perr := config.SelfPrincipalUID(dataDir)
	if perr != nil || principalUID == "" {
		t.Fatalf("self binding after genesis = (%q, %v)", principalUID, perr)
	}
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post-genesis): %v", rerr)
	}
	if got := principalAnchorOf(t, ctx, db, principalUID); got != ontology.GitHubAnchorNone {
		t.Fatalf("post-genesis anchor = %q, want %q", got, ontology.GitHubAnchorNone)
	}
	if n := countEventKind(t, dataDir, ontology.KindPrincipalRegistered); n != 1 {
		t.Fatalf("principalRegistered count after genesis = %d, want 1", n)
	}

	// Build a working anchorer: octocat publishes the local id_ed25519 key.
	sshDir := t.TempDir()
	pub := genAnchorSSHKey(t, sshDir)
	srv := keysServerFor(t, "octocat", pub)
	anchorer := identity.NewGitHubAnchorer(
		identity.WithGitHubBaseURL(srv.URL),
		identity.WithSSHDir(sshDir),
		identity.WithCommandRunner(anchorCommandRunner),
	)

	// Upgrade: appends a principalRegistered revision carrying the attestation.
	if uerr := kernelboot.UpgradeGitHubAnchor(ctx, log, db, idStore, anchorer, dataDir); uerr != nil {
		t.Fatalf("UpgradeGitHubAnchor: %v", uerr)
	}
	if n := countEventKind(t, dataDir, ontology.KindPrincipalRegistered); n != 2 {
		t.Fatalf("principalRegistered count after upgrade = %d, want 2 (a new revision)", n)
	}
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post-upgrade): %v", rerr)
	}
	if got := principalAnchorOf(t, ctx, db, principalUID); got != ontology.GitHubAnchorSSH {
		t.Fatalf("post-upgrade anchor = %q, want %q", got, ontology.GitHubAnchorSSH)
	}

	// Idempotent: the principal is now anchored, so a second run is a no-op.
	if uerr := kernelboot.UpgradeGitHubAnchor(ctx, log, db, idStore, anchorer, dataDir); uerr != nil {
		t.Fatalf("UpgradeGitHubAnchor (second run): %v", uerr)
	}
	if n := countEventKind(t, dataDir, ontology.KindPrincipalRegistered); n != 2 {
		t.Fatalf("principalRegistered count after idempotent re-run = %d, want 2 (no new revision)", n)
	}
}

// TestUpgradeGitHubAnchor_NoOpWhenAnchorerNil confirms the guard: with no
// anchorer there is nothing to attempt, so the call is a clean no-op even
// against a fresh data dir (no principal yet).
func TestUpgradeGitHubAnchor_NoOpWhenAnchorerNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	idStore := identity.New(providerWithWatchword(t))
	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })
	if uerr := kernelboot.UpgradeGitHubAnchor(ctx, log, nil, idStore, nil, dataDir); uerr != nil {
		t.Fatalf("UpgradeGitHubAnchor(nil anchorer) = %v, want nil no-op", uerr)
	}
}

// keysServerFor serves authorized_keys bytes at /{login}.keys for the anchor's
// FetchPublishedKeys (mirrors the identity package's own test helper).
func keysServerFor(t *testing.T, login string, keys []byte) *httptest.Server {
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
