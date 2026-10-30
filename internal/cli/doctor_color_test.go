package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/doctor"
)

// TestRenderDoctorCLI_ColorEmitsEscapes verifies enabled=true wraps bullets
// in ANSI escapes while glyphs and check names remain readable.
func TestRenderDoctorCLI_ColorEmitsEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	results := []doctor.Result{
		{Name: "sqlite.open", Subsystem: "storage", Status: doctor.StatusOK, DurationMs: 5},
		{Name: "mcp.socket", Subsystem: "mcp", Status: doctor.StatusFail, Detail: "permission denied", DurationMs: 1},
	}
	var buf bytes.Buffer
	if err := renderDoctorCLI(&buf, results, true); err != nil {
		t.Fatalf("renderDoctorCLI color: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("color path must emit ANSI escapes: %q", out)
	}
	// Glyphs must still be present in the colour path (symbol-first).
	if !strings.Contains(out, "✓") {
		t.Errorf("expected ✓ glyph in colored output: %q", out)
	}
	if !strings.Contains(out, "✗") {
		t.Errorf("expected ✗ glyph in colored output: %q", out)
	}
	if !strings.Contains(out, "sqlite.open") {
		t.Errorf("expected check name in colored output: %q", out)
	}
}

// TestRenderDoctorNDJSON_ColorPathUnchanged ensures the NDJSON path is
// never affected by the enabled flag (NDJSON is always plain).
func TestRenderDoctorNDJSON_ColorPathUnchanged(t *testing.T) {
	t.Parallel()
	results := []doctor.Result{
		{Name: "sqlite.open", Subsystem: "storage", Status: doctor.StatusOK, DurationMs: 5},
	}
	var buf bytes.Buffer
	if err := renderDoctorNDJSON(&buf, results); err != nil {
		t.Fatalf("renderDoctorNDJSON: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("NDJSON path must never contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, `"name"`) {
		t.Errorf("NDJSON output missing name field: %q", out)
	}
}
