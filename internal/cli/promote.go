package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// promoteToolName is the registered MCP promoteRelation tool name
// (PromoteRelation.Name()).
const promoteToolName = "promoteRelation"

func newPromoteCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "promote <relations/uid>",
		Short: "Promote a proposed RELATION to asserted (verify a connector-ingested fact)",
		Long: `Lift the trust quarantine on a RELATION edge an external connector proposed.

This command operates on RELATION trust status — not on entity records and not
on entity kinds in the taxonomy. The three "approve" paths in maz are
intentionally distinct:
  maz promote relations/{uid}        — verifies a proposed RELATION edge (this command)
  maz review approve <entity-uid>    — confirms a tentative ENTITY record (resolver queue)
  maz schema review approve <kind>   — approves a proposed entity KIND (taxonomy)

A fact extracted from an untrusted source (a Granola transcript, a Claude-chats
export) lands with trust status=proposed: it surfaces in recall and ranks on
relevance, but is excluded from supersession and predicate minting until a human
verifies it. 'maz promote' is that verification step — it flips the trust
status to asserted, after which the edge behaves like any directly-authored
relation and you become its signed asserter.

Trust status (proposed → asserted) is orthogonal to temporal validity
(open/closed/superseded/retracted): a relation can be asserted yet temporally
closed, or proposed and still open. 'maz list relations' shows temporal state;
'maz review proposed' shows the trust queue.

Pass the relation's resource name (relations/{uid}) exactly as recall, maz get,
or maz relations hand it back; a bare ULID is also accepted. Promotion is
idempotent: promoting an already-asserted edge is a no-op that exits 0.

The write is materialised synchronously via the daemon-or-in-process event log,
so the new status is visible the instant this command returns — no maz rebuild.

Related:
  maz review proposed                list the proposed-relation trust queue
  maz review proposed --approve-all  promote every listed edge in one shot

Exit codes:
  0   relation asserted (or already asserted)
  1   expected failure: relation not found, bad input
  2   unexpected failure: storage or wiring error

Examples:
  maz promote relations/01HZZZA...
  maz promote 01HZZZA... --scope team`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPromote(cmd, args[0], scope)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", `scope the promotion is written into: project|personal|team (default personal); "team" requires admitted team membership`)
	return cmd
}

func runPromote(cmd *cobra.Command, name, scope string) error {
	// Accept a bare ULID by normalising it to the relations/{uid} resource name
	// the tool's schema advertises; a name already carrying the prefix passes
	// through. The tool re-validates either form.
	if !strings.Contains(name, "/") {
		name = "relations/" + name
	}
	in := map[string]any{"name": name}
	if scope != "" {
		in["scope"] = scope
	}
	body, err := json.Marshal(in)
	if err != nil {
		return wrapPromoteFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}

	raw, err := invokeNavTool(cmd, promoteToolName, body)
	if err != nil {
		return wrapNavFatal(cmd, "promote", err)
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, werr := fmt.Fprintln(w, string(raw))
		return werr
	}
	var out struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Promoted bool   `json:"promoted"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return wrapPromoteFatal(cmd, fmt.Errorf("decode response: %w", uerr))
	}
	verb := "asserted"
	if !out.Promoted {
		verb = "already asserted"
	}
	_, werr := fmt.Fprintf(w, "%s %s\n", verb, out.Name)
	return werr
}

func wrapPromoteFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "promote", err)
}
