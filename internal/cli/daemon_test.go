package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// newLogsShowCmd builds a minimal command for testing runDaemonLogsShow.
func newLogsShowTestCmd(out *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "test", SilenceUsage: true, SilenceErrors: true}
	cmd.PersistentFlags().String("output", "", "output format")
	cmd.PersistentFlags().Bool("json", false, "JSON output")
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd
}

// TestRunDaemonLogsShowCLI asserts the plain-CLI output is just the path.
func TestRunDaemonLogsShowCLI(t *testing.T) {
	var buf bytes.Buffer
	cmd := newLogsShowTestCmd(&buf)
	if err := runDaemonLogsShow(cmd, &buf, "/tmp/maz-daemon.log"); err != nil {
		t.Fatalf("runDaemonLogsShow: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "/tmp/maz-daemon.log") {
		t.Errorf("CLI output missing path:\n got: %q", got)
	}
}

// TestRunDaemonLogsShowJSON asserts the JSON output carries the logFile key.
func TestRunDaemonLogsShowJSON(t *testing.T) {
	var buf bytes.Buffer
	cmd := newLogsShowTestCmd(&buf)
	// Force JSON mode via --output flag so output.From picks it up.
	_ = cmd.Flags().Set("output", "json")
	if err := runDaemonLogsShow(cmd, &buf, "/tmp/maz-daemon.log"); err != nil {
		t.Fatalf("runDaemonLogsShow: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "/tmp/maz-daemon.log") {
		t.Errorf("missing path in JSON output: %s", got)
	}
}

// TestRunDaemonLogsShowTUI asserts that the path appears in the output.
func TestRunDaemonLogsShowTUI(t *testing.T) {
	var buf bytes.Buffer
	cmd := newLogsShowTestCmd(&buf)
	if err := runDaemonLogsShow(cmd, &buf, "/tmp/maz-daemon.log"); err != nil {
		t.Fatalf("runDaemonLogsShow: %v", err)
	}
	if !strings.Contains(buf.String(), "/tmp/maz-daemon.log") {
		t.Errorf("path missing from output: %s", buf.String())
	}
}

// TestRunDaemonLogsFollowAppends verifies that lines appended to the file
// after follow starts are written to the output, and that the follow loop
// exits cleanly when the context is cancelled.
func TestRunDaemonLogsFollowAppends(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(logPath, []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := &syncBuffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	var followErr error
	go func() {
		defer wg.Done()
		followErr = runDaemonLogsFollow(ctx, out, logPath)
	}()

	// Give the follower a moment to seek to end.
	time.Sleep(100 * time.Millisecond)

	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	for _, line := range []string{"alpha\n", "beta\n", "gamma\n"} {
		if _, werr := f.WriteString(line); werr != nil {
			t.Fatalf("append: %v", werr)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close append: %v", err)
	}

	// Wait until all three appear (bounded).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := out.String()
		if strings.Contains(s, "alpha") && strings.Contains(s, "beta") && strings.Contains(s, "gamma") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	got := out.String()
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in follow output: %q", want, got)
		}
	}
	// "seed" was written before follow started, so it must NOT appear
	// (we seek to end).
	if strings.Contains(got, "seed") {
		t.Errorf("seed should not appear (seek-to-end): %q", got)
	}

	cancel()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("follow did not exit within 2s of context cancel")
	}
	if followErr != nil {
		t.Errorf("follow returned %v, want nil", followErr)
	}
}

// TestRunDaemonLogsFollowAcrossRotation simulates a lumberjack rotation by
// renaming the live file to a backup and creating a fresh file at the
// original path. The follow loop must detect the inode change, reopen, and
// continue reading from the new file.
func TestRunDaemonLogsFollowAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(logPath, []byte(""), 0o600); err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := &syncBuffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	var followErr error
	go func() {
		defer wg.Done()
		followErr = runDaemonLogsFollow(ctx, out, logPath)
	}()
	time.Sleep(100 * time.Millisecond)

	if err := appendLine(logPath, "pre-rotation\n"); err != nil {
		t.Fatalf("append pre: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Simulate lumberjack rotation.
	backup := filepath.Join(dir, "daemon-backup.log")
	if err := os.Rename(logPath, backup); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(""), 0o600); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if err := appendLine(logPath, "post-rotation\n"); err != nil {
		t.Fatalf("append post: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := out.String()
		if strings.Contains(s, "pre-rotation") && strings.Contains(s, "post-rotation") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	got := out.String()
	for _, want := range []string{"pre-rotation", "post-rotation"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in follow output across rotation: %q", want, got)
		}
	}

	cancel()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("follow did not exit within 2s of context cancel")
	}
	if followErr != nil {
		t.Errorf("follow returned %v, want nil", followErr)
	}
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// TestRunDaemonLogsFollowMissingFile asserts that following a non-existent
// log returns a friendly error.
func TestRunDaemonLogsFollowMissingFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.log")

	var buf bytes.Buffer
	err := runDaemonLogsFollow(ctx, &buf, missing)
	if err == nil {
		t.Fatal("expected error for missing log file, got nil")
	}
	if !strings.Contains(err.Error(), "daemon log not found") {
		t.Errorf("error %q should mention missing log", err)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer wrapper for the follow test
// which races a writer goroutine with the test reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
