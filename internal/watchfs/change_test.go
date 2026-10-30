package watchfs

import (
	"strings"
	"testing"
)

func TestChangeSourceAndProjectName(t *testing.T) {
	c := Change{Root: "/home/me/myrepo", Kind: ChangeModified}
	if got, want := c.ProjectName(), "myrepo"; got != want {
		t.Errorf("ProjectName() = %q, want %q", got, want)
	}
	if got, want := c.Source(), "fs-watch:myrepo:file_change"; got != want {
		t.Errorf("Source() = %q, want %q", got, want)
	}
	del := Change{Root: "/home/me/myrepo", Kind: ChangeDeleted}
	if got, want := del.Source(), "fs-watch:myrepo:file_delete"; got != want {
		t.Errorf("Source() = %q, want %q", got, want)
	}
}

func TestRenderTextModified(t *testing.T) {
	c := Change{
		Root:    "/home/me/myrepo",
		RelPath: "docs/notes.md",
		Kind:    ChangeModified,
		Content: []byte("Alice prefers dark mode"),
	}
	got := RenderText(&c)
	if !strings.Contains(got, "docs/notes.md") {
		t.Errorf("rendered text missing rel path: %q", got)
	}
	if !strings.Contains(got, "myrepo") {
		t.Errorf("rendered text missing project: %q", got)
	}
	if !strings.Contains(got, "Alice prefers dark mode") {
		t.Errorf("rendered text missing content: %q", got)
	}
	if strings.Contains(got, "truncated") {
		t.Errorf("non-truncated change should not carry a marker: %q", got)
	}
}

func TestRenderTextTruncatedMarker(t *testing.T) {
	c := Change{
		Root:      "/home/me/myrepo",
		RelPath:   "big.txt",
		Kind:      ChangeModified,
		Content:   []byte("0123456789"),
		Truncated: true,
	}
	got := RenderText(&c)
	if !strings.Contains(got, "[truncated: showing first 10 bytes]") {
		t.Errorf("truncated change missing marker: %q", got)
	}
}

func TestRenderTextDeleted(t *testing.T) {
	c := Change{Root: "/home/me/myrepo", RelPath: "gone.txt", Kind: ChangeDeleted}
	got := RenderText(&c)
	if !strings.Contains(got, "gone.txt") || !strings.Contains(got, "deleted") {
		t.Errorf("deleted render unexpected: %q", got)
	}
}
