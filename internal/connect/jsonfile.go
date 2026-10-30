package connect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// MazServerKey is the key Mazarbul writes itself under in every JSON
// client config. Stable across versions; tests and golden fixtures
// pin against this exact identifier.
const MazServerKey = "mazarbul"

// ReadJSONFile reads a JSON file into a generic map. Returns an empty
// map (not nil) when the file is absent so callers can write into it
// directly without a nil-check.
func ReadJSONFile(path string) (map[string]any, error) {
	clean := filepath.Clean(path)
	data, err := os.ReadFile(clean) // #nosec G304 -- path validated by adapter.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseJSONBytes(data, path)
}

// ParseJSONBytes decodes already-read config bytes into a map, treating empty
// input as an empty document (so callers can write into it directly). Shared so
// a caller that already holds the raw bytes — e.g. the JSONC guard in Detect —
// parses without a second read of the file. Path is used only for error context.
func ParseJSONBytes(data []byte, path string) (map[string]any, error) {
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// WriteJSONFileAtomic marshals doc with 2-space indent + trailing
// newline and replaces path atomically. Creates parent dirs (0o700)
// and the file (0o600) — configs may carry credentials. Calls
// SnapshotIfNew before writing so the original is preserved as a
// backup; returns the backup path (empty when no backup was taken,
// e.g. file did not exist or a backup already exists this minute).
func WriteJSONFileAtomic(path string, doc map[string]any) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	backupPath, err := SnapshotIfNew(path)
	if err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".maz-tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of temp file after rename.
	}()
	if _, err := tmp.Write(data); err != nil {
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

// EnsureServersMap returns the named map at root[key], creating it
// when absent. Used by adapters to locate "mcpServers" / "servers".
func EnsureServersMap(root map[string]any, key string) map[string]any {
	if v, ok := root[key].(map[string]any); ok {
		return v
	}
	m := map[string]any{}
	root[key] = m
	return m
}

// EntryEquals returns true iff existing matches the entry we'd write
// today. Used to distinguish StatusRegistered from
// StatusRegisteredStale, and to flag conflicts during Apply.
func EntryEquals(existing any, want ServerEntry) bool {
	m, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := m["command"].(string) //nolint:errcheck // best-effort: a non-string command means stale entry, treated as not-equal.
	if cmd != want.Command {
		return false
	}
	if !sliceEquals(m["args"], want.Args) {
		return false
	}
	return envEquals(m["env"], want.Env)
}

func sliceEquals(a any, want []string) bool {
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
	got := make([]string, len(arr))
	for i, x := range arr {
		s, ok := x.(string)
		if !ok {
			return false
		}
		got[i] = s
	}
	return reflect.DeepEqual(got, want)
}

func envEquals(a any, want map[string]string) bool {
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

// JSONEntry serialises a ServerEntry into the map shape json.Marshal
// would produce. Building it by hand lets the round-trip preserve
// key ordering predictably.
func JSONEntry(entry ServerEntry) map[string]any {
	m := map[string]any{"command": entry.Command}
	if len(entry.Args) > 0 {
		m["args"] = entry.Args
	}
	if len(entry.Env) > 0 {
		m["env"] = entry.Env
	}
	return m
}
