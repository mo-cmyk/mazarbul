package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// activityBucket is the JSON wire shape for a single date bucket.
// It is the stable, documented JSON element emitted by `maz activity --output json`.
type activityBucket struct {
	Date  string `json:"date"`  // "2006-01-02" (UTC)
	Count int    `json:"count"` // number of write events on this date
}

// activityFlags holds the parsed flag values for the activity command.
type activityFlags struct {
	span   string
	scopes []string
	author string
}

// activityData is the internal representation built from the event log.
type activityData struct {
	// dateCounts maps "2006-01-02" → write count.
	dateCounts map[string]int
	// authorCounts maps principalUID → write count.
	authorCounts map[string]int
	// authorNames maps principalUID → resolved display name (empty until
	// resolveAuthorNames runs; render falls back to the UID).
	authorNames map[string]string
	// total is the total number of events observed.
	total int
	// since marks the beginning of the queried span.
	since time.Time
	// span is the human-readable span label (e.g. "90d", "52w").
	span string
}

// newActivityCmd returns the cobra.Command for `maz activity`.
func newActivityCmd() *cobra.Command {
	flags := &activityFlags{span: "90d"}
	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show a contributions heatmap and per-author write counts",
		Long: `Display a day-of-week × week contributions heatmap built from the
write events recorded in the Mazarbul event log, plus a per-author bar chart
using each writer's stable identity hue.

The heatmap source is Event.CreateTime — cells represent memory writes, not
reads. Recall/access counters are in-memory and reset on daemon restart, so a
reads-over-time view is not available here.

Examples:
  # Default 90-day heatmap in color
  maz activity

  # Last 30 days, filtered to the project scope
  maz activity --span 30d --scope project

  # Full year, JSON buckets for a script
  maz activity --span 52w --output json | jq '.[] | select(.count > 5)'

  # Single-author focus
  maz activity --author local`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runActivity(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.span, "span", "90d",
		"time window to display: 30d | 90d | 52w (default 90d)")
	cmd.Flags().StringSliceVar(&flags.scopes, "scope", nil,
		"restrict to events in the given scope(s) (repeatable; default all scopes)")
	cmd.Flags().StringVar(&flags.author, "author", "",
		"restrict to events authored by this principalUID or display name prefix")
	// --output is the root's persistent flag (text|json|ndjson); do not shadow it
	// with a local registration — pflag.AddFlagSet would skip the persistent one
	// and `--help` would advertise an incomplete format list.
	return cmd
}

// spanDays returns the number of days covered by a span flag value.
// Valid values are "30d", "90d", "52w".
func spanDays(span string) (int, error) {
	switch strings.TrimSpace(span) {
	case "30d":
		return 30, nil
	case "90d":
		return 90, nil
	case "52w":
		return 52 * 7, nil
	default:
		return 0, fmt.Errorf("--span: must be 30d, 90d, or 52w (got %q)", span)
	}
}

// runActivity is the RunE body for the activity command.
func runActivity(cmd *cobra.Command, flags *activityFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	days, err := spanDays(flags.span)
	if err != nil {
		return respondFatal(cmd, "activity", err)
	}

	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return respondFatal(cmd, "activity", fmt.Errorf("open eventlog: %w", err))
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("activity: close eventlog", zap.Error(cerr))
		}
	}()

	// One namer (opens index.db once) serves both the --author filter's
	// display-name matching and the per-author bar labels.
	namer := newPrincipalNamer(ctx, dataDir, logger)
	defer namer.Close()

	data, err := collectActivity(ctx, log, since, flags, namer)
	if err != nil {
		return respondFatal(cmd, "activity", err)
	}

	// Resolve principal UIDs → display names (best-effort; mirrors digest so
	// the per-author bars read "● mo-cmyk" rather than a raw ULID).
	data.authorNames = namer.resolveAll(data.authorCounts)

	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return emitActivityJSON(cmd, wr, data)
	}
	return renderActivityText(cmd, wr, data)
}

// collectActivity reads the event log and populates an activityData value.
// Namer resolves principal display names so the --author filter can match a
// query against either the principalUID or the display name (per the flag
// docs); it is only consulted when --author is set.
func collectActivity(ctx context.Context, log kernel.EventLog, since time.Time, flags *activityFlags, namer *principalNamer) (*activityData, error) {
	opts := kernel.ReadOpts{Since: since}

	data := &activityData{
		dateCounts:   make(map[string]int),
		authorCounts: make(map[string]int),
		since:        since,
		span:         flags.span,
	}

	for ev, err := range log.Read(ctx, opts) {
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}

		// Scope filter (in-loop, since ReadOpts has no scope field).
		if len(flags.scopes) > 0 && !scopeMatches(string(ev.Scope), flags.scopes) {
			continue
		}

		// Author filter: match the query as a prefix of either the
		// principalUID or the resolved display name (the flag accepts both).
		uid := ev.Author.PrincipalUID
		if flags.author != "" && !authorMatches(uid, namer.name(uid), flags.author) {
			continue
		}

		dateKey := ev.CreateTime.UTC().Format("2006-01-02")
		data.dateCounts[dateKey]++
		data.authorCounts[uid]++
		data.total++
	}
	return data, nil
}

// principalNamer resolves principal UIDs to display names against the
// principals table (reusing digest's lookupPrincipalName), opening index.db
// once and caching every lookup. Best-effort: if the store cannot be opened,
// lookups return "" and callers fall back to the raw UID. The empty-UID
// sentinel (events predating the author field) always resolves to "local".
type principalNamer struct {
	ctx    context.Context
	store  *sqlite.Store // nil when the index could not be opened
	logger *zap.Logger
	cache  map[string]string
}

// newPrincipalNamer opens the principals index once (best-effort).
func newPrincipalNamer(ctx context.Context, dataDir string, logger *zap.Logger) *principalNamer {
	n := &principalNamer{ctx: ctx, logger: logger, cache: make(map[string]string)}
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "index.db"))
	if err != nil {
		logger.Warn("activity: open store for name resolution", zap.Error(err))
		return n
	}
	n.store = store
	return n
}

// name returns the cached display name for uid. The empty-UID sentinel maps to
// "local"; an unknown or unresolved UID returns "".
func (n *principalNamer) name(uid string) string {
	if uid == "" {
		return "local"
	}
	if cached, ok := n.cache[uid]; ok {
		return cached
	}
	var name string
	if n.store != nil {
		resolved, err := lookupPrincipalName(n.ctx, n.store.Reader(), uid)
		if err != nil {
			n.logger.Warn("activity: resolve principal name", zap.String("uid", uid), zap.Error(err))
		}
		name = resolved
	}
	n.cache[uid] = name
	return name
}

// resolveAll returns a uid→display-name map for every uid in counts. The
// empty-UID sentinel renders as "local"; unresolved UIDs are omitted so the
// render falls back to the raw UID.
func (n *principalNamer) resolveAll(counts map[string]int) map[string]string {
	names := make(map[string]string, len(counts))
	for uid := range counts {
		if nm := n.name(uid); nm != "" {
			names[uid] = nm
		}
	}
	return names
}

// Close releases the underlying store (no-op when it was never opened).
func (n *principalNamer) Close() {
	if n.store == nil {
		return
	}
	if cerr := n.store.Close(); cerr != nil {
		n.logger.Warn("activity: close store", zap.Error(cerr))
	}
}

// authorMatches reports whether an event's author matches the --author query as
// a prefix of either the principalUID or the resolved display name. DisplayName
// is "local" for the empty-UID sentinel.
func authorMatches(uid, displayName, query string) bool {
	if uid != "" && strings.HasPrefix(uid, query) {
		return true
	}
	return displayName != "" && strings.HasPrefix(displayName, query)
}

// scopeMatches reports whether scope is present in the accepted list.
func scopeMatches(scope string, accepted []string) bool {
	for _, a := range accepted {
		if scope == a {
			return true
		}
	}
	return false
}

// emitActivityJSON marshals the date buckets as a JSON array and emits it.
// Only dates with at least one event are included, sorted ascending.
func emitActivityJSON(cmd *cobra.Command, wr output.Writer, data *activityData) error {
	buckets := buildBuckets(data.dateCounts)
	if wr.Format() == output.FormatNDJSON {
		// NDJSON is one bucket per line. Emit on the whole slice would marshal a
		// single JSON-array line, breaking the one-object-per-line contract that
		// `--output ndjson` promises, so stream each bucket individually.
		for i := range buckets {
			if err := wr.Emit(buckets[i]); err != nil {
				return respondFatal(cmd, "activity", err)
			}
		}
		return wr.Flush()
	}
	if err := wr.Emit(buckets); err != nil {
		return respondFatal(cmd, "activity", err)
	}
	return wr.Flush()
}

// buildBuckets converts the date-count map to a sorted slice of activityBucket.
// Only dates with count > 0 are included. An empty map returns an empty (not
// nil) slice so `maz activity --output json` always produces a valid JSON array.
func buildBuckets(dateCounts map[string]int) []activityBucket {
	dates := make([]string, 0, len(dateCounts))
	for d := range dateCounts {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	buckets := make([]activityBucket, 0, len(dates))
	for _, d := range dates {
		if dateCounts[d] > 0 {
			buckets = append(buckets, activityBucket{Date: d, Count: dateCounts[d]})
		}
	}
	return buckets
}

// renderActivityText renders the heatmap + per-author bars to the Writer in
// text mode.
func renderActivityText(cmd *cobra.Command, wr output.Writer, data *activityData) error {
	w := cmd.OutOrStdout()
	enabled := interactive.ShouldStyle(cmd, w)
	clr := colorizer.New(enabled, w)

	return output.MaybePage(cmd, func(out io.Writer) error {
		return renderActivityTo(out, clr, enabled, data)
	})
}

// renderActivityTo writes the full activity view to out. Separated from
// renderActivityText so tests can capture the rendered bytes directly.
func renderActivityTo(out io.Writer, clr *colorizer.Colorizer, enabled bool, data *activityData) error {
	// ── Header ──────────────────────────────────────────────────────────────
	headerText := fmt.Sprintf("Memory writes  ·  %s  ·  %d events", friendlySpan(data.span), data.total)
	_, _ = fmt.Fprintln(out, clr.Accent(headerText)) //nolint:errcheck // best-effort render
	_, _ = fmt.Fprintln(out)                         //nolint:errcheck // best-effort render

	// ── Heatmap ─────────────────────────────────────────────────────────────
	renderHeatmap(out, clr, data)
	renderHeatmapLegend(out, clr)
	_, _ = fmt.Fprintln(out) //nolint:errcheck // best-effort render

	// ── Per-author bars ──────────────────────────────────────────────────────
	if len(data.authorCounts) > 0 {
		_, _ = fmt.Fprintln(out, clr.Dim("by author")) //nolint:errcheck // best-effort render
		renderAuthorBars(out, clr, enabled, data.authorCounts, data.authorNames)
	}
	return nil
}

// friendlySpan turns a span label ("90d", "52w") into prose ("last 90 days").
// The unit suffixes mirror timeparse.go's durationUnit exactly — note 'm' is
// minutes, not months (ParseSince has no month unit), so a "30m" span reads
// "last 30 minutes" and matches the data window it actually selects.
func friendlySpan(span string) string {
	if span == "" {
		return "all time"
	}
	unit := span[len(span)-1]
	n := span[:len(span)-1]
	// pluralize appends "s" unless the count is exactly 1 ("last 1 day", not
	// "last 1 days").
	pluralize := func(word string) string {
		if n == "1" {
			return "last 1 " + word
		}
		return "last " + n + " " + word + "s"
	}
	switch unit {
	case 'd':
		return pluralize("day")
	case 'w':
		return pluralize("week")
	case 'h':
		return pluralize("hour")
	case 'm':
		return pluralize("minute")
	case 's':
		return pluralize("second")
	default:
		return "last " + span
	}
}

// heatmapIndent is the visible width of the "Mon  " row-label gutter (3-char
// label + 2 spaces); the month axis and legend align cells to this column.
const heatmapIndent = 5

// renderHeatmap renders the GitHub-style day-of-week × week contribution grid:
// a month axis, then seven weekday rows (only Mon/Wed/Fri labelled), each cell
// a glyph from heatLevels ramped by daily volume.
//
//	       Apr   May   Jun
//	Mon  ░░▁▂░▂▃▅░░▁▂▇█▅░
//	Wed  ░▁▂▃░▁▅▇▂░░▃▅█▇▂
//	Fri  ▁▂░░▁▃▅░▁▂▅▇█▃░▁
func renderHeatmap(out io.Writer, clr *colorizer.Colorizer, data *activityData) {
	// Align grid start to the Monday of the week containing since.
	gridStart := alignToMonday(data.since)

	// Number of week columns: from gridStart to today (inclusive).
	today := time.Now().UTC().Truncate(24 * time.Hour)
	numWeeks := int(today.Sub(gridStart).Hours()/(24*7)) + 1

	// Compute max daily count for intensity scaling.
	maxCount := 0
	for _, c := range data.dateCounts {
		if c > maxCount {
			maxCount = c
		}
	}

	renderMonthAxis(out, clr, gridStart, numWeeks)

	// Day-of-week labels; only Mon/Wed/Fri are labelled (GitHub convention),
	// the rest keep a blank 3-char gutter so the grid stays aligned.
	dayLabels := []string{"Mon", "", "Wed", "", "Fri", "", ""}
	for dayIdx, label := range dayLabels {
		var sb strings.Builder
		if label == "" {
			sb.WriteString("   ")
		} else {
			sb.WriteString(clr.Dim(label))
		}
		sb.WriteString("  ")
		for week := range numWeeks {
			date := gridStart.AddDate(0, 0, week*7+dayIdx)
			if date.After(today) {
				// Future cell — emit a blank so the grid ends cleanly at today.
				sb.WriteString(" ")
				continue
			}
			dateKey := date.Format("2006-01-02")
			count := data.dateCounts[dateKey]
			sb.WriteString(heatmapCell(clr, count, maxCount))
		}
		_, _ = fmt.Fprintln(out, sb.String()) //nolint:errcheck // best-effort render
	}
}

// renderMonthAxis prints the month-label row above the grid: each month's
// 3-letter abbreviation begins at the week column where that month first
// appears (the Monday of that week), aligned to the cell columns.
func renderMonthAxis(out io.Writer, clr *colorizer.Colorizer, gridStart time.Time, numWeeks int) {
	row := []rune(strings.Repeat(" ", heatmapIndent+numWeeks))
	prevMonth := time.Month(0)
	// lastLabelEnd is the last grid column a label occupied; a new label is only
	// placed when it clears that column by ≥1 space, so a month spanning only a
	// few week-columns is skipped rather than abutting the next (GitHub does the
	// same). Starts well below 0 so the first month is always placed.
	lastLabelEnd := -10
	for week := range numWeeks {
		m := gridStart.AddDate(0, 0, week*7).Month()
		if m == prevMonth {
			continue
		}
		prevMonth = m
		if week < lastLabelEnd+2 {
			continue // not enough room — would abut the previous label
		}
		abbr := []rune(m.String()[:3])
		for k, r := range abbr {
			if pos := heatmapIndent + week + k; pos < len(row) {
				row[pos] = r
			}
		}
		lastLabelEnd = week + len(abbr) - 1
	}
	_, _ = fmt.Fprintln(out, clr.Dim(strings.TrimRight(string(row), " "))) //nolint:errcheck // best-effort render
}

// renderHeatmapLegend prints the `less ░▁▂▃▅▇█ more` intensity key, aligned
// under the grid cells.
func renderHeatmapLegend(out io.Writer, clr *colorizer.Colorizer) {
	_, _ = fmt.Fprintf(out, "%sless %s more\n", //nolint:errcheck // best-effort render
		strings.Repeat(" ", heatmapIndent), clr.Dim(string(heatLevels)))
}

// alignToMonday returns the Monday of the ISO week that contains t (UTC).
func alignToMonday(t time.Time) time.Time {
	t = t.UTC().Truncate(24 * time.Hour)
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7 // Sunday → ISO 7
	}
	return t.AddDate(0, 0, -(wd - 1))
}

// heatLevels are the GitHub-style intensity glyphs, matching the legend
// `less ░▁▂▃▅▇█ more`. Index 0 (░) marks an empty day; 1‥6 ramp ▁▂▃▅▇█ with
// daily volume.
//
//nolint:gochecknoglobals // package-level rune slice; cannot be const in Go.
var heatLevels = []rune{'░', '▁', '▂', '▃', '▅', '▇', '█'}

// heatmapCell returns a single heatmap cell glyph from heatLevels, coloured by
// density. Both the GLYPH and the colour ramp dim → accent, so the grid reads
// correctly under NO_COLOR (glyph carries the intensity).
//
//	0            → dim "░"        (no activity)
//	low          → dim ▁▂
//	medium       → success/changed ▃▅
//	peak         → accent ▇█
func heatmapCell(clr *colorizer.Colorizer, count, maxCount int) string {
	if count <= 0 {
		return clr.Dim(string(heatLevels[0]))
	}
	idx := 6
	if maxCount > 1 {
		// Ceil(count*6 / maxCount) maps (0, max] onto 1‥6 so any non-zero day
		// shows at least ▁ and the busiest day always lands on █.
		idx = (count*6 + maxCount - 1) / maxCount
	}
	if idx < 1 {
		idx = 1
	}
	if idx > 6 {
		idx = 6
	}
	g := string(heatLevels[idx])
	switch {
	case idx <= 2:
		return clr.Dim(g)
	case idx <= 3:
		return clr.Success(g)
	case idx <= 5:
		return clr.Changed(g)
	default:
		return clr.Accent(g)
	}
}

// renderAuthorBars renders a per-author bar chart sorted by event count desc.
// Each bar is coloured by style.AgentColor, gated by enabled. The bar width is
// scaled to barMaxWidth runes relative to the most-active author. AuthorNames
// maps the principal UID key to a resolved display name; rows fall back to the
// UID when a name is unavailable.
func renderAuthorBars(out io.Writer, clr *colorizer.Colorizer, enabled bool, authorCounts map[string]int, authorNames map[string]string) {
	const barMaxWidth = 20
	const nameWidth = 16

	// Sort authors by count descending, then by resolved name for stable order.
	type authorEntry struct {
		key   string // principal UID (stable hue seed + name-resolution key)
		name  string // resolved display name (falls back to key)
		count int
	}
	entries := make([]authorEntry, 0, len(authorCounts))
	maxCount := 0
	for key, count := range authorCounts {
		name := key
		if resolved := authorNames[key]; resolved != "" {
			name = resolved
		}
		entries = append(entries, authorEntry{key: key, name: name, count: count})
		if count > maxCount {
			maxCount = count
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name
	})

	for _, e := range entries {
		barWidth := barMaxWidth
		if maxCount > 0 {
			barWidth = int(float64(e.count) / float64(maxCount) * float64(barMaxWidth))
		}
		if barWidth < 1 && e.count > 0 {
			barWidth = 1
		}
		bar := strings.Repeat("▓", barWidth) + strings.Repeat("░", barMaxWidth-barWidth)

		// Colour the bar using the author's stable agent hue (gated by enabled).
		if enabled {
			barStyle := lipgloss.NewStyle().Foreground(style.AgentColor(e.name))
			bar = barStyle.Render(bar)
		}

		// Truncate long names; pad short names to align counts.
		displayName := e.name
		if len([]rune(displayName)) > nameWidth {
			displayName = string([]rune(displayName)[:nameWidth-1]) + "…"
		}
		namePadded := fmt.Sprintf("%-*s", nameWidth, displayName)

		_, _ = fmt.Fprintf(out, "  %s  %s  %d\n", //nolint:errcheck // best-effort render
			clr.Meta(namePadded), bar, e.count)
	}
}
