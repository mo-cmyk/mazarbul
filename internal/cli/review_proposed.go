package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// relationStatusProposed is the trust posture of a connector-extracted edge
// awaiting human verification. Mirrors ontology.RelationStatusProposed as a
// local literal so the CLI does not import the ontology package for one value.
const relationStatusProposed = "proposed"

// proposedReviewLimit caps the reviewer queue read. A connector ingest can
// propose many edges; 200 mirrors `maz review list` and keeps one screen of
// disposition tractable. Raise via --limit.
const proposedReviewLimit = 200

// proposedRow is one connector-proposed edge in the reviewer queue, in the
// stable JSON shape `maz review proposed --json` emits. Name is the
// relations/{uid} resource name promoteRelation accepts, so an agent pipes it
// straight back into `maz promote` without string surgery.
type proposedRow struct {
	Name       string  `json:"name"`
	Subject    string  `json:"subject"`
	Predicate  string  `json:"predicate"`
	Object     string  `json:"object"`
	Confidence float64 `json:"confidence"`
	CreateTime string  `json:"createTime"`
	Connector  string  `json:"connector"`
	Scope      string  `json:"scope"`
}

// proposedReviewResult is the top-level JSON shape: the flat edge list (each
// row carries its connector, so it stays scriptable with jq), a per-connector
// count summary, and the total. Text mode groups the same rows by connector.
type proposedReviewResult struct {
	Proposed   []proposedRow       `json:"proposed"`
	Connectors []connectorCountRow `json:"connectors"`
	Count      int                 `json:"count"`
}

// connectorCountRow is one connector and how many proposed edges it owns.
type connectorCountRow struct {
	Connector string `json:"connector"`
	Count     int    `json:"count"`
}

func newReviewProposedCmd() *cobra.Command {
	var (
		connector  string
		approveAll bool
		scopes     []string
		limit      int
	)
	cmd := &cobra.Command{
		Use:   "proposed",
		Short: "List connector-proposed RELATIONS awaiting trust verification",
		Long: `List the RELATION edges an external connector proposed (trust status=proposed)
so you can verify (promote) the ones you trust. This is the RELATION trust queue
— not the entity-merge queue ('maz review list') and not the taxonomy queue
('maz schema review list').

TRUST STATUS vs TEMPORAL STATE — two orthogonal axes:

  Trust status (shown here):
    proposed   extracted by a connector; surfaces in recall but excluded from
               supersession and predicate minting until a human verifies it
    asserted   verified; behaves like any directly-authored relation
  Temporal state (shown by 'maz list relations' and 'maz timeline'):
    open / closed / superseded / retracted

This command shows the trust queue. Temporal state is separate: an edge can be
proposed and still open, or asserted yet temporally closed.

The three "approve" paths in maz are intentionally distinct:
  maz promote relations/{uid}        — verifies a proposed RELATION (trust promotion)
  maz review approve <entity-uid>    — confirms a tentative ENTITY record (resolver queue)
  maz schema review approve <kind>   — approves a proposed entity KIND (taxonomy)

Reads are scope-isolated and fail closed: a team-scope proposal NEVER appears in
a personal review. The default span is {project, personal}; pass --scope to widen
or narrow it. Each edge is promoted in its OWN scope, so a team-scope edge needs
admitted team membership.

In a terminal with no flags this opens an interactive multi-select (space toggles,
enter promotes the selected edges). When stdout is piped or --json is set it
prints the queue grouped by connector with confidence and date, and exits without
prompting — pass --approve-all to promote every listed edge in one shot.

Rejecting a proposal is not yet a persisted action: a declined edge simply stays
proposed (there is no relationRejected event yet). Use 'maz forget' to retract
an edge entirely.

Related:
  maz promote relations/{uid}        promote a single edge from this queue
  maz review approve <entity-uid>    entity-merge queue (resolver, not connectors)
  maz list relations                 temporal state of all relation edges

Exit codes:
  0   listed (or promoted) successfully
  1   expected failure: bad --scope, or a team promotion without membership
  2   unexpected failure: storage or wiring error

Examples:
  maz review proposed
  maz review proposed --json
  maz review proposed --connector connector:granola --json
  maz promote relations/01J...           # promote one edge by id from the queue
  maz review proposed --approve-all
  maz review proposed --approve-all --scope team`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateScopeFlag(cmd, "review proposed", scopes); err != nil {
				return err
			}
			rows, err := listProposedRelations(cmd, scopes, connector, limit)
			if err != nil {
				return respondError(cmd, "review proposed", err)
			}
			return runReviewProposed(cmd, rows, approveAll)
		},
	}
	cmd.Flags().StringVar(&connector, "connector", "", "show only edges this connector proposed, e.g. connector:granola")
	cmd.Flags().BoolVar(&approveAll, "approve-all", false, "promote every listed proposed edge to asserted (in its own scope)")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to review (repeatable): project, personal, team. Default: project, personal.")
	cmd.Flags().IntVar(&limit, "limit", proposedReviewLimit, "maximum proposed edges to read")
	return cmd
}

// listProposedRelations reads the proposed-relation queue scope-isolated via a
// Navigator over the local SQLite projection (mirroring `maz review list` /
// `maz rebuild`'s direct read), narrowed to the requested scopes (default
// {project, personal}) and optionally a single connector.
func listProposedRelations(cmd *cobra.Command, scopes []string, connector string, limit int) ([]proposedRow, error) {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return nil, err
	}
	defer closeStoreQuietly(store)

	// DefaultReadSet: the review queue spans project+personal by default; caller overrides with --scope.
	nav, err := navigate.New(store.Reader())
	if err != nil {
		return nil, fmt.Errorf("navigator: %w", err)
	}
	scopeSet := kernel.DefaultReadSet()
	if len(scopes) > 0 {
		// Pre-validated by validateScopeFlag; re-parse for the typed set.
		parsed, perr := kernel.ParseScopeSet(scopes)
		if perr != nil {
			return nil, perr
		}
		scopeSet = parsed
	}
	rels, err := nav.WithScopes(scopeSet).ListRelationsByStatus(ctx, relationStatusProposed, connector, limit)
	if err != nil {
		return nil, err
	}
	return toProposedRows(rels), nil
}

// toProposedRows projects navigator rows into the stable CLI/JSON shape. An
// out-of-scope endpoint name was already redacted to "" by the reader; fall
// back to the UID so the row is never blank.
func toProposedRows(rels []navigate.ProposedRelation) []proposedRow {
	out := make([]proposedRow, 0, len(rels))
	for i := range rels {
		r := rels[i]
		out = append(out, proposedRow{
			Name:       "relations/" + r.UID,
			Subject:    orUID(r.SubjectName, r.SubjectUID),
			Predicate:  r.Predicate,
			Object:     orUID(r.ObjectName, r.ObjectUID),
			Confidence: r.Confidence,
			CreateTime: r.CreateTime,
			Connector:  r.Source,
			Scope:      r.Scope,
		})
	}
	return out
}

// orUID returns name when non-empty, else the uid fallback.
func orUID(name, uid string) string {
	if name != "" {
		return name
	}
	return uid
}

// runReviewProposed renders or approves the proposed queue per the active mode:
// JSON passthrough, --approve-all, interactive multi-select (TTY), or a grouped
// text listing.
func runReviewProposed(cmd *cobra.Command, rows []proposedRow, approveAll bool) error {
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON

	if approveAll {
		return approveProposed(cmd, rows)
	}
	if jsonMode {
		return emitProposedJSON(cmd, rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no proposed relations awaiting review")
		return err
	}
	// Interactive disposition only when it is safe to block on a prompt.
	if wr.IsTTY() && interactive.CanPromptInteractively() {
		return approveInteractive(cmd, rows)
	}
	return renderProposedText(cmd, rows)
}

// emitProposedJSON writes the stable JSON result.
func emitProposedJSON(cmd *cobra.Command, rows []proposedRow) error {
	res := proposedReviewResult{
		Proposed:   rows,
		Connectors: connectorCounts(rows),
		Count:      len(rows),
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
}

// connectorCounts summarises how many proposed edges each connector owns,
// ordered by descending count then connector name for stable output.
func connectorCounts(rows []proposedRow) []connectorCountRow {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Connector]++
	}
	out := make([]connectorCountRow, 0, len(counts))
	for c, n := range counts {
		out = append(out, connectorCountRow{Connector: c, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Connector < out[j].Connector
	})
	return out
}

// renderProposedText prints the queue grouped by connector with confidence and
// date, then a footer nudging --approve-all. Both modes carry the same data the
// JSON shape does.
func renderProposedText(cmd *cobra.Command, rows []proposedRow) error {
	w := cmd.OutOrStdout()
	groups := groupByConnector(rows)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, g := range groups {
		label := g.Connector
		if label == "" {
			label = "(unknown source)"
		}
		if _, err := fmt.Fprintf(tw, "\n%s  (%d)\n", label, len(g.Rows)); err != nil {
			return err
		}
		for _, r := range g.Rows {
			if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\tconf=%.2f\t%s\t%s\n",
				r.Subject, r.Predicate, r.Object, r.Name, r.Confidence, r.Scope, r.CreateTime); err != nil {
				return err
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n%d proposed relation(s). Approve all with: maz review proposed --approve-all\n", len(rows))
	return err
}

// connectorGroup is one connector and its proposed edges, for the text grouping.
type connectorGroup struct {
	Connector string
	Rows      []proposedRow
}

// groupByConnector buckets rows by connector, ordered by descending bucket size
// then connector name, matching connectorCounts' ordering.
func groupByConnector(rows []proposedRow) []connectorGroup {
	byConn := map[string][]proposedRow{}
	for _, r := range rows {
		byConn[r.Connector] = append(byConn[r.Connector], r)
	}
	order := connectorCounts(rows)
	out := make([]connectorGroup, 0, len(order))
	for _, c := range order {
		out = append(out, connectorGroup{Connector: c.Connector, Rows: byConn[c.Connector]})
	}
	return out
}

// approveProposed promotes every row in the queue. Each edge is promoted in its
// OWN scope so a team-scope edge is verified under team authority. A per-edge
// failure aborts with the tool's classified exit code.
func approveProposed(cmd *cobra.Command, rows []proposedRow) error {
	if len(rows) == 0 {
		return emitApproveResult(cmd, nil)
	}
	approved := make([]string, 0, len(rows))
	for i := range rows {
		if err := promoteOne(cmd, &rows[i]); err != nil {
			return respondError(cmd, "review proposed", err)
		}
		approved = append(approved, rows[i].Name)
	}
	return emitApproveResult(cmd, approved)
}

// approveInteractive opens a multi-select over the proposed edges and promotes
// the chosen ones. It only runs in a real TTY (gated by the caller), so it never
// blocks a scripted invocation.
func approveInteractive(cmd *cobra.Command, rows []proposedRow) error {
	opts := make([]huh.Option[string], 0, len(rows))
	for _, r := range rows {
		label := fmt.Sprintf("%s %s %s  [%s, conf=%.2f]", r.Subject, r.Predicate, r.Object, r.Connector, r.Confidence)
		opts = append(opts, huh.NewOption(label, r.Name))
	}
	var chosen []string
	form := uiform.New(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Approve proposed relations").
			Description("Space toggles an edge; enter promotes the selected ones to asserted.").
			Options(opts...).
			Value(&chosen),
	))
	if err := form.Run(); err != nil {
		return uiform.HandleFormCancellation(cmd.OutOrStdout(), "review proposed", err)
	}
	if len(chosen) == 0 {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), "no relations approved")
		return werr
	}
	byName := make(map[string]proposedRow, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	approved := make([]string, 0, len(chosen))
	for _, name := range chosen {
		row := byName[name]
		if err := promoteOne(cmd, &row); err != nil {
			return respondError(cmd, "review proposed", err)
		}
		approved = append(approved, name)
	}
	return emitApproveResult(cmd, approved)
}

// promoteOne routes a single edge through the promoteRelation tool, in the
// edge's own scope, via the daemon-or-in-process path.
func promoteOne(cmd *cobra.Command, r *proposedRow) error {
	in := map[string]any{"name": r.Name}
	if r.Scope != "" {
		in["scope"] = r.Scope
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal promote request: %w", err)
	}
	if _, ierr := invokeNavTool(cmd, promoteToolName, body); ierr != nil {
		return ierr
	}
	return nil
}

// emitApproveResult reports the approved edges: a JSON {approved:[…],count:N}
// shape in JSON mode, or a per-edge confirmation line in text mode.
func emitApproveResult(cmd *cobra.Command, approved []string) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"approved": approved,
			"count":    len(approved),
		})
	}
	if len(approved) == 0 {
		_, err := fmt.Fprintln(w, "no proposed relations to approve")
		return err
	}
	for _, name := range approved {
		if _, err := fmt.Fprintf(w, "asserted %s\n", name); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "approved %d relation(s)\n", len(approved))
	return err
}
