package render

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CursorSchemaVersion is bumped when the cursor JSON shape changes
// incompatibly. Exported so callers (e.g. the git transport) can stamp
// it on fresh cursors without duplicating the magic number.
const CursorSchemaVersion = 1

// cursorSchemaVersion preserves the lower-case identifier the existing
// LoadCursor path uses internally.
const cursorSchemaVersion = CursorSchemaVersion

// Cursor records the last rendered event ULID so incremental renders know
// where to resume.
type Cursor struct {
	// ULID is the last consumed event ULID.
	ULID string `json:"ulid"`
	// RenderedAt is the instant the render was completed, in UTC.
	RenderedAt time.Time `json:"renderedAt"`
	// SchemaVersion is bumped on incompatible shape changes.
	SchemaVersion int `json:"schemaVersion"`
}

// LoadCursor reads the cursor file from path. Returns a zero Cursor when
// the file does not exist (fresh repo). Path is always caller-controlled
// (derived from the scope data directory), so the gosec variable-path
// warning does not represent a real injection risk here.
func LoadCursor(path string) (Cursor, error) {
	//nolint:gosec // path is caller-controlled, not user-supplied input
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Cursor{SchemaVersion: cursorSchemaVersion}, nil
	}
	if err != nil {
		return Cursor{}, fmt.Errorf("render: read cursor: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return Cursor{}, fmt.Errorf("render: parse cursor: %w", err)
	}
	return c, nil
}

// WriteCursor atomically writes cursor to path using a temp+rename pattern
// so a crash mid-write leaves the previous cursor intact.
func WriteCursor(path string, cursor Cursor) error {
	raw, err := json.MarshalIndent(cursor, "", "  ")
	if err != nil {
		return fmt.Errorf("render: marshal cursor: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-cursor-*")
	if err != nil {
		return fmt.Errorf("render: create temp cursor: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) } //nolint:errcheck

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("render: write temp cursor: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("render: chmod cursor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("render: close cursor: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("render: rename cursor: %w", err)
	}
	return nil
}
