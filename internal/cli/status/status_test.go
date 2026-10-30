package status_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/cli/status"
)

// nonTTYPrinter returns a Printer backed by a plain buffer (never a TTY), so
// width always falls back to 60.
func nonTTYPrinter(enabled bool) *status.Printer {
	return status.New(&bytes.Buffer{}, enabled)
}

func TestDisabledOutputHasNoEscapes(t *testing.T) {
	p := nonTTYPrinter(false)

	cases := map[string]string{
		"IdentityBullet": p.IdentityBullet("init", "01ABC"),
		"SuccessBullet":  p.SuccessBullet("all good"),
		"FailureBullet":  p.FailureBullet("something failed"),
		"WarnBullet":     p.WarnBullet("watch out"),
		"SectionRule":    p.SectionRule("Sync"),
		"HorizontalRule": p.HorizontalRule(),
	}

	for name, out := range cases {
		if strings.Contains(out, "\x1b[") {
			t.Errorf("%s: disabled output contains ANSI escapes: %q", name, out)
		}
	}
}

func TestGlyphsSurviveWhenColorIsOff(t *testing.T) {
	p := nonTTYPrinter(false)

	if !strings.Contains(p.SuccessBullet("ok"), "✓") {
		t.Error("SuccessBullet: missing glyph ✓")
	}
	if !strings.Contains(p.FailureBullet("fail"), "✗") {
		t.Error("FailureBullet: missing glyph ✗")
	}
	if !strings.Contains(p.WarnBullet("warn"), "⚠") {
		t.Error("WarnBullet: missing glyph ⚠")
	}
	if !strings.Contains(p.IdentityBullet("label", "id"), "●") {
		t.Error("IdentityBullet: missing glyph ●")
	}
}

func TestHorizontalRuleWidthMatchesCap(t *testing.T) {
	p := nonTTYPrinter(false)
	rule := p.HorizontalRule()
	got := ansi.StringWidth(rule)
	if got != 60 {
		t.Errorf("HorizontalRule width = %d, want 60 (fallback cap)", got)
	}
}

func TestSectionRuleEmbedsLabel(t *testing.T) {
	p := nonTTYPrinter(false)
	rule := p.SectionRule("Sync")

	if !strings.Contains(rule, "Sync") {
		t.Errorf("SectionRule does not contain label %q: %q", "Sync", rule)
	}

	got := ansi.StringWidth(rule)
	if got != 60 {
		t.Errorf("SectionRule width = %d, want 60 (fallback cap)", got)
	}
}

func TestMetadataRowsAlignLabels(t *testing.T) {
	p := nonTTYPrinter(false)
	rows := [][2]string{
		{"scope", "local"},
		{"uid", "01ABC"},
	}
	out := p.MetadataRows(rows)
	lines := strings.Split(out, "\n")

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), out)
	}

	// Labels should appear padded (%-7s) at the start of each line.
	if !strings.HasPrefix(lines[0], "scope") {
		t.Errorf("line 0 should start with %q label, got %q", "scope", lines[0])
	}
	if !strings.HasPrefix(lines[1], "uid") {
		t.Errorf("line 1 should start with %q label, got %q", "uid", lines[1])
	}

	// Values should be present on their respective lines.
	if !strings.Contains(lines[0], "local") {
		t.Errorf("line 0 missing value %q: %q", "local", lines[0])
	}
	if !strings.Contains(lines[1], "01ABC") {
		t.Errorf("line 1 missing value %q: %q", "01ABC", lines[1])
	}

	// Labels are padded to the column width (7 chars) so there must be
	// trailing spaces between the label text and the value separator.
	// "scope" (5) → "scope  " (7) → "scope   local" (3 spaces total with sep).
	if !strings.Contains(lines[0], "scope  ") {
		t.Errorf("line 0 label not padded to 7 chars: %q", lines[0])
	}
}

func TestSectionRuleWithColorEnabled(t *testing.T) {
	// Even with colour enabled the visible width must match the cached cap
	// because ansi.StringWidth strips ANSI escape sequences.
	p := nonTTYPrinter(true)
	rule := p.SectionRule("Status")
	got := ansi.StringWidth(rule)
	if got != 60 {
		t.Errorf("SectionRule (colour on) width = %d, want 60", got)
	}
	if !strings.Contains(rule, "Status") {
		t.Errorf("SectionRule (colour on) does not contain label: %q", rule)
	}
}

func TestHorizontalRuleWithColorEnabled(t *testing.T) {
	p := nonTTYPrinter(true)
	rule := p.HorizontalRule()
	got := ansi.StringWidth(rule)
	if got != 60 {
		t.Errorf("HorizontalRule (colour on) width = %d, want 60", got)
	}
}
