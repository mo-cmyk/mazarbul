package browse

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// ── fake loaders ───────────────────────────────────────────────────────────────

var fakeRecords = []Record{
	{ID: "id-1", Title: "Frodo Baggins", Subtitle: "person · shire/hobbit · ⎇ main · Tolkien", Kind: "person"},
	{ID: "id-2", Title: "Gandalf", Subtitle: "person · wizard/grey · ⎇ main · Tolkien", Kind: "person"},
	{ID: "id-3", Title: "Shire", Subtitle: "place · eriador/northwest · ⎇ main · Tolkien", Kind: "place"},
}

var fakeDetails = map[string]string{
	"id-1": "# Frodo Baggins\n\nA hobbit of the Shire who carried the One Ring.",
	"id-2": "# Gandalf\n\nA wizard of great wisdom and power.",
	"id-3": "# Shire\n\nA peaceful land in the northwest of Middle-earth.",
}

var fakeNeighbors = map[string][]Record{
	"id-1": {fakeRecords[1], fakeRecords[2]},
	"id-2": {fakeRecords[0]},
	"id-3": {fakeRecords[0], fakeRecords[1]},
}

// fakeLoader is the base recall-style loader. Implements Loader only.
type fakeLoader struct{}

func (f *fakeLoader) Search(_ string, _ Filters) ([]Record, error) { return fakeRecords, nil }
func (f *fakeLoader) Detail(id string) (string, error)             { return fakeDetails[id], nil }
func (f *fakeLoader) Neighbors(id string) ([]Record, error) {
	return fakeNeighbors[id], nil
}

// fakeEditorLoader implements Loader + Editor so e/n keys are active.
type fakeEditorLoader struct {
	editCalled bool
	newCalled  bool
	lastEditID string
}

func (f *fakeEditorLoader) Search(_ string, _ Filters) ([]Record, error) { return fakeRecords, nil }
func (f *fakeEditorLoader) Detail(id string) (string, error)             { return fakeDetails[id], nil }
func (f *fakeEditorLoader) Neighbors(id string) ([]Record, error) {
	return fakeNeighbors[id], nil
}

func (f *fakeEditorLoader) Edit(id string) tea.Cmd {
	f.editCalled = true
	f.lastEditID = id
	return func() tea.Msg { return nil }
}

func (f *fakeEditorLoader) New() tea.Cmd {
	f.newCalled = true
	return func() tea.Msg { return nil }
}

// ── helpers ────────────────────────────────────────────────────────────────────

// newTestModel returns a Model with NO_COLOR set, width 80, height 24.
func newTestModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	m := New(&fakeLoader{}, fakeRecords, true)
	m.width = 80
	m.height = 24
	return m
}

// pressKey drives one Update and returns the updated Model via type assertion.
func pressKey(t *testing.T, m Model, msg tea.KeyPressMsg) Model {
	t.Helper()
	updated, _ := m.Update(msg)
	return updated.(Model)
}

// ── existing tests (updated for new Search signature) ─────────────────────────

func TestBrowse_ListsRecords(t *testing.T) {
	m := newTestModel(t)

	content := m.View().Content
	for _, rec := range fakeRecords {
		if !strings.Contains(content, rec.Title) {
			t.Errorf("View() missing record title %q; got:\n%s", rec.Title, content)
		}
	}
}

func TestBrowse_EnterOpensDetail(t *testing.T) {
	m := newTestModel(t)

	m2 := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m2.mode != modeDetail {
		t.Fatalf("after enter, mode = %d, want modeDetail (%d)", m2.mode, modeDetail)
	}

	// mdrender.Render applies glamour styling even with NO_COLOR set (the palette
	// is hardcoded in the renderer, not via style accessors), so heading words can
	// be split across ANSI reset runs.  Strip ANSI before the substring check.
	plain := ansi.Strip(m2.View().Content)
	if !strings.Contains(plain, "Frodo Baggins") {
		t.Errorf("detail view missing 'Frodo Baggins'; got (plain):\n%s", plain)
	}
}

// TestBrowse_FooterNotDoubled guards the two layout regressions the visual
// pass found: the master list rendered the picker's help line AND browse's own
// footer (a doubled help line), and the neighbor picker leaked its full help
// line into the detail column. "next page" is a help desc that occurs exactly
// once per rendered help line, so its count pins down both bugs.
func TestBrowse_FooterNotDoubled(t *testing.T) {
	m := newTestModel(t)

	master := ansi.Strip(m.View().Content)
	// The composed browse footer carries exactly one "enter open"; a doubled
	// footer would repeat it. The picker's own help line (which would say
	// "next page") must never leak into the master view.
	if got := strings.Count(master, "enter open"); got != 1 {
		t.Errorf("master view footer count = %d, want 1 (doubled footer regression);\n%s", got, master)
	}
	if got := strings.Count(master, "next page"); got != 0 {
		t.Errorf("picker help leaked into master footer (count %d, want 0);\n%s", got, master)
	}

	// Detail mode shows only an "esc: back" hint; the neighbor picker must not
	// render its own help line, so the browse footer is absent entirely.
	detail := ansi.Strip(pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}).View().Content)
	if got := strings.Count(detail, "enter open"); got != 0 {
		t.Errorf("detail view footer count = %d, want 0 (neighbor footer leak regression);\n%s", got, detail)
	}
	if !strings.Contains(detail, "neighbors") {
		t.Errorf("detail view missing 'neighbors' column heading;\n%s", detail)
	}
}

func TestBrowse_EscReturns(t *testing.T) {
	m := newTestModel(t)

	// Open detail.
	m2 := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m2.mode != modeDetail {
		t.Fatal("expected modeDetail after enter")
	}

	// Esc returns to browse.
	m3 := pressKey(t, m2, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m3.mode != modeBrowse {
		t.Fatalf("after esc, mode = %d, want modeBrowse (%d)", m3.mode, modeBrowse)
	}

	// Browse content still shows all records.
	content := m3.View().Content
	if !strings.Contains(content, "Frodo Baggins") {
		t.Errorf("browse view after esc missing 'Frodo Baggins'; got:\n%s", content)
	}
}

func TestBrowse_DisabledViewNoEscapes(t *testing.T) {
	// Do NOT set NO_COLOR — verify that enabled=false alone strips all ANSI escapes.
	os.Unsetenv("NO_COLOR")
	t.Cleanup(func() { os.Unsetenv("NO_COLOR") })

	m := New(&fakeLoader{}, fakeRecords, false)
	m.width = 80
	m.height = 24

	content := m.View().Content
	if strings.Contains(content, "\x1b[") {
		t.Errorf("disabled View() must not contain ANSI escapes; got:\n%s", content)
	}
}

func TestBrowse_LinesRespectWidth(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := New(&fakeLoader{}, fakeRecords, true)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m2 := updated.(Model)

	content := m2.View().Content
	for _, line := range strings.Split(content, "\n") {
		w := ansi.StringWidth(line)
		if w > 80 {
			t.Errorf("line width %d > 80: %q", w, line)
		}
	}
}

func TestBrowse_NeighborReseed(t *testing.T) {
	m := newTestModel(t)

	// Open detail for Frodo (id-1); neighbors are Gandalf + Shire.
	m2 := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m2.mode != modeDetail {
		t.Fatal("expected modeDetail after enter")
	}
	if m2.detailID != "id-1" {
		t.Fatalf("detailID = %q, want id-1", m2.detailID)
	}

	// Press Enter to select the first neighbor (Gandalf, id-2).
	m3 := pressKey(t, m2, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m3.mode != modeDetail {
		t.Fatalf("after neighbor enter, mode = %d, want modeDetail", m3.mode)
	}
	if m3.detailID != "id-2" {
		t.Fatalf("after neighbor enter, detailID = %q, want id-2", m3.detailID)
	}

	// Strip ANSI before checking — glamour splits heading words across escape runs.
	plain := ansi.Strip(m3.View().Content)
	if !strings.Contains(plain, "Gandalf") {
		t.Errorf("after neighbor reseed, View() missing 'Gandalf'; got (plain):\n%s", plain)
	}
}

func TestBrowse_QQuits(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := New(&fakeLoader{}, fakeRecords, true)
	m.width = 80
	m.height = 24

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("expected a quit cmd after q, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected QuitMsg after q, got %T", msg)
	}
}

// TestBrowse_EscClearsActiveSearch is the regression for m7: while the picker
// is in incremental-search mode, esc must CLEAR the query and return to the
// full list rather than quitting the program.
func TestBrowse_EscClearsActiveSearch(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := New(&fakeLoader{}, fakeRecords, true)
	m.width = 80
	m.height = 24

	// Enter search mode and type a query that narrows the list to Gandalf.
	m = pressKey(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	for _, r := range "gandalf" {
		m = pressKey(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	narrowed := m.View().Content
	if !strings.Contains(narrowed, "Gandalf") || strings.Contains(narrowed, "Frodo Baggins") {
		t.Fatalf("expected list narrowed to Gandalf; got:\n%s", narrowed)
	}

	// Esc must NOT quit — it clears the query.
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(Model)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatal("esc during active search quit the program; want query cleared")
			}
		}
	}
	restored := m.View().Content
	if !strings.Contains(restored, "Frodo Baggins") || !strings.Contains(restored, "Shire") {
		t.Errorf("after esc, full list not restored; got:\n%s", restored)
	}
}

// TestBrowse_QTypedIntoSearchDoesNotQuit guards the other half of m7: a literal
// "q" typed while searching is a query character, not the global Quit key.
func TestBrowse_QTypedIntoSearchDoesNotQuit(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := New(&fakeLoader{}, fakeRecords, true)
	m.width = 80
	m.height = 24

	m = pressKey(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatal("typing 'q' during active search quit the program")
			}
		}
	}
}

func TestBrowse_WindowSizeUpdates(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := New(&fakeLoader{}, fakeRecords, true)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m2 := updated.(Model)

	if m2.width != 120 || m2.height != 40 {
		t.Errorf("after WindowSizeMsg, width=%d height=%d, want 120 40", m2.width, m2.height)
	}
}

func TestBrowse_InitReturnsNil(t *testing.T) {
	m := New(&fakeLoader{}, fakeRecords, true)
	if m.Init() != nil {
		t.Error("Init() should return nil for the browse model")
	}
}

// ── filter-strip tests ────────────────────────────────────────────────────────

func TestFilterStrip_TogglesAndEmitsFilters(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Kind", Kind: BankToggle, Options: []string{"entity", "episode"}, Key: "kinds"},
	}
	s := NewFilterStrip(banks, true)

	// space toggles the focused pill (index 0 = "entity")
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	f := s.Filters()
	if len(f.Kinds) != 1 || f.Kinds[0] != "entity" {
		t.Errorf("after space, Kinds = %v, want [entity]", f.Kinds)
	}

	// → moves to "episode", space toggles it
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	f = s.Filters()
	if len(f.Kinds) != 2 || f.Kinds[0] != "entity" || f.Kinds[1] != "episode" {
		t.Errorf("after two toggles, Kinds = %v, want [entity episode]", f.Kinds)
	}
}

func TestFilterStrip_RResets(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Kind", Kind: BankToggle, Options: []string{"entity", "episode"}, Key: "kinds"},
	}
	s := NewFilterStrip(banks, true)
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace}) // toggle entity
	s = s.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	f := s.Filters()
	if len(f.Kinds) != 0 {
		t.Errorf("after r reset, Kinds = %v, want []", f.Kinds)
	}
}

func TestFilterStrip_SliderEmitsFloat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Score", Kind: BankSlider, Options: []string{"0", "0.05", "0.1", "0.2"}, Key: "minScore"},
	}
	s := NewFilterStrip(banks, true)
	// Slider starts at index 0 ("0").
	if got := s.Filters().MinScore; got != 0 {
		t.Errorf("initial MinScore = %v, want 0", got)
	}
	// → moves to index 1 ("0.05"), space selects it.
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if got := s.Filters().MinScore; got != 0.05 {
		t.Errorf("after selecting 0.05, MinScore = %v, want 0.05", got)
	}
}

// TestFilterStrip_SliderCommitsOnMove is the m6 regression: ←/→ on a slider
// change the committed value immediately, with no separate space press.
func TestFilterStrip_SliderCommitsOnMove(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Score", Kind: BankSlider, Options: []string{"0", "0.05", "0.1", "0.2"}, Key: "minScore"},
	}
	s := NewFilterStrip(banks, true)

	s = s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if got := s.Filters().MinScore; got != 0.05 {
		t.Errorf("after one → (no space), MinScore = %v, want 0.05 (commit-on-move)", got)
	}
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if got := s.Filters().MinScore; got != 0.1 {
		t.Errorf("after two →, MinScore = %v, want 0.1", got)
	}
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := s.Filters().MinScore; got != 0.05 {
		t.Errorf("after ←, MinScore = %v, want 0.05", got)
	}
}

// TestFilterStrip_ResetClearsSliderVisual is the m6 reset half: after r, the
// committed value AND the rendered bar both return to the first stop (no stale
// preview reading the old position).
func TestFilterStrip_ResetClearsSliderVisual(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Score", Kind: BankSlider, Options: []string{"0", "0.05", "0.1", "0.2"}, Key: "minScore"},
	}
	s := NewFilterStrip(banks, true).WithFocused(true)
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // committed 0.1
	if got := s.Filters().MinScore; got != 0.1 {
		t.Fatalf("precondition MinScore = %v, want 0.1", got)
	}
	s = s.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if got := s.Filters().MinScore; got != 0 {
		t.Errorf("after r, MinScore = %v, want 0", got)
	}
	v := s.View(120)
	if !strings.Contains(v, ">= 0") || strings.Contains(v, ">= 0.1") {
		t.Errorf("after r, slider bar should read \">= 0\", not stale \">= 0.1\"; got:\n%s", v)
	}
}

// TestFilterStrip_UpDownMovesBank is the n1 regression: ↑/↓ move focus between
// banks (they were previously dead keys on the strip).
func TestFilterStrip_UpDownMovesBank(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Kind", Kind: BankToggle, Options: []string{"entity", "episode"}, Key: "kinds"},
		{Label: "Scope", Kind: BankToggle, Options: []string{"personal", "project"}, Key: "scopes"},
	}
	s := NewFilterStrip(banks, true).WithFocused(true)

	// Focus starts on bank 0: space toggles "entity".
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	// ↓ moves focus to bank 1: space toggles "personal".
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	f := s.Filters()
	if len(f.Kinds) != 1 || f.Kinds[0] != "entity" {
		t.Errorf("Kinds = %v, want [entity] (↓ must not disturb bank 0)", f.Kinds)
	}
	if len(f.Scopes) != 1 || f.Scopes[0] != "personal" {
		t.Errorf("Scopes = %v, want [personal] (↓ should focus bank 1)", f.Scopes)
	}

	// ↑ returns to bank 0; → then space toggles "episode".
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	s = s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	f = s.Filters()
	if len(f.Kinds) != 2 {
		t.Errorf("after ↑ →space, Kinds = %v, want [entity episode]", f.Kinds)
	}
}

// TestBrowse_FooterSurfacesTabAndCapabilities is the C5a/C6 regression: the
// browse footer must advertise `tab` to reach the filter strip, surface a
// loader's capability keys (e/n/L) when present, and NOT advertise `n` as
// next-page when an Editor has remapped it to "new note".
func TestBrowse_FooterSurfacesTabAndCapabilities(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	t.Run("recall loader shows tab filters and paging", func(t *testing.T) {
		m := New(&fakeLoader{}, fakeRecords, true)
		m.width, m.height = 80, 24
		m.strip = NewFilterStrip([]Bank{{Label: "Kind", Kind: BankToggle, Options: []string{"a"}, Key: "kinds"}}, true)
		f := m.renderFooter()
		if !strings.Contains(f, "tab filters") {
			t.Errorf("footer missing 'tab filters'; got %q", f)
		}
		if !strings.Contains(f, "n/p page") {
			t.Errorf("non-editor footer should show paging; got %q", f)
		}
		if strings.Contains(f, "e edit") {
			t.Errorf("non-editor footer must not advertise edit keys; got %q", f)
		}
	})

	t.Run("editor loader shows e/n and not a paging n", func(t *testing.T) {
		m := New(&fakeEditorLoader{}, fakeRecords, true)
		m.width, m.height = 80, 24
		f := m.renderFooter()
		if !strings.Contains(f, "e edit") || !strings.Contains(f, "n new") {
			t.Errorf("editor footer missing e/n hints; got %q", f)
		}
		if strings.Contains(f, "n/p page") {
			t.Errorf("editor footer must not advertise n as next-page (it is new-note); got %q", f)
		}
	})

	t.Run("focused strip hint includes up/down", func(t *testing.T) {
		m := New(&fakeLoader{}, fakeRecords, true)
		m.width, m.height = 80, 24
		m.strip = NewFilterStrip([]Bank{{Label: "Kind", Kind: BankToggle, Options: []string{"a"}, Key: "kinds"}}, true).WithFocused(true)
		f := m.renderFooter()
		if !strings.Contains(f, "↑/↓") {
			t.Errorf("focused-strip footer missing ↑/↓ hint; got %q", f)
		}
	})
}

// TestFilterStrip_ActivePillShowsCheck is the C5b regression: a selected pill is
// marked with a ✓ in every mode, distinct from the focus brackets, so "is this
// filter on?" is unambiguous.
func TestFilterStrip_ActivePillShowsCheck(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{{Label: "Kind", Kind: BankToggle, Options: []string{"entity"}, Key: "kinds"}}
	s := NewFilterStrip(banks, true).WithFocused(true)

	if strings.Contains(s.View(120), "✓") {
		t.Fatal("inactive pill should not show a check")
	}
	s = s.Update(tea.KeyPressMsg{Code: tea.KeySpace}) // select "entity"
	if !strings.Contains(s.View(120), "✓") {
		t.Errorf("selected pill should be marked with ✓; got:\n%s", s.View(120))
	}
}

func TestFilterStrip_ViewContainsBankLabel(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Kind", Kind: BankToggle, Options: []string{"entity"}, Key: "kinds"},
	}
	s := NewFilterStrip(banks, true)
	v := s.View(120)
	if !strings.Contains(v, "Kind:") {
		t.Errorf("View() missing 'Kind:'; got: %s", v)
	}
	if !strings.Contains(v, "entity") {
		t.Errorf("View() missing 'entity' pill; got: %s", v)
	}
	if strings.Contains(v, "\x1b[") {
		t.Errorf("View() with NO_COLOR must not contain ANSI escapes; got: %s", v)
	}
}

// ── Tab focus cycling tests ───────────────────────────────────────────────────

func TestBrowse_TabFocusesStrip(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Kind", Kind: BankToggle, Options: []string{"entity", "episode"}, Key: "kinds"},
	}
	m := newTestModel(t)
	m = m.WithFilterStrip(banks)

	// Tab moves focus to the strip.
	m2 := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if !m2.strip.Focused() {
		t.Fatal("after Tab, strip.Focused() should be true")
	}

	// Strip footer hint is shown when focused.
	content := ansi.Strip(m2.View().Content)
	if !strings.Contains(content, "←/→") {
		t.Errorf("strip-focused footer missing '←/→' hint; got:\n%s", content)
	}

	// Tab again returns focus to picker.
	m3 := pressKey(t, m2, tea.KeyPressMsg{Code: tea.KeyTab})
	if m3.strip.Focused() {
		t.Fatal("after second Tab, strip.Focused() should be false")
	}
}

func TestBrowse_StripSpaceTogglesFilter(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Kind", Kind: BankToggle, Options: []string{"entity", "episode"}, Key: "kinds"},
	}
	m := newTestModel(t)
	m = m.WithFilterStrip(banks)

	// Focus strip then toggle first pill.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeySpace})

	f := m.strip.Filters()
	if len(f.Kinds) != 1 || f.Kinds[0] != "entity" {
		t.Errorf("after strip space, Kinds = %v, want [entity]", f.Kinds)
	}
}

// ── capability interface tests ────────────────────────────────────────────────

func TestBrowse_EditorLoaderRoutesEKey(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	fl := &fakeEditorLoader{}
	m := New(fl, fakeRecords, true)
	m.width = 80
	m.height = 24

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if cmd == nil {
		t.Fatal("expected non-nil cmd after 'e' on Editor loader")
	}
	if !fl.editCalled {
		t.Error("Edit() was not called on the Editor loader after 'e'")
	}
}

func TestBrowse_EditorLoaderRoutesNKey(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	fl := &fakeEditorLoader{}
	m := New(fl, fakeRecords, true)
	m.width = 80
	m.height = 24

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if cmd == nil {
		t.Fatal("expected non-nil cmd after 'n' on Editor loader")
	}
	if !fl.newCalled {
		t.Error("New() was not called on the Editor loader after 'n'")
	}
}

func TestBrowse_NonEditorLoaderEKeyInert(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newTestModel(t) // fakeLoader does NOT implement Editor

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	// e should be forwarded to the picker, which ignores it; no quit cmd.
	if cmd != nil {
		msg := cmd()
		if _, isQuit := msg.(tea.QuitMsg); isQuit {
			t.Error("'e' on non-Editor loader must not quit")
		}
	}
}

// ── RunWithStrip tests ────────────────────────────────────────────────────────

// TestBrowse_RunWithStrip_PillsVisible verifies that a model built via
// New(...).WithFilterStrip(banks) renders the filter strip pills in View().
// This covers the same code path that RunWithStrip uses internally.
func TestBrowse_RunWithStrip_PillsVisible(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	banks := []Bank{
		{Label: "Kind", Kind: BankToggle, Options: []string{"entity", "fact"}, Key: "kinds"},
	}
	m := New(&fakeLoader{}, fakeRecords, true).WithFilterStrip(banks)
	m.width = 80
	m.height = 24

	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "entity") {
		t.Errorf("WithFilterStrip model View() missing pill 'entity'; got:\n%s", view)
	}
	if !strings.Contains(view, "fact") {
		t.Errorf("WithFilterStrip model View() missing pill 'fact'; got:\n%s", view)
	}
	// The bordered box must be present.
	if !strings.Contains(view, "filters") {
		t.Errorf("WithFilterStrip model View() missing 'filters' box header; got:\n%s", view)
	}
}
