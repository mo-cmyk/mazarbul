package cli

import (
	"strings"
	"testing"
	"time"
)

func TestResolveTemplatePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"daily", "templates/daily.md"},
		{"daily.md", "templates/daily.md"},
		{"standup", "templates/standup.md"},
		{"project/weekly.md", "project/weekly.md"},   // literal path with slash
		{"project/weekly", "project/weekly.md"},      // slash path, extension appended
		{"templates/note", "templates/note.md"},      // templates/ prefix, no extension
		{"templates/daily.md", "templates/daily.md"}, // already templates/ prefix
		{"weekly.markdown", "templates/weekly.markdown"},
	} {
		got := resolveTemplatePath(tc.name)
		if got != tc.want {
			t.Errorf("resolveTemplatePath(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDefaultJournalPath(t *testing.T) {
	fixed := time.Date(2026, 6, 8, 14, 30, 0, 0, time.UTC)
	got := defaultJournalPath(fixed)
	want := "journal/2026-06-08.md"
	if got != want {
		t.Errorf("defaultJournalPath = %q, want %q", got, want)
	}
}

func TestApplyTemplatePlaceholders(t *testing.T) {
	now := time.Date(2026, 6, 8, 9, 5, 0, 0, time.UTC)
	for _, tc := range []struct {
		desc  string
		body  string
		title string
		want  string
	}{
		{
			desc:  "substitutes all three placeholders",
			body:  "# {{title}}\nDate: {{date}}\nTime: {{time}}",
			title: "My Note",
			want:  "# My Note\nDate: 2026-06-08\nTime: 09:05",
		},
		{
			desc:  "empty title placeholder stays empty",
			body:  "Title: {{title}}",
			title: "",
			want:  "Title: ",
		},
		{
			desc:  "no placeholders passes through unchanged",
			body:  "plain body",
			title: "unused",
			want:  "plain body",
		},
		{
			desc:  "substitutes in frontmatter block",
			body:  "---\ntitle: {{title}}\ndate: {{date}}\n---\nbody",
			title: "Daily",
			want:  "---\ntitle: Daily\ndate: 2026-06-08\n---\nbody",
		},
	} {
		got := applyTemplatePlaceholders(tc.body, tc.title, now)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.desc, got, tc.want)
		}
	}
}

func TestParseTemplateFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		desc string
		body string
		want templateFrontmatter
	}{
		{
			desc: "no frontmatter returns zero value",
			body: "# Heading\nbody",
			want: templateFrontmatter{},
		},
		{
			desc: "extracts title tags and pinned",
			body: "---\ntitle: Daily\ntags:\n  - journal\n  - daily\npinned: true\n---\nbody",
			want: templateFrontmatter{Title: "Daily", Pinned: true, Tags: []string{"journal", "daily"}},
		},
		{
			desc: "partial frontmatter only title",
			body: "---\ntitle: Weekly Review\n---\n# body",
			want: templateFrontmatter{Title: "Weekly Review"},
		},
		{
			desc: "malformed frontmatter returns zero",
			body: "---\n: broken: yaml: [\n---\nbody",
			want: templateFrontmatter{},
		},
		{
			desc: "unclosed frontmatter block returns zero",
			body: "---\ntitle: Open\nbody without closing delimiter",
			want: templateFrontmatter{},
		},
	} {
		got := parseTemplateFrontmatter(tc.body)
		if got.Title != tc.want.Title {
			t.Errorf("%s: title = %q, want %q", tc.desc, got.Title, tc.want.Title)
		}
		if got.Pinned != tc.want.Pinned {
			t.Errorf("%s: pinned = %v, want %v", tc.desc, got.Pinned, tc.want.Pinned)
		}
		if len(got.Tags) != len(tc.want.Tags) {
			t.Errorf("%s: tags = %v, want %v", tc.desc, got.Tags, tc.want.Tags)
			continue
		}
		for i, tag := range got.Tags {
			if tag != tc.want.Tags[i] {
				t.Errorf("%s: tags[%d] = %q, want %q", tc.desc, i, tag, tc.want.Tags[i])
			}
		}
	}
}

func TestTitleFromPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"notes/project-kickoff", "project-kickoff"},
		{"journal/2026-06-08.md", "2026-06-08"},
		{"standup.md", "standup"},
		{"deep/nested/note.markdown", "note"},
		{"bare", "bare"},
		{"trailing/", "trailing"},
	} {
		if got := titleFromPath(tc.path); got != tc.want {
			t.Errorf("titleFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestHasLeadingFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"---\ntitle: x\n---\nbody", true},
		{"---\r\ntitle: x\r\n---\r\nbody", true},
		{"# Heading\nno frontmatter", false},
		{"text --- not at start", false},
		{"", false},
	} {
		if got := hasLeadingFrontmatter(tc.body); got != tc.want {
			t.Errorf("hasLeadingFrontmatter(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestSplitLeadingFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		desc      string
		body      string
		wantBlock string
		wantTail  string
		wantOK    bool
	}{
		{
			desc:      "splits block and tail",
			body:      "---\ntitle: Daily\ntags:\n  - journal\n---\n# Daily\n\nNotes",
			wantBlock: "title: Daily\ntags:\n  - journal",
			wantTail:  "\n# Daily\n\nNotes",
			wantOK:    true,
		},
		{
			desc:   "no frontmatter fence",
			body:   "# Heading\nbody",
			wantOK: false,
		},
		{
			desc:   "unterminated block",
			body:   "---\ntitle: Open\nbody without closing fence",
			wantOK: false,
		},
	} {
		block, tail, ok := splitLeadingFrontmatter(tc.body)
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.desc, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if block != tc.wantBlock {
			t.Errorf("%s: block = %q, want %q", tc.desc, block, tc.wantBlock)
		}
		if tail != tc.wantTail {
			t.Errorf("%s: tail = %q, want %q", tc.desc, tail, tc.wantTail)
		}
	}
}

// TestMergeTemplateFrontmatter is the regression test for the Greptile finding:
// when a template body already carries its own YAML frontmatter, the CLI must
// fold any explicit --title/--pin/--tag override INTO that block (so writeDocument
// does not reject separate metadata fields) and must otherwise forward the body
// byte-for-byte.
func TestMergeTemplateFrontmatter(t *testing.T) {
	const body = "---\ntitle: Daily\ntags:\n  - journal\naliases:\n  - log\n---\n# Daily\n\nNotes"

	t.Run("no overrides returns body verbatim", func(t *testing.T) {
		got, err := mergeTemplateFrontmatter(body, &docNewFlags{})
		if err != nil {
			t.Fatalf("mergeTemplateFrontmatter: %v", err)
		}
		if got != body {
			t.Fatalf("body mutated without overrides:\n got %q\nwant %q", got, body)
		}
	})

	t.Run("pin override injects pinned and preserves other keys", func(t *testing.T) {
		got, err := mergeTemplateFrontmatter(body, &docNewFlags{pin: true})
		if err != nil {
			t.Fatalf("mergeTemplateFrontmatter: %v", err)
		}
		if !hasLeadingFrontmatter(got) {
			t.Fatalf("merged body lost its frontmatter fence: %q", got)
		}
		fm := parseTemplateFrontmatter(got)
		if !fm.Pinned {
			t.Errorf("pinned = false, want true; got body:\n%s", got)
		}
		if fm.Title != "Daily" {
			t.Errorf("title = %q, want preserved %q", fm.Title, "Daily")
		}
		if len(fm.Tags) != 1 || fm.Tags[0] != "journal" {
			t.Errorf("tags = %v, want preserved [journal]", fm.Tags)
		}
		if !strings.Contains(got, "aliases") {
			t.Errorf("unknown key 'aliases' dropped: %q", got)
		}
		_, tail, ok := splitLeadingFrontmatter(got)
		if !ok || tail != "\n# Daily\n\nNotes" {
			t.Errorf("body tail not preserved verbatim: ok=%v tail=%q", ok, tail)
		}
	})

	t.Run("title and tag overrides win over template", func(t *testing.T) {
		got, err := mergeTemplateFrontmatter(body, &docNewFlags{title: "Monday Review", tags: []string{"work", "review"}})
		if err != nil {
			t.Fatalf("mergeTemplateFrontmatter: %v", err)
		}
		fm := parseTemplateFrontmatter(got)
		if fm.Title != "Monday Review" {
			t.Errorf("title = %q, want overridden %q", fm.Title, "Monday Review")
		}
		if len(fm.Tags) != 2 || fm.Tags[0] != "work" || fm.Tags[1] != "review" {
			t.Errorf("tags = %v, want overridden [work review]", fm.Tags)
		}
	})

	t.Run("malformed frontmatter surfaces an error when overriding", func(t *testing.T) {
		// A frontmatter block that is not a YAML mapping cannot accept overrides.
		const bad = "---\n: : not: a: mapping: [\n---\nbody"
		if _, err := mergeTemplateFrontmatter(bad, &docNewFlags{pin: true}); err == nil {
			t.Fatal("want error merging into unparseable frontmatter")
		}
	})
}
