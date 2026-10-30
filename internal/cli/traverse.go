package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

type traverseFlags struct {
	depth      int
	predicates []string
	limit      int
}

func newTraverseCmd() *cobra.Command {
	flags := &traverseFlags{depth: 1, limit: 50}
	cmd := &cobra.Command{
		Use:   "traverse <entity>",
		Short: "Walk the entity graph from a seed entity",
		Long: `Lightweight N-hop walk (1..6) from the given entity via RELATES_TO edges;
returns the hydrated neighbor entities. For scope filtering, history-aware
walks, or temporal anchoring, use "maz neighbors" (a superset of traverse
that adds --scope, --include-inactive, --at, --as-of).

Accepts an entities/{uid} resource name (as returned by maz recall), a bare
uid, or a display name. A unique display name resolves directly; an ambiguous
or unknown name returns an error with candidate uids so you can retry with
the exact entities/{uid}.

The traversal honours the same temporal validity gate the recall lane uses
("now BETWEEN validFrom AND validUntil").

Examples:
  maz traverse entities/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz traverse "Merry Brandybuck" --depth 2
  maz traverse alice --predicate works_at --predicate reports_to --output=json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTraverse(cmd, args[0], flags)
		},
	}
	cmd.Flags().IntVar(&flags.depth, "depth", 1, fmt.Sprintf("hops to walk (1..%d)", navigate.MaxHops))
	cmd.Flags().StringArrayVar(&flags.predicates, "predicate", nil, "restrict to this predicate (repeatable), e.g. --predicate works_at")
	cmd.Flags().IntVar(&flags.limit, "limit", 50, "maximum neighbours to return")
	return cmd
}

func runTraverse(cmd *cobra.Command, seed string, flags *traverseFlags) error {
	if flags.depth < 1 || flags.depth > navigate.MaxHops {
		//nolint:errcheck // best-effort stderr on fatal path
		fmt.Fprintf(cmd.ErrOrStderr(), "traverse: --depth must be between 1 and %d (got %d)\n", navigate.MaxHops, flags.depth)
		return silentError{code: 1, err: fmt.Errorf("--depth must be between 1 and %d (got %d)", navigate.MaxHops, flags.depth)}
	}

	// Build the neighbors tool request — it handles name resolution via
	// resolveEntityArg and returns hydrated entities + edges.
	in := map[string]any{
		"seed":  seed,
		"depth": flags.depth,
		"limit": flags.limit,
	}
	if len(flags.predicates) > 0 {
		in["predicates"] = flags.predicates
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return wrapTraverseFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}

	raw, err := invokeNavTool(cmd, "neighbors", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "traverse", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}

	return renderTraverseResult(cmd, raw)
}

// traverseNeighbor is the decoded shape of one entry in the neighbors response.
type traverseNeighbor struct {
	Entity struct {
		UID         string `json:"uid"`
		DisplayName string `json:"displayName"`
		Kind        string `json:"kind"`
		Name        string `json:"name"`
	} `json:"entity"`
	Via struct {
		Predicate string `json:"predicate"`
	} `json:"via"`
	Direction string `json:"direction"`
	Hops      int    `json:"hops"`
}

// renderTraverseResult renders the neighbors tool response in the active format.
// In text mode it prints one line per neighbor: "uid  displayName  via predicate".
// In JSON/NDJSON mode the raw neighbors response is forwarded verbatim (same
// shape as maz neighbors), one object per line for NDJSON.
func renderTraverseResult(cmd *cobra.Command, raw json.RawMessage) error {
	wr := output.From(cmd)

	// Decode just enough to render; the full JSON is forwarded for JSON modes.
	var out struct {
		Neighbors []traverseNeighbor `json:"neighbors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return wrapTraverseFatal(cmd, fmt.Errorf("parse neighbors response: %w", err))
	}

	if len(out.Neighbors) == 0 {
		switch wr.Format() {
		case output.FormatNDJSON:
			// NDJSON consumers expect one object per line; emit nothing for empty.
			return nil
		case output.FormatJSON:
			_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		default:
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "no neighbors")
			return err
		}
	}

	switch wr.Format() {
	case output.FormatNDJSON:
		return renderTraverseNDJSON(cmd, out.Neighbors)
	case output.FormatJSON:
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	default:
		return renderTraverseText(cmd, out.Neighbors)
	}
}

// renderTraverseNDJSON emits one JSON object per neighbor to cmd's stdout.
func renderTraverseNDJSON(cmd *cobra.Command, neighbors []traverseNeighbor) error {
	for i := range neighbors {
		nb := neighbors[i]
		line, merr := json.Marshal(map[string]any{
			"name":        nb.Entity.Name,
			"uid":         nb.Entity.UID,
			"displayName": nb.Entity.DisplayName,
			"kind":        nb.Entity.Kind,
			"predicate":   nb.Via.Predicate,
			"direction":   nb.Direction,
			"hops":        nb.Hops,
		})
		if merr != nil {
			return wrapTraverseFatal(cmd, fmt.Errorf("marshal neighbor: %w", merr))
		}
		if _, werr := fmt.Fprintln(cmd.OutOrStdout(), string(line)); werr != nil {
			return werr
		}
	}
	return nil
}

// renderTraverseText prints one neighbor per line as tab-separated text.
func renderTraverseText(cmd *cobra.Command, neighbors []traverseNeighbor) error {
	w := cmd.OutOrStdout()
	for _, nb := range neighbors {
		uid := nb.Entity.UID
		if uid == "" {
			uid = nb.Entity.Name
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			uid, nb.Entity.DisplayName, nb.Via.Predicate, nb.Direction); err != nil {
			return err
		}
	}
	return nil
}

// wrapTraverseFatal handles an internal (unexpected) error from traverse,
// always exiting 2. Use wrapNavFatal for tool-layer errors so E_NOT_FOUND
// and E_VALIDATION route to exit 1.
func wrapTraverseFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "traverse", err)
}
