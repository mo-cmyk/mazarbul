//go:build darwin

package daemon

import (
	"bytes"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// TestApplyDataDirHygiene_SetsTimeMachineExclusion pins the Time
// Machine opt-out xattr that protects against the Tahoe 26.2-26.3
// backup-during-indexing freeze regression. The xattr is the same
// flag `tmutil addexclusion` writes: backupd parses the bplist value
// and ignores invalid encodings, so we pin the canonical 61-byte
// payload verified against a `tmutil addexclusion`-managed dir.
func TestApplyDataDirHygiene_SetsTimeMachineExclusion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ApplyDataDirHygiene(dir, zap.NewNop()); err != nil {
		t.Fatalf("ApplyDataDirHygiene: %v", err)
	}

	buf := make([]byte, 256)
	n, err := unix.Getxattr(dir, "com.apple.metadata:com_apple_backup_excludeItem", buf)
	if err != nil {
		t.Fatalf("xattr not set on %s: %v", dir, err)
	}
	if !bytes.Equal(buf[:n], timeMachineExcludeBplist) {
		t.Fatalf("xattr value = %x\n         want = %x", buf[:n], timeMachineExcludeBplist)
	}
}
