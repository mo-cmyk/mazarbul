package contextbrief

import (
	"context"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// TestBuildIndex_ListsDocumentsByFolder verifies that BuildIndex groups
// documents under their leading path-prefix folder and includes all expected
// structural sections.
func TestBuildIndex_ListsDocumentsByFolder(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		recent: []navigate.Document{
			{DocumentUID: "D1", Path: "notes/alpha.md", Title: "Alpha"},
			{DocumentUID: "D2", Path: "notes/beta.md", Title: "Beta"},
			{DocumentUID: "D3", Path: "projects/gamma.md", Title: "Gamma"},
			{DocumentUID: "D4", Path: "root.md", Title: "Root"},
		},
		entities: []navigate.Entity{{UID: "E1", Kind: "person", DisplayName: "Rosie"}},
	}
	a := New(reader, nil)
	idx, err := a.BuildIndex(context.Background(), IndexRequest{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	md := idx.Markdown
	for _, want := range []string{
		"# Vault Index",
		"## Recently updated",
		"## Documents by folder",
		"### notes",
		"### projects",
		"### (root)",
		"Alpha",
		"Beta",
		"Gamma",
		"Root",
		"## Key entities",
		"Rosie",
		"entities/E1",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("index missing %q\n---\n%s", want, md)
		}
	}

	if idx.Documents != 4 {
		t.Errorf("Documents = %d, want 4", idx.Documents)
	}
}

// TestBuildIndex_PinnedDocumentsStar verifies that pinned documents get the
// star marker in the index listing.
func TestBuildIndex_PinnedDocumentsStar(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		pinned: []navigate.Document{
			{DocumentUID: "P1", Path: "pinned.md", Title: "Pinned One", Pinned: true},
		},
		recent: []navigate.Document{
			{DocumentUID: "P1", Path: "pinned.md", Title: "Pinned One", Pinned: true},
		},
	}
	a := New(reader, nil)
	idx, err := a.BuildIndex(context.Background(), IndexRequest{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if !strings.Contains(idx.Markdown, "★") {
		t.Errorf("pinned doc missing star marker:\n%s", idx.Markdown)
	}
}

// TestBuildIndex_Determinism verifies two back-to-back index builds produce
// identical markdown bytes (no random maps in the output path).
func TestBuildIndex_Determinism(t *testing.T) {
	t.Parallel()
	docs := []navigate.Document{
		{DocumentUID: "A", Path: "notes/a.md", Title: "A"},
		{DocumentUID: "B", Path: "notes/b.md", Title: "B"},
		{DocumentUID: "C", Path: "projects/c.md", Title: "C"},
	}
	reader := &fakeReader{recent: docs}
	a := New(reader, nil)
	ctx := context.Background()

	first, err := a.BuildIndex(ctx, IndexRequest{})
	if err != nil {
		t.Fatalf("BuildIndex first: %v", err)
	}
	second, err := a.BuildIndex(ctx, IndexRequest{})
	if err != nil {
		t.Fatalf("BuildIndex second: %v", err)
	}
	if first.Markdown != second.Markdown {
		t.Errorf("BuildIndex not deterministic:\nfirst:\n%s\nsecond:\n%s", first.Markdown, second.Markdown)
	}
}

// TestBuildIndex_EmptyVaultStillWorks checks that an empty vault yields a
// valid (empty-section) index without errors.
func TestBuildIndex_EmptyVaultStillWorks(t *testing.T) {
	t.Parallel()
	a := New(&fakeReader{}, nil)
	idx, err := a.BuildIndex(context.Background(), IndexRequest{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if !strings.Contains(idx.Markdown, "# Vault Index") {
		t.Errorf("index title missing:\n%s", idx.Markdown)
	}
	if idx.Documents != 0 {
		t.Errorf("Documents = %d, want 0", idx.Documents)
	}
}

// TestBuildIndex_EnumeratesEveryDocumentInLargeFolder verifies the master
// directory is genuinely complete: a folder with more than the old per-folder
// cap (50) still lists a concrete link for every document, with no truncation
// marker.
func TestBuildIndex_EnumeratesEveryDocumentInLargeFolder(t *testing.T) {
	t.Parallel()
	const n = 120
	docs := make([]navigate.Document, 0, n)
	for i := range n {
		docs = append(docs, navigate.Document{
			DocumentUID: "U" + itoa(i),
			Path:        "notes/doc-" + itoa(i) + ".md",
			Title:       "Doc " + itoa(i),
		})
	}
	a := New(&fakeReader{recent: docs}, nil)
	idx, err := a.BuildIndex(context.Background(), IndexRequest{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	md := idx.Markdown
	for i := range n {
		want := "notes/doc-" + itoa(i) + ".md"
		if !strings.Contains(md, want) {
			t.Errorf("index omits document %q (directory must enumerate every doc)", want)
		}
	}
	if strings.Contains(md, "more documents in this folder") {
		t.Errorf("index still truncates a folder; the master directory must be complete:\n%s", md)
	}
}

// TestWriteIndexDocLine_EscapesLinkSyntax verifies that titles containing ']'
// and paths containing ')' or spaces do not produce malformed Markdown links.
func TestWriteIndexDocLine_EscapesLinkSyntax(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	d := navigate.Document{
		Title: "Weird ] title [x]",
		Path:  "notes/has (paren) and space.md",
	}
	writeIndexDocLine(&b, &d)
	got := b.String()
	// The raw ']' in the title must be backslash-escaped so it cannot close the
	// link label early.
	if strings.Contains(got, "] title") && !strings.Contains(got, `\] title`) {
		t.Errorf("link label not escaped: %q", got)
	}
	// A path with spaces/parens must be angle-wrapped.
	if !strings.Contains(got, "<notes/has (paren) and space.md>") {
		t.Errorf("link destination not angle-wrapped: %q", got)
	}
}

// TestEscapeMarkdownLinkDestination keeps the destination-escaping contract
// pinned: plain paths pass through; spaces/parens trigger angle-wrapping.
func TestEscapeMarkdownLinkDestination(t *testing.T) {
	t.Parallel()
	if got := escapeMarkdownLinkDestination("notes/foo.md"); got != "notes/foo.md" {
		t.Errorf("plain path mutated: %q", got)
	}
	if got := escapeMarkdownLinkDestination("a b.md"); got != "<a b.md>" {
		t.Errorf("spaced path = %q, want <a b.md>", got)
	}
}

// itoa is a tiny local int-to-string to avoid importing strconv just for tests.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestLeadingFolder pins the path-to-folder extraction logic.
func TestLeadingFolder(t *testing.T) {
	t.Parallel()
	cases := []struct{ path, want string }{
		{"notes/alpha.md", "notes"},
		{"a/b/c.md", "a"},
		{"root.md", ""},
		{"index.md", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := leadingFolder(c.path); got != c.want {
			t.Errorf("leadingFolder(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
