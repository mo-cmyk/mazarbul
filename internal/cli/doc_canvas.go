package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

type docCanvasFlags struct {
	hops            int
	limit           int
	predicates      []string
	scopes          []string
	out             string
	at              string
	asOf            string
	includeInactive bool
}

// exportCanvasInput projects the set flags into the exportCanvas tool-input map.
// Kept off runDocCanvas so the optional predicate/scope/bi-temporal folds do not
// inflate that function's cyclomatic complexity (mirrors entitiesInput). The
// at/asOf/includeInactive anchor narrows the subgraph edges; a bad timestamp is
// validated tool-side and surfaces as E_VALIDATION.
func (flags *docCanvasFlags) exportCanvasInput(seed string) map[string]any {
	in := map[string]any{"seed": seed, "hops": flags.hops, "limit": flags.limit}
	if len(flags.predicates) > 0 {
		in["predicates"] = flags.predicates
	}
	if len(flags.scopes) > 0 {
		in["scopes"] = flags.scopes
	}
	if flags.at != "" {
		in["at"] = flags.at
	}
	if flags.asOf != "" {
		in["asOf"] = flags.asOf
	}
	if flags.includeInactive {
		in["includeInactive"] = true
	}
	return in
}

// newDocCanvasCmd is `maz doc canvas <seed>`: export an N-hop subgraph rooted at
// the seed as a JSON Canvas (.canvas) file, openable in Obsidian. The walk and
// serialization run in the daemon (the exportCanvas tool); this command writes
// the result into the vault and materializes the referenced documents so their
// file nodes resolve.
func newDocCanvasCmd() *cobra.Command {
	flags := &docCanvasFlags{hops: 1, limit: 50}
	cmd := &cobra.Command{
		Use:   "canvas <seed>",
		Short: "Export a subgraph as a JSON Canvas (.canvas) file",
		Long: `Export an N-hop subgraph rooted at <seed> as a JSON Canvas (.canvas) file,
openable in Obsidian (or any JSON Canvas viewer). Nodes are entities and
documents; edges are their relations as labeled arrows. Documents render as
file nodes (the live vault .md), entities as text cards.

<seed> accepts an entities/{uid} resource name, a bare uid, or a display name
(resolved the same way as maz neighbors).

By default the canvas is written into the vault at canvases/<seed>.canvas. The
referenced documents are always materialized into the vault so their file nodes
resolve when the canvas is opened there. Use -o to write the .canvas elsewhere
(including inside the vault); file nodes resolve only when it is opened in the
vault.

The export writes plaintext. By default it reads the project and personal
scopes. Narrow with --scope to control which nodes appear (only entities in the
selected scopes are included, so a seed outside them will not resolve). Entities
created via the CLI default to the personal scope.

The walk is validity-gated exactly like neighbors: by default only edges live
now are included. --at / --as-of time-travel the edges (valid-time and
transaction-time anchors), and --include-inactive adds superseded / expired
edges (full history).

Examples:
  maz doc canvas "Sarah" --hops 2
  maz doc canvas entities/01ARZ3... --hops 1 -o /tmp/graph.canvas
  maz doc canvas alice --predicate works_at --scope project
  maz doc canvas "Sarah" --at 2025-01-01T00:00:00Z --include-inactive`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDocCanvas(cmd, args[0], flags)
		},
	}
	cmd.Flags().IntVar(&flags.hops, "hops", 1, fmt.Sprintf("subgraph radius in hops (1..%d)", navigate.MaxHops))
	cmd.Flags().IntVar(&flags.limit, "limit", 50, "maximum neighbor nodes across all hops (the seed is always included)")
	cmd.Flags().StringArrayVar(&flags.predicates, "predicate", nil, "restrict edges to these predicates (repeatable)")
	cmd.Flags().StringArrayVar(&flags.scopes, "scope", nil, "scopes to read: project, personal, team (repeatable; default project+personal)")
	cmd.Flags().StringVarP(&flags.out, "out", "o", "", "write the .canvas to this path instead of the vault")
	cmd.Flags().StringVar(&flags.at, "at", "", "valid-time anchor (RFC 3339 UTC); include edges live at this instant. Default now")
	cmd.Flags().StringVar(&flags.asOf, "as-of", "", "transaction-time anchor (RFC 3339 UTC); ignore edges recorded after this instant")
	cmd.Flags().BoolVar(&flags.includeInactive, "include-inactive", false, "include superseded / expired edges (full history)")
	return cmd
}

func runDocCanvas(cmd *cobra.Command, seed string, flags *docCanvasFlags) error {
	if flags.hops < 1 || flags.hops > navigate.MaxHops {
		//nolint:errcheck // best-effort stderr on fatal path
		fmt.Fprintf(cmd.ErrOrStderr(), "doc canvas: --hops must be between 1 and %d (got %d)\n", navigate.MaxHops, flags.hops)
		return silentError{code: 1, err: fmt.Errorf("--hops must be between 1 and %d (got %d)", navigate.MaxHops, flags.hops)}
	}

	reqBody, err := json.Marshal(flags.exportCanvasInput(seed))
	if err != nil {
		return wrapNavFatal(cmd, "doc canvas", err)
	}
	raw, err := invokeNavTool(cmd, "exportCanvas", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "doc canvas", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}

	var resp struct {
		Canvas    json.RawMessage `json:"canvas"`
		Documents []string        `json:"documents"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapNavFatal(cmd, "doc canvas", fmt.Errorf("parse exportCanvas response: %w", uerr))
	}

	pretty, err := indentCanvas(resp.Canvas)
	if err != nil {
		return wrapNavFatal(cmd, "doc canvas", err)
	}

	dest, err := canvasDestination(cmd, seed, resp.Documents, flags.out)
	if err != nil {
		return wrapNavFatal(cmd, "doc canvas", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(dest), 0o700); mkErr != nil {
		return wrapNavFatal(cmd, "doc canvas", fmt.Errorf("create canvas dir: %w", mkErr))
	}
	if wErr := os.WriteFile(dest, pretty, 0o600); wErr != nil {
		return wrapNavFatal(cmd, "doc canvas", fmt.Errorf("write %s: %w", dest, wErr))
	}

	nodes, edges := countCanvas(resp.Canvas)
	if format := output.From(cmd).Format(); format == output.FormatJSON || format == output.FormatNDJSON {
		return emitCanvasSummaryJSON(cmd, format, dest, nodes, edges, len(resp.Documents))
	}
	//nolint:errcheck // best-effort stdout
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%d nodes, %d edges, %d documents)\n", dest, nodes, edges, len(resp.Documents))
	return nil
}

// emitCanvasSummaryJSON prints the write summary as a JSON object, honoring the
// global --output flag so a script can parse where the canvas landed and how big
// it is. The .canvas itself is in the file at path; this is only the summary.
func emitCanvasSummaryJSON(cmd *cobra.Command, format output.Format, dest string, nodes, edges, documents int) error {
	summary := struct {
		Path      string `json:"path"`
		Nodes     int    `json:"nodes"`
		Edges     int    `json:"edges"`
		Documents int    `json:"documents"`
	}{dest, nodes, edges, documents}
	var b []byte
	var err error
	if format == output.FormatNDJSON {
		b, err = json.Marshal(summary)
	} else {
		b, err = json.MarshalIndent(summary, "", "  ")
	}
	if err != nil {
		return wrapNavFatal(cmd, "doc canvas", err)
	}
	//nolint:errcheck // best-effort stdout
	fmt.Fprintln(cmd.OutOrStdout(), string(b))
	return nil
}

// canvasDestination materializes the referenced documents (so their file nodes
// resolve in the vault) and resolves where to write the .canvas file. With -o it
// is that path made absolute; otherwise it is <vaultRoot>/canvases/<slug>.canvas,
// where the vault root is the one the daemon reports while materializing (falling
// back to the client-local root). Materialization runs in both cases because -o
// may itself point inside the vault, where a missing document .md would leave a
// broken file node; materializeVault is idempotent and never clobbers un-ingested
// edits.
func canvasDestination(cmd *cobra.Command, seed string, documents []string, out string) (string, error) {
	root, err := materializeDocs(cmd, documents)
	if err != nil {
		return "", err
	}
	if out != "" {
		return filepath.Abs(out)
	}
	if root == "" {
		// No document reported a vault root (a docless canvas, or the tool is
		// unavailable); fall back to the client-local vault root.
		if root, err = localVaultRoot(cmd); err != nil {
			return "", err
		}
	}
	return filepath.Join(root, "canvases", canvasSlug(seed)+".canvas"), nil
}

// materializeDocs materializes each referenced document through the daemon (so
// its file node resolves in the vault) and returns the daemon-authoritative vault
// root reported in the response, or "" when no document reported one (no
// documents, or the tool is unavailable). The caller resolves a fallback root.
func materializeDocs(cmd *cobra.Command, documents []string) (string, error) {
	root := ""
	for _, path := range documents {
		body, mErr := json.Marshal(map[string]any{"path": path})
		if mErr != nil {
			return "", mErr
		}
		raw, iErr := invokeNavTool(cmd, "materializeVault", body)
		if iErr != nil {
			return "", iErr
		}
		if raw == nil {
			break // tool unavailable
		}
		var out struct {
			Root string `json:"root"`
		}
		if json.Unmarshal(raw, &out) == nil && out.Root != "" {
			root = out.Root
		}
	}
	return root, nil
}

// indentCanvas re-renders the canvas object as tab-indented JSON with a trailing
// newline, the git-friendly .canvas shape. The tool already emits a stable
// field order, so this only normalizes whitespace.
func indentCanvas(canvas json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, canvas, "", "\t"); err != nil {
		return nil, fmt.Errorf("format canvas: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// countCanvas returns the node and edge counts of a canvas document for the
// summary line.
func countCanvas(canvas json.RawMessage) (nodes, edges int) {
	var c struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	//nolint:errcheck // best-effort counts for the summary line; zero on a decode miss is fine
	json.Unmarshal(canvas, &c)
	return len(c.Nodes), len(c.Edges)
}

// canvasSlug derives a filesystem-safe slug from the seed argument for the
// default canvas filename: it strips an entities/ prefix, lowercases, and
// collapses runs of non-alphanumeric characters to single dashes.
func canvasSlug(seed string) string {
	s := strings.ToLower(strings.TrimPrefix(seed, "entities/"))
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "canvas"
}
