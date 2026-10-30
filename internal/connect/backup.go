package connect

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// backupState is the process-lifetime registry of already-snapshotted paths.
// One struct rather than two separate package-level vars satisfies
// gochecknoglobals with a single nolint directive.
type backupState struct {
	mu      sync.Mutex
	entries map[string]int64 // path -> unix minute of last backup
}

var globalBackupState = &backupState{entries: make(map[string]int64)} //nolint:gochecknoglobals // process-lifetime mutable registry; no clean alternative for session-scoped dedup.

// SnapshotIfNew copies path to <path>.maz-backup-<unix-ts> before the
// first write. If a backup from the current minute already exists for
// this path (or the source file is absent) it is a no-op. Returns the
// backup path on success, or empty string when skipped.
func SnapshotIfNew(path string) (string, error) {
	return globalBackupState.snapshot(path)
}

func (b *backupState) snapshot(path string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UTC()
	minute := now.Unix() / 60
	if last, ok := b.entries[path]; ok && last == minute {
		return "", nil
	}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat %s: %w", path, err)
	}

	ts := strconv.FormatInt(now.Unix(), 10)
	backupPath := path + ".maz-backup-" + ts
	if err := copyFile(path, backupPath); err != nil {
		return "", fmt.Errorf("snapshot %s: %w", path, err)
	}
	b.entries[path] = minute
	return backupPath, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(filepath.Clean(src)) // #nosec G304 -- src validated by caller.
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	out, err := os.OpenFile(filepath.Clean(dst), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return out.Sync()
}
