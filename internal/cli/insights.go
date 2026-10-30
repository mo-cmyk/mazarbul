package cli

// insights.go — `maz insights …` — the single cognitive-reviewer surface over
// the Insight Inbox. It mirrors the MCP tools one-for-one through the
// established CLI→tool path (invokeNavTool):
//
//	maz insights [--state hot|demoted|dismissed|all]   list synthesized insights
//	maz insights show <id>                             render one insight + citations
//	maz insights dismiss <id>                          record an insightDismissed event
//	maz insights reflect                               trigger a synthesis pass now
//
// Every subcommand honours --output text|json|ndjson and the documented exit
// codes (0 ok, 1 expected failure, 2 unexpected). dismiss is an event append,
// never an in-place edit: it routes through the dismissInsight tool, which
// appends insightDismissed.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// Registered tool names the insights subcommands dispatch to.
const (
	insightsListToolName    = "listInsights"
	insightsDismissToolName = "dismissInsight"
	insightsReflectToolName = "triggerReflection"
	insightsGetToolName     = "get"
)

func newInsightsCmd() *cobra.Command {
	listFlags := &insightsListFlags{}
	cmd := &cobra.Command{
		Use:   "insights",
		Short: "Review synthesized insights: list, inspect, dismiss, or reflect (LLM-free reads)",
		Long: `The Insight Inbox: the single place to review what reflection has synthesized
about your graph. With no subcommand it lists the hot (live) insights, numbered
and most-confident first, each with the number of source records it cites.

Subcommands:
  show <id>      render one insight in full with its evidence / entity citations
  dismiss <id>   dismiss an insight you judge wrong or unhelpful
  reflect        trigger a synthesis pass now (normally runs autonomously)

An <id> is an insights/{uid} resource name (copy it from the listing's "name"
column or a recall hit) or a bare uid.

Dismissal records a durable insightDismissed EVENT (never an in-place edit): the
insight moves to a terminal 'dismissed' state, drops out of recall, and is never
re-promoted — and the decision survives a rebuild.

--state selects which insights to list: hot (default), demoted (decayed out of
recall), dismissed (explicitly dismissed), or all.

JSON shape (--output json): {"insights": [ {name, uid, statement, insightType,
state, currentConfidence, synthesizedConfidence, seedEntityUid, synthesizedAt,
sourceCount, sourceCitations[]}, ... ]}. NDJSON emits one insight per line.

Exit codes:
  0   success
  1   expected failure: insight not found, bad input
  2   unexpected failure: storage or wiring error

Examples:
  maz insights
  maz insights --state dismissed --output json
  maz insights show insights/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz insights dismiss 01ARZ3NDEKTSV4RRFFQ69G5FAV --reason "incorrect inference"
  maz insights reflect`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInsightsList(cmd, listFlags)
		},
	}
	listFlags.register(cmd)
	cmd.AddCommand(newInsightsListCmd())
	cmd.AddCommand(newInsightsShowCmd())
	cmd.AddCommand(newInsightsDismissCmd())
	cmd.AddCommand(newInsightsReflectCmd())
	return cmd
}

// ---- list ----

// insightsListFlags holds the listing knobs, shared by `maz insights` (the
// bare parent) and the explicit `maz insights list` subcommand.
type insightsListFlags struct {
	state  string
	limit  int
	scopes []string
}

func (f *insightsListFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.state, "state", "hot",
		"which insights to list: hot (default), demoted, dismissed, or all")
	cmd.Flags().IntVar(&f.limit, "limit", 50, "maximum insights to return")
	cmd.Flags().StringArrayVar(&f.scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
}

func newInsightsListCmd() *cobra.Command {
	f := &insightsListFlags{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List synthesized insights, numbered and most-confident first",
		Long: `List synthesized insights by lifecycle state, numbered and most-confident
first, each with the count of source records it cites. The same listing as the
bare "maz insights"; provided explicitly for discoverability.

Examples:
  maz insights list
  maz insights list --state all --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInsightsList(cmd, f)
		},
	}
	f.register(cmd)
	return cmd
}

func runInsightsList(cmd *cobra.Command, f *insightsListFlags) error {
	if err := validateScopeFlag(cmd, "insights", f.scopes); err != nil {
		return err
	}
	in := map[string]any{}
	if f.state != "" {
		in["state"] = f.state
	}
	if f.limit > 0 {
		in["limit"] = f.limit
	}
	if len(f.scopes) > 0 {
		in["scopes"] = f.scopes
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return wrapNavFatal(cmd, "insights", err)
	}
	raw, err := invokeNavTool(cmd, insightsListToolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "insights", err)
	}
	// NDJSON: emit one insight object per line before the generic passthrough.
	if output.From(cmd).Format() == output.FormatNDJSON {
		return renderInsightsNDJSON(cmd, raw)
	}
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}
	return renderInsightsCLI(cmd, raw, f.limit)
}

// insightRow is the subset of a listInsights item the text/NDJSON renderers read.
type insightRow struct {
	Name              string  `json:"name"`
	Statement         string  `json:"statement"`
	State             string  `json:"state"`
	CurrentConfidence float64 `json:"currentConfidence"`
	SourceCount       int     `json:"sourceCount"`
}

func renderInsightsCLI(cmd *cobra.Command, raw json.RawMessage, limit int) error {
	var out struct {
		Insights []insightRow `json:"insights"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("insights: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if len(out.Insights) == 0 {
		_, err := fmt.Fprintln(w, "no insights")
		return err
	}
	c := cmdColorizer(cmd)
	wr := output.From(cmd)
	if perr := output.MaybePage(cmd, func(pw io.Writer) error {
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, []output.Column{
			{Title: "#", Align: output.AlignRight},
			{Title: "insight", MaxWidth: 50, Truncate: true},
			{Title: "cites", Align: output.AlignRight},
			{Title: "conf", Align: output.AlignRight},
			{Title: "state"},
			{Title: "name"},
		})
		for i := range out.Insights {
			ins := &out.Insights[i]
			tbl.Row(i+1, ins.Statement, ins.SourceCount, fmt.Sprintf("%.2f", ins.CurrentConfidence), ins.State, ins.Name)
		}
		return tbl.Render()
	}); perr != nil {
		return perr
	}
	output.ShowingNofM(wr, c, len(out.Insights), 0, limit, "raise --limit")
	return nil
}

// renderInsightsNDJSON emits one JSON object per insight to cmd's stdout,
// matching the --output ndjson contract used by entities/relations/neighbors.
func renderInsightsNDJSON(cmd *cobra.Command, raw json.RawMessage) error {
	if raw == nil {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}
	var out struct {
		Insights []json.RawMessage `json:"insights"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("insights: parse ndjson response: %w", err)
	}
	w := cmd.OutOrStdout()
	for _, ins := range out.Insights {
		if _, err := fmt.Fprintln(w, string(ins)); err != nil {
			return err
		}
	}
	return nil
}

// ---- show ----

func newInsightsShowCmd() *cobra.Command {
	var scopes []string
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one insight in full with its evidence / entity citations",
		Long: `Show a single synthesized insight: its statement, type, lifecycle state,
confidence, and the source records it cites (as entities/{uid} resource names
you can follow with "maz get" / "maz neighbors").

<id> is an insights/{uid} resource name or a bare uid.

JSON shape (--output json): the get-tool discriminated union
{"type": "insight", "insight": { ...fields..., "sourceCitations": [...] }}.

Examples:
  maz insights show insights/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz insights show 01ARZ3NDEKTSV4RRFFQ69G5FAV --output json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInsightsShow(cmd, args[0], scopes)
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	return cmd
}

func runInsightsShow(cmd *cobra.Command, id string, scopes []string) error {
	if err := validateScopeFlag(cmd, "insights show", scopes); err != nil {
		return err
	}
	// A bare uid is promoted to insights/{uid}; the get tool routes on the
	// collection prefix and only an entity accepts a bare token.
	name := id
	if !strings.Contains(name, "/") {
		name = "insights/" + name
	}
	in := map[string]any{"name": name}
	if len(scopes) > 0 {
		in["scopes"] = scopes
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return wrapNavFatal(cmd, "insights show", err)
	}
	raw, err := invokeNavTool(cmd, insightsGetToolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "insights show", err)
	}
	// get returns a single object; NDJSON emits it as one JSON line.
	if output.From(cmd).Format() == output.FormatNDJSON {
		return renderGetNDJSON(cmd, raw)
	}
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}
	return renderInsightShowCLI(cmd, raw)
}

// insightShowRecord is the get-tool insight arm the show renderer consumes.
type insightShowRecord struct {
	Type    string `json:"type"`
	Insight *struct {
		UID                   string   `json:"uid"`
		Name                  string   `json:"name"`
		Statement             string   `json:"statement"`
		InsightType           string   `json:"insightType"`
		State                 string   `json:"state"`
		CurrentConfidence     float64  `json:"currentConfidence"`
		SynthesizedConfidence float64  `json:"synthesizedConfidence"`
		SeedEntityUID         string   `json:"seedEntityUid"`
		SynthesizedAt         string   `json:"synthesizedAt"`
		SourceUIDs            []string `json:"sourceUids"`
		SourceCitations       []string `json:"sourceCitations"`
	} `json:"insight"`
}

// renderInsightShowCLI prints one insight as a card: a ◆ statement header, a
// dimmed meta line, then an "evidence" section listing the entity citations.
func renderInsightShowCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var rec insightShowRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("insights show: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if rec.Type != "insight" || rec.Insight == nil {
		// The id resolved to a non-insight record; echo the raw payload rather than
		// pretending it is an insight.
		_, err := fmt.Fprintln(w, string(raw))
		return err
	}
	clr := colorizer.New(interactive.ShouldStyle(cmd, w), w)
	ins := rec.Insight
	if _, err := fmt.Fprintf(w, "%s %s\n", clr.Accent("◆"), ins.Statement); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  %s %s\n", clr.Dim("name"), clr.Ident(insightShowName(ins.Name, ins.UID))); err != nil {
		return err
	}
	if meta := insightMetaLine(ins.InsightType, ins.State, ins.CurrentConfidence, ins.SynthesizedAt); meta != "" {
		if _, err := fmt.Fprintf(w, "  %s\n", clr.Dim(meta)); err != nil {
			return err
		}
	}
	return renderInsightEvidence(w, clr, insightCitations(ins.SourceCitations, ins.SourceUIDs))
}

// insightShowName returns the resource name, falling back to insights/{uid}.
func insightShowName(name, uid string) string {
	if name != "" {
		return name
	}
	return "insights/" + uid
}

// insightMetaLine joins the present meta fields with " · " for the dimmed line.
func insightMetaLine(insightType, state string, conf float64, synthesizedAt string) string {
	meta := make([]string, 0, 4)
	if insightType != "" {
		meta = append(meta, insightType)
	}
	if state != "" {
		meta = append(meta, state)
	}
	if conf > 0 {
		meta = append(meta, fmt.Sprintf("conf %.2f", conf))
	}
	if synthesizedAt != "" {
		meta = append(meta, "synthesized "+showDatePart(synthesizedAt))
	}
	return strings.Join(meta, " · ")
}

// insightCitations prefers the typed entities/{uid} citations and falls back to
// the raw source uids when the server did not type them.
func insightCitations(typed, raw []string) []string {
	if len(typed) > 0 {
		return typed
	}
	return raw
}

// renderInsightEvidence writes the "evidence" section listing the citations.
func renderInsightEvidence(w io.Writer, clr *colorizer.Colorizer, citations []string) error {
	if len(citations) == 0 {
		_, err := fmt.Fprintf(w, "\n%s\n", clr.Dim("no citations"))
		return err
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", clr.Dim(fmt.Sprintf("evidence (%d)", len(citations)))); err != nil {
		return err
	}
	for _, c := range citations {
		if _, err := fmt.Fprintf(w, "  %s %s\n", clr.Dim("•"), clr.Ident(c)); err != nil {
			return err
		}
	}
	return nil
}

// ---- dismiss ----

func newInsightsDismissCmd() *cobra.Command {
	var reason string
	var scopes []string
	cmd := &cobra.Command{
		Use:   "dismiss <id>",
		Short: "Dismiss an insight (records an insightDismissed event; never an in-place edit)",
		Long: `Dismiss a synthesized insight you judge wrong or unhelpful.

This APPENDS a durable insightDismissed event — the event log stays the source
of truth — which the dispatcher projects to a terminal 'dismissed' state. The
insight drops out of recall and is never re-promoted by access, and the decision
survives "maz rebuild". It is NOT an in-place row edit, and it is distinct from
the system's own confidence decay (which produces a 'demoted', re-promotable
state).

<id> is an insights/{uid} resource name or a bare uid. Pass --reason to record
why (retained on the event for audit).

Exit codes:
  0   insight dismissed (or already dismissed — idempotent)
  1   expected failure: insight not found, bad input
  2   unexpected failure: storage or wiring error

Examples:
  maz insights dismiss insights/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz insights dismiss 01ARZ3NDEKTSV4RRFFQ69G5FAV --reason "incorrect inference"
  maz insights dismiss 01ARZ3... --output json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInsightsDismiss(cmd, args[0], reason, scopes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "",
		"optional free-text justification, retained on the event for audit")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to resolve the insight in (repeatable): project, personal, team. Default: project, personal.")
	return cmd
}

func runInsightsDismiss(cmd *cobra.Command, id, reason string, scopes []string) error {
	if err := validateScopeFlag(cmd, "insights dismiss", scopes); err != nil {
		return err
	}
	in := map[string]any{"name": id}
	if reason != "" {
		in["reason"] = reason
	}
	if len(scopes) > 0 {
		in["scopes"] = scopes
	}
	reqBody, berr := json.Marshal(in)
	if berr != nil {
		return respondFatal(cmd, "insights dismiss", fmt.Errorf("marshal dismiss request: %w", berr))
	}
	raw, err := invokeNavTool(cmd, insightsDismissToolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "insights dismiss", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr on the unavailable path
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}
	var resp struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return respondFatal(cmd, "insights dismiss", fmt.Errorf("parse dismiss response: %w", uerr))
	}
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{"name": resp.Name, "status": resp.Status})
	}
	_, err = fmt.Fprintf(w, "%s %s\n", resp.Name, resp.Status)
	return err
}

// ---- reflect ----

func newInsightsReflectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reflect",
		Short: "Trigger a reflection pass now: synthesize new insights from the graph",
		Long: `Trigger one reflection pass on demand: cluster connected entities and
synthesize durable insights, each citing the source records it drew from.
Reflection normally runs autonomously on an idle daily tick; this forces it now.

Returns the number of insights synthesized, or a "skipped" reason (breaker-open
when the LLM backend is down, no-candidates when there is nothing to reflect on).

JSON shape (--output json): {"appendedEventUids": [...], "appendedCount": N,
"skipped": "..."}.

Examples:
  maz insights reflect
  maz insights reflect --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInsightsReflect(cmd)
		},
	}
}

func runInsightsReflect(cmd *cobra.Command) error {
	raw, err := invokeNavTool(cmd, insightsReflectToolName, json.RawMessage(`{}`))
	if err != nil {
		return wrapNavFatal(cmd, "insights reflect", err)
	}
	// reflect returns a single object; NDJSON emits it as one JSON line.
	if output.From(cmd).Format() == output.FormatNDJSON {
		return renderGetNDJSON(cmd, raw)
	}
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}
	return renderInsightsReflectCLI(cmd, raw)
}

// renderInsightsReflectCLI prints a one-line human summary of a reflection pass.
func renderInsightsReflectCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		AppendedCount int    `json:"appendedCount"`
		Skipped       string `json:"skipped"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("insights reflect: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	switch {
	case out.Skipped != "":
		_, err := fmt.Fprintf(w, "reflection skipped: %s\n", out.Skipped)
		return err
	case out.AppendedCount == 0:
		_, err := fmt.Fprintln(w, "reflection ran; no new insights synthesized")
		return err
	default:
		_, err := fmt.Fprintf(w, "reflection synthesized %d insight(s)\n", out.AppendedCount)
		return err
	}
}
