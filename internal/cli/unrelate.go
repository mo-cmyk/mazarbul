package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// unrelateToolName is the registered MCP unrelate tool name (Unrelate.Name()).
const unrelateToolName = "unrelate"

type unrelateFlags struct {
	relation     string
	endedAt      string
	supersededBy string
}

func newUnrelateCmd() *cobra.Command {
	flags := &unrelateFlags{}
	cmd := &cobra.Command{
		Use:   "unrelate (<subject> <predicate> <object> | <relation>)",
		Short: "End a relation as of a date (preserves history; the inverse of relate)",
		Long: `End-date an existing relation by closing it with a valid_until, recording
that a relationship STOPPED being true (someone left a job, a project ended).

Unlike "maz forget relations/<ulid>", which deletes the edge, unrelate PRESERVES
history: the edge survives with a bounded validity, so "X worked at Y until
2026-05-22" stays in the graph and time-travel reads still see it. It drops out
of the default (point-in-time now) view.

Identify the relation to close either by its (subject, predicate, object) triple
— which closes the single currently-open match — or by its resource name with
--relation. Endpoints accept a display name, a bare ULID, entities/{uid}, or
"self". Pass --ended-at for when it ended (defaults to now); pass --superseded-by
when a newer relation replaces this one.

The close is materialised synchronously through the daemon-or-in-process tool
path, so maz relations <subject> reflects it the instant this command returns.

Exit codes:
  0   relation closed
  1   expected failure: relation not found, ambiguous triple, bad input
  2   unexpected failure: storage or wiring error

Examples:
  maz unrelate Sam works_at "Erebor Global" --ended-at 2026-05-22T00:00:00Z
  maz unrelate --relation relations/01HZZZA... --ended-at 2026-05-22T00:00:00Z`,
		Args: cobra.MaximumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnrelate(cmd, args, flags)
		},
	}
	cmd.Flags().StringVar(&flags.relation, "relation", "", "relations/{uid} or bare ULID of the edge to close (alternative to the positional triple)")
	cmd.Flags().StringVar(&flags.endedAt, "ended-at", "", "RFC 3339 instant the relationship ended; becomes the edge's valid_until (default now)")
	cmd.Flags().StringVar(&flags.supersededBy, "superseded-by", "", "relations/{uid} of a newer relation that replaces this one (a correction; omit for a pure close)")
	return cmd
}

func runUnrelate(cmd *cobra.Command, args []string, flags *unrelateFlags) error {
	in := map[string]any{}
	switch len(args) {
	case 3:
		if flags.relation != "" {
			return unrelateUsageErr(cmd, "provide either the positional triple or --relation, not both")
		}
		in["subject"], in["predicate"], in["object"] = args[0], args[1], args[2]
	case 1:
		if flags.relation != "" {
			return unrelateUsageErr(cmd, "provide either a positional relation or --relation, not both")
		}
		in["relation"] = args[0]
	case 0:
		if flags.relation == "" {
			return unrelateUsageErr(cmd, "give the (subject, predicate, object) triple or --relation <relations/{uid}>")
		}
		in["relation"] = flags.relation
	default:
		return unrelateUsageErr(cmd, "expected a single relation argument or a (subject, predicate, object) triple")
	}
	if flags.endedAt != "" {
		in["endedAt"] = flags.endedAt
	}
	if flags.supersededBy != "" {
		in["supersededBy"] = flags.supersededBy
	}

	reqBody, berr := json.Marshal(in)
	if berr != nil {
		return respondFatal(cmd, "unrelate", fmt.Errorf("marshal unrelate request: %w", berr))
	}

	raw, err := invokeNavTool(cmd, unrelateToolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "unrelate", err)
	}

	var resp struct {
		Name       string `json:"name"`
		ValidUntil string `json:"validUntil"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return respondFatal(cmd, "unrelate", fmt.Errorf("parse unrelate response: %w", uerr))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{
			"name":       resp.Name,
			"validUntil": resp.ValidUntil,
		})
	}
	_, err = fmt.Fprintf(w, "%s closed (valid until %s)\n", resp.Name, resp.ValidUntil)
	return err
}

// unrelateUsageErr emits a usage message to stderr and returns a clean exit-1.
func unrelateUsageErr(cmd *cobra.Command, msg string) error {
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(cmd.ErrOrStderr(), "unrelate: %s\n", msg)
	return silentError{code: 1, err: errors.New(msg)}
}
