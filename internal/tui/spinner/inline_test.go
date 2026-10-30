package spinner_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/tui/spinner"
)

func TestInlineDisabledDonePrintsCheckAndMessageOnly(t *testing.T) {
	var buf bytes.Buffer
	s := spinner.NewInline(&buf, "build", false)
	s.Done("built")

	got := buf.String()
	if got != "✓ built\n" {
		t.Fatalf("expected %q, got %q", "✓ built\n", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("unexpected escape sequence in disabled output: %q", got)
	}
}

func TestInlineEnabledLifecycleDoesNotPanic(t *testing.T) {
	var buf bytes.Buffer
	s := spinner.NewInline(&buf, "x", true)
	s.Start()
	s.Done("ok")

	if !strings.Contains(buf.String(), "ok") {
		t.Fatalf("expected output to contain %q, got %q", "ok", buf.String())
	}
}
