package cli

// share.go implements "maz share <ref>" — the CLI surface for the shareToTeam
// MCP tool. It re-publishes a personal-scope record (entity, fact, or
// relation) as ordinary team-scoped events so all admitted team members can
// read it.
//
// The personal original is unchanged; both copies coexist under separate scope
// lenses. Entity dedup against the team scope runs automatically (exact display
// name): an existing match returns status=reused instead of minting a
// duplicate.
//
// Exit codes:
//
//	0   record shared (or reused — an existing team entity matched)
//	1   expected failure: bad input, record not found, permission denied
//	2   unexpected failure: storage or wiring error

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

const shareToTeamToolName = "shareToTeam"

func newShareCmd() *cobra.Command {
	var withFacts, withRelations bool
	cmd := &cobra.Command{
		Use:   "share <ref>",
		Short: "Re-publish a personal record to the team scope",
		Long: `Re-publish a personal-scope record — entity, fact, or relation — as ordinary
team-scoped events so all admitted team members can read it. The personal
original is unchanged; both copies coexist under separate scope lenses.

<ref> accepts:
  entities/{uid}   share an entity (and optionally its subtree with --with-facts / --with-relations)
  facts/{uid}      share a single fact
  relations/{uid}  share a single relation
  <bare-ulid>      treated as an entity uid
  <display-name>   resolved case-insensitively (unique match required)

Entity dedup runs automatically against the team scope by exact display name: if
a matching entity already exists in team scope, it is returned as status=reused
without minting a duplicate.

Requires admitted team membership; a reader role cannot write team-scope events
(E_PERMISSION).

Exit codes:
  0   record shared (or entity reused — existing team match)
  1   expected failure: bad input, record not found, E_PERMISSION
  2   unexpected failure: storage or wiring error

Examples:
  maz share entities/01HZZZA...
  maz share "Eowyn" --with-facts --with-relations
  maz share facts/01HZZZB...`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShare(cmd, args[0], withFacts, withRelations)
		},
	}
	cmd.Flags().BoolVar(&withFacts, "with-facts", false, "Also share the entity's currently-open facts (entity refs only)")
	cmd.Flags().BoolVar(&withRelations, "with-relations", false, "Also share the entity's active relations (entity refs only)")
	return cmd
}

func runShare(cmd *cobra.Command, ref string, withFacts, withRelations bool) error {
	in := map[string]any{"ref": ref}
	if withFacts {
		in["withFacts"] = true
	}
	if withRelations {
		in["withRelations"] = true
	}
	body, err := json.Marshal(in)
	if err != nil {
		return wrapShareFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}

	raw, err := invokeNavTool(cmd, shareToTeamToolName, body)
	if err != nil {
		return wrapNavFatal(cmd, "share", err)
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, werr := fmt.Fprintln(w, string(raw))
		return werr
	}

	var out struct {
		Shared []struct {
			Name     string `json:"name"`
			TeamName string `json:"teamName"`
			Type     string `json:"type"`
			Status   string `json:"status"`
		} `json:"shared"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return wrapShareFatal(cmd, fmt.Errorf("decode response: %w", uerr))
	}

	for _, s := range out.Shared {
		switch s.Status {
		case "reused":
			fmt.Fprintf(w, "reused %s → %s\n", s.Name, s.TeamName) //nolint:errcheck
		default:
			fmt.Fprintf(w, "shared %s → %s\n", s.Name, s.TeamName) //nolint:errcheck
		}
	}
	return nil
}

func wrapShareFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "share", err)
}
