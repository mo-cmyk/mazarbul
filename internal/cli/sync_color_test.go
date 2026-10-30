package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// TestRenderSyncStatus_PlainDisabledNoEscapes asserts that enabled=false
// produces +N / -N signs and no ANSI escapes.
func TestRenderSyncStatus_PlainDisabledNoEscapes(t *testing.T) {
	t.Parallel()
	rows := []syncScopeReport{
		{Scope: "personal", Mode: "plaintext", LocalCount: 3, RemoteCount: 5, Ahead: 2, Behind: 1},
	}
	var buf bytes.Buffer
	if err := renderSyncStatus(&buf, rows, "", false, output.FormatText); err != nil {
		t.Fatalf("renderSyncStatus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain path must not contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "+2") {
		t.Errorf("expected ahead +2 in output: %q", out)
	}
	if !strings.Contains(out, "-1") {
		t.Errorf("expected behind -1 in output: %q", out)
	}
}

// TestRenderSyncStatus_ColorEnabledEmitsEscapes asserts that enabled=true
// wraps +N / -N in ANSI escapes while still including the sign text.
func TestRenderSyncStatus_ColorEnabledEmitsEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	rows := []syncScopeReport{
		{Scope: "personal", Mode: "plaintext", LocalCount: 3, RemoteCount: 5, Ahead: 2, Behind: 1},
	}
	var buf bytes.Buffer
	if err := renderSyncStatus(&buf, rows, "", true, output.FormatText); err != nil {
		t.Fatalf("renderSyncStatus color: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("color path must emit ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "+2") {
		t.Errorf("colored output must still contain +2: %q", out)
	}
	if !strings.Contains(out, "-1") {
		t.Errorf("colored output must still contain -1: %q", out)
	}
}

// TestRenderSyncStatus_ColorStripEqualsPlain guards the colored-cell alignment
// regression: padding (%-6s) must be applied to the plain value BEFORE colour,
// so stripping the ANSI from the colored render yields a byte-identical layout
// to the disabled render. Before the fix, the ahead/behind/remote columns lost
// their trailing padding when colored because the escape bytes were counted as
// visible width.
func TestRenderSyncStatus_ColorStripEqualsPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	rows := []syncScopeReport{
		{Scope: "personal", Mode: "plaintext", LocalCount: 3, RemoteCount: 5, Ahead: 2, Behind: 1},
		{Scope: "project", Mode: "encrypted", LocalCount: 10, RemoteCount: 10, Ahead: 0, Behind: 0},
		{Scope: "team", Mode: "plaintext", LocalCount: 1, RemoteErr: "boom"},
	}
	var plain, colored bytes.Buffer
	if err := renderSyncStatus(&plain, rows, "", false, output.FormatText); err != nil {
		t.Fatalf("renderSyncStatus plain: %v", err)
	}
	if err := renderSyncStatus(&colored, rows, "", true, output.FormatText); err != nil {
		t.Fatalf("renderSyncStatus colored: %v", err)
	}
	if got := ansi.Strip(colored.String()); got != plain.String() {
		t.Errorf("ANSI-stripped colored output must equal plain output (alignment regression)\nplain:\n%q\nstripped:\n%q", plain.String(), got)
	}
}

// TestRenderSyncStatus_NoRemotes uses the WarnBullet path and must not
// include ANSI escapes when disabled.
func TestRenderSyncStatus_NoRemotes_PlainNoEscapes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderSyncStatus(&buf, nil, "", false, output.FormatText); err != nil {
		t.Fatalf("renderSyncStatus empty: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain no-remotes path must not contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "⚠") {
		t.Errorf("no-remotes path must include ⚠ glyph: %q", out)
	}
	if !strings.Contains(out, "no sync remotes configured") {
		t.Errorf("expected 'no sync remotes configured' message: %q", out)
	}
}

// TestWriteChangelogText_PlainNoEscapes asserts no escapes when colorizer
// is disabled and commit hash / device appear in the output.
func TestWriteChangelogText_PlainNoEscapes(t *testing.T) {
	t.Parallel()
	entries := []changelogEntry{
		{CommitHash: "abc1234", Date: "2026-06-01 10:00", Device: "device-a", Scope: "personal", Summary: "test push"},
	}
	clr := colorizer.New(false, io.Discard)
	var buf bytes.Buffer
	if err := writeChangelogText(&buf, entries, clr); err != nil {
		t.Fatalf("writeChangelogText: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain path must not contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "abc1234") {
		t.Errorf("expected commit hash in output: %q", out)
	}
	if !strings.Contains(out, "device-a") {
		t.Errorf("expected device in output: %q", out)
	}
	if !strings.Contains(out, "test push") {
		t.Errorf("expected summary in output: %q", out)
	}
}

// TestWriteChangelogText_EmptyPrints placeholder message.
func TestWriteChangelogText_EmptyPrints(t *testing.T) {
	t.Parallel()
	clr := colorizer.New(false, io.Discard)
	var buf bytes.Buffer
	if err := writeChangelogText(&buf, nil, clr); err != nil {
		t.Fatalf("writeChangelogText empty: %v", err)
	}
	if !strings.Contains(buf.String(), "no changelog entries found") {
		t.Errorf("expected empty-list placeholder: %q", buf.String())
	}
}
