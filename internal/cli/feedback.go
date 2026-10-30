package cli

// feedback.go implements the "maz feedback" command group — the human review +
// triage surface of the dynamic lessons / feedback loop (Lane E).
//
//	maz feedback gotchas              show the promoted per-session guidance set
//	maz feedback list                 raw triage rows (carry cluster keys)
//	maz feedback record ...           file a friction point or a distilled lesson
//	maz feedback pin|unpin <key>      protect a cluster from cap eviction (toggle)
//	maz feedback veto|unveto <key>    suppress a cluster from promotion (toggle)
//	maz feedback edit <key> --rule    override a cluster's distilled rule text
//	maz feedback export [-o FILE]     allowlist-redacted diagnostic (json|jsonl|md)
//
// Every subcommand routes through invokeNavTool, so a running daemon serves them
// over its MCP transport and a daemon-less invocation falls back to an in-process
// wiring — the same dispatch every navigation verb uses. The read tool
// (feedbackQuery) and the review tool (feedbackReview) are Unlisted from agent
// tool discovery: review is a human override, not an agent action, and the agent
// already receives the promoted gotchas in its session instructions.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/feedback"
)

// validateSince checks that --since, when set, is strict RFC3339. It returns a
// silentError with exit 1 and prints the message to stderr so the caller can
// return directly. Empty is always valid (no bound).
func validateSince(cmd *cobra.Command, tool, since string) error {
	if since == "" {
		return nil
	}
	if _, err := feedback.ParseSince(since); err != nil {
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", tool, err); werr != nil {
			return werr
		}
		return silentError{code: 1, err: err}
	}
	return nil
}

// newFeedbackCmd returns the "maz feedback" parent command.
func newFeedbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feedback",
		Short: "Review the dynamic lessons / feedback loop (gotchas, triage, export)",
		Long: `Mazarbul learns from how agents use it. The agentFeedback MCP tool records
durable friction points and distilled lessons; recurring ones AUTO-PROMOTE into a
capped, fenced "## Known gotchas & lessons" section appended LAST to every new
session's instructions. This command is the human review + override surface for
that loop.

Subcommands:
  gotchas   Show the active promoted guidance set (recurrence + pin status)
  list      Raw triage rows with cluster keys (local-only)
  record    File a friction point or a distilled lesson from the CLI
  pin       Protect a cluster from cap eviction (unpin to undo)
  veto      Suppress a cluster from promotion (unveto to undo)
  edit      Override a cluster's distilled rule text
  export    Emit an allowlist-redacted diagnostic (no user memory ever leaves)

Promotion is recurrence-gated: a cluster promotes after 3 recurrences (2 for
high severity). Pinned clusters are exempt from the cap; vetoed clusters never
promote. The trusted base instructions are never rewritten — only distilled,
sanitized one-liners are appended.

Examples:
  maz feedback gotchas
  maz feedback record --category lesson --severity medium \
    --surface recall --summary "recall missed an exact name" \
    --rule "prefer get when you already hold the exact name"
  maz feedback list --category confusing
  maz feedback pin "confusing|recall|prefer get exact name"
  maz feedback export --format md -o feedback.md`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newFeedbackGotchasCmd())
	cmd.AddCommand(newFeedbackListCmd())
	cmd.AddCommand(newFeedbackRecordCmd())
	cmd.AddCommand(newFeedbackReviewCmd("pin", "Protect a cluster from cap eviction"))
	cmd.AddCommand(newFeedbackReviewCmd("unpin", "Remove pin protection from a cluster"))
	cmd.AddCommand(newFeedbackReviewCmd("veto", "Suppress a cluster from promotion"))
	cmd.AddCommand(newFeedbackReviewCmd("unveto", "Lift a veto on a cluster"))
	cmd.AddCommand(newFeedbackEditCmd())
	cmd.AddCommand(newFeedbackExportCmd())
	return cmd
}

// ---- gotchas ----

func newFeedbackGotchasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gotchas",
		Short: "Show the active promoted guidance set",
		Long: `Show the gotchas + lessons currently promoted into every new session's
instructions: the recurrence-gated, capped, review-aware set. Each row reports the
distilled rule, its recurrence count, category/severity, and whether it is pinned.
Read-only.

Examples:
  maz feedback gotchas
  maz feedback gotchas --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := invokeNavTool(cmd, "feedbackQuery", json.RawMessage(`{"view":"gotchas"}`))
			if err != nil {
				return wrapNavFatal(cmd, "feedback gotchas", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderFeedbackGotchasCLI(cmd, raw)
		},
	}
	return cmd
}

func renderFeedbackGotchasCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var resp struct {
		Gotchas []feedback.Gotcha `json:"gotchas"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("feedback gotchas: parse response: %w", err)
	}
	if len(resp.Gotchas) == 0 {
		_, err := fmt.Fprintln(cmd.ErrOrStderr(), "no gotchas promoted yet — recurring agent feedback promotes automatically")
		return err
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "RULE\tCATEGORY\tSEVERITY\tSEEN\tPINNED\tCLUSTER"); err != nil {
		return err
	}
	for i := range resp.Gotchas {
		g := &resp.Gotchas[i]
		pinned := ""
		if g.Pinned {
			pinned = "✓"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			g.Rule, g.Category, g.Severity, g.Recurrence, pinned, g.ClusterKey); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// ---- list ----

// feedbackFilterFlags holds the filter flags shared by list + export.
type feedbackFilterFlags struct {
	category string
	severity string
	surface  string
	since    string
	limit    int
}

func (f *feedbackFilterFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.category, "category", "", "filter by category (confusing|missing|wrong_result|slow|tool_error|suggestion|lesson)")
	cmd.Flags().StringVar(&f.severity, "severity", "", "filter by severity (low|medium|high)")
	cmd.Flags().StringVar(&f.surface, "surface", "", "filter by surface (tool/CLI/instruction)")
	cmd.Flags().StringVar(&f.since, "since", "", "RFC3339 lower bound on create time")
	cmd.Flags().IntVar(&f.limit, "limit", 0, "max rows (0 = default)")
}

func (f *feedbackFilterFlags) into(view string) (json.RawMessage, error) {
	in := map[string]any{"view": view}
	putIfSet(in, "category", f.category)
	putIfSet(in, "severity", f.severity)
	putIfSet(in, "surface", f.surface)
	putIfSet(in, "since", f.since)
	if f.limit > 0 {
		in["limit"] = f.limit
	}
	return json.Marshal(in)
}

func newFeedbackListCmd() *cobra.Command {
	f := &feedbackFilterFlags{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List raw feedback triage rows",
		Long: `List the raw recorded feedback rows newest-first, each carrying its cluster
key so you can pin/veto/edit it. Local-only triage view — for the shareable,
redacted form use "maz feedback export". Read-only.

Examples:
  maz feedback list
  maz feedback list --category confusing --severity high
  maz feedback list --since 2026-06-01T00:00:00Z --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateSince(cmd, "feedback list", f.since); err != nil {
				return err
			}
			reqBody, err := f.into("list")
			if err != nil {
				return wrapNavFatal(cmd, "feedback list", err)
			}
			raw, err := invokeNavTool(cmd, "feedbackQuery", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "feedback list", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderFeedbackListCLI(cmd, raw)
		},
	}
	f.register(cmd)
	return cmd
}

func renderFeedbackListCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var resp struct {
		Items []feedback.Item `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("feedback list: parse response: %w", err)
	}
	if len(resp.Items) == 0 {
		_, err := fmt.Fprintln(cmd.ErrOrStderr(), "no feedback recorded")
		return err
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "TIME\tCATEGORY\tSEVERITY\tSURFACE\tSUMMARY\tCLUSTER"); err != nil {
		return err
	}
	for i := range resp.Items {
		it := &resp.Items[i]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			it.CreateTime, it.Category, it.Severity, it.Surface, it.Summary, it.ClusterKey); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// ---- record ----

// feedbackRecordFlags holds the parsed flags for "maz feedback record".
type feedbackRecordFlags struct {
	category string
	severity string
	summary  string
	surface  string
	detail   string
	rule     string
	keywords []string
	scope    string
}

func newFeedbackRecordCmd() *cobra.Command {
	f := &feedbackRecordFlags{}
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Record a friction point or a distilled lesson",
		Long: `Record durable feedback about using Mazarbul from the CLI — the same intake
the agentFeedback MCP tool provides. Give a category and a one-line summary;
optionally the surface, longer detail, and a distilled one-line rule (the text
that gets promoted for a lesson). Recurring feedback auto-promotes into the
per-session guidance. File DURABLE friction or rules, not one-off disagreements.

Examples:
  maz feedback record --category slow --summary "defineEntityKind took ~3s"
  maz feedback record --category lesson --severity medium --surface recall \
    --summary "recall missed an exact name" \
    --rule "prefer get when you already hold the exact name"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := map[string]any{"category": f.category, "summary": f.summary}
			putIfSet(in, "severity", f.severity)
			putIfSet(in, "surface", f.surface)
			putIfSet(in, "detail", f.detail)
			putIfSet(in, "rule", f.rule)
			putIfSet(in, "scope", f.scope)
			if len(f.keywords) > 0 {
				in["keywords"] = f.keywords
			}
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "feedback record", err)
			}
			raw, err := invokeNavTool(cmd, "agentFeedback", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "feedback record", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderFeedbackStatusCLI(cmd, raw)
		},
	}
	cmd.Flags().StringVar(&f.category, "category", "", "required: confusing|missing|wrong_result|slow|tool_error|suggestion|lesson")
	cmd.Flags().StringVar(&f.severity, "severity", "", "low|medium|high (default low)")
	cmd.Flags().StringVar(&f.summary, "summary", "", "required: one-line description of the friction or rule")
	cmd.Flags().StringVar(&f.surface, "surface", "", "tool / CLI / instruction the feedback is about")
	cmd.Flags().StringVar(&f.detail, "detail", "", "optional longer context")
	cmd.Flags().StringVar(&f.rule, "rule", "", "for a lesson: the distilled one-line rule to promote")
	cmd.Flags().StringArrayVar(&f.keywords, "keyword", nil, "tag aiding clustering + search (repeatable)")
	cmd.Flags().StringVar(&f.scope, "scope", "", "scope to record into: personal|project|team (default personal)")
	//nolint:errcheck // flag is defined immediately above
	cmd.MarkFlagRequired("category")
	//nolint:errcheck // flag is defined immediately above
	cmd.MarkFlagRequired("summary")
	return cmd
}

// ---- pin / unpin / veto / unveto ----

func newFeedbackReviewCmd(action, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   action + " <cluster-key>",
		Short: short,
		Long: fmt.Sprintf(`%s.

The cluster key is the value shown in the CLUSTER column of "maz feedback gotchas"
or "maz feedback list". Review actions append an audited agentFeedbackReviewed
event; they never delete anything.

Examples:
  maz feedback %s "confusing|recall|prefer get exact name"`, short, action),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFeedbackReview(cmd, action, args[0], "")
		},
	}
	return cmd
}

func newFeedbackEditCmd() *cobra.Command {
	var rule string
	cmd := &cobra.Command{
		Use:   "edit <cluster-key> --rule <text>",
		Short: "Override a cluster's distilled rule text",
		Long: `Replace the auto-distilled rule for a cluster with your own one-liner. The
edited text is what subsequent sessions see in the gotchas section. Appends an
audited agentFeedbackReviewed event.

Examples:
  maz feedback edit "slow|defineEntityKind|took 3s" \
    --rule "defineEntityKind runs a ~3s semantic dedup pass — expected, not a hang"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(rule) == "" {
				return wrapNavFatal(cmd, "feedback edit", errors.New("--rule is required for edit"))
			}
			return runFeedbackReview(cmd, "edit", args[0], rule)
		},
	}
	cmd.Flags().StringVar(&rule, "rule", "", "the override rule text (required)")
	//nolint:errcheck // flag is defined immediately above
	cmd.MarkFlagRequired("rule")
	return cmd
}

// runFeedbackReview is the shared pin/unpin/veto/unveto/edit dispatch.
func runFeedbackReview(cmd *cobra.Command, action, clusterKey, editedRule string) error {
	in := map[string]any{"clusterKey": clusterKey, "action": action}
	putIfSet(in, "editedRule", editedRule)
	reqBody, err := json.Marshal(in)
	if err != nil {
		return wrapNavFatal(cmd, "feedback "+action, err)
	}
	raw, err := invokeNavTool(cmd, "feedbackReview", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "feedback "+action, err)
	}
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}
	return renderFeedbackStatusCLI(cmd, raw)
}

// renderFeedbackStatusCLI prints the {status, ...} acknowledgement from a write
// tool as aligned key/value rows.
func renderFeedbackStatusCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("feedback: parse response: %w", err)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	for _, key := range sortedKeys(fields) {
		if _, err := fmt.Fprintf(tw, "%s\t%v\n", key, fields[key]); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// ---- export ----

func newFeedbackExportCmd() *cobra.Command {
	f := &feedbackFilterFlags{}
	var format, outPath string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export an allowlist-redacted feedback diagnostic",
		Long: `Export the recorded feedback as a shareable diagnostic. The export is an
ALLOWLIST projection: it emits ONLY {id, ts, category, severity, surface, summary,
detail, keywords, version, activePreset}. It never emits user memory, principal
identifiers, cluster keys, source event ids, or team scope — the query reads the
feedback table alone and never joins, so redaction is structural.

Formats: json (a pretty array, default), jsonl (one object per line), md (a
human-readable markdown report). Write to a file with -o, else to stdout.

Examples:
  maz feedback export
  maz feedback export --format md -o feedback.md
  maz feedback export --format jsonl --severity high -o high.jsonl`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateSince(cmd, "feedback export", f.since); err != nil {
				return err
			}
			return runFeedbackExport(cmd, f, format, outPath)
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&format, "format", "json", "export format: json|jsonl|md")
	cmd.Flags().StringVarP(&outPath, "out", "o", "", "write to FILE instead of stdout")
	return cmd
}

func runFeedbackExport(cmd *cobra.Command, f *feedbackFilterFlags, format, outPath string) error {
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "", "json", "jsonl", "md":
	default:
		return wrapNavFatal(cmd, "feedback export", fmt.Errorf("unknown --format %q (want json|jsonl|md)", format))
	}

	reqBody, err := f.into("export")
	if err != nil {
		return wrapNavFatal(cmd, "feedback export", err)
	}
	// The tool renders markdown itself; json/jsonl share the items payload.
	if format == "md" {
		var m map[string]any
		if uerr := json.Unmarshal(reqBody, &m); uerr != nil {
			return wrapNavFatal(cmd, "feedback export", uerr)
		}
		m["format"] = "md"
		if reqBody, err = json.Marshal(m); err != nil {
			return wrapNavFatal(cmd, "feedback export", err)
		}
	}

	raw, err := invokeNavTool(cmd, "feedbackQuery", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "feedback export", err)
	}
	if raw == nil {
		return wrapNavFatal(cmd, "feedback export", errors.New(errNavUnavailableMsg))
	}

	payload, err := feedbackExportPayload(raw, format)
	if err != nil {
		return wrapNavFatal(cmd, "feedback export", err)
	}
	return writeFeedbackExport(cmd, payload, outPath)
}

// feedbackExportPayload turns the feedbackQuery export result into the bytes to
// emit for the chosen format.
func feedbackExportPayload(raw json.RawMessage, format string) ([]byte, error) {
	var resp struct {
		Markdown string                `json:"markdown"`
		Items    []feedback.ExportItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse export response: %w", err)
	}
	switch format {
	case "md":
		return []byte(resp.Markdown), nil
	case "jsonl":
		var b strings.Builder
		for i := range resp.Items {
			line, err := json.Marshal(resp.Items[i])
			if err != nil {
				return nil, fmt.Errorf("marshal export item: %w", err)
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	default: // json
		body, err := json.MarshalIndent(resp.Items, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal export items: %w", err)
		}
		return append(body, '\n'), nil
	}
}

// writeFeedbackExport writes payload to outPath (0600) or stdout. A file write
// confirms the path on stderr so stdout stays clean for redirection.
func writeFeedbackExport(cmd *cobra.Command, payload []byte, outPath string) error {
	if outPath == "" {
		_, err := cmd.OutOrStdout().Write(payload)
		return err
	}
	if err := os.WriteFile(outPath, payload, 0o600); err != nil {
		return wrapNavFatal(cmd, "feedback export", fmt.Errorf("write %s: %w", outPath, err))
	}
	//nolint:errcheck // best-effort confirmation on stderr
	fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", len(payload), outPath)
	return nil
}
