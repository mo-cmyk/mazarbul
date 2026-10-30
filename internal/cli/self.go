package cli

// self.go - `maz self`: the Agent Mirror. A one-screen "what Mazarbul knows
// about me" view with three sections, each backed by the same scope-isolated
// SQLite read the navigation verbs use:
//
//  1. "What I know about you"  - the open facts on the self/principal entity.
//  2. "Reinforced lately"      - entities ranked by memory_strength access count.
//  3. "Insights about you"     - insights synthesized with the self/principal seed.
//
// Self resolution: there are TWO self nodes by design - the local uid="self"
// anchor and the portable principal ULID, joined by a same_as relation. We read
// the UNION of both (principal first) and collapse them so the pair is shown
// once, never double-counted: facts dedup by (stateKey, value) and the
// reinforced row folds both nodes into a single "you" entry. The header
// hydrates from whichever node carries an identity row, regardless of the
// requested scope (the reader's own identity is never a cross-scope leak;
// mirrors the get/getEntityContext tools' self-identity widening).
//
// Both modes carry the same information: text sections on a TTY, a stable JSON
// shape when piped or with --json. Read-only; no prompts, so a scripted
// invocation never blocks.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// selfEntityAnchorUID is the local-only symbolic self anchor seeded by migration
// 0012. Mirrors kernelboot.seededSelfUID / resolver.SelfEntityUID as a local
// literal so this command does not import those packages for one value.
const selfEntityAnchorUID = "self"

// defaultSelfListLimit caps the "Reinforced lately" and "Insights about you"
// sections so the mirror stays one screen. Raise with --limit.
const defaultSelfListLimit = 10

// selfIdentity is the resolved "you" header: the canonical (principal when
// bound, else the seeded anchor) entity, plus the alias uids both self nodes
// occupy so a consumer can see the pair was collapsed.
type selfIdentity struct {
	Name        string   `json:"name"` // entities/{uid}
	UID         string   `json:"uid"`
	DisplayName string   `json:"displayName"`
	Kind        string   `json:"kind,omitempty"`
	Bound       bool     `json:"bound"`
	AliasUids   []string `json:"aliasUids,omitempty"`
}

// selfFact is one open scalar fact about the reader, in the stable JSON shape.
type selfFact struct {
	Name       string  `json:"name"` // facts/{uid}
	StateKey   string  `json:"stateKey"`
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence,omitempty"`
	Source     string  `json:"source,omitempty"`
}

// reinforcedRow is one entity ranked by memory_strength access count. Self is
// true on the single collapsed row representing the reader's own node(s).
type reinforcedRow struct {
	Name           string `json:"name"` // entities/{uid}
	UID            string `json:"uid"`
	DisplayName    string `json:"displayName"`
	Kind           string `json:"kind,omitempty"`
	AccessCount    int    `json:"accessCount"`
	LastAccessedAt string `json:"lastAccessedAt,omitempty"`
	Self           bool   `json:"self,omitempty"`
}

// selfInsight is one synthesized insight about the reader.
type selfInsight struct {
	Name              string  `json:"name"` // insights/{uid}
	Statement         string  `json:"statement"`
	InsightType       string  `json:"insightType,omitempty"`
	State             string  `json:"state"`
	CurrentConfidence float64 `json:"currentConfidence,omitempty"`
	SynthesizedAt     string  `json:"synthesizedAt,omitempty"`
}

// selfView is the top-level JSON shape `maz self --json` emits. Stable and
// documented in the command Long help.
type selfView struct {
	Self       selfIdentity    `json:"self"`
	Facts      []selfFact      `json:"facts"`
	Reinforced []reinforcedRow `json:"reinforced"`
	Insights   []selfInsight   `json:"insights"`
}

func newSelfCmd() *cobra.Command {
	var (
		scopes []string
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "self",
		Short: "Show what Mazarbul knows about you (the Agent Mirror)",
		Long: `Print a one-screen mirror of what Mazarbul has recorded about you, in three
sections:

  What I know about you   the open scalar facts on your identity (name, role,
                          preferences, ...).
  Reinforced lately       the entities you keep coming back to, ranked by how
                          many times recall has surfaced them (memory strength).
                          These are entities you revisit — not facts about you;
                          surfacing one (including viewing it here) counts as an
                          access, so ranks drift as you read.
  Insights about you      insights synthesized from your own neighborhood.

Your identity is two linked nodes by design (a local "self" anchor and a
portable principal); this command reads both and collapses them, so a fact or
your own node is shown ONCE, never double-counted.

Reads are scope-isolated and fail closed: the default span is {project,
personal}; pass --scope to widen or narrow it. Your identity header always
resolves (it is your own, never a cross-scope leak).

On a terminal this renders titled sections; when stdout is piped or --json is
set it emits a stable JSON object {self, facts[], reinforced[], insights[]} and
never prompts.

Exit codes:
  0   rendered successfully (an empty mirror is still success)
  1   expected failure: a bad --scope value
  2   unexpected failure: storage or wiring error

Examples:
  maz self
  maz self --json
  maz self --limit 20
  maz self --scope personal --scope team
  maz self --json | jq '.facts[].stateKey'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateScopeFlag(cmd, "self", scopes); err != nil {
				return err
			}
			return respondError(cmd, "self", runSelf(cmd, scopes, limit))
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	cmd.Flags().IntVar(&limit, "limit", defaultSelfListLimit,
		"maximum rows in the reinforced + insights sections")
	return cmd
}

// runSelf opens a scope-isolated navigator over the local SQLite projection
// (the same direct read `maz list` / `maz review proposed` use), resolves the
// self/principal pair, gathers the three sections, and emits them.
func runSelf(cmd *cobra.Command, scopes []string, limit int) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return err
	}
	defer closeStoreQuietly(store)

	// AllScopes: the reader's own identity must resolve regardless of --scope;
	// identity is never a cross-scope leak. Data reads below use scoped (narrowed).
	nav, err := navigate.NewAllScopes(store.Reader())
	if err != nil {
		return fmt.Errorf("navigator: %w", err)
	}
	scopeSet := kernel.DefaultReadSet()
	if len(scopes) > 0 {
		// Pre-validated by validateScopeFlag; re-parse for the typed set.
		parsed, perr := kernel.ParseScopeSet(scopes)
		if perr != nil {
			return perr
		}
		scopeSet = parsed
	}
	scoped := nav.WithScopes(scopeSet)

	// Normalize the section cap once, so the reinforced and insights sections
	// share one effective page size (a --limit of 0 or below means the default).
	if limit <= 0 {
		limit = defaultSelfListLimit
	}

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	// Header hydration uses the unscoped navigator: the reader's own identity is
	// not scoped project/team data and must resolve regardless of --scope.
	ident, selfUIDs := resolveSelfIdentity(ctx, nav, config.NewSelfBinder(dataDir))

	facts, err := gatherSelfFacts(ctx, scoped, selfUIDs)
	if err != nil {
		return err
	}
	reinforced, err := gatherReinforced(ctx, scoped, selfUIDs, ident.UID, limit)
	if err != nil {
		return err
	}
	insights, err := gatherSelfInsights(ctx, scoped, selfUIDs, limit)
	if err != nil {
		return err
	}
	return emitSelf(cmd, &selfView{
		Self:       ident,
		Facts:      facts,
		Reinforced: reinforced,
		Insights:   insights,
	})
}

// resolveSelfIdentity resolves the self/principal pair to a header and the
// ordered uid set to read first-person data for. The principal (when bound) is
// placed FIRST so it wins dedup ties - it is the canonical, synced node. The
// header is hydrated from whichever node carries an identity row.
func resolveSelfIdentity(ctx context.Context, nav *navigate.Navigator, binder *config.SelfBinder) (selfIdentity, []string) {
	primary := selfEntityAnchorUID
	bound := false
	uids := []string{selfEntityAnchorUID}
	if principal, ok := binder.SelfPrincipal(ctx); ok && principal != "" && principal != selfEntityAnchorUID {
		primary = principal
		bound = true
		uids = []string{principal, selfEntityAnchorUID}
	}
	id := selfIdentity{
		UID:         primary,
		Name:        "entities/" + primary,
		DisplayName: "you",
		Bound:       bound,
		AliasUids:   append([]string{}, uids...),
	}
	for _, uid := range uids {
		e, gerr := nav.GetEntity(ctx, uid)
		if gerr != nil {
			continue
		}
		id.UID = e.UID
		id.Name = "entities/" + e.UID
		if e.DisplayName != "" {
			id.DisplayName = e.DisplayName
		}
		id.Kind = e.Kind
		break
	}
	return id, uids
}

// gatherSelfFacts reads the open facts for every self node and merges them,
// deduping identical (stateKey, value) pairs so a fact that lives on BOTH the
// anchor and the principal is shown once. Ordered by stateKey for stable output.
func gatherSelfFacts(ctx context.Context, nav *navigate.Navigator, uids []string) ([]selfFact, error) {
	seen := make(map[string]struct{})
	out := make([]selfFact, 0)
	for _, uid := range uids {
		facts, err := nav.FactsForSubject(ctx, uid, navigate.FactFilter{})
		if err != nil {
			return nil, err
		}
		for i := range facts {
			f := facts[i]
			key := f.StateKey + "\x00" + f.Value
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, selfFact{
				Name:       f.Name,
				StateKey:   f.StateKey,
				Value:      f.Value,
				Confidence: f.Confidence,
				Source:     f.Source,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StateKey != out[j].StateKey {
			return out[i].StateKey < out[j].StateKey
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}

// gatherReinforced reads the top reinforced entities and collapses every self
// node into a SINGLE "you" row keyed by the primary uid - the two same_as nodes
// denote one individual, so their access counts are summed and the latest
// access wins. One extra row is fetched so the collapse never shrinks the page
// below --limit.
func gatherReinforced(ctx context.Context, nav *navigate.Navigator, selfUIDs []string, primaryUID string, limit int) ([]reinforcedRow, error) {
	rows, err := nav.ReinforcedEntities(ctx, limit+len(selfUIDs))
	if err != nil {
		return nil, err
	}
	isSelf := make(map[string]bool, len(selfUIDs))
	for _, u := range selfUIDs {
		isSelf[u] = true
	}
	out := make([]reinforcedRow, 0, len(rows))
	selfIdx := -1
	for i := range rows {
		r := rows[i]
		if isSelf[r.UID] {
			if selfIdx < 0 {
				selfIdx = len(out)
				out = append(out, reinforcedRow{
					Name: "entities/" + primaryUID, UID: primaryUID,
					DisplayName: r.DisplayName, Kind: r.Kind,
					AccessCount: r.AccessCount, LastAccessedAt: r.LastAccessedAt, Self: true,
				})
				continue
			}
			out[selfIdx].AccessCount += r.AccessCount
			if r.LastAccessedAt > out[selfIdx].LastAccessedAt {
				out[selfIdx].LastAccessedAt = r.LastAccessedAt
			}
			continue
		}
		out = append(out, reinforcedRow{
			Name: "entities/" + r.UID, UID: r.UID,
			DisplayName: r.DisplayName, Kind: r.Kind,
			AccessCount: r.AccessCount, LastAccessedAt: r.LastAccessedAt,
		})
	}
	// The collapse may have raised the self row's count above its neighbors;
	// re-establish descending order (stable, so equal counts keep query order)
	// before capping to the page size.
	sort.SliceStable(out, func(i, j int) bool { return out[i].AccessCount > out[j].AccessCount })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// gatherSelfInsights reads the insights seeded by any self node. An insight
// carries a single seed_entity_uid, so the IN-set query yields no duplicates.
func gatherSelfInsights(ctx context.Context, nav *navigate.Navigator, selfUIDs []string, limit int) ([]selfInsight, error) {
	ins, err := nav.InsightsForSeeds(ctx, selfUIDs, limit)
	if err != nil {
		return nil, err
	}
	out := make([]selfInsight, 0, len(ins))
	for i := range ins {
		out = append(out, selfInsight{
			Name:              ins[i].Name,
			Statement:         ins[i].Statement,
			InsightType:       ins[i].InsightType,
			State:             ins[i].State,
			CurrentConfidence: ins[i].CurrentConfidence,
			SynthesizedAt:     ins[i].SynthesizedAt,
		})
	}
	return out, nil
}

// emitSelf writes the mirror in the format output.From selects: a single JSON
// object (also for NDJSON - the view is one composite record) when piped or
// --json, else the titled text sections.
func emitSelf(cmd *cobra.Command, view *selfView) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(view)
	}
	return renderSelfText(cmd, view)
}

// renderSelfText prints the three titled sections. It carries exactly the same
// data the JSON shape does.
func renderSelfText(cmd *cobra.Command, view *selfView) error {
	w := cmd.OutOrStdout()
	c := cmdColorizer(cmd)

	header := view.Self.DisplayName
	if view.Self.Kind != "" {
		header += " (" + view.Self.Kind + ")"
	}
	if _, err := fmt.Fprintf(w, "%s %s  %s\n",
		c.Accent("Mazarbul knows about"), c.Ident(header), c.Dim(view.Self.Name)); err != nil {
		return err
	}

	if err := renderSelfFacts(w, c, view.Facts); err != nil {
		return err
	}
	if err := renderSelfReinforced(w, c, view.Reinforced); err != nil {
		return err
	}
	return renderSelfInsights(w, c, view.Insights)
}

// renderSelfFacts prints the "What I know about you" section.
func renderSelfFacts(w io.Writer, c *colorizer.Colorizer, facts []selfFact) error {
	if _, err := fmt.Fprintf(w, "\n%s\n", c.Accent("What I know about you")); err != nil {
		return err
	}
	if len(facts) == 0 {
		_, err := fmt.Fprintln(w, c.Dim("  (nothing recorded yet)"))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range facts {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", f.StateKey, f.Value); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// renderSelfReinforced prints the "Reinforced lately" section.
func renderSelfReinforced(w io.Writer, c *colorizer.Colorizer, rows []reinforcedRow) error {
	if _, err := fmt.Fprintf(w, "\n%s\n", c.Accent("Reinforced lately")); err != nil {
		return err
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, c.Dim("  (no reinforced entities yet)"))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		name := r.DisplayName
		if r.Self {
			name += c.Dim(" (you)")
		}
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\n",
			c.Meta(fmt.Sprintf("%d×", r.AccessCount)), name, c.Dim(r.Kind)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// renderSelfInsights prints the "Insights about you" section.
func renderSelfInsights(w io.Writer, c *colorizer.Colorizer, insights []selfInsight) error {
	if _, err := fmt.Fprintf(w, "\n%s\n", c.Accent("Insights about you")); err != nil {
		return err
	}
	if len(insights) == 0 {
		_, err := fmt.Fprintln(w, c.Dim("  (no insights synthesized yet)"))
		return err
	}
	for _, ins := range insights {
		marker := "•"
		if ins.State != "hot" && ins.State != "" {
			marker = c.Dim("◦")
		}
		if _, err := fmt.Fprintf(w, "  %s %s\n", marker, ins.Statement); err != nil {
			return err
		}
	}
	return nil
}
