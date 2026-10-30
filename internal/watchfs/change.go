package watchfs

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ChangeKind classifies a filesystem change. The values double as the
// changeKind provenance marker folded into a captured episode's source.
type ChangeKind string

const (
	// ChangeModified is a file create or write.
	ChangeModified ChangeKind = "file_change"
	// ChangeDeleted is a file removal or rename-away.
	ChangeDeleted ChangeKind = "file_delete"
)

// Change is a single debounced, ignore-filtered filesystem event ready
// to be turned into a memory episode. Content is the capped, text-only
// file body for ChangeModified; it is nil for ChangeDeleted.
type Change struct {
	// Root is the absolute watched root this change belongs to.
	Root string
	// Path is the absolute path of the changed file.
	Path string
	// RelPath is Path relative to Root, slash-separated, for display and
	// gitignore matching.
	RelPath string
	// Kind is the change classification.
	Kind ChangeKind
	// Content is the captured file body (capped at SizeCap). Nil for
	// deletes.
	Content []byte
	// Truncated reports that Content was cut at the size cap.
	Truncated bool
}

// ProjectName returns the watched root's base name. It is the
// human-facing project identifier folded into the episode source
// (fs-watch:<project>), mirroring the competitor's per-root session.
func (c *Change) ProjectName() string {
	return filepath.Base(c.Root)
}

// Source returns the provenance source string for this change, of the
// form "fs-watch:<project>:<changeKind>". The kind suffix keeps the
// changeKind discoverable without a dedicated event field.
func (c *Change) Source() string {
	return fmt.Sprintf("fs-watch:%s:%s", c.ProjectName(), c.Kind)
}

// RenderText builds the verbatim episode body for a change. For a
// modified file it is a one-line header followed by the captured
// content and, when capped, a truncation marker; for a delete it is a
// single sentence. The text is what recall indexes, so it is plain and
// substring-friendly.
func RenderText(c *Change) string {
	if c.Kind == ChangeDeleted {
		return fmt.Sprintf("File %s was deleted from %s.", c.RelPath, c.ProjectName())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "File %s was modified in %s:\n\n", c.RelPath, c.ProjectName())
	b.Write(c.Content)
	if c.Truncated {
		fmt.Fprintf(&b, "\n\n[truncated: showing first %d bytes]", len(c.Content))
	}
	return b.String()
}
