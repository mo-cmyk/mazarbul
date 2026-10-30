package cli

// entity_context.go — `maz entity context <entity>`: the one-shot "tell me the
// story of X" dossier, distinct from `maz get` (raw fields) and `maz context`
// (cold-start working-set brief). It invokes the MCP getEntityContext tool and
// renders its token-budgeted markdown card, collapsing five separate reads
// (get + relations + neighbors + recall + facts) into a single command.
//
// Surface: maz entity context <entity> [--budget N] [--at <t>] [--json]
//
// <entity> accepts a display name, bare ULID, entities/{uid} resource name, or
// "self". An ambiguous display name exits 1 (E_VALIDATION); an unknown entity
// exits 1 (E_NOT_FOUND).
//
// Exit codes:
//   0   dossier printed (or JSON emitted)
//   1   expected failure: unknown entity, ambiguous name, bad --at anchor
//   2   unexpected failure: storage or wiring error

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/tui/style/mdrender"
)

// entityContextToolName is the registered MCP tool name for the entity-dossier
// assembler: getEntityContext.
const entityContextToolName = "getEntityContext"

// entityContextFlags holds the parsed flag values for "maz entity context".
type entityContextFlags struct {
	budget int
	at     string
}

// newEntityContextCmd returns "maz entity context <entity>".
func newEntityContextCmd() *cobra.Command {
	flags := &entityContextFlags{}
	cmd := &cobra.Command{
		Use:   "context <entity>",
		Short: "Assemble a token-budgeted dossier for one entity (the one-shot story-of-X card)",
		Long: `Assemble a single token-budgeted markdown dossier for ONE entity, collapsing
the five reads that would otherwise require separate commands (get + relations +
neighbors + recall + facts) into one card.

The dossier includes:
  • A header:   name · kind · entities/{uid} · resolution status
  • Connections: typed relations as direction-aware predicate fragments
  • What I know: scalar facts (key/value attributes)
  • Mentioned in: recall hits naming this entity
  • A counts footer: N relations, M facts · budget X · used Y

The card never exceeds --budget tokens; the lowest-priority bullets are dropped
behind a visible "(N more omitted)" marker. Pass --at to time-slice the
Connections and What-I-know sections to a past instant (the header is
unchanged).

Distinct from:
  maz get <entity>           — raw fields only, no relations/facts/mentions
  maz context                — cold-start working-set brief for all entities

<entity> accepts:
  • A display name (resolved case-insensitively; ambiguous → exit 1)
  • A bare ULID:      01ARZ3NDEKTSV4RRFFQ69G5FAV
  • A resource name:  entities/01ARZ3NDEKTSV4RRFFQ69G5FAV
  • The keyword:      self  (resolves to the bound principal)

Exit codes:
  0   dossier printed (or JSON emitted)
  1   unknown entity, ambiguous name, or bad --at anchor
  2   unexpected failure: storage or wiring error

Examples:
  maz entity context "Merry Brandybuck"
  maz entity context self
  maz entity context entities/01ARZ3NDEKTSV4RRFFQ69G5FAV --budget 8000
  maz entity context "Acme Corp" --at 2025-01-01T00:00:00Z
  maz entity context "Sam Gamgee" --output json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntityContext(cmd, args[0], flags)
		},
	}
	cmd.Flags().IntVar(&flags.budget, "budget", 0,
		"token budget for the dossier (default 4000, tool maximum 100000); lower-priority bullets are dropped to fit")
	cmd.Flags().StringVar(&flags.at, "at", "",
		"valid-time anchor (RFC 3339 UTC); returns Connections and What-I-know as live at this instant. Defaults to now")
	return cmd
}

// entityContextResponse mirrors the getEntityContext tool output shape.
type entityContextResponse struct {
	// Markdown is the assembled entity dossier.
	Markdown string `json:"markdown"`
	// Tokens is the estimated token count of the dossier (always <= budget).
	Tokens int `json:"tokens"`
	// Budget is the token budget that was applied.
	Budget int `json:"budget"`
	// Dropped is the number of lower-priority bullets omitted to fit the budget.
	Dropped int `json:"dropped,omitempty"`
	// Sections carries per-section counts of what made it into the dossier.
	Sections []json.RawMessage `json:"sections,omitempty"`
}

// runEntityContext builds the getEntityContext tool request from flags, invokes
// it through the daemon-or-in-process path, and renders the response. In JSON
// mode the raw tool response is emitted via emitNavResult; in text mode the
// markdown is rendered via mdrender.RenderForWriter.
func runEntityContext(cmd *cobra.Command, entity string, flags *entityContextFlags) error {
	if entity == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "entity context: entity argument is required")
		return silentError{code: 1, err: errors.New("entity argument is required")}
	}

	in := map[string]any{"entity": entity}
	if flags.budget > 0 {
		in["tokenBudget"] = flags.budget
	}
	if flags.at != "" {
		in["at"] = flags.at
	}
	reqBody, merr := json.Marshal(in)
	if merr != nil {
		return wrapNavFatal(cmd, "entity context", merr)
	}

	raw, err := invokeNavTool(cmd, entityContextToolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "entity context", err)
	}

	// emitNavResult handles JSON-mode passthrough and the nil (tool not
	// registered) case. When handled is true the caller is done.
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}

	// Text mode: decode the markdown field and render it.
	var resp entityContextResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapNavFatal(cmd, "entity context", fmt.Errorf("parse dossier response: %w", uerr))
	}
	rendered := mdrender.RenderForWriter(cmd.OutOrStdout(), resp.Markdown)
	_, werr := fmt.Fprint(cmd.OutOrStdout(), rendered)
	return werr
}
