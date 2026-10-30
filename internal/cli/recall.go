package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/tui/browse"
)

// errRecallDisabledMsg is the message emitted when the recall tool is not
// found in the wired tool slice, indicating embeddings are disabled.
const errRecallDisabledMsg = `recall tool not available; run "maz init --embed=ollama" to enable embeddings`

// recallDefaultMinScore is the fused-score floor applied when the caller does
// not set --min-score explicitly. 0 means "return everything the kernel
// returns" — i.e. no filtering by default. A caller that wants to suppress
// clearly-irrelevant hits should pass --min-score 0.05 or higher.
const recallDefaultMinScore float64 = 0

// recallFlags holds the parsed flag values for "maz recall".
type recallFlags struct {
	limit            int
	at               string
	asOf             string // --as-of: RFC 3339 transaction-time anchor
	asOfEventUID     string // --as-of-event: resolve event ULID to its createTime as asOf
	noGraph          bool
	near             string
	radiusKm         float64
	rewriteQuery     bool
	minScore         float64
	scopes           []string
	kinds            []string
	tui              bool
	explain          bool     // --explain / --why: show per-lane score breakdown per hit
	anchor           string   // --anchor: restrict recall to N-hop frontier of this entity ULID
	anchorDepth      int      // --anchor-depth: hop radius (1..6, default 1)
	embedDeadlineMs  int      // --embed-deadline-ms: per-call embedder deadline
	source           []string // --source: constrain hits to these origin/connector sources
	hasPrimarySource bool     // --has-primary-source: keep only hits grounded in a primary source
}

func newRecallCmd() *cobra.Command {
	flags := &recallFlags{}
	cmd := &cobra.Command{
		Use:   "recall <query>",
		Short: "Search the memory layer for episodes and documents matching a query",
		Long: `Search the event log for episodes and documents matching a natural-language
query. The hybrid retrieval pipeline fuses vector, FTS, graph, and recency
scores via weighted fusion and returns ranked hits with a per-hit score breakdown.

Hit kinds returned: episode, entity, document, documentRevision.

To open the top result, pass the hit's own resource name — episodes/{uid},
shown as the "ref" column in --output=csv/kv — to "maz get" for the full record.
Each hit's entities[] also gives entity resource names (entities/{uid}); pass
one to "maz get", or to "maz neighbors"/"maz relations"/"maz traverse" to follow
the graph.

This command always runs a fresh search of the given text. To replay a
frequently-run query by a short name, save it with "maz saved add" and replay it
with "maz saved recall <name>" (note: "maz recall <name>" searches for the word
<name>, it does not replay a saved query).

JSON envelope top-level fields (--output=json):
  hits[]    ranked array of hit objects with fusedScore and components
  degraded  bool — set when a retrieval lane is unavailable (graceful degradation)
  lanes     which lanes ran: vector, fts, graph, nameEmb

In JSON mode the raw hits array is emitted verbatim including the components
block, so the plan's "components.graph > 0" assertion is directly observable
in scripts and CI.

See "maz help-topics recall" for tuning knobs (mmr_lambda, recency_boost, etc.).

Exit codes:
  0   results returned (zero hits is still exit 0)
  1   expected failure: bad input, recall disabled
  2   unexpected failure: storage or wiring error
  130 interrupted by Ctrl-C

Examples:
  # Plain-text ranked table
  maz recall "Berlin"

  # Show per-lane score breakdown (rawSemantic, rawBM25, graph, name-emb)
  maz recall "Berlin" --explain

  # JSON output with score components
  maz recall "Berlin" --output=json

  # Spatial filter within 100 km of Berlin
  maz recall "who do I know?" --near Berlin --radius-km 100

  # Limit results and pin temporal view
  maz recall "team structure" --limit 5 --at 2026-01-01T00:00:00Z

  # Skip graph lane for a fast vector+FTS-only search
  maz recall "project Zephyr" --no-graph

  # Document-only search (scriptable equivalent of the --tui Kind filter)
  maz recall "service" --kind document

  # Restrict to a single scope
  maz recall "team standup" --scope team

  # Read from both personal and team scopes
  maz recall "standup" --scope personal --scope team

  # Follow a recall hit into the graph
  maz recall "Erik" --output=json | jq -r '.hits[0].entities[0].name' | xargs maz get`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecall(cmd, args[0], flags)
		},
	}
	cmd.Flags().IntVar(&flags.limit, "limit", 10, "maximum hits to return (1..50)")
	cmd.Flags().StringVar(&flags.at, "at", "", "RFC 3339 UTC instant for the bi-temporal valid-time filter; restricts entity facts to those valid at this instant but does not affect episode visibility (episodes have no validity window)")
	cmd.Flags().StringVar(&flags.asOf, "as-of", "", "RFC 3339 UTC transaction-time anchor; hides events whose transaction time is later than this, answering 'what did we know on X?'")
	cmd.Flags().StringVar(&flags.asOfEventUID, "as-of-event", "", "event ULID; resolves to that event's createTime and uses it as the --as-of anchor (mutually exclusive with --as-of and --at)")
	cmd.Flags().BoolVar(&flags.noGraph, "no-graph", false, "skip graph traversal lane (vector + FTS only)")
	cmd.Flags().StringVar(&flags.near, "near", "", "restrict results to episodes near this named place, e.g. Berlin")
	cmd.Flags().Float64Var(&flags.radiusKm, "radius-km", 50, "search radius in kilometres when --near is set (default 50)")
	cmd.Flags().BoolVar(&flags.rewriteQuery, "rewrite-query", false, "expand the query via query-rewrite-v1 before retrieval")
	cmd.Flags().Float64Var(&flags.minScore, "min-score", recallDefaultMinScore,
		"drop hits whose fusedScore is below this floor (0=off, e.g. 0.05 suppresses clearly-irrelevant results)")
	cmd.Flags().StringArrayVar(&flags.scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	cmd.Flags().StringArrayVar(&flags.kinds, "kind", nil,
		"restrict hits to these record kinds (repeatable): document, episode, insight, consolidated. Omit for the default mix of episodes + documents. This is the scripted equivalent of the recall --tui Kind filter.")
	cmd.Flags().BoolVar(&flags.tui, "tui", false,
		"browse results in an interactive master-detail TUI (recall→get→neighbors; TTY only)")
	cmd.Flags().BoolVar(&flags.explain, "explain", false,
		"show per-lane score breakdown for each hit: rawSemantic, rawBM25, graph, name-emb, and the fusion mode")
	// --why is a second binding to the same bool — both flags toggle the same explain field.
	cmd.Flags().BoolVar(&flags.explain, "why", false, "alias for --explain")
	cmd.Flags().StringVar(&flags.anchor, "anchor", "", "entity ULID; restrict recall candidates to episodes citing entities within --anchor-depth hops of this entity")
	cmd.Flags().IntVar(&flags.anchorDepth, "anchor-depth", 1, "hop radius around --anchor (1..6)")
	cmd.Flags().IntVar(&flags.embedDeadlineMs, "embed-deadline-ms", 0, "per-call embedder deadline in milliseconds; 0 means use the session context deadline")
	cmd.Flags().StringArrayVar(&flags.source, "source", nil,
		"constrain hits to this origin source (repeatable, OR): a connector name like granola or its connector:granola form, or remember/extractor/v1. Filtered after ranking; an unknown source returns no hits.")
	cmd.Flags().BoolVar(&flags.hasPrimarySource, "has-primary-source", false,
		"keep only hits grounded in a primary source (a recorded source_ref or an active has_primary_source edge)")
	return cmd
}

// runRecall wires the daemon, finds the recall tool, invokes it, and renders
// the hits in the active output mode.
func runRecall(cmd *cobra.Command, query string, flags *recallFlags) error { //nolint:gocyclo // routing function; branches are independent format switches
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	if err := validateRecallFlags(cmd, flags); err != nil {
		return err
	}

	// --tui drives the master-detail browse surface (recall→get→neighbors).
	// It only takes effect in text mode on a real terminal; JSON/NDJSON and
	// piped output fall through to the table render so scripts are unaffected.
	if flags.tui && interactive.IsTTY(cmd.OutOrStdout()) && output.From(cmd).Format() == output.FormatText {
		loader := &recallBrowseLoader{cmd: cmd, flags: flags}
		banks := buildRecallBanks()
		// Silence the per-call daemon-dispatch advisory while the alt-screen owns
		// the terminal (C1): each debounced re-query would otherwise write to
		// stderr and corrupt the frame. Restore before reporting any TUI error.
		restoreErr := suppressCmdStderr(cmd)
		berr := browse.RunWithStrip(loader, query, interactive.ShouldStyle(cmd, cmd.OutOrStdout()), banks)
		restoreErr()
		if berr != nil {
			return wrapRecallFatal(cmd, berr)
		}
		return nil
	}

	reqBody, err := buildRecallRequest(cmd, query, flags)
	if err != nil {
		return err
	}

	rawResult, err := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger,
		"recall", reqBody,
		func(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, req json.RawMessage) (json.RawMessage, error) {
			return invokeNamedTool(ctx, wired, "recall", req)
		},
	)
	if err != nil {
		return wrapRecallFatal(cmd, err)
	}

	if rawResult == nil {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errRecallDisabledMsg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: fmt.Errorf("%s", errRecallDisabledMsg)}
	}

	rawResult, err = applyRecallPostFilters(cmd, rawResult, flags)
	if err != nil {
		return err
	}

	// When recall finds nothing and a saved recall shares the query's name, the
	// user most likely meant to REPLAY it: `recall <name>` runs a literal search,
	// `saved recall <name>` replays the stored query. Nudge on stderr so stdout
	// machine output is untouched, and only on the genuinely-confusing 0-hit case.
	maybeHintSavedRecall(cmd, rawResult, query)

	w := output.From(cmd)
	switch w.Format() {
	case output.FormatNDJSON:
		// Strip FTS <b>…</b> highlight markup from the verbatim machine envelope
		// so json/ndjson match csv/kv/table (which already strip). Agents ground
		// on the clean `text` field; the snippet should not carry display markup.
		return renderRecallNDJSON(cmd, stripSnippetMarkup(rawResult))
	case output.FormatJSON:
		// --explain is a no-op in JSON mode: the full components block is already
		// on the wire (rawSemantic, rawBM25, graph, nameEmb, recency, strength).
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(stripSnippetMarkup(rawResult)))
		return err
	case output.FormatCSV:
		return renderRecallCSV(cmd, rawResult)
	case output.FormatKV:
		return renderRecallKV(cmd, rawResult)
	default:
		return renderRecallText(cmd, w, rawResult, flags, query)
	}
}

// recallLimitMin and recallLimitMax are the documented bounds for --limit on
// both "maz recall" and "maz ask". Values outside [1, 50] are rejected at
// parse time so the caller receives a clear E_VALIDATION message rather than
// a silent no-op or a tool-layer error that leaks implementation detail.
const (
	recallLimitMin = 1
	recallLimitMax = 50
)

// recallFlagError writes msg to stderr and returns a silentError with exit
// code 1. Routing every validateRecallFlags violation through one helper keeps
// that function below the cognitive-complexity gate: each inline stderr write
// previously added a branch (the err-check on Fprintln) to the parent.
func recallFlagError(cmd *cobra.Command, msg string) error {
	//nolint:errcheck // best-effort advisory; the silentError is the real signal
	fmt.Fprintln(cmd.ErrOrStderr(), msg)
	return silentError{code: 1, err: errors.New(msg)}
}

// validateRecallFlags enforces cross-flag constraints and returns a
// silentError with code 1 on violation.
func validateRecallFlags(cmd *cobra.Command, flags *recallFlags) error {
	if flags.limit < recallLimitMin || flags.limit > recallLimitMax {
		return recallFlagError(cmd, fmt.Sprintf(
			"recall: --limit %d out of range: must be between %d and %d",
			flags.limit, recallLimitMin, recallLimitMax))
	}
	if err := validateRecallTemporalFlags(cmd, flags); err != nil {
		return err
	}
	if flags.near == "" && cmd.Flags().Changed("radius-km") {
		return recallFlagError(cmd, "recall: --radius-km requires --near")
	}
	if len(flags.scopes) > 0 {
		if _, err := kernel.ParseScopeSet(flags.scopes); err != nil {
			return recallFlagError(cmd, fmt.Sprintf("recall: --scope: %v", err))
		}
	}
	for _, k := range flags.kinds {
		if !validRecallKind(k) {
			return recallFlagError(cmd, fmt.Sprintf(
				"recall: --kind %q: unknown kind (valid: document, episode, insight, consolidated)", k))
		}
	}
	if err := validateRecallAnchorFlags(cmd, flags); err != nil {
		return err
	}
	return nil
}

// anchorDepthMin and anchorDepthMax are the validated bounds for --anchor-depth.
// The schema already advertises minimum:1 maximum:6; the CLI validates here so
// the user sees a clear message even when calling through the cobra flag layer
// directly (the schema is enforced by the MCP adapter, not the CLI layer).
const (
	anchorDepthMin = 1
	anchorDepthMax = 6
)

// hitTypeLabelEp and hitTypeLabelEnt are the short type labels rendered in
// recall tables, CSV rows, and KV blocks.
const (
	hitTypeLabelEp  = "episode"
	hitTypeLabelEnt = "entity"
)

// validateRecallAnchorFlags enforces anchor / anchor-depth co-constraints:
//   - --anchor-depth <1 or >6 is an error.
//   - --anchor-depth set without --anchor produces a warning (no-op flag).
//   - An episode UID passed as --anchor (episodes/ prefix) is rejected with a
//     clear diagnostic; only entity UIDs (entities/ or bare ULIDs) are valid.
func validateRecallAnchorFlags(cmd *cobra.Command, flags *recallFlags) error {
	depthChanged := cmd.Flags().Changed("anchor-depth")
	if depthChanged {
		if flags.anchorDepth < anchorDepthMin || flags.anchorDepth > anchorDepthMax {
			msg := fmt.Sprintf("recall: --anchor-depth %d out of range: must be between %d and %d",
				flags.anchorDepth, anchorDepthMin, anchorDepthMax)
			if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
				return werr
			}
			return silentError{code: 1, err: errors.New(msg)}
		}
		if flags.anchor == "" {
			if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
				"recall: warning: --anchor-depth has no effect without --anchor"); werr != nil {
				return werr
			}
		}
	}
	if flags.anchor != "" && strings.HasPrefix(flags.anchor, "episodes/") {
		msg := "recall: --anchor expects an entity ULID or entities/{uid}, not an episode name; use the entity name from the hit's entities[] field"
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: errors.New(msg)}
	}
	return nil
}

// validateRecallTemporalFlags validates the mutually-exclusive temporal
// anchor flags: --at, --as-of, and --as-of-event. Each must be a valid
// RFC 3339 timestamp when set, and asOfEventUID is exclusive with both.
func validateRecallTemporalFlags(cmd *cobra.Command, flags *recallFlags) error {
	if flags.at != "" {
		if _, perr := time.Parse(time.RFC3339, flags.at); perr != nil {
			msg := fmt.Sprintf("recall: --at %q: invalid RFC 3339 timestamp (example: 2006-01-02T15:04:05Z)", flags.at)
			if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
				return werr
			}
			return silentError{code: 1, err: errors.New(msg)}
		}
	}
	if flags.asOf != "" {
		if _, perr := time.Parse(time.RFC3339, flags.asOf); perr != nil {
			msg := fmt.Sprintf("recall: --as-of %q: invalid RFC 3339 timestamp (example: 2026-01-01T00:00:00Z)", flags.asOf)
			if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
				return werr
			}
			return silentError{code: 1, err: errors.New(msg)}
		}
	}
	if flags.asOfEventUID != "" && (flags.asOf != "" || flags.at != "") {
		msg := "recall: --as-of-event is mutually exclusive with --as-of and --at"
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: errors.New(msg)}
	}
	return nil
}

// applyRecallOptionalFlags populates the optional recall tool input fields from
// the parsed flags. Extracted to keep buildRecallRequest within the cyclomatic
// complexity budget.
func applyRecallOptionalFlags(in map[string]interface{}, flags *recallFlags) {
	if flags.at != "" {
		in["at"] = flags.at
	}
	if flags.asOf != "" {
		in["asOf"] = flags.asOf
	}
	if flags.asOfEventUID != "" {
		in["asOfEventUID"] = flags.asOfEventUID
	}
	if flags.noGraph {
		in["noGraph"] = true
	}
	if flags.rewriteQuery {
		in["rewriteQuery"] = true
	}
	if flags.near != "" {
		in["near"] = map[string]interface{}{
			"placeQuery": flags.near,
			"radiusKm":   flags.radiusKm,
		}
	}
	if len(flags.scopes) > 0 {
		in["scopes"] = flags.scopes
	}
	if flags.anchor != "" {
		in["anchor"] = flags.anchor
		if flags.anchorDepth > 1 {
			in["anchorDepth"] = flags.anchorDepth
		}
	}
	if flags.embedDeadlineMs > 0 {
		in["embedDeadlineMs"] = flags.embedDeadlineMs
	}
	if flags.explain {
		in["explain"] = true
	}
	if srcs := nonEmptyStrings(flags.source); len(srcs) > 0 {
		in["source"] = srcs
	}
	if flags.hasPrimarySource {
		in["hasPrimarySource"] = true
	}
}

// buildRecallRequest marshals the tool input JSON from the parsed flags.
func buildRecallRequest(cmd *cobra.Command, query string, flags *recallFlags) (json.RawMessage, error) {
	in := map[string]interface{}{
		"query": query,
		"limit": flags.limit,
	}
	applyRecallOptionalFlags(in, flags)
	body, err := json.Marshal(in)
	if err != nil {
		return nil, wrapRecallFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}
	// Inject the --kind filter via the same friendly→wire mapping the TUI Kind
	// bank uses, so scripted recall ≡ the interactive strip.
	return patchKindsIntoRequest(body, flags.kinds), nil
}

// nonEmptyStrings returns vals with blank (whitespace-only) entries dropped and
// the survivors trimmed. Used to sanitise repeatable string flags (e.g.
// --source) before they reach the tool input so an accidental empty value never
// becomes a filter that matches nothing.
func nonEmptyStrings(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// recallHit is a partial decode of a single hit from the recall tool output.
// Only the fields needed for table and explain rendering are decoded; the raw
// JSON is forwarded verbatim in JSON mode so the full components block is
// preserved.
type recallHit struct {
	HitType    string         `json:"hitType"`
	Name       string         `json:"name"`
	Snippet    string         `json:"snippet"`
	FusedScore float32        `json:"fusedScore"`
	Components recallHitComps `json:"components"`
	Source     *recallHitSrc  `json:"source"`
	// Author is the display name of the principal who authored the source event.
	// Present only in verbose mode when the daemon's author-lookup is wired;
	// shown after the hit reference line in --explain output.
	Author string `json:"author,omitempty"`
}

// recallHitSrc is the section anchor carried by document hits.
type recallHitSrc struct {
	Path    string `json:"path"`
	Heading string `json:"heading"`
}

// recallHitComps maps the per-lane score components the kernel/MCP layer
// emits (scoreComponentsJSON in internal/mcp/tools/recall.go). The CLI uses
// the weighted-fusion fields (RawSemantic, RawBM25) for --explain; the
// RRF-only Vector/FTS columns are retained here for JSON round-trip fidelity
// but are NOT rendered in the plain-text table (they're misleading under the
// default weighted fusion).
type recallHitComps struct {
	Vector      float32 `json:"vector"`
	FTS         float32 `json:"fts"`
	Graph       float32 `json:"graph"`
	NameEmb     float32 `json:"nameEmb"`
	Recency     float32 `json:"recency"`
	RawSemantic float32 `json:"rawSemantic"`
	RawBM25     float32 `json:"rawBM25"`
	Strength    float32 `json:"strength"`
}

// renderRecallText dispatches to the explain or table renderer for text-mode
// output. Extracted from runRecall to keep that function's cyclomatic
// complexity under the linter threshold.
func renderRecallText(cmd *cobra.Command, w output.Writer, raw json.RawMessage, flags *recallFlags, query string) error {
	styled := interactive.ShouldStyle(cmd, cmd.OutOrStdout())
	c := colorizer.New(styled, cmd.OutOrStdout())
	if flags.explain {
		return renderRecallExplain(cmd, w, c, raw, flags.limit, query)
	}
	return renderRecallCLI(cmd, w, c, raw, flags.limit)
}

// renderRecallCLI parses the hits array and prints a compact ranked table
// using the shared output.NewTable renderer. The RRF-only vec/fts columns
// are dropped; per-lane details are available via --explain. Long output is
// routed through MaybePage and the footer reports how many hits were shown.
func renderRecallCLI(cmd *cobra.Command, w output.Writer, c *colorizer.Colorizer, raw json.RawMessage, limit int) error {
	var out struct {
		Hits []recallHit `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("recall: parse response: %w", err)
	}

	if len(out.Hits) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no hits")
		return err
	}

	// Column definitions are local — not a package-level var — so there is no
	// mutable global state (gochecknoglobals). The definition is tiny.
	cols := []output.Column{
		{Title: "#", Align: output.AlignRight},
		{Title: "type"},
		{Title: "fused", Align: output.AlignRight},
		{Title: "ref", MaxWidth: 24, Truncate: true},
		{Title: "snippet", MaxWidth: 60, Truncate: true},
	}
	hits := out.Hits
	if err := output.MaybePage(cmd, func(dst io.Writer) error {
		dstW := output.New(output.FormatText, dst, cmd.ErrOrStderr())
		tbl := output.NewTable(dstW, c, cols)
		for i, h := range hits {
			hitTypeLabel := hitTypeLabelEp
			if h.HitType == diffKindEntity {
				hitTypeLabel = hitTypeLabelEnt
			}
			// Document hits identify by their vault location (path › heading);
			// episodes and entities display the UID prefix.
			ref := resourceUID(h.Name, 24)
			if h.Source != nil {
				ref = h.Source.Path
				if h.Source.Heading != "" {
					ref += " › " + h.Source.Heading
				}
			}
			snippet := stripHighlight(strings.ReplaceAll(h.Snippet, "\n", " "))
			tbl.Row(i+1, hitTypeLabel, fmt.Sprintf("%.3f", h.FusedScore), ref, snippet)
		}
		return tbl.Render()
	}); err != nil {
		return err
	}

	output.ShowingNofM(w, c, len(hits), 0, limit, "raise --limit")
	return nil
}

// explainLabelWidth is the fixed visible width of the per-lane label column
// in --explain output. All labels fit within 9 runes; padding keeps values
// column-aligned in both colour-on and colour-off modes.
const explainLabelWidth = 9

// explainBarWidth is the cell count of the per-lane unicode magnitude bar.
const explainBarWidth = 10

// scoreBar renders an explainBarWidth-cell unicode bar for a value in [0, 1].
// Filled cells use ▇, empty cells use ░. The bar is decorative; the numeric
// value beside it carries the informational signal (and survives NO_COLOR).
func scoreBar(v float32) string {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	filled := int(v*float32(explainBarWidth) + 0.5)
	return strings.Repeat("▇", filled) + strings.Repeat("░", explainBarWidth-filled)
}

// fusedMeter renders the fused score in entire.io's idiom: a dot-SIZE bullet
// (· • ● ⬤, bucketed at the same .25/.5/.75 ratios entire.io's activityRune
// uses) followed by the score as a percent — e.g. "⬤ 85%". The glyph grows
// and brightens with the score, so it reads correctly under NO_COLOR.
func fusedMeter(c *colorizer.Colorizer, v float32) string {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	pct := int(v*100 + 0.5)
	var dot string
	switch {
	case v >= 0.75:
		dot = c.Accent("⬤")
	case v >= 0.5:
		dot = c.Changed("●")
	case v >= 0.25:
		dot = c.Success("•")
	default:
		dot = c.Dim("·")
	}
	return fmt.Sprintf("%s %d%%", dot, pct)
}

// laneFiredThreshold is the minimum lane value that earns a ✓ chip / fired
// marker. It matches the 2-decimal display rounding, so a lane whose value
// prints as "0.00" never shows a contradictory ✓ on the hit header.
const laneFiredThreshold = 0.005

// laneSummary renders the inline fired-lane chips on the hit header, e.g.
// `✓ sem ✓ kw ✓ graph`. Only lanes that contributed (value ≥ the display
// rounding threshold) appear — a missing lane is silence, not a false ✗ —
// matching the issue mockup.
func laneSummary(c *colorizer.Colorizer, comps *recallHitComps) string {
	parts := make([]string, 0, 4)
	if comps.RawSemantic >= laneFiredThreshold {
		parts = append(parts, c.Success("✓ sem"))
	}
	if comps.RawBM25 >= laneFiredThreshold {
		parts = append(parts, c.Success("✓ kw"))
	}
	if comps.Graph >= laneFiredThreshold {
		parts = append(parts, c.Success("✓ graph"))
	}
	if comps.NameEmb >= laneFiredThreshold {
		parts = append(parts, c.Success("✓ name"))
	}
	return strings.Join(parts, " ")
}

// appendMagLane writes a magnitude lane row: `label  0.78  ▇▇▇▇▇▇▇░░░  annotation`.
// Used for semantic / keyword / recency, whose raw value is always meaningful.
func appendMagLane(dst io.Writer, c *colorizer.Colorizer, label string, v float32, annotation string) error {
	_, err := fmt.Fprintf(dst, "  %s  %s  %s  %s\n",
		c.Meta(fmt.Sprintf("%-*s", explainLabelWidth, label)),
		fmt.Sprintf("%4.2f", v),
		scoreBar(v),
		c.Dim(annotation))
	return err
}

// appendFiredLane writes a fired/not-fired lane row (graph / name-emb): the
// value + bar + `✓ fired` when the lane contributed, else a dim dash + empty
// bar + `not fired`. Columns stay aligned with appendMagLane.
func appendFiredLane(dst io.Writer, c *colorizer.Colorizer, label string, v float32) error {
	if v >= laneFiredThreshold {
		_, err := fmt.Fprintf(dst, "  %s  %s  %s  %s\n",
			c.Meta(fmt.Sprintf("%-*s", explainLabelWidth, label)),
			fmt.Sprintf("%4.2f", v),
			scoreBar(v),
			c.Success("✓ fired"))
		return err
	}
	_, err := fmt.Fprintf(dst, "  %s  %s  %s  %s\n",
		c.Meta(fmt.Sprintf("%-*s", explainLabelWidth, label)),
		c.Dim("   –"),
		c.Dim(scoreBar(0)),
		c.Dim("not fired"))
	return err
}

// explainRef returns the hit's display reference: the section path for document
// hits, else the resource name (episodes/… or entities/…).
func explainRef(h *recallHit) string {
	if h.Source != nil {
		ref := h.Source.Path
		if h.Source.Heading != "" {
			ref += " › " + h.Source.Heading
		}
		return ref
	}
	return h.Name
}

// appendExplainHit writes the per-hit header (#N dot-meter+percent ref ✓lanes)
// and its per-lane score block to dst; h is passed by pointer to avoid the
// 96-byte copy. Extracted to keep renderRecallExplain's cognitive complexity
// below the linter threshold.
func appendExplainHit(dst io.Writer, i int, h *recallHit, c *colorizer.Colorizer) error {
	byClause := ""
	if h.Author != "" {
		byClause = "  " + c.Dim("by "+h.Author)
	}
	if _, err := fmt.Fprintf(dst, "%s  %s  %s   %s%s\n",
		c.Accent(fmt.Sprintf("#%d", i+1)),
		fusedMeter(c, h.FusedScore),
		c.Ident(explainRef(h)),
		laneSummary(c, &h.Components),
		byClause); err != nil {
		return err
	}

	// semantic and keyword (BM25) are always shown — core weighted inputs.
	if err := appendMagLane(dst, c, "semantic", h.Components.RawSemantic, "raw cosine"); err != nil {
		return err
	}
	if err := appendMagLane(dst, c, "keyword", h.Components.RawBM25, "bm25 (sigmoid)"); err != nil {
		return err
	}

	// graph and name-emb: fired state (zero = not fired).
	if err := appendFiredLane(dst, c, "graph", h.Components.Graph); err != nil {
		return err
	}
	if err := appendFiredLane(dst, c, "name-emb", h.Components.NameEmb); err != nil {
		return err
	}

	// recency — only shown when configured (never a misleading zero row).
	if h.Components.Recency > 0 {
		if err := appendMagLane(dst, c, "recency", h.Components.Recency, "recency boost"); err != nil {
			return err
		}
	}
	return nil
}

// renderRecallExplain renders the per-lane score breakdown for each hit under
// --explain / --why: a query header, then per-hit a dot-meter + fused score +
// fired-lane chips, then the weighted-fusion inputs (rawSemantic, rawBM25,
// graph fired, name-emb fired) with recency shown only when > 0.
// The function is a no-op in JSON mode: --explain is already on the wire.
func renderRecallExplain(cmd *cobra.Command, w output.Writer, c *colorizer.Colorizer, raw json.RawMessage, limit int, query string) error {
	var out struct {
		Hits     []recallHit `json:"hits"`
		Degraded bool        `json:"degraded"`
		Lanes    []string    `json:"lanes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("recall: parse response: %w", err)
	}

	if len(out.Hits) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no hits")
		return err
	}

	// Derive the fusion-mode label from the degraded flag. When degraded=true
	// the vector lane was unavailable and recall ran FTS+graph only — the
	// weighted fusion inputs (rawSemantic) are effectively zero.
	fusionLabel := "weighted"
	if out.Degraded {
		fusionLabel = "degraded"
	}

	hits := out.Hits
	if err := output.MaybePage(cmd, func(dst io.Writer) error {
		// Query header: ◆ recall "<query>"   fusion: weighted · N hits
		if _, err := fmt.Fprintf(dst, "%s recall %s   %s\n\n",
			c.Accent("◆"),
			c.Ident(strconv.Quote(query)),
			c.Dim(fmt.Sprintf("fusion: %s · %d hits", fusionLabel, len(hits)))); err != nil {
			return err
		}
		for i := range hits {
			if err := appendExplainHit(dst, i, &hits[i], c); err != nil {
				return err
			}
			if i < len(hits)-1 {
				if _, err := fmt.Fprintln(dst); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}

	output.ShowingNofM(w, c, len(hits), 0, limit, "raise --limit")
	return nil
}

// resourceUID returns the first n runes of the UID portion of an AIP resource
// name (strips "episodes/" or "entities/" prefix), or the whole UID if shorter.
func resourceUID(name string, n int) string {
	uid := name
	for _, prefix := range []string{"episodes/", "entities/"} {
		if strings.HasPrefix(name, prefix) {
			uid = strings.TrimPrefix(name, prefix)
			break
		}
	}
	runes := []rune(uid)
	if len(runes) <= n {
		return uid
	}
	return string(runes[:n])
}

// stripHighlight removes the FTS5 <b>…</b> highlight markers from a snippet so
// the human-facing CLI table shows clean text. The MCP wire shape keeps the
// markers (a structured client may want them); the CLI is display-only.
func stripHighlight(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	return strings.ReplaceAll(s, "</b>", "")
}

// renderRecallNDJSON emits one JSON object per hit line, matching the
// contract for --output ndjson on list/recall results.  The raw hit
// objects are forwarded verbatim so the full components block is preserved.
func renderRecallNDJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Hits []json.RawMessage `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("recall: parse ndjson hits: %w", err)
	}
	w := cmd.OutOrStdout()
	for _, h := range out.Hits {
		if _, err := fmt.Fprintln(w, string(h)); err != nil {
			return err
		}
	}
	return nil
}

// stripSnippetMarkup removes FTS5 <b>…</b> highlight markers from the snippet
// field of every hit in a recall JSON envelope, leaving all other fields (and
// top-level keys: degraded, lanes, _meta) intact. It lets the verbatim machine
// formats (json/ndjson) match the csv/kv/table renderers, which already strip.
// On any parse/marshal failure it returns the original bytes unchanged, so a
// response we cannot understand is never silently corrupted.
func stripSnippetMarkup(raw json.RawMessage) json.RawMessage {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw
	}
	hitsRaw, ok := env["hits"]
	if !ok {
		return raw
	}
	var hits []map[string]json.RawMessage
	if err := json.Unmarshal(hitsRaw, &hits); err != nil {
		return raw
	}
	changed := false
	for _, h := range hits {
		snipRaw, ok := h["snippet"]
		if !ok {
			continue
		}
		var snip string
		if err := json.Unmarshal(snipRaw, &snip); err != nil {
			continue
		}
		stripped := stripHighlight(snip)
		if stripped == snip {
			continue
		}
		b, err := json.Marshal(stripped)
		if err != nil {
			continue
		}
		h["snippet"] = b
		changed = true
	}
	if !changed {
		return raw
	}
	nh, err := json.Marshal(hits)
	if err != nil {
		return raw
	}
	env["hits"] = nh
	out, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	return out
}

// maybeHintSavedRecall prints a stderr nudge when a recall returns zero hits and
// a saved recall happens to share the query string's name — the likely case of a
// user typing `maz recall standup` (a literal search) when they meant
// `maz saved recall standup` (replay the stored query). Best-effort and silent
// on any error; never touches stdout, so machine output is unaffected.
func maybeHintSavedRecall(cmd *cobra.Command, raw json.RawMessage, query string) {
	var out struct {
		Hits []json.RawMessage `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Hits) > 0 {
		return
	}
	saved, err := loadSavedRecalls(savedDataDir(cmd))
	if err != nil {
		return
	}
	if _, ok := saved[query]; !ok {
		return
	}
	//nolint:errcheck // best-effort advisory on stderr
	fmt.Fprintf(cmd.ErrOrStderr(),
		"note: no hits for %q. A saved recall named %q exists — did you mean `maz saved recall %s` to replay it? (`recall` searches the literal text.)\n",
		query, query, query)
}

// wrapRecallFatal prints the error to stderr and returns a silentError.
// Expected failures (E_VALIDATION, E_NOT_FOUND, E_PERMISSION) resolve to
// exit code 1; unexpected failures resolve to exit code 2. Using
// respondError (not respondFatal) lets MCP validation errors from the tool
// layer — such as a malformed --as-of-event ULID — surface as exit 1 rather
// than the generic fatal exit 2.
func wrapRecallFatal(cmd *cobra.Command, err error) error {
	return respondError(cmd, "recall", err)
}

// applyRecallPostFilters runs the post-fetch, pre-render steps shared by every
// output mode: the fused-score floor (--min-score) and, when a source /
// has-primary-source filter trimmed everything away, the empty-result stderr
// hint. Returns the (possibly filtered) response. Split out of runRecall to keep
// that function under the gocyclo threshold.
func applyRecallPostFilters(cmd *cobra.Command, raw json.RawMessage, flags *recallFlags) (json.RawMessage, error) {
	// Apply the fused-score floor before rendering (both text and JSON modes).
	if flags.minScore > 0 {
		if filtered, ferr := applyRecallMinScore(raw, float32(flags.minScore)); ferr == nil {
			raw = filtered
		}
	}
	// When a source filter was active and nothing survived, nudge the caller:
	// an unknown source name is not an error, but a silent empty result is easy
	// to misread as "the query found nothing". The hint goes to stderr so it
	// never pollutes JSON/NDJSON stdout.
	if (len(flags.source) > 0 || flags.hasPrimarySource) && recallHitCount(raw) == 0 {
		if werr := printRecallSourceHint(cmd, flags); werr != nil {
			return nil, werr
		}
	}
	return raw, nil
}

// recallHitCount returns the number of hits in a raw recall JSON response. A
// parse failure reports -1 so callers can distinguish "couldn't tell" from a
// genuine empty result and avoid emitting a misleading hint.
func recallHitCount(raw json.RawMessage) int {
	var out struct {
		Hits []json.RawMessage `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return -1
	}
	return len(out.Hits)
}

// printRecallSourceHint writes a one-line stderr hint when a source /
// has-primary-source filter trimmed the result to nothing, so an unknown source
// name reads as "no match here" rather than "nothing in memory". It is advisory,
// not an error: recall still exits 0.
func printRecallSourceHint(cmd *cobra.Command, flags *recallFlags) error {
	var b strings.Builder
	b.WriteString("recall: no hits after filtering")
	if len(flags.source) > 0 {
		fmt.Fprintf(&b, " on --source %s", strings.Join(flags.source, ", "))
	}
	if flags.hasPrimarySource {
		if len(flags.source) > 0 {
			b.WriteString(" and")
		}
		b.WriteString(" --has-primary-source")
	}
	// Tail explanation must match the active filter: only --source can fail on an
	// unknown source name; --has-primary-source has no source argument, so a blank
	// result there just means no ranked hit carries recorded provenance.
	switch {
	case len(flags.source) > 0 && flags.hasPrimarySource:
		b.WriteString("; the source name may be unknown, or no ranked hit matches both filters")
	case len(flags.source) > 0:
		b.WriteString("; the source name may be unknown or no ranked match carries it")
	default: // --has-primary-source only
		b.WriteString("; no ranked hit carries a recorded primary source (source_ref or has_primary_source edge)")
	}
	_, err := fmt.Fprintln(cmd.ErrOrStderr(), b.String())
	return err
}

// applyRecallMinScore filters the hits in a raw recall JSON response, keeping
// only those whose fusedScore is >= floor. It re-serialises the hits array so
// the caller can forward the filtered JSON verbatim. A parse failure returns
// the original raw bytes unchanged so a broken response is never silently
// discarded.
func applyRecallMinScore(raw json.RawMessage, floor float32) (json.RawMessage, error) {
	var out struct {
		Hits []json.RawMessage `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return raw, err
	}
	kept := out.Hits[:0]
	for _, h := range out.Hits {
		var s struct {
			FusedScore float32 `json:"fusedScore"`
		}
		if err := json.Unmarshal(h, &s); err != nil {
			kept = append(kept, h) // keep unparseable hits
			continue
		}
		if s.FusedScore >= floor {
			kept = append(kept, h)
		}
	}
	type filteredOut struct {
		Hits []json.RawMessage `json:"hits"`
	}
	body, err := json.Marshal(filteredOut{Hits: kept})
	if err != nil {
		return raw, err
	}
	return body, nil
}

// renderRecallCSV emits a CSV table with a header row followed by one row per
// hit. Columns: rank, type, score, ref, snippet. The ref is the full resource
// handle (e.g. episodes/<ulid>) so it can be passed straight to `maz get`;
// snippet newlines are collapsed to spaces so each hit occupies exactly one CSV
// line. The output is parseable by standard CSV readers (RFC 4180).
func renderRecallCSV(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Hits []recallHit `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("recall: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(w, "rank,type,score,ref,snippet"); err != nil {
		return err
	}
	for i, h := range out.Hits {
		hitTypeLabel := hitTypeLabelEp
		if h.HitType == diffKindEntity {
			hitTypeLabel = hitTypeLabelEnt
		}
		// Full handle (episodes/<ulid> | entities/<ulid>) so a reader can feed
		// ref straight to `maz get`; doc hits keep their vault location.
		ref := h.Name
		if h.Source != nil {
			ref = h.Source.Path
			if h.Source.Heading != "" {
				ref += " > " + h.Source.Heading
			}
		}
		snippet := strings.ReplaceAll(stripHighlight(h.Snippet), "\n", " ")
		// Quote fields that may contain commas or quotes per RFC 4180.
		row := fmt.Sprintf("%d,%s,%.4f,%s,%s",
			i+1,
			hitTypeLabel,
			h.FusedScore,
			csvQuote(ref),
			csvQuote(snippet),
		)
		if _, err := fmt.Fprintln(w, row); err != nil {
			return err
		}
	}
	return nil
}

// csvQuote wraps s in double-quotes when it contains a comma, double-quote, or
// newline, escaping any embedded double-quotes by doubling them (RFC 4180).
func csvQuote(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// renderRecallKV emits one blank-line-separated block per hit in key: value
// format. Each block contains rank, type, score, ref, and snippet fields.
// This format is suitable for shell pipelines (grep, awk) and human scanning.
func renderRecallKV(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Hits []recallHit `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("recall: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	for i, h := range out.Hits {
		hitTypeLabel := hitTypeLabelEp
		if h.HitType == diffKindEntity {
			hitTypeLabel = hitTypeLabelEnt
		}
		// Full handle so ref is `maz get`-ready; doc hits keep their location.
		ref := h.Name
		if h.Source != nil {
			ref = h.Source.Path
			if h.Source.Heading != "" {
				ref += " > " + h.Source.Heading
			}
		}
		snippet := strings.ReplaceAll(stripHighlight(h.Snippet), "\n", " ")
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		lines := []string{
			fmt.Sprintf("rank: %d", i+1),
			"type: " + hitTypeLabel,
			fmt.Sprintf("score: %.4f", h.FusedScore),
			"ref: " + ref,
			"snippet: " + snippet,
		}
		for _, line := range lines {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	return nil
}
