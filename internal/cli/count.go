package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// countGroupByAlias returns the camelCase column name that AggregateEntities /
// entityColumn accepts for a user-supplied snake_case or alias --group-by
// field name. Returns the input unchanged for any unrecognised field.
// This lets `maz count --group-by status` work even though the real column is
// resolutionStatus, and normalises snake_case variants (e.g. create_time).
func countGroupByAlias() map[string]string {
	return map[string]string{
		"status":            "resolutionStatus",
		"resolution_status": "resolutionStatus",
		"resolution-status": "resolutionStatus",
		"create_time":       "createTime",
		"create-time":       "createTime",
		"display_name":      "displayName",
		"display-name":      "displayName",
	}
}

// normaliseGroupBy maps a user-supplied --group-by field name to the form
// that AggregateEntities understands, applying countGroupByAlias.
func normaliseGroupBy(field string) string {
	if mapped, ok := countGroupByAlias()[field]; ok {
		return mapped
	}
	return field
}

// newCountCmd is the deterministic "how many …" surface: it counts entities
// matching an AIP-160 filter, or — with --group-by — prints a per-value
// breakdown. It shares the count MCP tool, so the grammar matches
// `maz entities --filter`.
func newCountCmd() *cobra.Command {
	var (
		filter  string
		kind    string
		groupBy string
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "count",
		Short: "Count entities matching a filter (LLM-free), optionally grouped",
		Long: `Count the entities matching an AIP-160 --filter — the deterministic answer
to "how many …". With --group-by, print a per-value breakdown (e.g. people per
status) instead of a single total.

The --filter grammar matches 'maz entities --filter': entity columns (kind,
displayName, confidence, resolutionStatus, createTime), fact slots by state_key
(e.g. status = "pending"), and related("predicate", "target"), combined with
= != < > <= >= : and AND / OR / NOT.

Only project and personal rows are counted; team-scope rows are excluded. To
count the team scope use the 'count' MCP tool with scopes:["team"].

Examples:
  maz count --filter 'kind = "person" AND displayName : "Marcus"'
  maz count --kind person --group-by status
  maz count --filter 'kind = "person"' --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := map[string]any{}
			if filter != "" {
				in["filter"] = filter
			}
			if kind != "" {
				in["kind"] = kind
			}
			if groupBy != "" {
				in["groupBy"] = normaliseGroupBy(groupBy)
			}
			if cmd.Flags().Changed("limit") {
				in["limit"] = limit
			}
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "count", err)
			}
			raw, err := invokeNavTool(cmd, "count", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "count", err)
			}
			// Normalize: when --group-by is set and the tool returns a bare
			// object with no "groups" key, inject an empty groups array so every
			// structured consumer gets the same envelope shape regardless of
			// whether any rows matched.
			if groupBy != "" {
				raw = normalizeCountGroups(raw)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderCountCLI(cmd, raw)
		},
	}
	cmd.Flags().StringVar(&filter, "filter", "", `AIP-160 filter, e.g. 'kind = "person" AND status = "pending"'`)
	cmd.Flags().StringVar(&kind, "kind", "", "convenience prefilter to one entity kind, AND-combined with --filter")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "return a per-value breakdown counted by this field (entity column or fact state_key) instead of a total")
	cmd.Flags().IntVar(&limit, "limit", 50, "max groups to return with --group-by, most-populous first")
	return cmd
}

// normalizeCountGroups ensures a count tool response that has a "groupBy" in
// the request always carries a "groups" key in the response JSON, even when
// no rows matched (the tool may return {} instead of {"groups":[]}).
// A missing or null "groups" field is rewritten to an empty array so every
// downstream consumer — JSON passthrough, CSV tabular, text table — sees a
// consistent envelope regardless of result size.
func normalizeCountGroups(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return []byte(`{"groups":[]}`)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw // non-object; pass through unchanged
	}
	if _, ok := obj["groups"]; !ok {
		obj["groups"] = []byte(`[]`)
		out, err := json.Marshal(obj)
		if err != nil {
			return raw
		}
		return out
	}
	return raw
}

// renderCountCLI prints either the single total or the group breakdown of a
// count tool response.
func renderCountCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Count  *int64 `json:"count"`
		Groups []struct {
			Value string `json:"value"`
			Count int64  `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("count: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if out.Count != nil {
		_, err := fmt.Fprintf(w, "%d\n", *out.Count)
		return err
	}
	if len(out.Groups) == 0 {
		_, err := fmt.Fprintln(w, "no matches")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, g := range out.Groups {
		value := g.Value
		if value == "" {
			value = "(none)"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%d\n", value, g.Count); err != nil {
			return err
		}
	}
	return tw.Flush()
}
