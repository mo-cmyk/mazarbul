package daemon_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// staleDeadPID spawns a tiny subprocess, waits for it to exit, and returns
// its PID. The PID is dead the instant Wait returns. If the OS happened to
// recycle it before we can probe (extraordinarily rare in test sandboxes,
// but possible on a heavily loaded host), the test calling this helper
// re-probes and skips itself rather than report a false negative.
func staleDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	if pid <= 0 {
		t.Fatalf("subprocess returned non-positive PID %d", pid)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Skipf("PID %d was recycled between Wait and probe; skipping flaky case", pid)
	}
	return pid
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func TestWritePID_RoundtripsToken(t *testing.T) {
	// Greptile P1 #107 round 5 (security): the per-launch token
	// MUST land in the PID file so `daemon stop` can read it and
	// verify ownership via the trailing-suffix check. Roundtrip
	// guarantees the two-line format survives Read after Write.
	path := filepath.Join(t.TempDir(), "maz.pid")
	const tok = "deadbeefcafebabe0123456789abcdef"
	const dir = "/tmp/maz-roundtrip-store"
	if err := daemon.WritePID(path, tok, dir); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	info, err := daemon.ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if info.Token != tok {
		t.Errorf("Token roundtrip = %q, want %q", info.Token, tok)
	}
	if info.DataDir != dir {
		t.Errorf("DataDir roundtrip = %q, want %q", info.DataDir, dir)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
}

// TestReadPID_PreservesDataDirWhitespace pins the Greptile P2 on #181: a data
// dir is a filesystem path, so leading/trailing spaces are valid characters.
// ReadPID must not TrimSpace them away — that would rewrite the path and let
// daemonServesDataDir mismatch a daemon-held store. Only a CRLF artifact is
// stripped.
func TestReadPID_PreservesDataDirWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	const dir = " /tmp/maz store/trailing " // leading AND trailing spaces, all valid
	if err := daemon.WritePID(path, "tok", dir); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	info, err := daemon.ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if info.DataDir != dir {
		t.Errorf("DataDir whitespace not preserved: got %q, want %q", info.DataDir, dir)
	}
}

func TestReadPID_LegacyTwoLineNoDataDir(t *testing.T) {
	// A two-line PID file (PID + token, no data dir) written before the
	// data dir was recorded must read as DataDir == "" without error, so
	// the data-dir-aware preflight degrades to its default-dir heuristic.
	path := filepath.Join(t.TempDir(), "maz.pid")
	writeFile(t, path, strconv.Itoa(os.Getpid())+"\ntok123\n")
	info, err := daemon.ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID two-line: %v", err)
	}
	if info.Token != "tok123" {
		t.Errorf("Token = %q, want tok123", info.Token)
	}
	if info.DataDir != "" {
		t.Errorf("DataDir from two-line file = %q, want empty", info.DataDir)
	}
}

func TestReadPID_LegacySingleLineToken(t *testing.T) {
	// Backward compat: a single-line PID file (no token) must read
	// as Token == "" without error. Predates token-bearing daemons;
	// the `daemon stop` IsMazDaemon call falls back to the legacy
	// comm+adjacency check, which is weaker but functional.
	path := filepath.Join(t.TempDir(), "maz.pid")
	writeFile(t, path, strconv.Itoa(os.Getpid())+"\n")
	info, err := daemon.ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID legacy: %v", err)
	}
	if info.Token != "" {
		t.Errorf("Token from single-line file = %q, want empty", info.Token)
	}
}

func TestWritePID_Fresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")

	if err := daemon.WritePID(path, "", ""); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 0600", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strconv.Itoa(os.Getpid())
	if got != want {
		t.Errorf("contents = %q, want %q", got, want)
	}
}

func TestWritePID_Stale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	deadPID := staleDeadPID(t)
	writeFile(t, path, strconv.Itoa(deadPID)+"\n")

	if err := daemon.WritePID(path, "", ""); err != nil {
		t.Fatalf("WritePID over stale file: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strconv.Itoa(os.Getpid())
	if got != want {
		t.Errorf("contents after overwrite = %q, want %q", got, want)
	}
}

func TestWritePID_Live(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	writeFile(t, path, strconv.Itoa(os.Getpid())+"\n")
	// WritePID's already-running gate requires both liveness AND
	// maz-daemon identity. The test runner is alive but not a maz
	// daemon, so stub the identity probe to claim ownership.
	restore := daemon.SetIsMazDaemonForTest(func(int, string) bool { return true })
	defer daemon.SetIsMazDaemonForTest(restore)

	err := daemon.WritePID(path, "", "")
	if !errors.Is(err, daemon.ErrAlreadyRunning) {
		t.Fatalf("WritePID over live PID: err = %v, want ErrAlreadyRunning", err)
	}
}

func TestWritePID_WriteFailure(t *testing.T) {
	// Parent directory does not exist; os.WriteFile must fail and the wrapped
	// error must unwrap to os.ErrNotExist so callers can errors.Is-match it.
	path := filepath.Join(t.TempDir(), "missing-dir", "maz.pid")

	err := daemon.WritePID(path, "", "")
	if err == nil {
		t.Fatalf("WritePID to missing dir: err = nil, want non-nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want one wrapping os.ErrNotExist", err)
	}
}

func TestWritePID_OverwriteEnforces0600(t *testing.T) {
	// Greptile P1 (security): os.WriteFile preserves an existing inode's
	// mode, so a stale 0644 file would stay world-readable after overwrite.
	// The Remove + O_CREATE|O_EXCL pattern must land a fresh 0600 file
	// regardless of the prior mode.
	path := filepath.Join(t.TempDir(), "maz.pid")
	deadPID := staleDeadPID(t)
	if err := os.WriteFile(path, []byte(strconv.Itoa(deadPID)+"\n"), 0o644); err != nil {
		t.Fatalf("seed 0644 stale file: %v", err)
	}
	if got, _ := os.Stat(path); got.Mode().Perm() != 0o644 {
		t.Fatalf("precondition: stale file mode = %o, want 0644", got.Mode().Perm())
	}

	if err := daemon.WritePID(path, "", ""); err != nil {
		t.Fatalf("WritePID over stale 0644 file: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after WritePID: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode after stale overwrite = %o, want 0600", got)
	}
}

func TestWritePID_SymlinkRejected(t *testing.T) {
	// Greptile P1 (security): WritePID must not follow a planted symlink.
	// Seed `path` as a symlink to `target`; call WritePID; verify `path` is
	// now a regular 0600 file containing our PID and `target` is untouched.
	dir := t.TempDir()
	path := filepath.Join(dir, "maz.pid")
	target := filepath.Join(dir, "innocent-bystander.txt")
	if err := os.WriteFile(target, []byte("do not corrupt me\n"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	if err := daemon.WritePID(path, "", ""); err != nil {
		t.Fatalf("WritePID over symlink: %v", err)
	}

	// Target file must be unchanged.
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if got := string(data); got != "do not corrupt me\n" {
		t.Errorf("target corrupted: got %q", got)
	}

	// path must now be a regular file (not a symlink) with our PID and 0600.
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat path: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("path is %s, want regular file", fi.Mode())
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("path mode = %o, want 0600", got)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read path: %v", err)
	}
	want := strconv.Itoa(os.Getpid()) + "\n"
	if string(got) != want {
		t.Errorf("path contents = %q, want %q", string(got), want)
	}
}

func TestReadPID_SymlinkRejected(t *testing.T) {
	// ReadPID's Lstat check refuses to follow symlinks so an attacker cannot
	// trick the caller into reading another file's contents through the
	// predictable PID-file path.
	dir := t.TempDir()
	path := filepath.Join(dir, "maz.pid")
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("12345\n"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	info, err := daemon.ReadPID(path)
	if err == nil {
		t.Fatalf("ReadPID(symlink): err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want one mentioning 'not a regular file'", err)
	}
	if info.PID != 0 || info.Running {
		t.Errorf("info = %+v, want zero value", info)
	}
}

func TestReadPID_Present(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	writeFile(t, path, strconv.Itoa(os.Getpid())+"\n")

	info, err := daemon.ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
	if !info.Running {
		t.Errorf("Running = false, want true for self PID")
	}
}

func TestReadPID_Missing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")

	info, err := daemon.ReadPID(path)
	if err == nil {
		t.Fatalf("ReadPID(missing): err = nil, want non-nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want one wrapping os.ErrNotExist", err)
	}
	if info.PID != 0 || info.Running {
		t.Errorf("info = %+v, want zero value", info)
	}
}

func TestReadPID_Malformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	writeFile(t, path, "not-a-pid\n")

	info, err := daemon.ReadPID(path)
	if err == nil {
		t.Fatalf("ReadPID(malformed): err = nil, want non-nil")
	}
	if info.PID != 0 || info.Running {
		t.Errorf("info = %+v, want zero value", info)
	}
}

func TestReadPID_Stale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	deadPID := staleDeadPID(t)
	writeFile(t, path, strconv.Itoa(deadPID)+"\n")

	info, err := daemon.ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID stale: %v", err)
	}
	if info.PID != deadPID {
		t.Errorf("PID = %d, want %d", info.PID, deadPID)
	}
	if info.Running {
		t.Errorf("Running = true, want false for dead PID %d", deadPID)
	}
}

func TestDeletePID_Missing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	if err := daemon.DeletePID(path); err != nil {
		t.Fatalf("DeletePID(missing) = %v, want nil", err)
	}
}

func TestDeletePID_Present(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maz.pid")
	writeFile(t, path, "123\n")

	if err := daemon.DeletePID(path); err != nil {
		t.Fatalf("DeletePID: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still present after delete: stat err = %v", err)
	}

	// Idempotent: a second delete on the now-missing path is also nil.
	if err := daemon.DeletePID(path); err != nil {
		t.Errorf("second DeletePID = %v, want nil", err)
	}
}

func TestDefaultPath(t *testing.T) {
	tests := []struct {
		name    string
		envFile string
		xdg     string
		tmp     string
		want    string
	}{
		{
			name:    "MAZ_PIDFILE override wins",
			envFile: "/var/run/custom/maz.pid",
			xdg:     "/run/user/1000",
			tmp:     "/var/folders/xx",
			want:    "/var/run/custom/maz.pid",
		},
		{
			name: "XDG_RUNTIME_DIR wins absent override",
			xdg:  "/run/user/1000",
			tmp:  "/var/folders/xx",
			want: "/run/user/1000/mazarbul.pid",
		},
		{
			name: "TMPDIR fallback",
			tmp:  "/var/folders/xx",
			want: "/var/folders/xx/mazarbul.pid",
		},
		{
			name: "final fallback /tmp",
			want: "/tmp/mazarbul.pid",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAZ_PIDFILE", tc.envFile)
			t.Setenv("XDG_RUNTIME_DIR", tc.xdg)
			t.Setenv("TMPDIR", tc.tmp)
			if got := daemon.DefaultPath(); got != tc.want {
				t.Errorf("DefaultPath() = %q, want %q", got, tc.want)
			}
		})
	}
}
