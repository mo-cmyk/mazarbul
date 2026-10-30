//go:build e2e

// TestPresetInstall_RemoteHappyPath_E2E drives the #224 remote-install flow
// against a BUILT maz binary: publish a preset to a local git repo, install it
// by file:// URL with --yes and an integrity pin, confirm the provenance is
// recorded, then uninstall (clean cut). It is a black-box test, so it lives in
// package cli_test alongside buildMaz/runMaz and shells out to the binary; the
// preset bundle, git repo, and data dir are built here from exported APIs only.
// Gated behind //go:build e2e because it compiles maz and migrates a fresh data
// dir, which needs the sqlite_fts5 tag (supplied explicitly by buildMazFTS5).

package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

func TestPresetInstall_RemoteHappyPath_E2E(t *testing.T) {
	mazBin := buildMazFTS5(t)
	cloneURL, wantSum := newRemotePresetRepo(t) // skips if git is not on PATH
	dataDir := newRemotePresetDataDir(t)        // embedder-free config
	env := append(os.Environ(), "MAZ_DATA_DIR="+dataDir)

	// Install the preset by URL with pre-consent and an integrity pin.
	runMazE2E(t, mazBin, env, 0, "preset", "install", cloneURL, "--yes", "--checksum", wantSum, "--data-dir", dataDir)

	// preset show surfaces the source, pinned commit, and checksum.
	show := runMazE2E(t, mazBin, env, 0, "preset", "show", "crm", "--output", "json", "--data-dir", dataDir)
	if !strings.Contains(show, cloneURL) || !strings.Contains(show, wantSum) {
		t.Fatalf("preset show missing provenance (source=%s checksum=%s):\n%s", cloneURL, wantSum, show)
	}

	// Uninstall is a clean cut: the active row is gone.
	runMazE2E(t, mazBin, env, 0, "preset", "uninstall", "crm", "--yes", "--data-dir", dataDir)
	list := runMazE2E(t, mazBin, env, 0, "preset", "list", "--output", "json", "--data-dir", dataDir)
	if strings.TrimSpace(list) != "[]" {
		t.Fatalf("post-uninstall list = %q, want []", strings.TrimSpace(list))
	}
}

// runMazE2E invokes maz with the given args and env, asserts the exit code, and
// returns stdout only. stderr (daemon warn-logs) is kept separate so JSON
// assertions are not polluted, but is surfaced on an unexpected exit. Unlike the
// shared runMaz it does NOT inject --no-tui (no longer a flag); a piped, non-TTY
// stdout already forces CLI mode.
func runMazE2E(t *testing.T, bin string, env []string, wantExit int, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v\nstderr:\n%s", bin, args, err, stderr.String())
		}
	}
	if exit != wantExit {
		t.Fatalf("maz %v exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", args, exit, wantExit, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// buildMazFTS5 compiles maz with the sqlite_fts5 tag so a fresh data dir can run
// migration 0002 (the FTS5 virtual table). The bare buildMaz omits the tag and
// only works when GOFLAGS already carries it, which is not guaranteed here.
func buildMazFTS5(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "maz")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-tags=sqlite_fts5", "-o", bin, "../../cmd/maz")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build maz (sqlite_fts5): %v", err)
	}
	return bin
}

// newRemotePresetRepo writes a minimal preset bundle, commits it to a local git
// repo, and returns the file:// clone URL plus the canonical bundle checksum.
func newRemotePresetRepo(t *testing.T) (cloneURL, checksum string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	writeRemotePresetBundle(t, repo)
	sum, err := schemapack.BundleChecksum(os.DirFS(repo), ".")
	if err != nil {
		t.Fatalf("checksum fixture: %v", err)
	}
	gitE2E(t, repo, "init", "--quiet", "--initial-branch=main")
	gitE2E(t, repo, "add", "-A")
	gitE2E(t, repo, "commit", "--quiet", "-m", "preset")
	return "file://" + repo, sum
}

// writeRemotePresetBundle writes the four-file preset contract into dir.
func writeRemotePresetBundle(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "kinds"), 0o755); err != nil {
		t.Fatalf("mkdir kinds: %v", err)
	}
	files := map[string]string{
		"manifest.toml": "name        = \"crm\"\n" +
			"version     = \"0.1.0\"\n" +
			"description = \"Test preset fixture.\"\n" +
			"category    = \"extension\"\n" +
			"kind        = \"schema-pack\"\n",
		"kinds/account.yaml": "name: crm:account\n" +
			"displayName: Account\n" +
			"description: A company or organization tracked in the CRM.\n" +
			"extends: organization\n",
		"predicates.yaml": "predicates:\n" +
			"  - predicate: works_at\n" +
			"    kind: non-functional\n" +
			"    description: Subject person works at the object account.\n",
		"instructions.md": "# CRM operating instructions\n\n" +
			"When the user mentions a company, model it as a crm:Account.\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// gitE2E runs git in dir with a hardened, hermetic environment. The
// GIT_DIR/GIT_INDEX_FILE scrub is mandatory: `go test` invoked from a git hook
// inherits those and would otherwise commit into the developer's real repo.
func gitE2E(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	keep := os.Environ()[:0:0]
	drop := []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_PREFIX=", "GIT_COMMON_DIR="}
	for _, kv := range os.Environ() {
		skip := false
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				skip = true
				break
			}
		}
		if !skip {
			keep = append(keep, kv)
		}
	}
	keep = append(keep,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	cmd.Env = keep
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newRemotePresetDataDir creates an isolated data dir with an embedder-free
// config so the binary boots without contacting Ollama.
func newRemotePresetDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}
