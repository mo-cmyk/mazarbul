// Package tuitest provides shared test helpers for Mazarbul TUI snapshot and
// integration tests. It is strictly a test-support package; production TUI code
// must NOT import it.
//
// Three primitives:
//
//   - [LockRender]: forces monochrome (NO_COLOR=1) for deterministic golden
//     comparisons and restores the prior state on cleanup.
//   - [CompareGolden]: write-on-update via -update flag, else diff.
//   - [DriveKeys]: sends bubbletea KeyPressMsg values to a Model via Update
//     and collects the final view string.
//   - [StripANSI]: removes ANSI escape sequences from a string so golden
//     comparisons are not tripped up by terminal colour codes.
package tuitest

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// update is the -update flag consumed by [CompareGolden]. Running tests with
// -update writes the current output to the golden file instead of comparing.
//

var update = flag.Bool("update", false, "overwrite golden files with the current output")

// LockRender forces monochrome output for the duration of a test, then restores
// the prior state. All style accessors in internal/tui/style read
// os.Getenv("NO_COLOR") on every call, so no global lipgloss renderer state
// needs to be locked — this environment variable is the sole colour gate.
//
// Call at the top of any test that wants deterministic, colour-free View output:
//
//	restore := tuitest.LockRender(t)
//	defer restore()
//
// The returned func is idempotent; calling it more than once is safe.
func LockRender(t testing.TB) func() {
	t.Helper()
	prev, had := os.LookupEnv("NO_COLOR")
	if err := os.Setenv("NO_COLOR", "1"); err != nil {
		t.Fatalf("tuitest.LockRender: setenv NO_COLOR: %v", err)
	}
	var restored bool
	return func() {
		if restored {
			return
		}
		restored = true
		if had {
			if err := os.Setenv("NO_COLOR", prev); err != nil {
				t.Errorf("tuitest.LockRender restore: setenv NO_COLOR: %v", err)
			}
		} else {
			if err := os.Unsetenv("NO_COLOR"); err != nil {
				t.Errorf("tuitest.LockRender restore: unsetenv NO_COLOR: %v", err)
			}
		}
	}
}

// CompareGolden compares got against the golden file at path (relative to
// the test's working directory, typically internal/tui/<pkg>/testdata/).
//
// When the -update flag is provided the golden file is (over)written with got
// and the test always passes. Otherwise the file is read and diffed against
// got; any mismatch fails the test with a line-level diff.
//
// The golden file is stored as plain text; got should already have ANSI
// stripped if colour-free comparison is desired (use [StripANSI] before
// calling).
func CompareGolden(t testing.TB, path, got string) {
	t.Helper()
	if *update {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("CompareGolden: mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("CompareGolden: write golden %s: %v", path, err)
		}
		t.Logf("CompareGolden: wrote golden file %s", path)
		return
	}
	raw, err := os.ReadFile(path) //nolint:gosec // golden-file path is caller-controlled test infrastructure
	if err != nil {
		t.Fatalf("CompareGolden: read golden %s: %v\n(run with -update to create it)", path, err)
	}
	want := string(raw)
	if want == got {
		return
	}
	// Simple line-by-line diff on failure.
	t.Errorf("CompareGolden: %s mismatch:\n%s", path, lineDiff(want, got))
}

// DriveKeys applies each msg to m via Update, collecting Cmds, and returns the
// final Model (type-asserted to the concrete tea.Model type M) and the view
// string from the last state. It does not execute returned Cmds; interaction
// tests that need Cmd execution should run the full Program via teatest.
//
// Example — driving a watch.Model through a keypress:
//
//	m2, view := tuitest.DriveKeys[watch.Model](t, m, tea.KeyPressMsg{Code: '5', Text: "5"})
func DriveKeys[M tea.Model](t testing.TB, m M, msgs ...tea.Msg) (M, string) {
	t.Helper()
	var cur tea.Model = m
	for _, msg := range msgs {
		next, _ := cur.Update(msg)
		cur = next
	}
	concrete, ok := cur.(M)
	if !ok {
		t.Fatalf("DriveKeys: Update returned %T, want %T", cur, m)
	}
	return concrete, concrete.View().Content
}

// ansiRE matches all ANSI CSI escape sequences (colours, cursor movement, etc.)
// as well as OSC and single-char C1 sequences. Stripping these yields the plain
// rune content as a human sees it.
//

var ansiRE = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[A-Za-z]|\][^\x1b]*\x07|[^[])`)

// StripANSI removes ANSI terminal escape sequences from s. Use this before
// [CompareGolden] or string-Contains assertions when the view under test may
// emit colour codes even in test mode (e.g. glamour markdown renderer which
// uses its own colour table independent of NO_COLOR).
func StripANSI(s string) string {
	if !utf8.ValidString(s) {
		// If the string contains bad UTF-8 just return it unchanged;
		// the test failure will surface the real issue.
		return s
	}
	return ansiRE.ReplaceAllString(s, "")
}

// ── internal helpers ──────────────────────────────────────────────────────────

// diffHunk is a half-open line range [lo, hi) to render in a diff.
type diffHunk struct{ lo, hi int }

// lineDiff produces a compact unified-style diff between want and got, showing
// only changed lines with ±3 context lines. Not a full Myers diff; good enough
// for golden-file failure messages.
func lineDiff(want, got string) string {
	wLines := splitLines(want)
	gLines := splitLines(got)
	maxLine := max(len(wLines), len(gLines))
	mismatched := mismatchedIndices(wLines, gLines, maxLine)
	if len(mismatched) == 0 {
		return "(no line differences found — possible trailing newline or BOM mismatch)"
	}
	hunks := clusterHunks(mismatched, maxLine)
	return renderHunks(hunks, wLines, gLines)
}

// mismatchedIndices returns the line indices where wLines and gLines differ.
func mismatchedIndices(wLines, gLines []string, maxLine int) []int {
	var out []int
	for i := range maxLine {
		if lineAt(wLines, i) != lineAt(gLines, i) {
			out = append(out, i)
		}
	}
	return out
}

// clusterHunks merges mismatched indices (with ctx=3 padding) into non-overlapping hunks.
func clusterHunks(mismatched []int, maxLine int) []diffHunk {
	const ctx = 3
	var hunks []diffHunk
	for _, idx := range mismatched {
		lo := max(0, idx-ctx)
		hi := min(maxLine, idx+ctx+1)
		if len(hunks) > 0 && lo <= hunks[len(hunks)-1].hi {
			hunks[len(hunks)-1].hi = hi
		} else {
			hunks = append(hunks, diffHunk{lo, hi})
		}
	}
	return hunks
}

// renderHunks formats the hunk list as a minimal unified diff string.
func renderHunks(hunks []diffHunk, wLines, gLines []string) string {
	out := make([]byte, 0, 512)
	out = append(out, "--- want\n+++ got\n"...)
	for _, h := range hunks {
		out = appendHunk(out, h, wLines, gLines)
	}
	return string(out)
}

// appendHunk appends one hunk's lines to out and returns the extended slice.
func appendHunk(out []byte, h diffHunk, wLines, gLines []string) []byte {
	for i := h.lo; i < h.hi; i++ {
		wl, gl := lineAt(wLines, i), lineAt(gLines, i)
		if wl == gl {
			out = append(out, ' ')
			out = append(out, wl...)
		} else {
			out = append(out, '-')
			out = append(out, wl...)
			out = append(out, '\n')
			out = append(out, '+')
			out = append(out, gl...)
		}
		out = append(out, '\n')
	}
	return out
}

// lineAt returns lines[i] or "" when i is out of range.
func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i, ch := range s {
		if ch == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
