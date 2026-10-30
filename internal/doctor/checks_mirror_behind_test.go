package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitDoctorRun runs a git command in dir (or the cwd when dir is empty),
// failing the test on any error.
func gitDoctorRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestMirrorBehindCount_DetectsLaggingMirror is the F7c regression. When the
// local mirror's main lags origin/main — the state a device lands in after
// losing a push-contention race — the chunk-set-based `maz sync status` can
// still report "up to date". The git-ancestry check must surface the
// divergence so a split-brain is never reported as synced.
func TestMirrorBehindCount_DetectsLaggingMirror(t *testing.T) {
	t.Parallel()
	gitBin, lookErr := exec.LookPath("git")
	if lookErr != nil {
		t.Skipf("git not in PATH: %v", lookErr)
	}
	ctx := context.Background()

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitDoctorRun(t, "", "init", "--bare", "--initial-branch=main", origin)

	dataDir := filepath.Join(root, "data")
	repo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(repo), 0o750); err != nil {
		t.Fatal(err)
	}
	gitDoctorRun(t, "", "clone", "--quiet", origin, repo)
	gitDoctorRun(t, repo, "config", "user.email", "t@t")
	gitDoctorRun(t, repo, "config", "user.name", "t")

	// C1 committed and pushed → mirror and origin both at C1.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitDoctorRun(t, repo, "add", ".")
	gitDoctorRun(t, repo, "commit", "--quiet", "-m", "c1")
	gitDoctorRun(t, repo, "push", "--quiet", "origin", "HEAD:main")

	if n := mirrorBehindCount(ctx, gitBin, repo); n != 0 {
		t.Fatalf("baseline behind = %d, want 0", n)
	}

	// A second clone advances origin to C2.
	other := filepath.Join(root, "other")
	gitDoctorRun(t, "", "clone", "--quiet", origin, other)
	gitDoctorRun(t, other, "config", "user.email", "t@t")
	gitDoctorRun(t, other, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(other, "b.txt"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitDoctorRun(t, other, "add", ".")
	gitDoctorRun(t, other, "commit", "--quiet", "-m", "c2")
	gitDoctorRun(t, other, "push", "--quiet", "origin", "HEAD:main")

	// The mirror fetches (updating the origin/main cache) but does NOT merge —
	// exactly the wedged/lagging state. It is now behind by one commit.
	gitDoctorRun(t, repo, "fetch", "--quiet", "origin")
	if n := mirrorBehindCount(ctx, gitBin, repo); n != 1 {
		t.Fatalf("behind after origin advanced = %d, want 1", n)
	}

	behind := findBehindScopes(ctx, gitBin, dataDir, []string{"personal"})
	if len(behind) != 1 || behind[0] != "personal" {
		t.Fatalf("findBehindScopes = %v, want [personal]", behind)
	}
}
