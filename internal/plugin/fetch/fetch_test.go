package fetch_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/plugin/fetch"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// gitCmd runs a git command in dir with a hardened, hermetic environment. The
// GIT_DIR/GIT_INDEX_FILE scrub is mandatory: `go test` invoked from a git hook
// inherits those and would otherwise commit into the developer's real repo.
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	cmd.Env = scrubGitEnv(env)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func scrubGitEnv(env []string) []string {
	drop := []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_PREFIX=", "GIT_COMMON_DIR="}
	out := env[:0]
	for _, kv := range env {
		keep := true
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// gitOut runs git in dir and returns its trimmed stdout (fatal on error).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = scrubGitEnv(append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func writeBundleInto(t *testing.T, dir string, withInstructions bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "kinds"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(dir, "manifest.toml"), `name = "crm"
version = "0.1.0"
description = "CRM vocab."
category = "extension"
kind = "schema-pack"
`)
	write(t, filepath.Join(dir, "predicates.yaml"),
		"predicates:\n  - predicate: works_at\n    kind: functional\n    description: Employment.\n")
	write(t, filepath.Join(dir, "kinds", "company.yaml"),
		"name: \"extension:crm:company\"\ndisplayName: Company\ndescription: A company.\n")
	if withInstructions {
		write(t, filepath.Join(dir, "instructions.md"), "# Steering\nUse the CRM vocab.\n")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newBundleRepo creates a git repo whose root is a preset bundle and returns the
// file:// clone URL and the canonical checksum of the committed bundle.
func newBundleRepo(t *testing.T) (cloneURL, checksum string) {
	t.Helper()
	requireGit(t)
	repo := t.TempDir()
	writeBundleInto(t, repo, true)
	sum, err := schemapack.BundleChecksum(os.DirFS(repo), ".")
	if err != nil {
		t.Fatalf("checksum fixture: %v", err)
	}
	gitCmd(t, repo, "init", "--quiet", "--initial-branch=main")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "--quiet", "-m", "preset")
	return "file://" + repo, sum
}

func TestFetch_HappyPathPinsAndChecksums(t *testing.T) {
	t.Parallel()
	cloneURL, wantSum := newBundleRepo(t)

	dir, prov, err := fetch.Fetch(context.Background(), cloneURL, fetch.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if prov.Checksum != wantSum {
		t.Errorf("prov.Checksum = %q, want %q", prov.Checksum, wantSum)
	}
	if len(prov.PinnedRef) != 40 {
		t.Errorf("prov.PinnedRef = %q, want a 40-char commit SHA", prov.PinnedRef)
	}
	if prov.SourceURL != cloneURL {
		t.Errorf("prov.SourceURL = %q, want %q", prov.SourceURL, cloneURL)
	}
	// The returned dir must parse as a preset through the unchanged #210 path.
	pack, perr := schemapack.ParseDir(dir)
	if perr != nil {
		t.Fatalf("ParseDir(%q): %v", dir, perr)
	}
	if !pack.IsPreset() {
		t.Errorf("fetched bundle is not a preset")
	}
}

// TestFetch_CommitSHACheckout pins a NON-HEAD commit by SHA and verifies the
// fetch detaches HEAD at exactly that commit. Regression for the pathspec bug:
// `git checkout -- <sha>` treated the SHA as a file path and always errored, so
// commit-SHA pinning (a headline feature) was broken. Pinning the EARLIER commit
// and asserting the v1 content also proves the SHA is honored, not silently HEAD.
func TestFetch_CommitSHACheckout(t *testing.T) {
	t.Parallel()
	requireGit(t)
	repo := t.TempDir()
	writeBundleInto(t, repo, true)
	gitCmd(t, repo, "init", "--quiet", "--initial-branch=main")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "--quiet", "-m", "v1")
	pinned := gitOut(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "instructions.md"), "# Steering\nEDITED in a later commit.\n")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "--quiet", "-m", "v2")

	dir, prov, err := fetch.Fetch(context.Background(), "file://"+repo+"@"+pinned, fetch.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Fetch at commit SHA: %v", err)
	}
	if prov.PinnedRef != pinned {
		t.Errorf("prov.PinnedRef = %q, want the pinned commit %q", prov.PinnedRef, pinned)
	}
	body, rerr := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if rerr != nil {
		t.Fatalf("read fetched instructions: %v", rerr)
	}
	if strings.Contains(string(body), "EDITED") {
		t.Errorf("fetched the v2 instructions; the SHA pin should have checked out the v1 content")
	}
}

func TestFetch_ChecksumMatchAccepted(t *testing.T) {
	t.Parallel()
	cloneURL, wantSum := newBundleRepo(t)
	if _, _, err := fetch.Fetch(context.Background(), cloneURL, fetch.Options{DataDir: t.TempDir(), WantChecksum: wantSum}); err != nil {
		t.Fatalf("Fetch with matching checksum: %v", err)
	}
}

func TestFetch_ChecksumMismatchRejected(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newBundleRepo(t)
	_, _, err := fetch.Fetch(context.Background(), cloneURL, fetch.Options{
		DataDir:      t.TempDir(),
		WantChecksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	if !errors.Is(err, fetch.ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got %v", err)
	}
}

func TestFetch_MalformedChecksumRejectedBeforeClone(t *testing.T) {
	t.Parallel()
	// A malformed --checksum must fail fast with a clear FORMAT error — before
	// any clone — rather than cloning and then reporting an opaque mismatch
	// against garbage. A forge handle is a valid spec, so parsing succeeds and the
	// checksum validation is the first thing to reject; no network is touched.
	cases := []string{
		"not-a-checksum",
		"sha256:tooshort",
		"md5:00000000000000000000000000000000",
		"sha256:zzzz000000000000000000000000000000000000000000000000000000000000",
	}
	for _, want := range cases {
		_, _, err := fetch.Fetch(context.Background(), "github.com/u/r",
			fetch.Options{DataDir: t.TempDir(), WantChecksum: want})
		if err == nil || !strings.Contains(err.Error(), "--checksum must be") {
			t.Errorf("Fetch with --checksum %q: err = %v, want a format-rejection", want, err)
		}
		if errors.Is(err, fetch.ErrChecksumMismatch) {
			t.Errorf("Fetch with malformed --checksum %q returned a mismatch, want a format error", want)
		}
	}
}

func TestFetch_CacheReuse(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newBundleRepo(t)
	dataDir := t.TempDir()
	first, _, err := fetch.Fetch(context.Background(), cloneURL, fetch.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Fetch (first): %v", err)
	}
	second, _, err := fetch.Fetch(context.Background(), cloneURL, fetch.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Fetch (second): %v", err)
	}
	if first != second {
		t.Errorf("cache not reused: %q vs %q", first, second)
	}
}

func TestFetch_CacheTamperTriggersRefetch(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newBundleRepo(t)
	dataDir := t.TempDir()
	first, _, err := fetch.Fetch(context.Background(), cloneURL, fetch.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Fetch (first): %v", err)
	}
	// Tamper the cached bundle: the next fetch must detect the checksum drift and
	// re-materialize the entry from a fresh clone rather than reuse poisoned bytes.
	tampered := filepath.Join(first, "instructions.md")
	write(t, tampered, "# Steering\nTAMPERED\n")
	second, _, err := fetch.Fetch(context.Background(), cloneURL, fetch.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Fetch (second): %v", err)
	}
	if first != second {
		t.Fatalf("cache key changed unexpectedly: %q vs %q", first, second)
	}
	got, rerr := os.ReadFile(filepath.Join(second, "instructions.md"))
	if rerr != nil {
		t.Fatalf("read refetched instructions: %v", rerr)
	}
	if strings.Contains(string(got), "TAMPERED") {
		t.Fatalf("tampered cache entry was reused instead of re-materialized: %q", got)
	}
}

// TestFetch_DashRefRejected guards the argument-injection defense: a ref that
// looks like a git flag (e.g. "--detach") must be treated as a ref name — which
// does not exist — and error, never silently interpreted as a checkout flag.
func TestFetch_DashRefRejected(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newBundleRepo(t)
	_, _, err := fetch.Fetch(context.Background(), cloneURL+"@--detach", fetch.Options{DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("want error for flag-like ref %q, got nil", "--detach")
	}
}

// TestFetch_OverCapInstructionsRejectedByParseDir proves a remote body is
// subject to the SAME parse-time defenses as a local one: fetch only relocates
// the bytes; the unchanged schemapack.ParseDir enforces the instructions byte
// cap on the returned dir (the sanitize/fence/privacy controls share this path).
func TestFetch_OverCapInstructionsRejectedByParseDir(t *testing.T) {
	t.Parallel()
	requireGit(t)
	repo := t.TempDir()
	writeBundleInto(t, repo, false)
	// An instructions.md larger than the cap (8 KiB) must be refused by ParseDir.
	big := "# Steering\n" + strings.Repeat("A", schemapack.MaxInstructionsBytes+1)
	write(t, filepath.Join(repo, "instructions.md"), big)
	gitCmd(t, repo, "init", "--quiet", "--initial-branch=main")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "--quiet", "-m", "oversized")

	dir, _, err := fetch.Fetch(context.Background(), "file://"+repo, fetch.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, perr := schemapack.ParseDir(dir); !errors.Is(perr, schemapack.ErrInstructionsTooLarge) {
		t.Fatalf("ParseDir on over-cap remote body: want ErrInstructionsTooLarge, got %v", perr)
	}
}

// TestFetch_NonexistentBranchErrorsDirectly guards the up-front clone strategy:
// a non-existent (non-SHA) ref must fail fast on the shallow --branch clone, not
// silently fall back to a full clone of the whole history.
func TestFetch_NonexistentBranchErrorsDirectly(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newBundleRepo(t)
	_, _, err := fetch.Fetch(context.Background(), cloneURL+"@no-such-branch", fetch.Options{DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("want error for non-existent branch ref, got nil")
	}
}

// TestFetch_MultiPackRepoListsSubdirs checks the ambiguous-repo error names the
// candidate bundle directories so the operator knows which #subdir to pass.
func TestFetch_MultiPackRepoListsSubdirs(t *testing.T) {
	t.Parallel()
	requireGit(t)
	repo := t.TempDir()
	// Two bundles in subdirs, no manifest at the root.
	for _, name := range []string{"crm", "notes"} {
		writeBundleInto(t, filepath.Join(repo, name), true)
	}
	gitCmd(t, repo, "init", "--quiet", "--initial-branch=main")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "--quiet", "-m", "multi")

	_, _, err := fetch.Fetch(context.Background(), "file://"+repo, fetch.Options{DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("want error for ambiguous multi-pack repo, got nil")
	}
	if !strings.Contains(err.Error(), "crm") || !strings.Contains(err.Error(), "notes") {
		t.Fatalf("error should list candidate subdirs crm and notes, got: %v", err)
	}
}

func TestFetch_MissingManifestInSubdir(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newBundleRepo(t)
	_, _, err := fetch.Fetch(context.Background(), cloneURL+"#no/such/dir", fetch.Options{DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("want error for missing subdir manifest, got nil")
	}
}

func TestFetch_RejectsSymlinkedCanonicalFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	requireGit(t)
	repo := t.TempDir()
	writeBundleInto(t, repo, false)
	// Commit instructions.md as a symlink to an arbitrary file.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	write(t, secret, "PRIVATE\n")
	if err := os.Symlink(secret, filepath.Join(repo, "instructions.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	gitCmd(t, repo, "init", "--quiet", "--initial-branch=main")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "--quiet", "-m", "evil")

	_, _, err := fetch.Fetch(context.Background(), "file://"+repo, fetch.Options{DataDir: t.TempDir()})
	if !errors.Is(err, schemapack.ErrBundleSymlink) {
		t.Fatalf("want ErrBundleSymlink, got %v", err)
	}
}
