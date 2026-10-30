package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

func testLogEvent() kernel.Event {
	return kernel.Event{
		UID:        "01HZTEST00000000000000ULID",
		Kind:       "episode",
		CreateTime: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Payload:    []byte(`{"text":"hello"}`),
	}
}

// TestWriteLogLine_PlainNoEscapes verifies that a disabled colorizer
// produces no ANSI escapes while all columns are still present.
func TestWriteLogLine_PlainNoEscapes(t *testing.T) {
	t.Parallel()
	ev := testLogEvent()
	clr := colorizer.New(false, io.Discard)
	var buf bytes.Buffer
	if err := writeLogLine(&buf, &ev, false, clr); err != nil {
		t.Fatalf("writeLogLine: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain path must not contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "01HZTEST00000000000000ULID") {
		t.Errorf("expected UID in output: %q", out)
	}
	if !strings.Contains(out, "episode") {
		t.Errorf("expected kind in output: %q", out)
	}
	if !strings.Contains(out, "2026-06-01T12:00:00Z") {
		t.Errorf("expected timestamp in output: %q", out)
	}
}

// TestWriteLogLine_JSONModeNoEscapes verifies that JSON mode never
// emits ANSI escapes even when the colorizer is enabled.
func TestWriteLogLine_JSONModeNoEscapes(t *testing.T) {
	t.Parallel()
	ev := testLogEvent()
	// color enabled but JSON mode must suppress all escapes
	clr := colorizer.New(true, io.Discard)
	var buf bytes.Buffer
	if err := writeLogLine(&buf, &ev, true /* jsonMode */, clr); err != nil {
		t.Fatalf("writeLogLine json mode: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("JSON mode must not contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, `"uid"`) {
		t.Errorf("JSON output must contain uid field: %q", out)
	}
}

// TestLogVerbColor_RemovedTakesPriority guards the precedence bug where the
// "relation" prefix in logIsAddedKind also matches "relationRetracted": a
// retraction must paint Removed (red), not Added (green). It also confirms the
// plain relation/assertion kinds still read as Added.
func TestLogVerbColor_RemovedTakesPriority(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	clr := colorizer.New(true, io.Discard)
	const sample = "x"
	cases := []struct {
		kind kernel.EventKind
		want string // the colorizer method the kind must resolve to
	}{
		{"relationRetracted", clr.Removed(sample)}, // regression: not Added
		{"episodeForgotten", clr.Removed(sample)},
		{"relation", clr.Added(sample)},
		{"relationCandidatesProposed", clr.Added(sample)},
		{"entityAsserted", clr.Added(sample)},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := logVerbColor(clr, tc.kind)(sample); got != tc.want {
				t.Errorf("logVerbColor(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

// TestLogAuthorDisplay_EmptyUIDIsUnsigned guards the regression where an event
// predating the EventAuthor field (PrincipalUID == "") rendered as "✓ " — a
// success glyph with an empty label — implying a verified signer. Empty and the
// "local" sentinel must both read as unsigned (‼ local).
func TestLogAuthorDisplay_EmptyUIDIsUnsigned(t *testing.T) {
	t.Parallel()
	clr := colorizer.New(false, io.Discard) // disabled: assert on plain glyph + label
	cases := []struct {
		name string
		uid  string
		want string
	}{
		{"empty uid", "", "‼ local"},
		{"local sentinel", "local", "‼ local"},
		{"signed principal", "01HXYZPRINCIPAL", "✓ 01HXYZPRINCI"}, // abbreviated to 12 runes
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := logAuthorDisplay(clr, kernel.EventAuthor{PrincipalUID: tc.uid})
			if got != tc.want {
				t.Errorf("logAuthorDisplay(uid=%q) = %q, want %q", tc.uid, got, tc.want)
			}
		})
	}
}

// TestWriteLogLine_ColorEmitsEscapes verifies that an enabled colorizer
// wraps the columns in ANSI escapes while the text remains.
func TestWriteLogLine_ColorEmitsEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	ev := testLogEvent()
	clr := colorizer.New(true, io.Discard)
	var buf bytes.Buffer
	if err := writeLogLine(&buf, &ev, false, clr); err != nil {
		t.Fatalf("writeLogLine color: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("color path must emit ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "01HZTEST00000000000000ULID") {
		t.Errorf("UID must still appear in colored output: %q", out)
	}
	if !strings.Contains(out, "episode") {
		t.Errorf("kind must still appear in colored output: %q", out)
	}
}
