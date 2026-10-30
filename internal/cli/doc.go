package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// newDocCmd is the `maz doc` group: the markdown-vault surface. Subcommands
// route through the same daemon-or-in-process tool dispatch as the navigation
// verbs (importDocuments / listDocuments / readDocument / getBacklinks).
func newDocCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "doc",
		// "notes" is a discoverability alias: new users reach for `maz notes`
		// before learning the vault is the `doc` surface.
		Aliases: []string{"notes"},
		Short:   "Work with markdown vault documents (aka notes)",
		Long: `Import, list, read, and inspect the links of markdown vault documents.

Documents are first-class citizens of the same knowledge graph as structured
memory: [[wikilinks]] resolve to canonical entities (a note about a person links
to that Person) and to other documents, so a document and a fact about the same
entity are two edges on one node.`,
		// Bare `maz doc` shows help; an unknown subcommand (e.g. `doc foo`) errors
		// via NoArgs rather than printing help and exiting 0.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newDocImportCmd())
	cmd.AddCommand(newDocLsCmd())
	cmd.AddCommand(newDocShowCmd())
	cmd.AddCommand(newDocLinksCmd())
	cmd.AddCommand(newDocLogCmd())
	cmd.AddCommand(newDocDiffCmd())
	cmd.AddCommand(newDocNewCmd())
	cmd.AddCommand(newDocEditCmd())
	cmd.AddCommand(newDocPathCmd())
	cmd.AddCommand(newDocOpenCmd())
	cmd.AddCommand(newDocSyncCmd())
	cmd.AddCommand(newDocLintCmd())
	cmd.AddCommand(newDocRmCmd())
	cmd.AddCommand(newDocMvCmd())
	cmd.AddCommand(newDocCanvasCmd())
	cmd.AddCommand(newDocBrowseCmd())
	cmd.AddCommand(newDocRehomeCmd())
	return cmd
}

// ---- doc import ----

func newDocImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <path>",
		Short: "Import a markdown file or vault directory",
		Long: `Import a markdown file, or a directory of them (recursive), as event-sourced
documents. Wikilinks resolve to canonical entities and other documents; the
import is idempotent by content hash, so re-running it skips unchanged files.

Imported document bodies are indexed for "maz recall" (FTS5 + vector), so
recall queries will surface document hits alongside episodes and entities.

Examples:
  maz doc import ~/notes
  maz doc import ~/notes/project-alpha.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve to an absolute path: the import runs in the daemon, whose
			// working directory differs from the user's shell.
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return wrapNavFatal(cmd, "doc import", err)
			}
			reqBody, err := json.Marshal(map[string]any{"path": abs})
			if err != nil {
				return wrapNavFatal(cmd, "doc import", err)
			}
			raw, err := invokeNavTool(cmd, "importDocuments", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "doc import", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDocImport(cmd, raw)
		},
	}
}

func renderDocImport(cmd *cobra.Command, raw json.RawMessage) error {
	var res struct {
		Imported, Skipped, Edges, Unresolved, Reconciled int
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("doc import: parse response: %w", err)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"imported %d, skipped %d (unchanged), edges %d, unresolved %d, reconciled %d\n",
		res.Imported, res.Skipped, res.Edges, res.Unresolved, res.Reconciled)
	return err
}

// ---- doc ls ----

func newDocLsCmd() *cobra.Command {
	var (
		prefix string
		tag    string
		pinned bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List vault documents",
		Long:  "List vault documents, most-recently-updated first. Bodies are omitted; use `maz doc show` for one document's full body.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := map[string]any{}
			if prefix != "" {
				in["pathPrefix"] = prefix
			}
			if tag != "" {
				in["tag"] = tag
			}
			if pinned {
				in["pinned"] = true
			}
			if limit > 0 {
				in["limit"] = limit
			}
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "doc ls", err)
			}
			raw, err := invokeNavTool(cmd, "listDocuments", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "doc ls", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDocLs(cmd, raw)
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "restrict to documents whose path starts with this prefix")
	cmd.Flags().StringVar(&tag, "tag", "", "restrict to documents whose frontmatter tags include this value")
	cmd.Flags().BoolVar(&pinned, "pinned", false, "only pinned documents")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum documents to return")
	return cmd
}

func renderDocLs(cmd *cobra.Command, raw json.RawMessage) error {
	var resp struct {
		Documents []struct {
			Path   string `json:"path"`
			Title  string `json:"title"`
			Pinned bool   `json:"pinned"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("doc ls: parse response: %w", err)
	}
	if len(resp.Documents) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no documents")
		return err
	}
	c := cmdColorizer(cmd)
	return output.MaybePage(cmd, func(pw io.Writer) error {
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, []output.Column{
			{Title: "path"},
			{Title: "title"},
			{Title: "pinned"},
		})
		for _, d := range resp.Documents {
			pin := ""
			if d.Pinned {
				pin = "★"
			}
			tbl.Row(d.Path, d.Title, pin)
		}
		return tbl.Render()
	})
}

// ---- doc reference resolution ----

// docResolveRow is the partial listDocuments decode used to resolve a document
// reference to its canonical path.
type docResolveRow struct {
	Path  string `json:"path"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// matchDocRef resolves a user-supplied reference against the document list. An
// exact path or slug match wins outright; otherwise a case-insensitive title
// match is used, which may be ambiguous. Returns the canonical path on a unique
// match, "" with a nil candidates slice on no match (the caller falls back to
// the literal ref so the tool emits its own not-found error), or "" with a
// populated candidates slice when a title resolves to more than one document.
func matchDocRef(ref string, docs []docResolveRow) (path string, candidates []string) {
	lc := strings.ToLower(strings.TrimSpace(ref))
	withMD := lc
	if !strings.HasSuffix(withMD, ".md") {
		withMD += ".md"
	}
	for _, d := range docs {
		dp := strings.ToLower(d.Path)
		if dp == lc || dp == withMD || strings.EqualFold(d.Slug, lc) {
			return d.Path, nil
		}
	}
	var matches []string
	for _, d := range docs {
		if strings.EqualFold(d.Title, lc) {
			matches = append(matches, d.Path)
		}
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		return "", matches
	}
}

// resolveDocRef maps a document reference — a vault-relative path, a slug, or a
// human title (as shown in `doc ls`) — to its canonical path, so `doc show` and
// `doc links` accept the same identifier a user sees, mirroring how `maz get`
// resolves display names. It returns the original ref unchanged when it cannot
// list documents or finds no match (the downstream tool then produces its own
// path-based error); an ambiguous title returns an E_VALIDATION error naming the
// candidates.
func resolveDocRef(cmd *cobra.Command, tool, ref string) (string, error) {
	raw, err := invokeNavTool(cmd, "listDocuments", json.RawMessage(`{}`))
	if err != nil || raw == nil {
		return ref, nil //nolint:nilerr // best-effort resolve: on a list failure fall back to the literal ref so the downstream tool emits its own error
	}
	var resp struct {
		Documents []docResolveRow `json:"documents"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return ref, nil //nolint:nilerr // same best-effort fallback on a malformed list response
	}
	path, candidates := matchDocRef(ref, resp.Documents)
	if len(candidates) > 0 {
		msg := fmt.Sprintf("%q matches %d documents (%s); pass the exact path", ref, len(candidates), strings.Join(candidates, ", "))
		return "", wrapNavFatal(cmd, tool, mcp.NewValidationError(msg, nil))
	}
	if path == "" {
		return ref, nil
	}
	return path, nil
}

// ---- doc show ----

func newDocShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <path|slug|title>",
		Short: "Print a document's body and links",
		Long:  "Print a document's title, body, and its wikilink edges (links + backlinks). Identify the document by its vault-relative path, its slug, or its human title (the same forms shown in `maz doc ls`).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, rerr := resolveDocRef(cmd, "doc show", args[0])
			if rerr != nil {
				return rerr
			}
			reqBody, err := json.Marshal(map[string]any{"path": ref})
			if err != nil {
				return wrapNavFatal(cmd, "doc show", err)
			}
			raw, err := invokeNavTool(cmd, "readDocument", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "doc show", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDocShow(cmd, raw)
		},
	}
}

func renderDocShow(cmd *cobra.Command, raw json.RawMessage) error {
	var resp struct {
		Document struct {
			Path  string `json:"path"`
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"document"`
		Links           []docLinkRow `json:"links"`
		Backlinks       []docLinkRow `json:"backlinks"`
		UnresolvedLinks []string     `json:"unresolvedLinks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("doc show: parse response: %w", err)
	}
	out := cmd.OutOrStdout()
	// One H1 (the title) then the path as a dim sub-line — NOT a second H1, which
	// read as the document having two titles. The verbatim body follows.
	if _, err := fmt.Fprintf(out, "# %s\n_%s_\n\n%s\n", resp.Document.Title, resp.Document.Path, resp.Document.Body); err != nil {
		return err
	}
	return writeLinkSections(out, resp.Links, resp.Backlinks, resp.UnresolvedLinks)
}

// ---- doc links ----

type docLinkRow struct {
	Predicate   string `json:"predicate"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
}

func newDocLinksCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "links <path|slug|title>",
		Aliases: []string{"backlinks"},
		Short:   "Show a document's wikilink edges",
		Long:    "Show a document's links (what it references) and backlinks (what references it), without its body. Identify the document by its vault-relative path, its slug, or its human title.",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, rerr := resolveDocRef(cmd, "doc links", args[0])
			if rerr != nil {
				return rerr
			}
			reqBody, err := json.Marshal(map[string]any{"path": ref})
			if err != nil {
				return wrapNavFatal(cmd, "doc links", err)
			}
			raw, err := invokeNavTool(cmd, "getBacklinks", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "doc links", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			var resp struct {
				Links     []docLinkRow `json:"links"`
				Backlinks []docLinkRow `json:"backlinks"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("doc links: parse response: %w", err)
			}
			return writeLinkSections(cmd.OutOrStdout(), resp.Links, resp.Backlinks, nil)
		},
	}
}

// writeLinkSections prints the links + backlinks sections shared by show/links.
// Unresolved names the document's dead [[wikilinks]] (targets with no edge);
// pass nil where that signal is unavailable (e.g. `doc links`, which reads
// getBacklinks and has no unresolved-link data).
func writeLinkSections(out interface{ Write([]byte) (int, error) }, links, backlinks []docLinkRow, unresolved []string) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "\nLINKS (%d)\n", len(links)); err != nil {
		return err
	}
	for _, l := range links {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\n", l.Predicate, l.DisplayName, l.Name); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(tw, "BACKLINKS (%d)\n", len(backlinks)); err != nil {
		return err
	}
	for _, b := range backlinks {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\n", b.Predicate, b.DisplayName, b.Name); err != nil {
			return err
		}
	}
	if len(unresolved) > 0 {
		if _, err := fmt.Fprintf(tw, "DEAD LINKS (%d)\n", len(unresolved)); err != nil {
			return err
		}
		for _, t := range unresolved {
			if _, err := fmt.Fprintf(tw, "  ⚠\t%s\t(unresolved)\n", t); err != nil {
				return err
			}
		}
	}
	return tw.Flush()
}
