package cli

// doc_browse.go — `maz doc browse` command and its browse.Loader/Editor/LinkFollower
// implementation.  All data access routes through invokeNavTool (daemon or
// in-process), so the command works identically against a live daemon and in
// integration tests.
//
// Reload-after-edit note: browse.go (the TUI package, a separate lane) does
// not currently have a handler for an "edit-complete reload" message type.
// After an Edit() command completes, the detail card will not auto-refresh.
// The user can press Esc and Enter on the same item to re-open the updated
// detail.  This seam is flagged to the orchestrator so browse.go can be
// extended without touching this file.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/tui/browse"
)

// ── compile-time interface assertions ─────────────────────────────────────────

var (
	_ browse.Loader       = (*docBrowseLoader)(nil)
	_ browse.Editor       = (*docBrowseLoader)(nil)
	_ browse.LinkFollower = (*docBrowseLoader)(nil)
)

// ── loader ────────────────────────────────────────────────────────────────────

// docBrowseLoader implements browse.Loader, browse.Editor, and
// browse.LinkFollower for `maz doc browse`.  All navigator round-trips go
// through the invoker field, which defaults to invokeNavTool so production
// code requires no extra wiring while tests can inject a canned response.
type docBrowseLoader struct {
	cmd *cobra.Command
	// invoker is a test seam over invokeNavTool.  A nil value means "use the
	// real invokeNavTool".
	invoker func(*cobra.Command, string, json.RawMessage) (json.RawMessage, error)
}

// invoke dispatches a navigation tool, using the injected invoker when set.
func (l *docBrowseLoader) invoke(toolName string, body json.RawMessage) (json.RawMessage, error) {
	if l.invoker != nil {
		return l.invoker(l.cmd, toolName, body)
	}
	return invokeNavTool(l.cmd, toolName, body)
}

// ── wire shapes (only the fields browse needs) ────────────────────────────────

// docBrowseWireDoc is the partial decode of a documentWire returned by
// listDocuments / readDocument.
type docBrowseWireDoc struct {
	Name        string   `json:"name"`        // entities/{uid}
	DocumentUID string   `json:"documentUid"` // bare UID
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	Pinned      bool     `json:"pinned"`
	Tags        []string `json:"tags"`
	Body        string   `json:"body"`
}

// docBrowseLinkWire is the partial decode of a docLinkWire JSON edge.
type docBrowseLinkWire struct {
	Predicate   string `json:"predicate"`
	Name        string `json:"name"` // entities/{uid}
	DisplayName string `json:"displayName"`
}

// ── browse.Loader ─────────────────────────────────────────────────────────────

// Search calls listDocuments and maps each result to a browse.Record.
// PathPrefix from f overrides the query prefix when set.
// Tag and Pinned from f are forwarded verbatim.
//
//nolint:gocritic // hugeParam: browse.Filters is passed by value to satisfy the browse.Loader interface
func (l *docBrowseLoader) Search(query string, f browse.Filters) ([]browse.Record, error) {
	in := map[string]any{"limit": 100}
	prefix := query
	if f.PathPrefix != "" {
		prefix = f.PathPrefix
	}
	if prefix != "" {
		in["pathPrefix"] = prefix
	}
	if f.Tag != "" {
		in["tag"] = f.Tag
	}
	if f.Pinned {
		in["pinned"] = true
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("doc browse search: %w", err)
	}
	raw, err := l.invoke("listDocuments", reqBody)
	if err != nil {
		return nil, fmt.Errorf("doc browse search: %w", err)
	}
	var resp struct {
		Documents []docBrowseWireDoc `json:"documents"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("doc browse search: parse: %w", err)
	}
	records := make([]browse.Record, 0, len(resp.Documents))
	for _, d := range resp.Documents {
		title := d.Title
		if title == "" {
			title = d.Path
		}
		records = append(records, browse.Record{
			ID:       d.Path,
			Title:    title,
			Subtitle: docBrowseSubtitle(d),
			Kind:     entityKindDocument,
		})
	}
	return records, nil
}

// docBrowseUnknownLabel is shown for dangling links that carry no display name.
const docBrowseUnknownLabel = "(unknown)"

// docBrowseSubtitle formats the compact subtitle line shown in the master list.
// Format: "<dir> · [★ pinned ·] [tags]".
//
//nolint:gocritic // hugeParam: docBrowseWireDoc is passed by value; callers hold range-loop copies from JSON decode
func docBrowseSubtitle(d docBrowseWireDoc) string {
	var parts []string
	dir := filepath.Dir(d.Path)
	if dir != "." {
		parts = append(parts, dir)
	}
	if d.Pinned {
		parts = append(parts, "★ pinned")
	}
	if len(d.Tags) > 0 {
		parts = append(parts, strings.Join(d.Tags, " "))
	}
	return strings.Join(parts, " · ")
}

// Detail calls readDocument and returns a plain-markdown string for the detail
// card.  The string is rendered by browse.go via mdrender.Render (glamour), so
// it must not contain raw ANSI escape sequences.
//
// The detail body has three sections:
//  1. The document title (H1) + body verbatim.
//  2. "## Links" — outgoing wikilinks; → resolved, ⚠ dangling (no displayName).
//  3. "## Linked from" — incoming backlinks with the same affordances.
func (l *docBrowseLoader) Detail(id string) (string, error) {
	var reqBody json.RawMessage
	var merr error
	if strings.HasPrefix(id, "entities/") {
		uid := strings.TrimPrefix(id, "entities/")
		reqBody, merr = json.Marshal(map[string]any{"documentUid": uid})
	} else {
		reqBody, merr = json.Marshal(map[string]any{"path": id})
	}
	if merr != nil {
		return "", fmt.Errorf("doc browse detail: %w", merr)
	}
	raw, err := l.invoke("readDocument", reqBody)
	if err != nil {
		return "", fmt.Errorf("doc browse detail: %w", err)
	}
	var resp struct {
		Document        docBrowseWireDoc    `json:"document"`
		Links           []docBrowseLinkWire `json:"links"`
		Backlinks       []docBrowseLinkWire `json:"backlinks"`
		UnresolvedLinks []string            `json:"unresolvedLinks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("doc browse detail: parse: %w", err)
	}
	return docBrowseDetailMarkdown(resp.Document, resp.Links, resp.Backlinks, resp.UnresolvedLinks), nil
}

// docBrowseDetailMarkdown composes the markdown string for the detail card from
// the document body and its wikilink edges. Dangling wikilinks (unresolved
// targets, which readDocument returns separately because they have no edge) are
// listed in the Links section with the warning glyph, so a reader navigating the
// detail learns the note has a broken reference — matching `maz doc lint`.
//
//nolint:gocritic // hugeParam: docBrowseWireDoc is passed by value; caller holds decoded resp.Document copy
func docBrowseDetailMarkdown(doc docBrowseWireDoc, links, backlinks []docBrowseLinkWire, unresolved []string) string {
	var sb strings.Builder
	if doc.Title != "" {
		sb.WriteString("# " + doc.Title + "\n\n")
	}
	// readDocument returns the verbatim body, frontmatter included. Strip the
	// leading YAML block so glamour renders prose only — otherwise the `---`
	// fences become horizontal rules and `tags:`/`title:` lines leak in as text
	// (a duplicate of the H1 above).
	if body := stripDocFrontmatter(doc.Body); body != "" {
		sb.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}
	if len(links) > 0 || len(unresolved) > 0 {
		sb.WriteString("## Links\n\n")
		for _, lk := range links {
			sb.WriteString(docBrowseLinkLine(lk, "→", "⚠"))
		}
		for _, target := range unresolved {
			sb.WriteString("- ⚠ " + target + " (unresolved)\n")
		}
		sb.WriteByte('\n')
	}
	if len(backlinks) > 0 {
		sb.WriteString("## Linked from\n\n")
		for _, bl := range backlinks {
			sb.WriteString(docBrowseLinkLine(bl, "←", "⚠"))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// stripDocFrontmatter removes a leading YAML frontmatter block from a document
// body so the detail card shows prose only. It reuses document.Parse so the
// fence-detection rules match the importer exactly (no second implementation to
// drift); a body with no frontmatter is returned unchanged.
func stripDocFrontmatter(body string) string {
	parsed, _ := document.Parse([]byte(body)) //nolint:errcheck // partial result is fine: only FrontmatterRaw is used
	rest := strings.TrimPrefix(parsed.Body, parsed.FrontmatterRaw)
	return strings.TrimLeft(rest, "\r\n")
}

// docBrowseLinkLine formats one link row.
// Resolved links (have a displayName) use the resolvedGlyph; dangling ones
// (no displayName) use the danglingGlyph so the difference is visible without
// colour.
func docBrowseLinkLine(lk docBrowseLinkWire, resolvedGlyph, danglingGlyph string) string {
	if lk.DisplayName != "" {
		return "- " + resolvedGlyph + " " + lk.DisplayName + "\n"
	}
	// Dangling: no resolved display name; show the raw name/uid with a warning glyph.
	label := lk.Name
	if label == "" {
		label = docBrowseUnknownLabel
	}
	return "- " + danglingGlyph + " " + label + "\n"
}

// Neighbors returns the document's outgoing + incoming links (from
// getBacklinks) as browse.Record values for the neighbor side panel.
func (l *docBrowseLoader) Neighbors(id string) ([]browse.Record, error) {
	return l.fetchLinkRecords(id, true)
}

// Links returns only the outgoing wikilinks for the L-key link-follow.
// Implements browse.LinkFollower.
func (l *docBrowseLoader) Links(id string) []browse.Record {
	recs, err := l.fetchLinkRecords(id, false)
	if err != nil {
		return nil // LinkFollower.Links has no error return; drop silently
	}
	return recs
}

// fetchLinkRecords calls getBacklinks and returns the link/backlink records.
// When includeBacklinks is false, only outgoing links are returned (for
// browse.LinkFollower.Links).
func (l *docBrowseLoader) fetchLinkRecords(id string, includeBacklinks bool) ([]browse.Record, error) {
	var reqBody json.RawMessage
	var merr error
	if strings.HasPrefix(id, "entities/") {
		uid := strings.TrimPrefix(id, "entities/")
		reqBody, merr = json.Marshal(map[string]any{"documentUid": uid})
	} else {
		reqBody, merr = json.Marshal(map[string]any{"path": id})
	}
	if merr != nil {
		return nil, fmt.Errorf("doc browse links: %w", merr)
	}
	raw, err := l.invoke("getBacklinks", reqBody)
	if err != nil {
		return nil, fmt.Errorf("doc browse links: %w", err)
	}
	var resp struct {
		Links     []docBrowseLinkWire `json:"links"`
		Backlinks []docBrowseLinkWire `json:"backlinks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("doc browse links: parse: %w", err)
	}
	cap := len(resp.Links)
	if includeBacklinks {
		cap += len(resp.Backlinks)
	}
	records := make([]browse.Record, 0, cap)
	for _, lk := range resp.Links {
		records = append(records, docBrowseLinkRecord(lk, "→"))
	}
	if includeBacklinks {
		for _, bl := range resp.Backlinks {
			records = append(records, docBrowseLinkRecord(bl, "←"))
		}
	}
	return records, nil
}

// docBrowseLinkRecord converts one docBrowseLinkWire to a browse.Record.
func docBrowseLinkRecord(lk docBrowseLinkWire, arrow string) browse.Record {
	title := lk.DisplayName
	if title == "" {
		title = lk.Name
	}
	return browse.Record{
		ID:       lk.Name,
		Title:    title,
		Subtitle: arrow + " " + lk.Predicate,
		Kind:     "document",
	}
}

// ── browse.Editor ─────────────────────────────────────────────────────────────

// Edit opens the document identified by id (a vault-relative path) in $EDITOR.
// On save, the updated body is re-ingested via the writeDocument tool.
//
// Returns a tea.Cmd using tea.ExecProcess so the alt-screen is properly
// suspended and restored around the editor session.
//
// When id is an entities/ resource name (a neighbor record rather than a
// primary search result), Edit is a no-op: those records do not have an
// editable vault path available without an additional readDocument round-trip.
func (l *docBrowseLoader) Edit(id string) tea.Cmd {
	if strings.HasPrefix(id, "entities/") {
		return func() tea.Msg { return nil }
	}
	file, err := resolveVaultFileFn(l.cmd, id)
	if err != nil {
		return func() tea.Msg { return nil }
	}
	var before []byte
	if b, rerr := os.ReadFile(file); rerr == nil {
		before = b // capture prior content; unreadable treated as empty
	}

	edName := resolveEditor()
	exeName, exeArgs := editorCommand(edName, file)
	ctx := l.cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	execCmd := exec.CommandContext(ctx, exeName, exeArgs...)

	docPath := id
	return tea.ExecProcess(execCmd, func(_ error) tea.Msg {
		after, rerr := os.ReadFile(file)
		if rerr != nil || bytes.Equal(before, after) {
			return nil
		}
		reqBody, merr := json.Marshal(map[string]any{"path": docPath, "body": string(after)})
		if merr != nil {
			return nil
		}
		// Write-behind inside a tea.ExecProcess callback; no caller to propagate errors to.
		if _, werr := l.invoke("writeDocument", reqBody); werr != nil {
			return nil
		}
		// browse.go has no reload-detail msg handler; detail refreshes when the
		// user re-opens the item (esc → enter).  See package comment.
		return nil
	})
}

// newNoteSeed is the scaffold written to a freshly created note before the
// editor opens. An unchanged seed (editor exited without saving) is discarded
// rather than persisted as a blank document.
const newNoteSeed = "# New Note\n\n"

// newNoteIsDiscardable reports whether a freshly created note's editor buffer
// holds no real content — blank/whitespace-only or still byte-identical to the
// seed scaffold. Such a note is removed rather than persisted (n3).
func newNoteIsDiscardable(body string) bool {
	return strings.TrimSpace(body) == "" || body == newNoteSeed
}

// New opens $EDITOR on a timestamp-generated path under notes/ and ingests the
// result.  The path is auto-generated (notes/YYYY-MM-DD-HHmmss.md) so v1
// avoids any interactive path-prompt that would fight the tea event loop.
//
// Returns a tea.Cmd using tea.ExecProcess.
func (l *docBrowseLoader) New() tea.Cmd {
	path := fmt.Sprintf("notes/%s.md", time.Now().Format("2006-01-02-150405"))
	file, err := resolveVaultFileFn(l.cmd, path)
	if err != nil {
		return func() tea.Msg { return nil }
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(file), 0o700); mkdirErr != nil {
		return func() tea.Msg { return nil }
	}
	if writeErr := os.WriteFile(file, []byte(newNoteSeed), 0o600); writeErr != nil {
		return func() tea.Msg { return nil }
	}

	edName := resolveEditor()
	exeName, exeArgs := editorCommand(edName, file)
	ctx := l.cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	execCmd := exec.CommandContext(ctx, exeName, exeArgs...)

	return tea.ExecProcess(execCmd, func(_ error) tea.Msg {
		body, rerr := os.ReadFile(file)
		// Discard the stub when the editor exited without writing meaningful
		// content: unreadable, blank, or still byte-identical to the seed (e.g.
		// EDITOR=true / the user quit without saving). Only a changed,
		// non-empty note is persisted.
		if rerr != nil || newNoteIsDiscardable(string(body)) {
			if remErr := os.Remove(file); remErr != nil && !os.IsNotExist(remErr) {
				return nil // best-effort cleanup; ignore if file is already gone
			}
			return nil
		}
		reqBody, merr := json.Marshal(map[string]any{"path": path, "body": string(body)})
		if merr != nil {
			return nil
		}
		// Write-behind inside a tea.ExecProcess callback; no caller to propagate errors to.
		if _, werr := l.invoke("writeDocument", reqBody); werr != nil {
			return nil
		}
		return nil
	})
}

// ── command ───────────────────────────────────────────────────────────────────

// docBrowseBanks returns the filter-strip banks for `maz doc browse`.
// A single Pinned toggle lets the user narrow the list to pinned documents.
// A Tag bank would require an extra listDocuments call to enumerate available
// tags, which is non-trivial at browse startup — omitted in v1.
func docBrowseBanks() []browse.Bank {
	return []browse.Bank{
		{
			Label:   "Pinned",
			Kind:    browse.BankToggle,
			Options: []string{"pinned"},
			Key:     "pinned",
		},
	}
}

// newDocBrowseCmd returns the `maz doc browse [pathPrefix]` subcommand.
func newDocBrowseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "browse [pathPrefix]",
		Short: "Browse vault documents in an interactive two-pane TUI",
		Long: `Open an interactive two-pane document browser (requires a TTY).

The left pane lists documents matching an optional path prefix. The right pane
shows the selected document's title, body, and wikilink sections (Links and
Linked from).  Navigate with arrow keys; press enter to open the detail view.

Keys available in the browser:

  e      edit the selected document in $EDITOR (re-ingests on save)
  n      create a new note in $EDITOR (auto-generated path: notes/YYYY-MM-DD-HHmmss.md)
  L      show the selected document's outgoing wikilinks in the list
  tab    cycle focus to the filter strip (Pinned toggle)
  esc    return to list / quit
  q      quit

The filter strip (tab to focus) has a Pinned toggle that narrows the list to
pinned documents.

Examples:
  maz doc browse
  maz doc browse notes/
  maz doc browse project-alpha`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive.IsTTY(cmd.OutOrStdout()) || output.From(cmd).Format() != output.FormatText {
				fmt.Fprintln(cmd.ErrOrStderr(), "doc browse: requires an interactive terminal; use maz doc ls for scripted listing") //nolint:errcheck // best-effort advisory
				return silentError{code: 1, err: errors.New("not a TTY")}
			}
			prefix := ""
			if len(args) == 1 {
				prefix = args[0]
			}
			loader := &docBrowseLoader{cmd: cmd}
			enabled := interactive.ShouldStyle(cmd, cmd.OutOrStdout())
			// Silence the per-call daemon-dispatch advisory while the alt-screen
			// owns the terminal (C1): Enter/L/Pinned each trigger a daemon round
			// trip whose stderr advisory would otherwise corrupt the frame.
			restoreErr := suppressCmdStderr(cmd)
			berr := browse.RunWithStrip(loader, prefix, enabled, docBrowseBanks())
			restoreErr()
			if berr != nil {
				return wrapNavFatal(cmd, "doc browse", berr)
			}
			return nil
		},
	}
}
