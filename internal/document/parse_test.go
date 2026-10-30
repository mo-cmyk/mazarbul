package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

func TestParse_BodyVerbatimAndHash(t *testing.T) {
	raw := []byte("---\ntitle: Alpha\n---\n# Alpha\n\nBody text with a [[Link]].\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Body != string(raw) {
		t.Errorf("Body must be the entire verbatim file\n got: %q\nwant: %q", p.Body, raw)
	}
	want := sha256.Sum256(raw)
	if p.ContentHash != hex.EncodeToString(want[:]) {
		t.Errorf("ContentHash = %q, want sha256 of raw bytes", p.ContentHash)
	}
}

func TestParse_Frontmatter(t *testing.T) {
	raw := []byte("---\ntitle: Project Alpha\npinned: true\ntags: [planning, q3]\n---\nbody\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Title != "Project Alpha" {
		t.Errorf("Title = %q, want Project Alpha", p.Title)
	}
	if !p.Pinned {
		t.Error("Pinned = false, want true")
	}
	if len(p.Tags) != 2 || p.Tags[0] != "planning" || p.Tags[1] != "q3" {
		t.Errorf("Tags = %v, want [planning q3]", p.Tags)
	}
	if p.FrontmatterRaw != "---\ntitle: Project Alpha\npinned: true\ntags: [planning, q3]\n---\n" {
		t.Errorf("FrontmatterRaw not byte-exact: %q", p.FrontmatterRaw)
	}
	var fm map[string]any
	if err := json.Unmarshal(p.Frontmatter, &fm); err != nil {
		t.Fatalf("Frontmatter is not valid JSON: %v", err)
	}
	if fm["title"] != "Project Alpha" {
		t.Errorf("decoded frontmatter title = %v", fm["title"])
	}
}

func TestParse_Canonical(t *testing.T) {
	t.Run("canonical true sets Canonical", func(t *testing.T) {
		raw := []byte("---\ntitle: Doc\ncanonical: true\n---\nbody\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !p.Canonical {
			t.Error("Canonical = false, want true for canonical:true frontmatter")
		}
	})
	t.Run("canonical false leaves Canonical false", func(t *testing.T) {
		raw := []byte("---\ntitle: Doc\ncanonical: false\n---\nbody\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if p.Canonical {
			t.Error("Canonical = true, want false for canonical:false frontmatter")
		}
	})
	t.Run("canonical absent leaves Canonical false", func(t *testing.T) {
		raw := []byte("---\ntitle: Doc\n---\nbody\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if p.Canonical {
			t.Error("Canonical = true, want false when key is absent")
		}
	})
	t.Run("no frontmatter leaves Canonical false", func(t *testing.T) {
		raw := []byte("# Just a heading\n\nbody\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if p.Canonical {
			t.Error("Canonical = true, want false when no frontmatter block")
		}
	})
}

func TestParse_FrontmatterRawIsPrefixOfBody(t *testing.T) {
	// The materializer reconstructs the file from Body alone, but FrontmatterRaw
	// must be an exact prefix of Body so the two views never disagree.
	raw := []byte("---\ntitle: X\n---\ncontent\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := p.Body[:len(p.FrontmatterRaw)]; got != p.FrontmatterRaw {
		t.Errorf("FrontmatterRaw %q is not a prefix of Body %q", p.FrontmatterRaw, got)
	}
}

func TestParse_TitleFallbackToH1(t *testing.T) {
	p, err := Parse([]byte("# Heading Title\n\nbody\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Title != "Heading Title" {
		t.Errorf("Title = %q, want Heading Title", p.Title)
	}
	if p.FrontmatterRaw != "" {
		t.Errorf("FrontmatterRaw = %q, want empty (no frontmatter)", p.FrontmatterRaw)
	}
}

func TestParse_NoFrontmatterNoTitle(t *testing.T) {
	p, err := Parse([]byte("just some prose, no heading\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Title != "" {
		t.Errorf("Title = %q, want empty", p.Title)
	}
	if p.Frontmatter != nil {
		t.Errorf("Frontmatter = %s, want nil", p.Frontmatter)
	}
}

func TestParse_DashesNotFrontmatter(t *testing.T) {
	// A thematic break / longer rule must not be mistaken for a frontmatter open.
	for _, body := range []string{"----\nx\n", "text\n---\nmore\n"} {
		p, err := Parse([]byte(body))
		if err != nil {
			t.Fatalf("Parse(%q): %v", body, err)
		}
		if p.FrontmatterRaw != "" {
			t.Errorf("Parse(%q): FrontmatterRaw = %q, want empty", body, p.FrontmatterRaw)
		}
	}
}

func TestParse_UnclosedFrontmatterIsBody(t *testing.T) {
	p, err := Parse([]byte("---\ntitle: X\nno closing fence\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.FrontmatterRaw != "" {
		t.Errorf("unclosed frontmatter should be treated as body; got raw %q", p.FrontmatterRaw)
	}
}

func TestParse_MalformedFrontmatterDegrades(t *testing.T) {
	// Malformed YAML returns an error but still yields a usable body + links.
	raw := []byte("---\n: : not: valid: yaml\n\t- broken\n---\nbody [[Keep]]\n")
	p, err := Parse(raw)
	if err == nil {
		t.Fatal("expected a frontmatter decode error")
	}
	if p.Body != string(raw) {
		t.Error("Body must still be the verbatim file on frontmatter error")
	}
	if len(p.Wikilinks) != 1 || p.Wikilinks[0].Target != "Keep" {
		t.Errorf("wikilinks must still parse on frontmatter error; got %v", p.Wikilinks)
	}
}

func TestScanWikilinks_Grammar(t *testing.T) {
	body := "See [[Rosie Cotton]], [[Project Alpha#Goals]], [[notes/foo|the note]], " +
		"[[notes/bar#Section|Bar]] and [[relates_to::Widget]].\n"
	links, err := scanWikilinks(body)
	if err != nil {
		t.Fatalf("scanWikilinks: %v", err)
	}
	if len(links) != 5 {
		t.Fatalf("got %d links, want 5: %+v", len(links), links)
	}
	cases := []struct {
		target, section, alias, relType string
	}{
		{"Rosie Cotton", "", "", ""},
		{"Project Alpha", "Goals", "", ""},
		{"notes/foo", "", "the note", ""},
		{"notes/bar", "Section", "Bar", ""},
		{"Widget", "", "", "relates_to"},
	}
	for i, c := range cases {
		got := links[i]
		if got.Target != c.target || got.Section != c.section || got.Alias != c.alias || got.RelType != c.relType {
			t.Errorf("link %d = %+v, want target=%q section=%q alias=%q relType=%q",
				i, got, c.target, c.section, c.alias, c.relType)
		}
	}
}

func TestScanWikilinks_SkipsCode(t *testing.T) {
	body := "Real [[Live]].\n\n" +
		"```\nfenced [[NotALink]] block\n```\n\n" +
		"Inline `[[AlsoNot]]` code and another [[Real Two]].\n" +
		"~~~\ntilde [[NopeFence]]\n~~~\n"
	links, err := scanWikilinks(body)
	if err != nil {
		t.Fatalf("scanWikilinks: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("got %d links, want 2 (code spans excluded): %+v", len(links), links)
	}
	if links[0].Target != "Live" || links[1].Target != "Real Two" {
		t.Errorf("targets = %q, %q; want Live, Real Two", links[0].Target, links[1].Target)
	}
}

func TestScanWikilinks_EmptyTargetDropped(t *testing.T) {
	links, err := scanWikilinks("an empty [[]] link and [[ ]] spaces\n")
	if err != nil {
		t.Fatalf("scanWikilinks: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("empty-target links must be dropped; got %+v", links)
	}
}

func TestScanWikilinks_TypedConfidencePrefix(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		target     string
		relType    string
		confidence float64
	}{
		{"bracket reltype only", "[relates_to] [[Widget]]\n", "Widget", "relates_to", 0},
		{"bracket reltype + confidence", "[contradicts:0.8] [[Old Plan]]\n", "Old Plan", "contradicts", 0.8},
		{"prefix overrides inner ::", "[supports:0.5] [[blocks::Thing]]\n", "Thing", "supports", 0.5},
		{"confidence clamped above one", "[x:1.5] [[Y]]\n", "Y", "x", 1},
		{"far bracket is not a prefix", "[decision] we chose [[Approach]]\n", "Approach", "", 0},
		{"tab between prefix and link", "[a:0.25]\t[[Z]]\n", "Z", "a", 0.25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			links, err := scanWikilinks(c.body)
			if err != nil {
				t.Fatalf("scanWikilinks: %v", err)
			}
			if len(links) != 1 {
				t.Fatalf("got %d links, want 1: %+v", len(links), links)
			}
			got := links[0]
			if got.Target != c.target || got.RelType != c.relType || got.Confidence != c.confidence {
				t.Errorf("link = %+v, want target=%q relType=%q confidence=%v",
					got, c.target, c.relType, c.confidence)
			}
		})
	}
}

func TestScanObservations_Grammar(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		ok       bool
		category string
		text     string
		tags     []string
		context  string
	}{
		{"plain", "- [note] Rosie leads the launch", true, "note", "Rosie leads the launch", nil, ""},
		{"with tag", "- [decision] ship Q3 #planning", true, "decision", "ship Q3 #planning", []string{"planning"}, ""},
		{"tag and context", "- [fact] revenue up #finance (per Q2 report)", true, "fact", "revenue up #finance", []string{"finance"}, "per Q2 report"},
		{"star bullet", "* [action] email the team", true, "action", "email the team", nil, ""},
		{"plus bullet", "+ [risk] vendor may slip", true, "risk", "vendor may slip", nil, ""},
		{"multiple tags dedup", "- [note] a #x b #y c #x", true, "note", "a #x b #y c #x", []string{"x", "y"}, ""},
		{"no category is not an observation", "- just a list item", false, "", "", nil, ""},
		{"empty category dropped", "- [] text", false, "", "", nil, ""},
		{"empty text dropped", "- [note]   ", false, "", "", nil, ""},
		{"not a bullet", "[note] not a list item", false, "", "", nil, ""},
		{"mid-text parens stay", "- [note] call (urgent) Rosie", true, "note", "call (urgent) Rosie", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ob, ok := parseObservation(strings.TrimSpace(c.line))
			if ok != c.ok {
				t.Fatalf("parseObservation ok=%v, want %v (ob=%+v)", ok, c.ok, ob)
			}
			if !c.ok {
				return
			}
			if ob.Category != c.category || ob.Text != c.text || ob.Context != c.context {
				t.Errorf("ob = %+v, want category=%q text=%q context=%q", ob, c.category, c.text, c.context)
			}
			if !equalStrings(ob.Tags, c.tags) {
				t.Errorf("tags = %v, want %v", ob.Tags, c.tags)
			}
		})
	}
}

func TestScanObservations_SkipsCodeFences(t *testing.T) {
	body := "- [note] real one\n\n" +
		"```\n- [note] fenced not counted\n```\n\n" +
		"- [action] real two #now\n"
	obs, err := scanObservations(body)
	if err != nil {
		t.Fatalf("scanObservations: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("got %d observations, want 2 (fence excluded): %+v", len(obs), obs)
	}
	if obs[0].Category != "note" || obs[1].Category != "action" || obs[1].Text != "real two #now" {
		t.Errorf("observations = %+v", obs)
	}
}

func TestParse_PopulatesObservations(t *testing.T) {
	raw := []byte("# Doc\n\nProse here.\n\n- [decision] ship Q3 #planning (board approved)\n- [note] see [[Rosie]]\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Observations) != 2 {
		t.Fatalf("got %d observations, want 2: %+v", len(p.Observations), p.Observations)
	}
	if p.Observations[0].Category != "decision" || p.Observations[0].Context != "board approved" {
		t.Errorf("obs[0] = %+v", p.Observations[0])
	}
	// The wikilink inside an observation line is still scanned as a link.
	if len(p.Wikilinks) != 1 || p.Wikilinks[0].Target != "Rosie" {
		t.Errorf("wikilinks = %+v, want one [[Rosie]]", p.Wikilinks)
	}
}

// equalStrings compares two string slices for element-wise equality, treating
// nil and empty as equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParse_CRLFFrontmatter(t *testing.T) {
	raw := []byte("---\r\ntitle: Windows Note\r\npinned: true\r\ntags: [alpha]\r\n---\r\n# Heading\r\nSee [[Link]].\r\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Title != "Windows Note" {
		t.Errorf("Title = %q, want %q (CRLF frontmatter must parse)", p.Title, "Windows Note")
	}
	if !p.Pinned {
		t.Error("Pinned = false, want true")
	}
	if len(p.Tags) != 1 || p.Tags[0] != "alpha" {
		t.Errorf("Tags = %v, want [alpha]", p.Tags)
	}
	wantRaw := "---\r\ntitle: Windows Note\r\npinned: true\r\ntags: [alpha]\r\n---\r\n"
	if p.FrontmatterRaw != wantRaw {
		t.Errorf("FrontmatterRaw = %q, want %q", p.FrontmatterRaw, wantRaw)
	}
	if p.Body != string(raw) {
		t.Error("Body must stay verbatim for CRLF files")
	}
	if len(p.Wikilinks) != 1 || p.Wikilinks[0].Target != "Link" {
		t.Errorf("Wikilinks = %+v, want one Link", p.Wikilinks)
	}
}

func TestParse_OversizedLineSurfacesScanError(t *testing.T) {
	// A single line beyond the wikilink scanner's 8 MiB token limit must
	// surface an error (the link graph is partial) instead of returning
	// success with silently missing links.
	huge := strings.Repeat("x", 9*1024*1024)
	raw := []byte("[[First]]\n" + huge + "\n[[After]]\n")
	p, err := Parse(raw)
	if err == nil {
		t.Fatal("expected a scan error for a >8MiB line")
	}
	if p.Body != string(raw) {
		t.Error("Body must still be verbatim on a scan error")
	}
	if len(p.Wikilinks) != 1 || p.Wikilinks[0].Target != "First" {
		t.Errorf("links found before the failure must still be returned; got %+v", p.Wikilinks)
	}
}

func TestParse_StringTagsNormalisedInFrontmatterJSON(t *testing.T) {
	// The stored frontmatter JSON must always carry tags as an array, even
	// when the author used the string form, so storage-side json_each('$.tags')
	// tag filters match these documents.
	p, err := Parse([]byte("---\ntags: alpha, beta\n---\nx\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var fm struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(p.Frontmatter, &fm); err != nil {
		t.Fatalf("frontmatter JSON not decodable: %v", err)
	}
	if len(fm.Tags) != 2 || fm.Tags[0] != "alpha" || fm.Tags[1] != "beta" {
		t.Errorf("frontmatter $.tags = %v, want [alpha beta] (string form must normalise to an array)", fm.Tags)
	}
}

func TestImportDir_RejectsNonMarkdownFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(file, []byte("plain text"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	im := NewImporter(nil, nil, nil) // errors before touching appender/reader
	if _, err := im.ImportDir(context.Background(), kernel.DefaultScope, file); err == nil {
		t.Fatal("importing a .txt file must be rejected")
	}
}

func TestParse_TagsStringForm(t *testing.T) {
	p, err := Parse([]byte("---\ntags: alpha, beta gamma\n---\nx\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(p.Tags) != len(want) {
		t.Fatalf("Tags = %v, want %v", p.Tags, want)
	}
	for i := range want {
		if p.Tags[i] != want[i] {
			t.Errorf("Tags[%d] = %q, want %q", i, p.Tags[i], want[i])
		}
	}
}

// TestParseFrontmatterLinks_AboutProducesDescribes verifies that the "about:"
// frontmatter key emits WikiLinks with RelType="describes", not "mentions".
// This is the core of the about-to-describes fix: the graph edge must express
// high-confidence authorial intent rather than a weak wikilink mention.
func TestParseFrontmatterLinks_AboutProducesDescribes(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		target string
	}{
		{"scalar plain name", "about: Rosie Cotton\n", "Rosie Cotton"},
		{"scalar wikilink", "about: \"[[Rosie Cotton]]\"\n", "Rosie Cotton"},
		{"list plain names", "about:\n  - Rosie Cotton\n", "Rosie Cotton"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := []byte("---\n" + c.yaml + "---\nbody\n")
			p, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			var found bool
			for _, wl := range p.FrontmatterLinks {
				if wl.Target == c.target {
					found = true
					if wl.RelType != predicateDescribes {
						t.Errorf("about: target %q RelType = %q, want %q", c.target, wl.RelType, predicateDescribes)
					}
				}
			}
			if !found {
				t.Errorf("FrontmatterLinks = %+v; want a link with Target=%q", p.FrontmatterLinks, c.target)
			}
		})
	}
}

// TestParseFrontmatterLinks_EntityProducesMentions verifies that the "entity:"
// frontmatter key continues to emit WikiLinks with RelType="mentions" after the
// about-to-describes change. The entity: key anchors the document to a
// reference entity and must not be changed to describes.
func TestParseFrontmatterLinks_EntityProducesMentions(t *testing.T) {
	raw := []byte("---\nentity: self\n---\nbody\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.FrontmatterLinks) != 1 {
		t.Fatalf("FrontmatterLinks = %+v, want exactly 1", p.FrontmatterLinks)
	}
	wl := p.FrontmatterLinks[0]
	if wl.Target != "self" {
		t.Errorf("Target = %q, want self", wl.Target)
	}
	if wl.RelType != predicateMentions {
		t.Errorf("entity: RelType = %q, want %q", wl.RelType, predicateMentions)
	}
}

// TestParse_BodyWikilinkStillMentions is a regression guard: body [[wikilinks]]
// must continue to produce RelType="" (resolved to mentions/links_to by kind)
// after the about-to-describes change; the fix must not bleed into body links.
func TestParse_BodyWikilinkStillMentions(t *testing.T) {
	raw := []byte("---\nabout: Rosie Cotton\n---\nSee [[Mithril Corp]].\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Wikilinks) != 1 || p.Wikilinks[0].Target != "Mithril Corp" {
		t.Fatalf("Wikilinks = %+v, want [[Mithril Corp]]", p.Wikilinks)
	}
	if p.Wikilinks[0].RelType != "" {
		t.Errorf("body wikilink RelType = %q, want empty (default predicate set by resolver)", p.Wikilinks[0].RelType)
	}
	// The frontmatter about: link must still be describes.
	if len(p.FrontmatterLinks) != 1 || p.FrontmatterLinks[0].RelType != predicateDescribes {
		t.Errorf("FrontmatterLinks = %+v, want one describes link", p.FrontmatterLinks)
	}
}
