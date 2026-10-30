package cli

// scope.go implements the "maz scope" command group — the CLI surface for the
// draft staging workflow (Lane F):
//
//	maz scope draft list     show staged draft episodes/entities/relations
//	maz scope draft diff     compare draft set against personal set
//	maz scope draft promote  move a draft episode to personal
//	maz scope draft discard  remove a draft episode from staging
//
// All four verbs route through invokeNavTool so a running daemon serves them
// over the MCP transport. The write verbs (promote, discard) hit the unlisted
// promoteDraft/discardDraft tools; the read verbs (list, diff) hit draftStage.

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// newScopeCmd returns the "maz scope" parent command.
func newScopeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scope",
		Short: "Private draft staging — stage notes privately, then promote or discard them",
		Long: `Private draft staging and scope lifecycle. The draft staging workflow lets you
keep speculative notes OUT of your main memory: stage episodes privately
(invisible to recall, list, and search), review them, then promote the keepers
into personal scope or discard the rest.

To STAGE a draft there is no "create" subcommand — write it with --scope draft on
the normal write verbs:
  maz remember "speculative note" --scope draft
  maz relate "Acme" "acquired" "Globex" --scope draft

Then review and resolve the staged set with the subcommands:
  draft list     show staged draft episodes, entities, and relations
  draft diff     compare the draft set against the personal set
  draft promote  move a draft episode into personal scope
  draft discard  remove a draft episode from staging

Examples:
  maz remember "tide-powered phone charger idea" --scope draft
  maz scope draft list
  maz scope draft diff
  maz scope draft promote episodes/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz scope draft discard episodes/01ARZ3NDEKTSV4RRFFQ69G5FAV`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newScopeDraftCmd())
	return cmd
}

// newScopeDraftCmd returns the "maz scope draft" subgroup.
func newScopeDraftCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "draft",
		Short: "Manage draft-scoped staging area",
		Long: `The draft scope is a private staging area: records written with
--scope=draft are invisible to recall, getContext, and all navigation until
you explicitly promote them. Use this to stage episodes before committing them
to your personal or project memory.

To STAGE a draft, write it with --scope draft on the normal write commands:
  maz remember "speculative note" --scope draft
  maz relate "Acme" "acquired" "Globex" --scope draft

Then review and resolve it with the subcommands below:
  maz scope draft list        # show what is staged
  maz scope draft diff         # compare staged vs your live memory
  maz scope draft promote      # make a draft visible (re-scope to personal)
  maz scope draft discard      # drop a draft for good`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newScopeDraftListCmd())
	cmd.AddCommand(newScopeDraftDiffCmd())
	cmd.AddCommand(newScopeDraftPromoteCmd())
	cmd.AddCommand(newScopeDraftDiscardCmd())
	return cmd
}

// newScopeDraftListCmd returns "maz scope draft list".
func newScopeDraftListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List staged draft episodes, entities, and relations",
		Long: `List all episodes, entities, and relations currently in the draft
staging area. Draft records are invisible to recall until promoted.

Examples:
  maz scope draft list
  maz scope draft list --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reqBody, err := json.Marshal(map[string]any{"view": "list"})
			if err != nil {
				return wrapNavFatal(cmd, "scope draft list", err)
			}
			raw, err := invokeNavTool(cmd, "draftStage", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "scope draft list", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDraftListCLI(cmd, raw)
		},
	}
	return cmd
}

// newScopeDraftDiffCmd returns "maz scope draft diff".
func newScopeDraftDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare draft set against personal set",
		Long: `Show draft-scoped records alongside personal-scoped records so you
can review what promotion would add to your personal memory before committing.

Examples:
  maz scope draft diff
  maz scope draft diff --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reqBody, err := json.Marshal(map[string]any{"view": "diff"})
			if err != nil {
				return wrapNavFatal(cmd, "scope draft diff", err)
			}
			raw, err := invokeNavTool(cmd, "draftStage", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "scope draft diff", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDraftDiffCLI(cmd, raw)
		},
	}
	return cmd
}

// newScopeDraftPromoteCmd returns "maz scope draft promote <name>".
func newScopeDraftPromoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "promote <name>",
		Short: "Promote a draft episode to personal scope",
		Long: `Promote a draft-scoped episode out of the staging area and into personal
scope, making it visible to recall and getContext. The episode and all derived
rows (mentions, relations, facts) are re-scoped atomically.

Pass the resource name (episodes/{uid}) or bare uid from "maz scope draft list".

Examples:
  maz scope draft promote episodes/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz scope draft promote 01ARZ3NDEKTSV4RRFFQ69G5FAV`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reqBody, err := json.Marshal(map[string]any{"name": args[0]})
			if err != nil {
				return wrapNavFatal(cmd, "scope draft promote", err)
			}
			raw, err := invokeNavTool(cmd, "promoteDraft", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "scope draft promote", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDraftPromoteCLI(cmd, raw)
		},
	}
	return cmd
}

// newScopeDraftDiscardCmd returns "maz scope draft discard <name>".
func newScopeDraftDiscardCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "discard <name>",
		Short: "Discard a draft episode from the staging area",
		Long: `Remove a draft-scoped episode from the staging area. The episode and all
derived rows (entities, mentions, relations, facts) are deleted from every
projection, so it will not surface in any recall, list, or search. An audit
record is retained in the immutable event log (the staged text is never
physically scrubbed, but it is no longer reachable through any query surface).

Pass the resource name (episodes/{uid}) or bare uid from "maz scope draft list".

This is the discard verb for DRAFT-scoped episodes; for an episode already in
personal scope, use "maz forget" instead.

Examples:
  maz scope draft discard episodes/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz scope draft discard 01ARZ3NDEKTSV4RRFFQ69G5FAV --reason "not relevant"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{"name": args[0]}
			putIfSet(in, "reason", reason)
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "scope draft discard", err)
			}
			raw, err := invokeNavTool(cmd, "discardDraft", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "scope draft discard", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDraftDiscardCLI(cmd, raw)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "optional reason for discarding (recorded in audit event)")
	return cmd
}

// ---- renderers ---------------------------------------------------------------

// draftSetView is the subset of a draftSet the text renderer prints.
type draftSetView struct {
	Episodes []struct {
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"episodes"`
	Entities []struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Kind        string `json:"kind"`
	} `json:"entities"`
	Relations []struct {
		Name      string `json:"name"`
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
		Object    string `json:"object"`
	} `json:"relations"`
}

func printDraftSet(tw *tabwriter.Writer, label string, ds draftSetView) error {
	if _, err := fmt.Fprintf(tw, "%s:\n", label); err != nil {
		return err
	}
	if len(ds.Episodes) == 0 && len(ds.Entities) == 0 && len(ds.Relations) == 0 {
		if _, err := fmt.Fprintf(tw, "  (empty)\n"); err != nil {
			return err
		}
		return nil
	}
	for _, ep := range ds.Episodes {
		text := ep.Text
		if len(text) > 80 {
			text = text[:77] + "..."
		}
		if _, err := fmt.Fprintf(tw, "  episode\t%s\t%s\n", ep.Name, text); err != nil {
			return err
		}
	}
	for _, e := range ds.Entities {
		if _, err := fmt.Fprintf(tw, "  entity\t%s\t%s (%s)\n", e.Name, e.DisplayName, e.Kind); err != nil {
			return err
		}
	}
	for _, r := range ds.Relations {
		if _, err := fmt.Fprintf(tw, "  relation\t%s\t%s -[%s]-> %s\n", r.Name, r.Subject, r.Predicate, r.Object); err != nil {
			return err
		}
	}
	return nil
}

// renderDraftListCLI prints a draftStage view=list response.
func renderDraftListCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v struct {
		Draft draftSetView `json:"draft"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "scope draft list", fmt.Errorf("parse response: %w", err))
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if err := printDraftSet(tw, "draft", v.Draft); err != nil {
		return err
	}
	return tw.Flush()
}

// renderDraftDiffCLI prints a draftStage view=diff response.
func renderDraftDiffCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v struct {
		Draft    draftSetView  `json:"draft"`
		Personal *draftSetView `json:"personal"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "scope draft diff", fmt.Errorf("parse response: %w", err))
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if err := printDraftSet(tw, "draft (staged)", v.Draft); err != nil {
		return err
	}
	if v.Personal != nil {
		if _, err := fmt.Fprintln(tw); err != nil {
			return err
		}
		if err := printDraftSet(tw, "personal (current)", *v.Personal); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// renderDraftPromoteCLI prints a promoteDraft response.
func renderDraftPromoteCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v struct {
		Status  string `json:"status"`
		Name    string `json:"name"`
		ToScope string `json:"toScope"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "scope draft promote", fmt.Errorf("parse response: %w", err))
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s promoted to %s\n", v.Name, v.ToScope)
	return err
}

// renderDraftDiscardCLI prints a discardDraft response.
func renderDraftDiscardCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v struct {
		Status string `json:"status"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "scope draft discard", fmt.Errorf("parse response: %w", err))
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s discarded\n", v.Name)
	return err
}
