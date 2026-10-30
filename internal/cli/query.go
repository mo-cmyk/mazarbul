package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// newQueryCmd is the read-only SQL escape hatch: it runs a single SELECT/WITH
// statement over the curated views (v_entities / v_relations / v_facts) and
// prints the rows as an aligned table. Writes, chained statements, and DDL are
// rejected by the query tool; the connection is read-only regardless.
func newQueryCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "query <sql>",
		Short: "Run a read-only SQL SELECT over the curated views (LLM-free)",
		Long: `Run a single read-only SELECT (or WITH … SELECT) over the curated views —
the analytical escape hatch for questions the structured 'maz entities --filter'
and 'maz count' surfaces can't express.

Query these stable views (run 'maz introspect' for their columns):
  v_entities   uid, kind, display_name, confidence, resolution_status, …
  v_relations  subject_name, predicate, object_name, attributes, active, …
  v_facts      subject_uid, subject_name, state_key, value, …  (open facts)

Writes, multiple statements, PRAGMA/ATTACH and DDL are rejected; the connection
is opened read-only. Results are capped (see --limit).

Only project and personal rows are visible; team-scope rows are excluded. To
query the team scope use the 'query' MCP tool with scopes:["team"].

Examples:
  maz query 'SELECT kind, COUNT(*) FROM v_entities GROUP BY kind'
  maz query 'SELECT state_key, COUNT(*) FROM v_facts GROUP BY state_key' --json
  maz query 'SELECT subject_name, object_name FROM v_relations WHERE predicate = "works_at"'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{"sql": args[0]}
			if cmd.Flags().Changed("limit") {
				in["limit"] = limit
			}
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "query", err)
			}
			raw, err := invokeNavTool(cmd, "query", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "query", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderQueryCLI(cmd, raw)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 200, "maximum rows to return (hard-capped at 1000)")
	return cmd
}

// renderQueryCLI prints a query result as a header row plus aligned value rows,
// appending a truncation note when more rows matched than were returned.
func renderQueryCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Columns   []string         `json:"columns"`
		Rows      []map[string]any `json:"rows"`
		RowCount  int              `json:"rowCount"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("query: parse response: %w", err)
	}
	if out.RowCount == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "(0 rows)")
		return err
	}
	c := cmdColorizer(cmd)
	cols := make([]output.Column, len(out.Columns))
	for i, name := range out.Columns {
		cols[i] = output.Column{Title: name}
	}
	if perr := output.MaybePage(cmd, func(pw io.Writer) error {
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, cols)
		for _, row := range out.Rows {
			cells := make([]any, len(out.Columns))
			for i, col := range out.Columns {
				cells[i] = formatCell(row[col])
			}
			tbl.Row(cells...)
		}
		return tbl.Render()
	}); perr != nil {
		return perr
	}
	if out.Truncated {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "(%d rows; truncated — more matched)\n", out.RowCount)
		return err
	}
	return nil
}

// formatCell renders a JSON-decoded scalar for the text table: NULL for nil, the
// integer form for whole-number floats (json.Unmarshal decodes all numbers to
// float64), and %v otherwise.
func formatCell(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
