package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// WriteTOMLFileAtomic encodes v as TOML and atomically replaces path.
// The parent directory must already exist. On success the file ends up
// at mode 0o600 and any reader sees either the previous file contents
// or the new ones, never a partial write.
//
// Used for both ${dataDir}/config.toml and ${dataDir}/state.toml; the
// split lets the user own config.toml (hand-edited, comment-friendly)
// while the TUI owns state.toml (atomic writes, regenerated on save).
//
// Comments are not preserved on round-trip — neither BurntSushi/toml nor
// pelletier/go-toml v2 supports comment-preserving round-trip in Go
// today. State.toml is machine-managed so this is acceptable; config.toml
// stays comment-friendly because the TUI never writes to it.
func WriteTOMLFileAtomic(path string, v any) error {
	if path == "" {
		return errors.New("config: path is required")
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			_ = rmErr
		}
	}
	if encErr := toml.NewEncoder(tmp).Encode(v); encErr != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: encode: %w", encErr)
	}
	if chmodErr := tmp.Chmod(0o600); chmodErr != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: chmod temp: %w", chmodErr)
	}
	if syncErr := tmp.Sync(); syncErr != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: sync temp: %w", syncErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		cleanup()
		return fmt.Errorf("config: close temp: %w", closeErr)
	}
	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		cleanup()
		return fmt.Errorf("config: rename: %w", renameErr)
	}
	return nil
}
