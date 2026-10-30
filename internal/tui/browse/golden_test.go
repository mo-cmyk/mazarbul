package browse

// golden_test.go — golden snapshot tests and tuitest-driven interaction tests
// for the browse TUI (recall strip and doc-browse modes).
//
// Regenerate goldens:
//
//	go test -tags=sqlite_fts5 -run TestGolden ./internal/tui/browse/ -update

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/tui/tuitest"
)

// ── doc-browse fake loader ────────────────────────────────────────────────────

// fakeDocLoader simulates a docBrowseLoader for golden tests.  It returns
// document-style records and a detail body containing Links + Linked-from
// sections with both resolved (→) and dangling (⚠) entries.
type fakeDocLoader struct{}

func (f *fakeDocLoader) Search(_ string, _ Filters) ([]Record, error) {
	return []Record{
		{
			ID: "vault/erebor-kickoff.md", Title: "Erebor migration kickoff",
			Subtitle: "vault · ★ pinned", Kind: "document",
		},
		{
			ID: "vault/erebor-roadmap.md", Title: "Erebor roadmap",
			Subtitle: "vault", Kind: "document",
		},
		{
			ID: "vault/bree-brief.md", Title: "Bree Ltd. brief",
			Subtitle: "vault", Kind: "document",
		},
	}, nil
}

func (f *fakeDocLoader) Detail(id string) (string, error) {
	if id == "vault/erebor-kickoff.md" {
		return "# Erebor migration kickoff\n\n" +
			"We aligned on moving the RFC integration.\n" +
			"Sam owns the rollout.\n\n" +
			"## Links\n\n" +
			"- → Ring Mk 4000 / RFC 1149\n" +
			"- ⚠ Project Eriador\n\n" +
			"## Linked from\n\n" +
			"- ← Erebor roadmap\n", nil
	}
	return "# " + id + "\n\n(no body)", nil
}

func (f *fakeDocLoader) Neighbors(id string) ([]Record, error) {
	if id == "vault/erebor-kickoff.md" {
		return []Record{
			{ID: "vault/erebor-roadmap.md", Title: "Erebor roadmap", Kind: "document"},
		}, nil
	}
	return nil, nil
}

// ── recall-strip fake loader (reuses fakeRecords from browse_test.go) ─────────

// browseGoldenBanks returns the recall filter-strip banks used for golden tests.
func browseGoldenBanks() []Bank {
	return []Bank{
		{
			Label: "Kind", Kind: BankToggle,
			Options: []string{"person", "org", "place"}, Key: "kinds",
		},
		{
			Label: "Scope", Kind: BankToggle,
			Options: []string{"personal", "project"}, Key: "scopes",
		},
	}
}

// ── golden helpers ────────────────────────────────────────────────────────────

// makeRecallModel returns a browse model wired for recall (fakeLoader + strip).
func makeRecallModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	m := New(&fakeLoader{}, fakeRecords, true).WithFilterStrip(browseGoldenBanks())
	m.width = 100
	m.height = 30
	return m
}

// makeDocModel returns a browse model wired for doc-browse (fakeDocLoader).
func makeDocModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	recs, _ := (&fakeDocLoader{}).Search("", Filters{})
	m := New(&fakeDocLoader{}, recs, true)
	m.width = 100
	m.height = 30
	return m
}

// ── golden snapshots ──────────────────────────────────────────────────────────

// TestGolden_RecallListView captures the recall browse list with all fakeRecords
// visible and the filter strip pills in their default (un-toggled) state.
func TestGolden_RecallListView(t *testing.T) {
	restore := tuitest.LockRender(t)
	defer restore()

	view := tuitest.StripANSI(makeRecallModel(t).View().Content)
	tuitest.CompareGolden(t, "testdata/recall_list.golden.txt", view)
}

// TestGolden_RecallStripToggled captures the filter strip after toggling
// Kind=person and Kind=org and Scope=personal, to show the pill-selected state.
func TestGolden_RecallStripToggled(t *testing.T) {
	restore := tuitest.LockRender(t)
	defer restore()

	m := makeRecallModel(t)

	// Focus strip → toggle "person" (idx 0) → right → toggle "org" (idx 1)
	// → tab to Scope bank → toggle "personal" (idx 0).
	m, _ = tuitest.DriveKeys(t, m,
		tea.KeyPressMsg{Code: tea.KeyTab},   // focus strip
		tea.KeyPressMsg{Code: tea.KeySpace}, // toggle person
		tea.KeyPressMsg{Code: tea.KeyRight}, // → org
		tea.KeyPressMsg{Code: tea.KeySpace}, // toggle org
		tea.KeyPressMsg{Code: tea.KeyTab},   // next bank: Scope
		tea.KeyPressMsg{Code: tea.KeySpace}, // toggle personal
	)

	view := tuitest.StripANSI(m.View().Content)
	tuitest.CompareGolden(t, "testdata/recall_strip_toggled.golden.txt", view)
}

// TestGolden_RecallDetailView captures the detail card for Frodo Baggins.
// Detail view uses glamour which emits ANSI even under NO_COLOR; strip before
// comparison so the golden is colour-independent.
func TestGolden_RecallDetailView(t *testing.T) {
	restore := tuitest.LockRender(t)
	defer restore()

	m := makeRecallModel(t)
	// Enter opens detail for the first record (Frodo, id-1).
	m, _ = tuitest.DriveKeys(t, m,
		tea.KeyPressMsg{Code: tea.KeyEnter},
	)

	view := tuitest.StripANSI(m.View().Content)
	tuitest.CompareGolden(t, "testdata/recall_detail.golden.txt", view)
}

// TestGolden_DocListView captures the doc-browse list with document records.
func TestGolden_DocListView(t *testing.T) {
	restore := tuitest.LockRender(t)
	defer restore()

	view := tuitest.StripANSI(makeDocModel(t).View().Content)
	tuitest.CompareGolden(t, "testdata/doc_list.golden.txt", view)
}

// TestGolden_DocDetailView captures the detail card for the Erebor kickoff doc,
// which includes resolved (→) and dangling (⚠) link lines and a Linked-from
// section.  ANSI stripped for colour-independent comparison.
func TestGolden_DocDetailView(t *testing.T) {
	restore := tuitest.LockRender(t)
	defer restore()

	m := makeDocModel(t)
	// Enter opens detail for the first doc ("Erebor migration kickoff").
	m, _ = tuitest.DriveKeys(t, m,
		tea.KeyPressMsg{Code: tea.KeyEnter},
	)

	view := tuitest.StripANSI(m.View().Content)
	tuitest.CompareGolden(t, "testdata/doc_detail.golden.txt", view)
}

// ── interaction tests ─────────────────────────────────────────────────────────

// TestInteraction_RecallTabStripToggle drives Tab to focus the filter strip,
// Space to toggle the first Kind pill, and asserts the filter state is updated
// and the view still renders correctly.
func TestInteraction_RecallTabStripToggle(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := makeRecallModel(t)

	// Focus strip.
	m2, _ := tuitest.DriveKeys(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if !m2.strip.Focused() {
		t.Fatal("strip should be focused after Tab")
	}

	// Toggle first pill (person).
	m3, view := tuitest.DriveKeys(t, m2, tea.KeyPressMsg{Code: tea.KeySpace})
	f := m3.strip.Filters()
	if len(f.Kinds) != 1 || f.Kinds[0] != "person" {
		t.Errorf("after toggle, Kinds = %v, want [person]", f.Kinds)
	}
	plain := tuitest.StripANSI(view)
	// The filter strip section should still be visible.
	if !strings.Contains(plain, "filters") {
		t.Errorf("view missing 'filters' box after toggle:\n%s", plain)
	}
}

// TestInteraction_RecallJKEnterDetail drives j to move the cursor from Frodo to
// Gandalf, then Enter to open the detail, and asserts the detail card shows
// Gandalf's content.
func TestInteraction_RecallJKEnterDetail(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := makeRecallModel(t)

	// j → moves cursor to Gandalf (index 1); Enter → opens detail.
	m2, _ := tuitest.DriveKeys(t, m,
		tea.KeyPressMsg{Code: 'j', Text: "j"},
		tea.KeyPressMsg{Code: tea.KeyEnter},
	)

	if m2.mode != modeDetail {
		t.Fatalf("after j+Enter, mode = %d, want modeDetail (%d)", m2.mode, modeDetail)
	}
	if m2.detailID != "id-2" {
		t.Errorf("detailID = %q, want id-2 (Gandalf)", m2.detailID)
	}
	plain := ansi.Strip(m2.View().Content)
	if !strings.Contains(plain, "Gandalf") {
		t.Errorf("detail view missing 'Gandalf' after j+Enter:\n%s", plain)
	}
}

// TestInteraction_DocBrowseEnterDetail drives Enter on the first doc record and
// asserts the detail view shows the resolved link (→) and dangling link (⚠).
func TestInteraction_DocBrowseEnterDetail(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := makeDocModel(t)

	m2, _ := tuitest.DriveKeys(t, m,
		tea.KeyPressMsg{Code: tea.KeyEnter},
	)

	if m2.mode != modeDetail {
		t.Fatalf("after Enter, mode = %d, want modeDetail (%d)", m2.mode, modeDetail)
	}
	if m2.detailID != "vault/erebor-kickoff.md" {
		t.Errorf("detailID = %q, want vault/erebor-kickoff.md", m2.detailID)
	}
	// The detail body contains the Links section with resolved and dangling entries.
	// Strip ANSI because glamour adds colour codes even under NO_COLOR.
	plain := ansi.Strip(m2.View().Content)
	for _, want := range []string{"→ Ring Mk 4000", "⚠ Project Eriador", "← Erebor roadmap"} {
		if !strings.Contains(plain, want) {
			t.Errorf("detail view missing link line %q:\n%s", want, plain)
		}
	}
}
