package tuitest_test

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/tuitest"
)

// ── LockRender ────────────────────────────────────────────────────────────────

func TestLockRender_SetsNOCOLOR(t *testing.T) {
	// Clear any inherited NO_COLOR so we can observe the transition.
	t.Setenv("NO_COLOR", "")
	os.Unsetenv("NO_COLOR")

	restore := tuitest.LockRender(t)
	if got := os.Getenv("NO_COLOR"); got != "1" {
		t.Errorf("LockRender: NO_COLOR = %q, want 1", got)
	}
	restore()
	if got := os.Getenv("NO_COLOR"); got != "" {
		t.Errorf("LockRender restore: NO_COLOR = %q, want empty", got)
	}
}

func TestLockRender_RestoresPriorValue(t *testing.T) {
	// Pre-set NO_COLOR to a non-empty value.
	t.Setenv("NO_COLOR", "prev")

	restore := tuitest.LockRender(t)
	if got := os.Getenv("NO_COLOR"); got != "1" {
		t.Errorf("LockRender: NO_COLOR = %q, want 1", got)
	}
	restore()
	if got := os.Getenv("NO_COLOR"); got != "prev" {
		t.Errorf("LockRender restore: NO_COLOR = %q, want prev", got)
	}
}

func TestLockRender_IdempotentRestore(t *testing.T) {
	restore := tuitest.LockRender(t)
	restore()
	// Second call must not panic or double-unset.
	restore()
}

// ── StripANSI ─────────────────────────────────────────────────────────────────

func TestStripANSI_RemovesColorCodes(t *testing.T) {
	// A string with a bold red colour + reset sequence.
	input := "\x1b[1;31mhello\x1b[0m world"
	want := "hello world"
	if got := tuitest.StripANSI(input); got != want {
		t.Errorf("StripANSI(%q) = %q, want %q", input, got, want)
	}
}

func TestStripANSI_PlainStringUnchanged(t *testing.T) {
	plain := "just plain text, no escapes"
	if got := tuitest.StripANSI(plain); got != plain {
		t.Errorf("StripANSI(plain) = %q, want %q", got, plain)
	}
}

func TestStripANSI_EmptyString(t *testing.T) {
	if got := tuitest.StripANSI(""); got != "" {
		t.Errorf("StripANSI(\"\") = %q, want \"\"", got)
	}
}

// ── CompareGolden ─────────────────────────────────────────────────────────────

func TestCompareGolden_WritesAndReads(t *testing.T) {
	// Write a golden file via -update semantics using a real temp file.
	dir := t.TempDir()
	path := dir + "/snap.golden.txt"
	want := "line one\nline two\n"

	// Simulate -update: write the file.
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatalf("setup: write golden: %v", err)
	}

	// CompareGolden should pass when content matches.
	tuitest.CompareGolden(t, path, want)
}

func TestCompareGolden_FailsOnMismatch(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/snap.golden.txt"
	if err := os.WriteFile(path, []byte("expected content\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Use a sub-test so we can capture the failure without failing the parent.
	failed := false
	ft := &fakeT{TB: t, failNow: func() { failed = true }}
	tuitest.CompareGolden(ft, path, "different content\n")
	if !failed && !ft.errored {
		t.Error("CompareGolden should have reported a mismatch but did not")
	}
}

// ── DriveKeys ─────────────────────────────────────────────────────────────────

// minimalModel is the smallest possible bubbletea model for testing DriveKeys.
type minimalModel struct {
	received []string
}

func (m minimalModel) Init() tea.Cmd { return nil }

func (m minimalModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if kp, ok := msg.(tea.KeyPressMsg); ok {
		m.received = append(m.received, kp.String())
	}
	return m, nil
}

func (m minimalModel) View() tea.View {
	v := tea.View{}
	v.SetContent("keys: " + strings.Join(m.received, ","))
	return v
}

func TestDriveKeys_AccumulatesMessages(t *testing.T) {
	m := minimalModel{}
	m2, view := tuitest.DriveKeys(t, m,
		tea.KeyPressMsg{Code: 'a', Text: "a"},
		tea.KeyPressMsg{Code: 'b', Text: "b"},
	)
	if len(m2.received) != 2 {
		t.Errorf("received %d keys, want 2", len(m2.received))
	}
	if !strings.Contains(view, "a") || !strings.Contains(view, "b") {
		t.Errorf("view %q missing expected keys", view)
	}
}

func TestDriveKeys_ZeroMessages(t *testing.T) {
	m := minimalModel{}
	m2, view := tuitest.DriveKeys(t, m)
	if len(m2.received) != 0 {
		t.Errorf("zero msgs: received %d keys, want 0", len(m2.received))
	}
	if !strings.Contains(view, "keys:") {
		t.Errorf("view %q should contain 'keys:'", view)
	}
}

// ── fakeT ─────────────────────────────────────────────────────────────────────

// fakeT captures Fatalf/Errorf calls so tests can assert on CompareGolden
// failure behaviour without the parent test failing.
type fakeT struct {
	testing.TB
	errored bool
	failNow func()
}

func (f *fakeT) Helper() {}

func (f *fakeT) Fatalf(_ string, _ ...any) {
	f.errored = true
	if f.failNow != nil {
		f.failNow()
	}
}

func (f *fakeT) Errorf(_ string, _ ...any) {
	f.errored = true
}
