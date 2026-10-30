package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// newPresetGitRepo turns a preset fixture into a local git repo and returns its
// file:// clone URL plus the canonical checksum of the committed bundle.
func newPresetGitRepo(t *testing.T) (cloneURL, checksum string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := writePresetFixture(t, "crm", true)
	sum, err := schemapack.BundleChecksum(os.DirFS(repo), ".")
	if err != nil {
		t.Fatalf("checksum fixture: %v", err)
	}
	hermeticGit(t, repo, "init", "--quiet", "--initial-branch=main")
	hermeticGit(t, repo, "config", "user.email", "t@example.com")
	hermeticGit(t, repo, "config", "user.name", "t")
	hermeticGit(t, repo, "add", "-A")
	hermeticGit(t, repo, "commit", "--quiet", "-m", "preset")
	return "file://" + repo, sum
}

// TestPresetPackage_ValidEmitsStableChecksum validates a bundle and prints a
// stable sha256 checksum (the verifiable-publish artifact).
func TestPresetPackage_ValidEmitsStableChecksum(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixture(t, "crm", true)
	run := func() []byte {
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "package", fixture, "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset package: %v (stderr=%q)", err, errBuf.String())
		}
		return out.Bytes()
	}
	var first struct {
		Checksum string `json:"checksum"`
		IsPreset bool   `json:"isPreset"`
	}
	if err := json.Unmarshal(run(), &first); err != nil {
		t.Fatalf("package JSON: %v", err)
	}
	if !strings.HasPrefix(first.Checksum, "sha256:") {
		t.Errorf("checksum %q missing sha256: prefix", first.Checksum)
	}
	if !first.IsPreset {
		t.Errorf("fixture with instructions.md should report isPreset=true")
	}
	var second struct {
		Checksum string `json:"checksum"`
	}
	if err := json.Unmarshal(run(), &second); err != nil {
		t.Fatalf("package JSON (second): %v", err)
	}
	if first.Checksum != second.Checksum {
		t.Errorf("checksum not stable across runs: %q vs %q", first.Checksum, second.Checksum)
	}
}

// TestPresetPackage_InvalidRejected rejects a bundle with a malformed manifest
// and names the offending field.
func TestPresetPackage_InvalidRejected(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixture(t, "crm", true)
	// Drop the required version from the manifest.
	bad := "name = \"crm\"\ndescription = \"x\"\ncategory = \"extension\"\nkind = \"schema-pack\"\n"
	if err := os.WriteFile(filepath.Join(fixture, "manifest.toml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "package", fixture})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for invalid bundle, got nil")
	}
	if !strings.Contains(errBuf.String(), "version") {
		t.Errorf("stderr %q should name the missing version", errBuf.String())
	}
}

// TestPresetCmd_RemoteRefusedWithoutYes refuses a remote install when the body
// was not human-reviewed (non-interactive, no --yes). The bundle is fetched but
// never seeded.
func TestPresetCmd_RemoteRefusedWithoutYes(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newPresetGitRepo(t)
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", cloneURL, "--output", "json", "--data-dir", t.TempDir()})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal installing a remote preset without --yes, got nil")
	}
	if !strings.Contains(errBuf.String(), "--yes") {
		t.Errorf("stderr %q should tell the user to re-run with --yes", errBuf.String())
	}
}

// TestPresetCmd_RemoteChecksumMismatchRefused refuses a remote install whose
// fetched content does not match a supplied --checksum, before any seeding.
func TestPresetCmd_RemoteChecksumMismatchRefused(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newPresetGitRepo(t)
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{
		"preset", "install", cloneURL, "--yes", "--data-dir", t.TempDir(),
		"--checksum", "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal on checksum mismatch, got nil")
	}
}

// TestPresetCmd_RemoteYesSuppressesBody confirms a --yes remote install does NOT
// echo the verbatim instructions body anywhere on stdout/stderr: with consent
// given up front there is no review to print, and the body is sensitive.
func TestPresetCmd_RemoteYesSuppressesBody(t *testing.T) {
	skipIfShortLiveBackend(t)
	cloneURL, _ := newPresetGitRepo(t)
	dataDir := newPresetDataDir(t)

	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", cloneURL, "--yes", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("remote install: %v (stderr=%q)", err, errBuf.String())
	}
	// A distinctive sentence from presetInstructionsBody must not leak.
	const sentinel = "When the user mentions a company"
	if strings.Contains(out.String(), sentinel) || strings.Contains(errBuf.String(), sentinel) {
		t.Fatalf("instructions body leaked on --yes install:\nstdout=%q\nstderr=%q", out.String(), errBuf.String())
	}
}

// TestInstallCmd_RemoteChecksumMismatchRefused checks the --checksum tamper gate
// fires on the plain-pack `maz install` path too (not just preset install),
// before anything is parsed or installed.
func TestInstallCmd_RemoteChecksumMismatchRefused(t *testing.T) {
	t.Parallel()
	cloneURL, _ := newPresetGitRepo(t)
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{
		"install", cloneURL, "--data-dir", t.TempDir(),
		"--checksum", "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal on checksum mismatch, got nil")
	}
}

// TestPresetCmd_RemoteInstallRecordsProvenance installs a preset from a local
// git repo and confirms `preset show` surfaces the source, pinned commit, and
// checksum.
func TestPresetCmd_RemoteInstallRecordsProvenance(t *testing.T) {
	skipIfShortLiveBackend(t)
	cloneURL, wantSum := newPresetGitRepo(t)
	dataDir := newPresetDataDir(t)

	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "install", cloneURL, "--yes", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("remote install: %v (stderr=%q)", err, errBuf.String())
		}
	}

	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "show", "crm", "--output", "json", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("show: %v (stderr=%q)", err, errBuf.String())
	}
	var got struct {
		SourceURL string `json:"sourceUrl"`
		PinnedRef string `json:"pinnedRef"`
		Checksum  string `json:"checksum"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("show JSON: %v (raw=%q)", err, out.String())
	}
	if got.SourceURL != cloneURL {
		t.Errorf("sourceUrl = %q, want %q", got.SourceURL, cloneURL)
	}
	if len(got.PinnedRef) != 40 {
		t.Errorf("pinnedRef = %q, want a 40-char commit SHA", got.PinnedRef)
	}
	if got.Checksum != wantSum {
		t.Errorf("checksum = %q, want %q", got.Checksum, wantSum)
	}
}
