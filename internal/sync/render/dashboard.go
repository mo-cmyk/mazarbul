package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// dashboardPath is the repo-root file the renderer emits on every plaintext
// push: a GitHub-native, at-a-glance view of the scope's memory (counts,
// activity, and a Mermaid graph). It is a sibling of README.md and chunks/,
// NOT nested under rendered/, so a visitor landing on the repo sees it
// immediately. It is regenerated wholesale each push and pruned on an
// encrypted flip (see git transport's pruneRenderedTree).
const dashboardPath = "DASHBOARD.md"

// Dashboard layout caps. The Mermaid node/edge caps keep the graph readable on
// GitHub (it silently truncates very large diagrams); the month caps keep the
// activity sparkline/table compact. DashboardMonthLimit is a runaway guard for
// the contiguous-month fill in case two UIDs decode to absurdly distant epochs.
const (
	dashboardMaxNodes    = 20
	dashboardMaxEdges    = 40
	dashboardMaxMonths   = 24
	dashboardTableMonths = 6
	dashboardMonthLimit  = 1200
)

// writeDashboard renders DASHBOARD.md from the current graph state, this push's
// Stats, and the injected (non-pure) Input values (RenderedAt + Devices). It is
// pure with respect to those inputs — no clock, no random, no map-order
// dependence — so the determinism guarantee holds.
func writeDashboard(state *GraphState, in Input, stats Stats) []byte {
	var buf strings.Builder
	writeDashboardHeader(&buf, in)
	writeDashboardHighlights(&buf, stats)
	writeDashboardCounts(&buf, state)
	writeDashboardActivity(&buf, state)
	writeDashboardGraph(&buf, state)
	writeDashboardFooter(&buf)
	return []byte(buf.String())
}

// writeDashboardHeader writes the title, the do-not-edit banner, and the
// last-synced line. RenderedAt and Devices are the two injected values; a zero
// RenderedAt (no head ULID) suppresses the timestamp rather than printing a
// year-1 sentinel.
func writeDashboardHeader(buf *strings.Builder, in Input) {
	fmt.Fprintf(buf, "# Your Mazarbul memory — `%s` scope\n\n", in.Scope)
	buf.WriteString("> 🤖 Generated on every sync — **do not edit**. " +
		"This file is rebuilt from `chunks/` on each push; hand edits are overwritten.\n\n")

	devs := uniqueSorted(in.Devices)
	switch {
	case !in.RenderedAt.IsZero() && len(devs) > 0:
		fmt.Fprintf(buf, "_Last synced %s from %d device(s): %s._\n",
			in.RenderedAt.Format("2006-01-02 15:04 UTC"), len(devs), strings.Join(devs, ", "))
	case !in.RenderedAt.IsZero():
		fmt.Fprintf(buf, "_Last synced %s._\n", in.RenderedAt.Format("2006-01-02 15:04 UTC"))
	case len(devs) > 0:
		fmt.Fprintf(buf, "_Synced from %d device(s): %s._\n", len(devs), strings.Join(devs, ", "))
	}
}

// writeDashboardHighlights lists this push's non-zero change counts. Skipped
// entirely when nothing changed (an incremental no-op push or the determinism
// test's prior-less render with an empty Stats).
func writeDashboardHighlights(buf *strings.Builder, s Stats) {
	var parts []string
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("**%d** %s", n, label))
		}
	}
	add(s.EntitiesAdded, "new entities")
	add(s.EntitiesChanged, "updated entities")
	add(s.EntitiesOrphaned, "orphaned entities")
	add(s.RelationsAdded, "new relations")
	add(s.RelationsSuperseded, "superseded relations")
	add(s.RelationsForgotten, "forgotten relations")
	add(s.PredicatesAdded, "new predicates")
	add(s.DocumentsAdded, "new documents")
	add(s.DocumentsChanged, "updated documents")
	if len(parts) == 0 {
		return
	}
	buf.WriteString("\n## This sync\n\n")
	for _, p := range parts {
		buf.WriteString("- " + p + "\n")
	}
}

// writeDashboardCounts writes the at-a-glance counts table: live entities by
// kind, then a bold total, orphans, live relations, predicates, documents, and
// facts. Zero rows for documents/facts are suppressed to keep the table tight.
func writeDashboardCounts(buf *strings.Builder, state *GraphState) {
	byKind, orphaned := entityKindCounts(state)
	buf.WriteString("\n## At a glance\n\n")
	buf.WriteString("| What | Count |\n|------|------:|\n")

	total := 0
	for _, kind := range orderedKinds(byKind) {
		c := byKind[kind]
		total += c
		fmt.Fprintf(buf, "| %s %s | %d |\n", kindGlyph(kind), kind, c)
	}
	fmt.Fprintf(buf, "| **Entities** | **%d** |\n", total)
	if orphaned > 0 {
		fmt.Fprintf(buf, "| ↺ orphaned entities | %d |\n", orphaned)
	}
	fmt.Fprintf(buf, "| 🔗 relations (live) | %d |\n", len(liveRelationsSorted(state)))
	fmt.Fprintf(buf, "| 🧩 predicates | %d |\n", len(state.Predicates))
	if dc := documentCount(state); dc > 0 {
		fmt.Fprintf(buf, "| 📄 documents | %d |\n", dc)
	}
	if len(state.Facts) > 0 {
		fmt.Fprintf(buf, "| 💬 facts | %d |\n", len(state.Facts))
	}
}

// writeDashboardActivity writes a single-row Unicode sparkline of memory growth
// by month plus a compact table of the most recent months. Buckets are derived
// from the ULID-embedded timestamps already in state, so it needs no clock.
func writeDashboardActivity(buf *strings.Builder, state *GraphState) {
	labels, counts := activityMonths(state)
	if len(counts) == 0 {
		return
	}
	buf.WriteString("\n## Activity\n\n")
	buf.WriteString("New records per month (entities + relations + facts + documents):\n\n")
	buf.WriteString("`" + sparklineRow(counts) + "`\n\n")

	start := 0
	if len(labels) > dashboardTableMonths {
		start = len(labels) - dashboardTableMonths
	}
	buf.WriteString("| Month | New |\n|-------|----:|\n")
	for i := start; i < len(labels); i++ {
		fmt.Fprintf(buf, "| %s | %d |\n", labels[i], counts[i])
	}
}

// writeDashboardGraph writes a Mermaid diagram of the top-N most-connected
// entities and the edges induced among them. Node ids are "n"+UID (a leading
// letter keeps them Mermaid-safe), and labels are escaped so quotes/percents/
// newlines in a display name can't break the diagram.
func writeDashboardGraph(buf *strings.Builder, state *GraphState) {
	degrees := entityDegrees(state)
	top := topConnectedEntities(state, degrees, dashboardMaxNodes)
	buf.WriteString("\n## Graph\n\n")
	if len(top) == 0 {
		buf.WriteString("_No connected entities yet — relations you add will appear here as a graph._\n")
		return
	}
	inTop := make(map[string]struct{}, len(top))
	for _, uid := range top {
		inTop[uid] = struct{}{}
	}
	buf.WriteString("```mermaid\ngraph LR\n")
	writeGraphNodes(buf, state, top)
	writeGraphEdges(buf, state, inTop)
	buf.WriteString("```\n\n")
	fmt.Fprintf(buf, "_Top %d most-connected entities. Explore the full graph locally with `maz doc canvas`._\n", len(top))
}

// writeGraphNodes emits one Mermaid node declaration per top entity.
func writeGraphNodes(buf *strings.Builder, state *GraphState, top []string) {
	for _, uid := range top {
		row := state.Entities[uid]
		fmt.Fprintf(buf, "  n%s[\"%s\"]\n", uid, mermaidEscape(row.DisplayName))
	}
}

// writeGraphEdges emits the live relations whose endpoints are both in the
// top set, de-duplicated by (subject, predicate, object) and capped at
// dashboardMaxEdges so the diagram stays legible.
func writeGraphEdges(buf *strings.Builder, state *GraphState, inTop map[string]struct{}) {
	seen := make(map[string]struct{})
	edges := 0
	for _, r := range liveRelationsSorted(state) {
		if edges >= dashboardMaxEdges {
			break
		}
		if _, ok := inTop[r.SubjectUID]; !ok {
			continue
		}
		if _, ok := inTop[r.ObjectUID]; !ok {
			continue
		}
		key := r.SubjectUID + "|" + r.Predicate + "|" + r.ObjectUID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		fmt.Fprintf(buf, "  n%s -->|\"%s\"| n%s\n", r.SubjectUID, mermaidEscape(r.Predicate), r.ObjectUID)
		edges++
	}
}

// writeDashboardFooter ties the dashboard back to the editability contract.
func writeDashboardFooter(buf *strings.Builder) {
	buf.WriteString("\n---\n\n")
	buf.WriteString("This dashboard and the `rendered/` tree are **machine-generated** — " +
		"see [`README.md`](README.md) for what you may edit. " +
		"Your memory lives in the append-only `chunks/`; everything else is rebuilt from it on each sync.\n")
}

// entityKindCounts tallies non-orphaned entities by kind and returns the
// orphaned count separately (an orphaned entity has lost all its mention
// provenance and is excluded from the live per-kind counts, matching the
// kind-index !Orphaned filter).
func entityKindCounts(state *GraphState) (map[string]int, int) {
	byKind := make(map[string]int)
	orphaned := 0
	for _, uid := range sortedKeys(state.Entities) {
		row := state.Entities[uid]
		if row.Orphaned {
			orphaned++
			continue
		}
		byKind[string(row.Kind)]++
	}
	return byKind, orphaned
}

// orderedKinds returns the kinds present in byKind, built-in kinds first (in
// allKinds order) and any custom kinds appended in lexical order, so a
// dynamically-registered entity kind is still counted rather than hidden.
func orderedKinds(byKind map[string]int) []string {
	seen := make(map[string]bool, len(byKind))
	var out []string
	for _, k := range allKinds() {
		ks := string(k)
		if byKind[ks] > 0 {
			out = append(out, ks)
			seen[ks] = true
		}
	}
	var extra []string
	for k, c := range byKind {
		if c > 0 && !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// kindGlyph maps a built-in entity kind to a small emoji; custom kinds fall
// back to a neutral bullet.
func kindGlyph(kind string) string {
	switch ontology.EntityKind(kind) {
	case ontology.EntityKindPerson:
		return "👤"
	case ontology.EntityKindOrganization:
		return "🏢"
	case ontology.EntityKindPlace:
		return "📍"
	case ontology.EntityKindProject:
		return "🛠"
	case ontology.EntityKindEvent:
		return "📅"
	case ontology.EntityKindDocument:
		return "📄"
	default:
		return "•"
	}
}

// documentCount returns the number of non-deleted documents.
func documentCount(state *GraphState) int {
	n := 0
	for _, d := range state.Documents {
		if !d.Deleted {
			n++
		}
	}
	return n
}

// liveRelationsSorted returns the relations that are neither superseded nor
// forgotten, in UID order (deterministic).
func liveRelationsSorted(state *GraphState) []*RelationRow {
	out := make([]*RelationRow, 0, len(state.Relations))
	for _, uid := range sortedKeys(state.Relations) {
		r := state.Relations[uid]
		if r.Forgotten || r.SupersededByUID != "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// entityDegrees counts, per entity, the live relations that touch it. Only
// relations whose endpoints both still exist as entities contribute (a dangling
// edge would otherwise inflate a degree without a node to render).
func entityDegrees(state *GraphState) map[string]int {
	deg := make(map[string]int)
	for _, r := range liveRelationsSorted(state) {
		if !entityExists(state, r.SubjectUID) || !entityExists(state, r.ObjectUID) {
			continue
		}
		deg[r.SubjectUID]++
		deg[r.ObjectUID]++
	}
	return deg
}

// entityExists reports whether uid names a known entity row.
func entityExists(state *GraphState, uid string) bool {
	_, ok := state.Entities[uid]
	return ok
}

// topConnectedEntities returns up to n entity UIDs ranked by degree (desc),
// UID-ascending on ties. Entities with no live edges are excluded — they would
// be isolated nodes in the graph.
func topConnectedEntities(state *GraphState, degrees map[string]int, n int) []string {
	type scored struct {
		uid string
		deg int
	}
	var list []scored
	for _, uid := range sortedKeys(state.Entities) {
		if d := degrees[uid]; d > 0 {
			list = append(list, scored{uid, d})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].deg != list[j].deg {
			return list[i].deg > list[j].deg
		}
		return list[i].uid < list[j].uid
	})
	if len(list) > n {
		list = list[:n]
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.uid
	}
	return out
}

// mermaidEscape neutralises the characters that would break a quoted Mermaid
// label: a double quote ends the label, a pipe is the outer delimiter of an
// edge label (`-->|"label"|`) so a raw one truncates it, a percent starts a
// Mermaid directive, and a raw newline breaks the diagram line. A carriage
// return is dropped so CRLF display names don't leak a stray glyph. Used for
// both node labels (`["…"]`) and user-defined predicate edge labels.
func mermaidEscape(s string) string {
	s = strings.ReplaceAll(s, "\"", "#quot;")
	s = strings.ReplaceAll(s, "|", "#124;")
	s = strings.ReplaceAll(s, "%", "#37;")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "<br/>")
	return s
}

// sparklineRow renders counts as a single row of block glyphs scaled to the
// max. Integer math keeps it deterministic; an all-zero series renders as the
// lowest block.
func sparklineRow(counts []int) string {
	bars := []rune{'▁', '▂', '▃', '▅', '▇'}
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	var b strings.Builder
	for _, c := range counts {
		idx := 0
		if maxCount > 0 {
			idx = c * (len(bars) - 1) / maxCount
		}
		b.WriteRune(bars[idx])
	}
	return b.String()
}

// activityMonths buckets every record's ULID-embedded month into a contiguous,
// gap-filled series (capped to the most recent dashboardMaxMonths). Returns
// parallel label/count slices, or nil when state is empty.
func activityMonths(state *GraphState) ([]string, []int) {
	hist := monthHistogram(state)
	if len(hist) == 0 {
		return nil, nil
	}
	lo, hi := monthRange(hist)
	months := contiguousMonths(lo, hi)
	if len(months) > dashboardMaxMonths {
		months = months[len(months)-dashboardMaxMonths:]
	}
	labels := make([]string, len(months))
	counts := make([]int, len(months))
	for i, ym := range months {
		labels[i] = fmt.Sprintf("%04d-%02d", ym[0], ym[1])
		counts[i] = hist[ym]
	}
	return labels, counts
}

// monthHistogram counts records by [year, month], derived from the ULID
// timestamp of each entity/relation/fact/document UID. Map-order independent:
// it only increments commutatively, so no sorted iteration is needed.
func monthHistogram(state *GraphState) map[[2]int]int {
	hist := make(map[[2]int]int)
	addUID := func(uid string) {
		y, m := yearMonthFromULID(uid)
		hist[[2]int{y, m}]++
	}
	for uid := range state.Entities {
		addUID(uid)
	}
	for uid := range state.Relations {
		addUID(uid)
	}
	for _, f := range state.Facts {
		addUID(f.UID)
	}
	for uid := range state.Documents {
		addUID(uid)
	}
	return hist
}

// monthRange returns the earliest and latest [year, month] keys in hist.
func monthRange(hist map[[2]int]int) ([2]int, [2]int) {
	first := true
	var lo, hi [2]int
	for k := range hist {
		if first {
			lo, hi = k, k
			first = false
			continue
		}
		if monthLess(k, lo) {
			lo = k
		}
		if monthLess(hi, k) {
			hi = k
		}
	}
	return lo, hi
}

// monthLess orders [year, month] pairs chronologically.
func monthLess(a, b [2]int) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

// contiguousMonths returns every [year, month] from lo to hi inclusive, filling
// gaps so the sparkline has one slot per calendar month. DashboardMonthLimit
// caps a pathological span (e.g. a corrupt UID decoding to a far epoch).
func contiguousMonths(lo, hi [2]int) [][2]int {
	var out [][2]int
	y, m := lo[0], lo[1]
	for len(out) < dashboardMonthLimit {
		out = append(out, [2]int{y, m})
		if y == hi[0] && m == hi[1] {
			break
		}
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	return out
}

// uniqueSorted returns the distinct values of in, sorted lexicographically.
func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
