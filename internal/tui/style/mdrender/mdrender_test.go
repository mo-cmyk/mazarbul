package mdrender

import (
	"bytes"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
)

// longParagraph is a 200-word paragraph used to trigger word wrapping.
const longParagraph = `Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor
incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud
exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat. Duis aute
irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla
pariatur. Excepteur sint occaecat cupidatat non proident, sunt in culpa qui officia
deserunt mollit anim id est laborum. Sed ut perspiciatis unde omnis iste natus error
sit voluptatem accusantium doloremque laudantium, totam rem aperiam, eaque ipsa quae
ab illo inventore veritatis et quasi architecto beatae vitae dicta sunt explicabo.`

func TestRenderHeadings(t *testing.T) {
	got := Render("# Hi\n\nbody", 80, true)
	if got == "" {
		t.Fatal("Render returned empty string")
	}
	if !strings.Contains(got, "Hi") {
		t.Errorf("rendered output does not contain heading text %q; got: %q", "Hi", got)
	}
}

func TestRenderForWriterNonTTY(t *testing.T) {
	input := "# Hi"
	got := RenderForWriter(&bytes.Buffer{}, input)
	if got != input {
		t.Errorf("RenderForWriter with non-TTY writer: want raw %q, got %q", input, got)
	}
}

func TestRenderForWriterNOCOLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	input := "# Hi"
	// Even if we somehow passed a TTY (we can't in tests), NO_COLOR must win.
	got := RenderForWriter(&bytes.Buffer{}, input)
	if got != input {
		t.Errorf("RenderForWriter with NO_COLOR: want raw %q, got %q", input, got)
	}
}

func TestRenderWidthCap(t *testing.T) {
	got := Render(longParagraph, 80, true)
	for _, line := range strings.Split(got, "\n") {
		w := xansi.StringWidth(line)
		if w > 80 {
			t.Errorf("line exceeds 80 visible cols (%d): %q", w, line)
		}
	}
}

func TestRenderWidthZeroUsesMax(t *testing.T) {
	// width <= 0 should silently use 80; must not panic or return empty.
	got := Render("# Hello\n\nbody text", 0, true)
	if got == "" {
		t.Fatal("Render with width=0 returned empty string")
	}
	for _, line := range strings.Split(got, "\n") {
		w := xansi.StringWidth(line)
		if w > 80 {
			t.Errorf("line exceeds 80 visible cols (%d): %q", w, line)
		}
	}
}

func TestRenderMalformedNoPanic(t *testing.T) {
	// Odd markdown constructs (no code fences — see NOTE below) must not panic.
	//
	// NOTE: code-fence inputs are intentionally excluded here.  Glamour's
	// CodeBlockElement.Render acquires a package-level sync.Mutex without a
	// defer Unlock.  If chroma.MustNewStyle panics (e.g. due to a style
	// registration race or invalid entry), the mutex is never released and any
	// subsequent code-block rendering deadlocks.  Our recover() correctly
	// catches such panics, but cannot un-lock a mutex held in a frame that
	// panicked.  Code-block rendering is tested separately in TestRenderCodeBlock
	// where a well-formed fence lets the chroma path succeed cleanly.
	inputs := []string{
		"**bold without close",
		"[link with no href]()",
		strings.Repeat("=", 200),
		"# " + strings.Repeat("A", 200),
		"\x00\x01\x02\x03",
	}
	for _, in := range inputs {
		got := Render(in, 80, true)
		_ = got // just confirm no panic
	}
}

func TestRenderCodeBlock(t *testing.T) {
	input := "```go\nfunc hello() {}\n```\n"
	got := Render(input, 80, true)
	if got == "" {
		t.Fatal("Render of valid code block returned empty string")
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("code block render missing content %q; got: %q", "hello", got)
	}
	// Guard against silent degradation: if glamour's code-block path panicked,
	// Render's recover would hand back the raw input verbatim. A real render
	// rewraps/indents the fence, so the output must differ from the input.
	if got == input {
		t.Fatal("code block render returned the raw input — glamour likely panicked and Render recovered")
	}
}

// TestRenderCodeBlockRepeated is the regression guard for the glamour
// MustNewStyle mutex-leak deadlock: the first code-block render used to panic
// (leaking glamour's package mutex) and every render after it deadlocked. By
// rendering several code blocks in a single process it would hang here if the
// bug returned — no -count needed.
func TestRenderCodeBlockRepeated(t *testing.T) {
	for i := range 5 {
		// Strip ANSI before the content check: chroma styles each token in its
		// own escape span, so "f()" is not a contiguous substring of the raw
		// output even though it renders correctly.
		got := xansi.Strip(Render("```go\nfunc f() {}\n```\n", 80, true))
		if !strings.Contains(got, "f()") {
			t.Fatalf("iteration %d: code block render missing content; got: %q", i, got)
		}
	}
}

func TestRenderLightTheme(t *testing.T) {
	got := Render("## Section\n\nbody", 80, false)
	if got == "" {
		t.Fatal("Render (light) returned empty string")
	}
	if !strings.Contains(got, "Section") {
		t.Errorf("light render does not contain heading text %q; got: %q", "Section", got)
	}
}
