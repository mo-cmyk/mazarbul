package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
)

// TestEntityRow_PlainHasGlyphsNoEscapes asserts that disabled colorizer
// produces status glyphs but no ANSI escapes.
func TestEntityRow_PlainHasGlyphsNoEscapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status    string
		wantGlyph string
	}{
		{"confirmed", "✓"},
		{"split_candidate", "⚠"},
		{"tentative", "●"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			r := entityRow{
				UID:         "01HZTEST00000000000000ENTT",
				Kind:        "person",
				DisplayName: "Frodo Baggins",
				Confidence:  0.95,
				Status:      tc.status,
				clr:         colorizer.New(false, io.Discard),
			}
			var buf bytes.Buffer
			if err := r.RenderText(&buf); err != nil {
				t.Fatalf("RenderText: %v", err)
			}
			out := buf.String()
			if strings.Contains(out, "\x1b[") {
				t.Errorf("plain path must not contain ANSI escapes: %q", out)
			}
			if !strings.Contains(out, tc.wantGlyph) {
				t.Errorf("expected glyph %q for status %q: %q", tc.wantGlyph, tc.status, out)
			}
			if !strings.Contains(out, "person") {
				t.Errorf("expected kind in output: %q", out)
			}
			if !strings.Contains(out, "Frodo Baggins") {
				t.Errorf("expected display name in output: %q", out)
			}
		})
	}
}

// TestEntityRow_ColorEmitsEscapes verifies ANSI escapes are emitted
// when the colorizer is enabled.
func TestEntityRow_ColorEmitsEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	r := entityRow{
		UID:         "01HZTEST00000000000000ENTT",
		Kind:        "person",
		DisplayName: "Frodo Baggins",
		Confidence:  0.95,
		Status:      "confirmed",
		clr:         colorizer.New(true, io.Discard),
	}
	var buf bytes.Buffer
	if err := r.RenderText(&buf); err != nil {
		t.Fatalf("RenderText color: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("color path must emit ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("glyph must appear in colored output: %q", out)
	}
}

// TestEntityRow_JSONExcludesClrField verifies the unexported clr field
// does not leak into JSON serialisation and no ANSI escapes appear in
// marshalled values.
func TestEntityRow_JSONExcludesClrField(t *testing.T) {
	t.Parallel()
	r := entityRow{
		UID:         "01HZTEST00000000000000JSON",
		Kind:        "person",
		DisplayName: "Test Person",
		Confidence:  0.99,
		Status:      "confirmed",
		clr:         colorizer.New(true, io.Discard),
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	out := string(b)
	if strings.Contains(out, "clr") {
		t.Errorf("clr field must not appear in JSON: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("ANSI escapes must not appear in JSON values: %q", out)
	}
	if !strings.Contains(out, `"uid"`) {
		t.Errorf("uid field missing from JSON: %q", out)
	}
}

// TestRelationRow_PlainHasGlyphsNoEscapes covers the open/closed glyphs.
func TestRelationRow_PlainHasGlyphsNoEscapes(t *testing.T) {
	t.Parallel()
	// open relation (valid_until null)
	open := relationRow{
		UID:        "01HZTEST00000000000000REL1",
		SubjectUID: "subj",
		Predicate:  "knows",
		ObjectUID:  "obj",
		Confidence: 0.9,
		clr:        colorizer.New(false, io.Discard),
	}
	var buf bytes.Buffer
	if err := open.RenderText(&buf); err != nil {
		t.Fatalf("RenderText open: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain open relation must not contain escapes: %q", out)
	}
	if !strings.Contains(out, "●") {
		t.Errorf("expected ● glyph for open relation: %q", out)
	}
	if !strings.Contains(out, "knows") {
		t.Errorf("expected predicate in output: %q", out)
	}
}

// TestEpisodeRow_PlainHasIdentifierNoEscapes covers episodeRow.RenderText.
func TestEpisodeRow_PlainHasIdentifierNoEscapes(t *testing.T) {
	t.Parallel()
	r := episodeRow{
		UID:        "01HZTEST00000000000000EPI1",
		CreateTime: "2026-06-01T12:00:00Z",
		Source:     "claude-code",
		Text:       "hello world",
		clr:        colorizer.New(false, io.Discard),
	}
	var buf bytes.Buffer
	if err := r.RenderText(&buf); err != nil {
		t.Fatalf("RenderText episode: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain episode must not contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "01HZTEST00000000000000EPI1") {
		t.Errorf("expected UID in output: %q", out)
	}
	if !strings.Contains(out, "claude-code") {
		t.Errorf("expected source in output: %q", out)
	}
}
