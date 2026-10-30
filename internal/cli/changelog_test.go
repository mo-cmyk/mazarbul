package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunChangelog_PrintsOneFragmentPerCommit pins gap #1: `maz changelog`
// must surface each per-push CHANGELOG fragment as a chronological row,
// reading from the scope's local git mirror under
// <dataDir>/sync/<scope>/repo/CHANGELOG/. Reverse order, newest first,
// limit honored.
//
// We build a minimal mirror with two empty commits, each touching one
// CHANGELOG fragment, and assert the output contains both fragment
// filenames in newest-first order. Before this command lands the test
// fails with "command not found".
func TestRunChangelog_PrintsOneFragmentPerCommit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git required: %v", err)
	}

	dir := t.TempDir()
	writePersonalConfigToPath(t, dir, "git@example.com:demo.git")
	repoDir := filepath.Join(dir, "sync", "personal", "repo")

	ctx := context.Background()
	mustGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", repoDir}, args...)
		if out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := exec.CommandContext(ctx, "git", "init", "--quiet", "--initial-branch=main", repoDir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	mustGit("config", "user.email", "log@test")
	mustGit("config", "user.name", "log")
	mustGit("commit", "--allow-empty", "-m", "first push")

	frag1 := filepath.Join(repoDir, "CHANGELOG", "2026", "06", "01HZAA00000000000000000001__device-a.md")
	if err := exec.CommandContext(ctx, "mkdir", "-p", filepath.Dir(frag1)).Run(); err != nil {
		t.Fatalf("mkdir frag1: %v", err)
	}
	if err := writeFile(frag1, []byte("# Push 01HZAA\n\n- +1 entity\n")); err != nil {
		t.Fatalf("write frag1: %v", err)
	}
	mustGit("add", "CHANGELOG/2026/06/01HZAA00000000000000000001__device-a.md")
	mustGit("commit", "-m", "sync: personal +1 plaintext chunks via device-a")

	frag2 := filepath.Join(repoDir, "CHANGELOG", "2026", "06", "01HZBB00000000000000000002__device-a.md")
	if err := writeFile(frag2, []byte("# Push 01HZBB\n\n- ⤳1 relation\n")); err != nil {
		t.Fatalf("write frag2: %v", err)
	}
	mustGit("add", "CHANGELOG/2026/06/01HZBB00000000000000000002__device-a.md")
	mustGit("commit", "-m", "sync: personal +1 plaintext chunks via device-a")

	var buf bytes.Buffer
	if err := runChangelog(ctx, &buf, dir, "personal", 50); err != nil {
		t.Fatalf("runChangelog: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "01HZBB") || !strings.Contains(out, "01HZAA") {
		t.Errorf("changelog missing one or both fragment ULIDs; got:\n%s", out)
	}
	// Newest-first: 01HZBB must appear before 01HZAA in the output.
	bbIdx := strings.Index(out, "01HZBB")
	aaIdx := strings.Index(out, "01HZAA")
	if bbIdx < 0 || aaIdx < 0 || bbIdx > aaIdx {
		t.Errorf("changelog rows not in reverse-chrono order; output:\n%s", out)
	}
}

// writeFile is a thin os.WriteFile wrapper so the test reads top-down
// without inlining boilerplate per fragment. Files written here are
// inside t.TempDir, so 0o600 is sufficient.
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
