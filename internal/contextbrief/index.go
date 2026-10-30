package contextbrief

// index.go — BuildIndex assembles a deterministic, LLM-free master DIRECTORY
// listing of the vault (index.md). This is DISTINCT from Build (brief/hot.md):
//
//   - hot.md = token-budgeted BRIEF (Build output) — pinned → recent →
//     entities → relevant; the "working-set cache" an agent loads at session
//     open.
//   - index.md = master DIRECTORY — a human-navigable listing of EVERY vault
//     document grouped by path-prefix folder, with a "recently updated"
//     section and a compact key-entity roll-up. Lighter per-entry, more
//     complete enumeration; NOT budget-gated.
//
// BuildIndex is deterministic (no LLM, no timestamps unless injected) and
// cheap: it reads list projections over the SQLite reader pool, the same
// projections ListDocuments/ListEntities use everywhere in the read path.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// Index section caps — all overridable via IndexRequest fields.
const (
	indexRecentLimit = 20 // Recent documents in the "Recently updated" section.
	indexEntityLimit = 30 // Key entities in the entity roll-up.
)

// IndexRequest configures one index build.
type IndexRequest struct {
	// RecentLimit overrides indexRecentLimit (0 → default).
	RecentLimit int
	// EntityLimit overrides indexEntityLimit (0 → default).
	EntityLimit int
}

// Index is the assembled vault directory.
type Index struct {
	// Markdown is the full index.md content.
	Markdown string `json:"markdown"`
	// Documents is the total number of live vault documents enumerated.
	Documents int `json:"documents"`
	// Folders is the number of distinct path-prefix groups.
	Folders int `json:"folders"`
}

// BuildIndex assembles a master vault directory from the unified graph.
// Deterministic and LLM-free: it reads document + entity projections and
// groups them by path-prefix folder. No token budget is applied; the result
// is always complete.
func (a *Assembler) BuildIndex(ctx context.Context, req IndexRequest) (Index, error) {
	recentLimit := limitOr(req.RecentLimit, indexRecentLimit)
	entityLimit := limitOr(req.EntityLimit, indexEntityLimit)

	// Fetch all live documents (no body needed; listing is path+title+pinned).
	allDocs, err := a.reader.ListDocuments(ctx, navigate.DocumentFilter{Limit: -1})
	if err != nil {
		return Index{}, fmt.Errorf("contextbrief/index: list documents: %w", err)
	}

	// Fetch recently-updated documents (separate query so the recency order is
	// preserved independently of the folder grouping below).
	recentDocs, err := a.reader.ListDocuments(ctx, navigate.DocumentFilter{Limit: recentLimit})
	if err != nil {
		return Index{}, fmt.Errorf("contextbrief/index: list recent: %w", err)
	}

	// Fetch key entities for the roll-up.
	ents, err := a.reader.ListEntities(ctx, navigate.EntityFilter{Limit: entityLimit})
	if err != nil {
		return Index{}, fmt.Errorf("contextbrief/index: list entities: %w", err)
	}

	md := buildIndexMarkdown(a.nowText, allDocs, recentDocs, ents)
	return Index{
		Markdown:  md,
		Documents: len(allDocs),
		Folders:   countFolders(allDocs),
	}, nil
}

// buildIndexMarkdown renders the markdown directory from the gathered data.
// Separated from BuildIndex so it can be tested without a live store.
func buildIndexMarkdown(nowText string, allDocs, recentDocs []navigate.Document, ents []navigate.Entity) string {
	var b strings.Builder

	b.WriteString("# Vault Index\n")
	if nowText != "" {
		fmt.Fprintf(&b, "\n_Directory generated %s._\n", nowText)
	}

	buildIndexRecentSection(&b, recentDocs)
	buildIndexFolderSection(&b, allDocs)
	buildIndexEntitySection(&b, ents)

	return b.String()
}

// buildIndexRecentSection appends the "Recently updated" section.
func buildIndexRecentSection(b *strings.Builder, recentDocs []navigate.Document) {
	if len(recentDocs) == 0 {
		return
	}
	b.WriteString("\n## Recently updated\n")
	for i := range recentDocs {
		writeIndexDocLine(b, &recentDocs[i])
	}
}

// buildIndexFolderSection appends the "Documents by folder" section.
func buildIndexFolderSection(b *strings.Builder, allDocs []navigate.Document) {
	groups, order := groupByFolder(allDocs)
	if len(order) == 0 {
		return
	}
	b.WriteString("\n## Documents by folder\n")
	for _, folder := range order {
		writeFolderGroup(b, folder, groups[folder])
	}
}

// writeFolderGroup appends one folder heading and its complete document list.
// The index is the master directory: it is NOT budget-gated, so every document
// in the folder is enumerated (no per-folder cap) — an agent or human must be
// able to navigate to any vault document from this artifact.
func writeFolderGroup(b *strings.Builder, folder string, docs []navigate.Document) {
	if folder == "" {
		b.WriteString("\n### (root)\n")
	} else {
		fmt.Fprintf(b, "\n### %s\n", escapeMarkdownText(folder))
	}
	for i := range docs {
		writeIndexDocLine(b, &docs[i])
	}
}

// buildIndexEntitySection appends the "Key entities" section.
func buildIndexEntitySection(b *strings.Builder, ents []navigate.Entity) {
	if len(ents) == 0 {
		return
	}
	b.WriteString("\n## Key entities\n")
	for i := range ents {
		name := ents[i].DisplayName
		if name == "" {
			name = ents[i].UID
		}
		fmt.Fprintf(b, "- **%s** (%s) — `entities/%s`\n", escapeMarkdownText(name), ents[i].Kind, ents[i].UID)
	}
}

// writeIndexDocLine appends one document bullet to b. Pinned docs are starred.
// The link label (title) and destination (path) are escaped so a title
// containing ']' or a path containing ')' or spaces cannot break the generated
// Markdown link.
func writeIndexDocLine(b *strings.Builder, d *navigate.Document) {
	title := d.Title
	if title == "" {
		title = d.Path
	}
	label := escapeMarkdownLinkLabel(title)
	dest := escapeMarkdownLinkDestination(d.Path)
	if d.Pinned {
		fmt.Fprintf(b, "- [%s](%s) ★\n", label, dest)
	} else {
		fmt.Fprintf(b, "- [%s](%s)\n", label, dest)
	}
}

// escapeMarkdownText escapes the characters that would otherwise be interpreted
// as Markdown emphasis/structure inside plain inline text (headings, bold
// labels). It is conservative: it backslash-escapes the inline metacharacters
// that can corrupt a heading or **bold** span. Newlines are collapsed to spaces
// so a stray newline in a stored title cannot split the line.
func escapeMarkdownText(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
	)
	return r.Replace(s)
}

// escapeMarkdownLinkLabel escapes a string for safe use as the label inside
// `[label](dest)`. Brackets are escaped (an unescaped ']' would close the label
// early) and newlines collapsed to spaces.
func escapeMarkdownLinkLabel(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := strings.NewReplacer(
		`\`, `\\`,
		"[", `\[`,
		"]", `\]`,
	)
	return r.Replace(s)
}

// escapeMarkdownLinkDestination escapes a string for safe use as the
// destination inside `[label](dest)`. A destination containing spaces or
// parentheses is wrapped in angle brackets (the CommonMark form that tolerates
// both), with any literal '<', '>', or newline escaped/stripped so the wrapper
// cannot be broken out of.
func escapeMarkdownLinkDestination(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if !strings.ContainsAny(s, " ()<>") {
		return s
	}
	r := strings.NewReplacer(
		`\`, `\\`,
		"<", `\<`,
		">", `\>`,
	)
	return "<" + r.Replace(s) + ">"
}

// groupByFolder partitions docs by their leading path segment. Returns a
// map (folder → docs) and a deterministically sorted slice of folder names.
// The root group (documents with no slash in their path) is keyed by "".
func groupByFolder(docs []navigate.Document) (map[string][]navigate.Document, []string) {
	groups := map[string][]navigate.Document{}
	seen := map[string]struct{}{}

	for i := range docs {
		folder := leadingFolder(docs[i].Path)
		groups[folder] = append(groups[folder], docs[i])
		seen[folder] = struct{}{}
	}

	order := make([]string, 0, len(seen))
	for f := range seen {
		order = append(order, f)
	}
	sort.Strings(order)
	return groups, order
}

// leadingFolder returns the first path segment of p before the first slash, or
// "" when p contains no slash. Examples: "notes/foo.md" → "notes",
// "index.md" → "", "a/b/c.md" → "a".
func leadingFolder(p string) string {
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

// countFolders returns the number of distinct path-prefix groups in docs.
func countFolders(docs []navigate.Document) int {
	seen := map[string]struct{}{}
	for i := range docs {
		seen[leadingFolder(docs[i].Path)] = struct{}{}
	}
	return len(seen)
}
