package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/tui/browse"
)

// ── compile-time interface assertions ─────────────────────────────────────────
// These duplicate the production assertions so a test failure pinpoints the
// interface rather than just emitting a linker error deep in the browse pkg.

var (
	_ browse.Loader       = (*docBrowseLoader)(nil)
	_ browse.Editor       = (*docBrowseLoader)(nil)
	_ browse.LinkFollower = (*docBrowseLoader)(nil)
)

// ── test helpers ──────────────────────────────────────────────────────────────

// newDocBrowseTestLoader builds a docBrowseLoader with a fake invoker.
// responses maps toolName → JSON to return; unknown tools return an empty
// object so callers do not have to stub every tool.
func newDocBrowseTestLoader(t *testing.T, responses map[string]json.RawMessage) *docBrowseLoader {
	t.Helper()
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", t.TempDir(), "")
	cmd := &cobra.Command{Use: "doc"}
	root.AddCommand(cmd)
	cmd.SetContext(context.Background())

	return &docBrowseLoader{
		cmd: cmd,
		invoker: func(_ *cobra.Command, toolName string, _ json.RawMessage) (json.RawMessage, error) {
			if raw, ok := responses[toolName]; ok {
				return raw, nil
			}
			return json.RawMessage(`{}`), nil
		},
	}
}

// mustMarshal marshals v to JSON or fatals the test.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustMarshal: %v", err)
	}
	return b
}

// ── Search tests ──────────────────────────────────────────────────────────────

func TestDocBrowseLoader_Search_maps_documents_to_records(t *testing.T) {
	listResp := mustMarshal(t, map[string]any{
		"documents": []any{
			map[string]any{
				"name":        "entities/01HXYZ",
				"documentUid": "01HXYZ",
				"path":        "notes/project-alpha.md",
				"title":       "Project Alpha",
				"pinned":      false,
				"tags":        []string{"work", "active"},
			},
			map[string]any{
				"name":        "entities/01HABC",
				"documentUid": "01HABC",
				"path":        "notes/standup.md",
				"title":       "Standup",
				"pinned":      true,
				"tags":        nil,
			},
		},
	})

	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{
		"listDocuments": listResp,
	})

	records, err := loader.Search("notes/", browse.Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("len(records) = %d, want 2", len(records))
	}

	// First record: path as ID, title populated, kind = document.
	r0 := records[0]
	if r0.ID != "notes/project-alpha.md" {
		t.Errorf("records[0].ID = %q, want notes/project-alpha.md", r0.ID)
	}
	if r0.Title != "Project Alpha" {
		t.Errorf("records[0].Title = %q, want Project Alpha", r0.Title)
	}
	if r0.Kind != "document" {
		t.Errorf("records[0].Kind = %q, want document", r0.Kind)
	}
	// Subtitle should include the tags.
	if !strings.Contains(r0.Subtitle, "work") {
		t.Errorf("records[0].Subtitle = %q, want 'work' tag visible", r0.Subtitle)
	}

	// Second record: pinned — subtitle should flag it.
	r1 := records[1]
	if !strings.Contains(r1.Subtitle, "pinned") {
		t.Errorf("records[1].Subtitle = %q, want '★ pinned' visible", r1.Subtitle)
	}
}

func TestDocBrowseLoader_Search_empty_title_falls_back_to_path(t *testing.T) {
	listResp := mustMarshal(t, map[string]any{
		"documents": []any{
			map[string]any{
				"path":  "scratch/untitled.md",
				"title": "",
			},
		},
	})
	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{"listDocuments": listResp})

	records, err := loader.Search("", browse.Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if records[0].Title != "scratch/untitled.md" {
		t.Errorf("Title = %q, want path fallback scratch/untitled.md", records[0].Title)
	}
}

func TestDocBrowseLoader_Search_pinned_filter_forwarded(t *testing.T) {
	var capturedReq json.RawMessage
	loader := newDocBrowseTestLoader(t, nil)
	loader.invoker = func(_ *cobra.Command, toolName string, req json.RawMessage) (json.RawMessage, error) {
		if toolName == "listDocuments" {
			capturedReq = req
		}
		return mustMarshal(t, map[string]any{"documents": []any{}}), nil
	}

	_, _ = loader.Search("", browse.Filters{Pinned: true})
	var body map[string]any
	if err := json.Unmarshal(capturedReq, &body); err != nil {
		t.Fatalf("unmarshal req: %v", err)
	}
	if pinned, _ := body["pinned"].(bool); !pinned {
		t.Errorf("listDocuments request missing pinned=true; got %v", body)
	}
}

// ── Detail tests ──────────────────────────────────────────────────────────────

func TestDocBrowseLoader_Detail_composes_markdown(t *testing.T) {
	readResp := mustMarshal(t, map[string]any{
		"document": map[string]any{
			"path":  "notes/project-alpha.md",
			"title": "Project Alpha",
			"body":  "This note covers project alpha.\n",
		},
		"links": []any{
			map[string]any{
				"predicate":   "mentions",
				"name":        "entities/01HPERSON",
				"displayName": "Alice",
			},
		},
		"backlinks": []any{
			map[string]any{
				"predicate":   "links_to",
				"name":        "entities/01HDOC2",
				"displayName": "Weekly Review",
			},
		},
		"observations": []any{},
	})

	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{
		"readDocument": readResp,
	})

	detail, err := loader.Detail("notes/project-alpha.md")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	// Title heading.
	if !strings.Contains(detail, "# Project Alpha") {
		t.Errorf("detail missing title heading; got:\n%s", detail)
	}
	// Body.
	if !strings.Contains(detail, "project alpha") {
		t.Errorf("detail missing body; got:\n%s", detail)
	}
	// Links section with resolved glyph.
	if !strings.Contains(detail, "## Links") {
		t.Errorf("detail missing Links section; got:\n%s", detail)
	}
	if !strings.Contains(detail, "→ Alice") {
		t.Errorf("detail missing resolved link '→ Alice'; got:\n%s", detail)
	}
	// Backlinks section.
	if !strings.Contains(detail, "## Linked from") {
		t.Errorf("detail missing 'Linked from' section; got:\n%s", detail)
	}
	if !strings.Contains(detail, "← Weekly Review") {
		t.Errorf("detail missing backlink '← Weekly Review'; got:\n%s", detail)
	}
}

func TestDocBrowseLoader_Detail_dangling_link_shows_warning_glyph(t *testing.T) {
	readResp := mustMarshal(t, map[string]any{
		"document":     map[string]any{"path": "notes/x.md", "title": "X", "body": ""},
		"links":        []any{map[string]any{"predicate": "mentions", "name": "entities/DANGLING", "displayName": ""}},
		"backlinks":    []any{},
		"observations": []any{},
	})
	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{"readDocument": readResp})

	detail, err := loader.Detail("notes/x.md")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	// Dangling link should show ⚠ and the raw name.
	if !strings.Contains(detail, "⚠") {
		t.Errorf("detail missing ⚠ for dangling link; got:\n%s", detail)
	}
	if !strings.Contains(detail, "entities/DANGLING") {
		t.Errorf("detail missing dangling link name; got:\n%s", detail)
	}
}

// TestDocBrowseLoader_Detail_renders_unresolved_links is the B1 regression: a
// dangling [[wikilink]] (returned by readDocument as unresolvedLinks, since it
// has no edge) is listed in the detail's Links section with the ⚠ glyph so a
// reader navigating the detail learns the note has a broken reference.
func TestDocBrowseLoader_Detail_renders_unresolved_links(t *testing.T) {
	readResp := mustMarshal(t, map[string]any{
		"document":        map[string]any{"path": "notes/kickoff.md", "title": "Kickoff", "body": "See [[Mythical Module]]."},
		"links":           []any{map[string]any{"predicate": "links_to", "name": "entities/01HAUTH", "displayName": "Auth Service"}},
		"backlinks":       []any{},
		"observations":    []any{},
		"unresolvedLinks": []any{"Mythical Module"},
	})
	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{"readDocument": readResp})

	detail, err := loader.Detail("notes/kickoff.md")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if !strings.Contains(detail, "→ Auth Service") {
		t.Errorf("detail missing resolved link; got:\n%s", detail)
	}
	if !strings.Contains(detail, "⚠ Mythical Module (unresolved)") {
		t.Errorf("detail missing dangling link marker; got:\n%s", detail)
	}
}

func TestDocBrowseLoader_Detail_entities_id_uses_documentUid(t *testing.T) {
	var capturedReq json.RawMessage
	loader := newDocBrowseTestLoader(t, nil)
	loader.invoker = func(_ *cobra.Command, toolName string, req json.RawMessage) (json.RawMessage, error) {
		if toolName == "readDocument" {
			capturedReq = req
		}
		return mustMarshal(t, map[string]any{
			"document": map[string]any{"path": "notes/x.md", "title": "X", "body": ""},
			"links":    []any{}, "backlinks": []any{}, "observations": []any{},
		}), nil
	}

	_, _ = loader.Detail("entities/01HUID")
	var body map[string]any
	if err := json.Unmarshal(capturedReq, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if uid, _ := body["documentUid"].(string); uid != "01HUID" {
		t.Errorf("readDocument called with documentUid=%q, want 01HUID; req=%s", uid, capturedReq)
	}
}

// TestDocBrowseLoader_Detail_strips_frontmatter is the n2 regression: a body
// with a leading YAML frontmatter block must render as prose only — the `---`
// fences and the frontmatter keys must not leak into the detail markdown.
func TestDocBrowseLoader_Detail_strips_frontmatter(t *testing.T) {
	body := "---\ntitle: Kickoff\ntags: [meeting, q3]\npinned: true\n---\n\nKickoff agenda and notes.\n"
	readResp := mustMarshal(t, map[string]any{
		"document":     map[string]any{"path": "notes/kickoff.md", "title": "Kickoff", "body": body},
		"links":        []any{},
		"backlinks":    []any{},
		"observations": []any{},
	})
	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{"readDocument": readResp})

	detail, err := loader.Detail("notes/kickoff.md")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if strings.Contains(detail, "---") {
		t.Errorf("detail leaked frontmatter fence '---'; got:\n%s", detail)
	}
	if strings.Contains(detail, "tags:") || strings.Contains(detail, "pinned:") {
		t.Errorf("detail leaked frontmatter keys; got:\n%s", detail)
	}
	if !strings.Contains(detail, "Kickoff agenda and notes.") {
		t.Errorf("detail missing prose body; got:\n%s", detail)
	}
}

// TestNewNoteIsDiscardable is the n3 regression: a blank or unchanged-seed note
// is discardable (not persisted); a note with real content is not.
func TestNewNoteIsDiscardable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", true},
		{"whitespace only", "   \n\t\n", true},
		{"unchanged seed", newNoteSeed, true},
		{"seed plus content", newNoteSeed + "Real content here.\n", false},
		{"different content", "# Retro\n\nNotes.\n", false},
	}
	for _, tc := range cases {
		if got := newNoteIsDiscardable(tc.body); got != tc.want {
			t.Errorf("newNoteIsDiscardable(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// ── Neighbors tests ───────────────────────────────────────────────────────────

func TestDocBrowseLoader_Neighbors_returns_links_and_backlinks(t *testing.T) {
	backlinksResp := mustMarshal(t, map[string]any{
		"links": []any{
			map[string]any{"predicate": "mentions", "name": "entities/01HA", "displayName": "Entity A"},
		},
		"backlinks": []any{
			map[string]any{"predicate": "links_to", "name": "entities/01HB", "displayName": "Doc B"},
		},
	})
	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{"getBacklinks": backlinksResp})

	records, err := loader.Neighbors("notes/x.md")
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("len(records) = %d, want 2", len(records))
	}
	// Outgoing link comes first.
	if records[0].Title != "Entity A" {
		t.Errorf("records[0].Title = %q, want Entity A", records[0].Title)
	}
	if !strings.HasPrefix(records[0].Subtitle, "→") {
		t.Errorf("records[0].Subtitle = %q, want → prefix", records[0].Subtitle)
	}
	// Backlink comes second.
	if records[1].Title != "Doc B" {
		t.Errorf("records[1].Title = %q, want Doc B", records[1].Title)
	}
	if !strings.HasPrefix(records[1].Subtitle, "←") {
		t.Errorf("records[1].Subtitle = %q, want ← prefix", records[1].Subtitle)
	}
}

// ── Links (LinkFollower) tests ────────────────────────────────────────────────

func TestDocBrowseLoader_Links_returns_only_outgoing(t *testing.T) {
	backlinksResp := mustMarshal(t, map[string]any{
		"links": []any{
			map[string]any{"predicate": "mentions", "name": "entities/01HA", "displayName": "Entity A"},
		},
		"backlinks": []any{
			map[string]any{"predicate": "links_to", "name": "entities/01HB", "displayName": "Doc B"},
		},
	})
	loader := newDocBrowseTestLoader(t, map[string]json.RawMessage{"getBacklinks": backlinksResp})

	records := loader.Links("notes/x.md")
	// Only outgoing links, no backlinks.
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1 (only outgoing)", len(records))
	}
	if records[0].Title != "Entity A" {
		t.Errorf("records[0].Title = %q, want Entity A", records[0].Title)
	}
}

// ── Command registration and help tests ───────────────────────────────────────

func TestDocBrowseCmd_Help_is_non_empty_and_includes_examples(t *testing.T) {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().String("color", "auto", "color when")

	docCmd := newDocCmd()
	root.AddCommand(docCmd)

	var out bytes.Buffer
	root.SetOut(&out)

	root.SetArgs([]string{"doc", "browse", "--help"})
	// Help exits with nil on success; errors are acceptable (cobra returns nil
	// for --help even without a context).
	_ = root.Execute()

	help := out.String()
	if help == "" {
		t.Fatal("--help produced empty output")
	}
	for _, want := range []string{"maz doc browse", "Examples:", "$EDITOR", "notes/"} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q; got:\n%s", want, help)
		}
	}
}

func TestDocBrowseCmd_NonTTY_exits_1(t *testing.T) {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", t.TempDir(), "")
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().Bool("no-tui", false, "")
	root.PersistentFlags().String("output", "", "")
	root.PersistentFlags().String("color", "never", "")
	root.AddCommand(newDocCmd())
	root.SetContext(context.Background())

	// Redirect stdout to a non-TTY bytes.Buffer so IsTTY returns false.
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"doc", "browse"})

	err := root.Execute()
	// The command should return a silentError{code:1} when not a TTY.
	// cobra.Command.Execute() returns the error wrapped or nil; check the
	// stderr advisory message as a proxy.
	_ = err
	if !strings.Contains(errOut.String(), "interactive terminal") {
		t.Errorf("stderr = %q, want 'interactive terminal' advisory", errOut.String())
	}
}
