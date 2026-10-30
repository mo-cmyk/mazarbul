package connect

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ReadTOMLFile reads a TOML file into a generic map. Returns an empty
// map (not nil) when the file is absent so callers can write into it
// directly without a nil-check.
func ReadTOMLFile(path string) (map[string]any, error) {
	clean := filepath.Clean(path)
	data, err := os.ReadFile(clean) // #nosec G304 -- path validated by adapter.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := toml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// WriteTOMLFileAtomic marshals doc as TOML and replaces path atomically.
// Creates parent dirs (0o700) and the file (0o600) — configs may carry
// credentials. Calls SnapshotIfNew before writing so the original is
// preserved as a backup.
func WriteTOMLFileAtomic(path string, doc map[string]any) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	backupPath, err := SnapshotIfNew(path)
	if err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".maz-tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of temp file after rename.
	}()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return "", fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("rename: %w", err)
	}
	return backupPath, nil
}

// TOMLEntry serialises a ServerEntry into the map shape the TOML
// encoder expects. Nil slices and maps are omitted to keep the output
// tidy.
func TOMLEntry(entry ServerEntry) map[string]any {
	m := map[string]any{"command": entry.Command}
	if len(entry.Args) > 0 {
		m["args"] = entry.Args
	}
	if len(entry.Env) > 0 {
		m["env"] = entry.Env
	}
	return m
}

// TOMLEntryEquals returns true iff existing matches the entry we'd
// write today. Mirrors EntryEquals for the TOML decode path where
// numbers arrive as int64 and strings remain strings.
func TOMLEntryEquals(existing any, want ServerEntry) bool {
	m, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := m["command"].(string) //nolint:errcheck // non-string command means stale entry.
	if cmd != want.Command {
		return false
	}
	if !tomlSliceEquals(m["args"], want.Args) {
		return false
	}
	return tomlEnvEquals(m["env"], want.Env)
}

func tomlSliceEquals(a any, want []string) bool {
	if len(want) == 0 {
		if a == nil {
			return true
		}
		if arr, ok := a.([]any); ok && len(arr) == 0 {
			return true
		}
		return false
	}
	arr, ok := a.([]any)
	if !ok || len(arr) != len(want) {
		return false
	}
	for i, x := range arr {
		s, ok := x.(string)
		if !ok || s != want[i] {
			return false
		}
	}
	return true
}

func tomlEnvEquals(a any, want map[string]string) bool {
	if len(want) == 0 {
		if a == nil {
			return true
		}
		if m, ok := a.(map[string]any); ok && len(m) == 0 {
			return true
		}
		return false
	}
	m, ok := a.(map[string]any)
	if !ok || len(m) != len(want) {
		return false
	}
	for k, v := range want {
		gv, ok := m[k].(string)
		if !ok || gv != v {
			return false
		}
	}
	return true
}
