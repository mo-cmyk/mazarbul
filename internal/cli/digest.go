package cli

import (
	"context"
	"database/sql"
	"errors"
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

// sparkLevels are the Unicode block elements used to draw the daily sparkline.
// Six levels: empty → thin → medium → thick, matching the plan's ▁▂▃▅▇█ set.
//
//nolint:gochecknoglobals // package-level rune slice; cannot be const in Go.
var sparkLevels = []rune{'▁', '▂', '▃', '▅', '▇', '█'}

// digestByWriter and digestByKind are the valid --by flag values.
const (
	digestByWriter = "writer"
	digestByKind   = "kind"
)

// digestFlags holds the parsed flag set for `maz digest`.
type digestFlags struct {
	since  string
	scopes []string
	by     string // digestByWriter | digestByKind | "" (all)
}

// writerStat is a per-principal event breakdown included in the digest result.
type writerStat struct {
	PrincipalUID string         `json:"principalUID"`
	DisplayName  string         `json:"displayName"`
	Total        int            `json:"total"`
	ByKind       map[string]int `json:"byKind"`
}

// kindStat is an event count per EventKind, used in both the by-kind view and
// the JSON envelope.
type kindStat struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// entityKindCount is a count of new entities since --since, grouped by kind.
type entityKindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// dailyBucket is one calendar day's write count. Emitted in JSON mode and used
// to derive the sparkline.
type dailyBucket struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// digestResult is the stable JSON wire shape for `maz digest --output json`.
// All fields are always present (empty arrays/strings rather than null).
type digestResult struct {
	Since        string            `json:"since"`
	TotalEvents  int               `json:"totalEvents"`
	Writers      []writerStat      `json:"writers"`
	NewEntities  []entityKindCount `json:"newEntities"`
	KindCounts   []kindStat        `json:"kindCounts"`
	DailyBuckets []dailyBucket     `json:"dailyBuckets"`
	Sparkline    string            `json:"sparkline"`
}

// digestRender carries the human-facing header / sparkline-axis labels that
// are derived from flags but are not part of the JSON wire shape (digestResult).
type digestRender struct {
	spanLabel  string // e.g. "last 7 days" or "since Jun 1, 2026"
	scopeLabel string // e.g. "project, personal"
	startLabel string // sparkline axis left bound, e.g. "Jun 1"
	endLabel   string // sparkline axis right bound, e.g. "Jun 8"
}

// digestSpanLabel renders the --since value as prose: a bare duration ("7d",
// "1w") becomes "last 7 days"; an RFC 3339 timestamp becomes "since <date>".
func digestSpanLabel(raw string, since time.Time) string {
	if isDurationSpan(raw) {
		return friendlySpan(raw)
	}
	return "since " + since.Format("Jan 2, 2006")
}

// isDurationSpan reports whether raw is a bare duration span like "7d"/"2h"/
// "90m"/"1w"/"30s" (digits followed by a single s/m/h/d/w unit). The unit set
// mirrors timeparse.go's durationPattern exactly so digest never falls back to
// "since <date>" for a value ParseSince accepts.
func isDurationSpan(raw string) bool {
	if len(raw) < 2 {
		return false
	}
	if !strings.ContainsRune("smhdw", rune(raw[len(raw)-1])) {
		return false
	}
	for _, r := range raw[:len(raw)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// scopeNamesLabel joins a scope set's names for the digest header, e.g.
// "project, personal".
func scopeNamesLabel(s kernel.ScopeSet) string {
	scopes := s.Scopes()
	parts := make([]string, len(scopes))
	for i, sc := range scopes {
		parts[i] = string(sc)
	}
	return strings.Join(parts, ", ")
}

// newDigestCmd builds the `maz digest` cobra command.
//
// Digest produces a "what did I learn" rollup over a time window. It is cheap:
// it reads the JSONL event log for writer/kind/date stats and queries SQLite for
// new-entity counts. No network calls, no LLM, no daemon required.
func newDigestCmd() *cobra.Command {
	flags := &digestFlags{}
	cmd := &cobra.Command{
		Use:   "digest",
		Short: "Summarise memory writes over a time window",
		Long: `Roll up what was written to Mazarbul in a time window — by writer, by
event kind, new entities created, and a daily sparkline.

All data is read locally from the event log and SQLite index; no network
calls or LLM are needed.

Examples:
  # What happened in the last week?
  maz digest --since 7d

  # Writer breakdown for a specific date range
  maz digest --since 2026-06-01T00:00:00Z --by writer

  # Emit clean JSON for scripting
  maz digest --since 30d --output json | jq .writers`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDigest(cmd, flags)
		},
	}

	cmd.Flags().StringVar(&flags.since, "since", "7d",
		"lower bound on events: RFC 3339 timestamp or duration (7d, 2h, 90m, 1w)")
	cmd.Flags().StringArrayVar(&flags.scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	cmd.Flags().StringVar(&flags.by, "by", "",
		"focus view: writer — group by author; kind — group by event kind. Default: show all sections.")

	return cmd
}

// runDigest is the entry-point for `maz digest`.
func runDigest(cmd *cobra.Command, flags *digestFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	since, scopeSet, err := digestParseFlags(cmd, flags, logger)
	if err != nil {
		return err
	}

	events, err := digestReadEvents(ctx, cmd, dataDir, since, scopeSet, logger)
	if err != nil {
		return err
	}

	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "index.db"))
	if err != nil {
		return wrapDigestFatal(cmd, fmt.Errorf("open sqlite: %w", err))
	}
	defer closeStoreQuietly(store)

	entityCounts, err := queryEntityKindCounts(ctx, store.Reader(), since, scopeSet)
	if err != nil {
		return wrapDigestFatal(cmd, fmt.Errorf("entity counts: %w", err))
	}

	now := time.Now().UTC()
	writerMap, kindMap, daily := computeDigestStats(events)
	writers := resolveWriters(ctx, store.Reader(), writerMap, logger)
	spark := sparklineStr(sparklineCounts(daily, since, now))

	result := digestResult{
		Since:        since.UTC().Format(time.RFC3339),
		TotalEvents:  len(events),
		Writers:      writers,
		NewEntities:  entityCounts,
		KindCounts:   kindMapToSlice(kindMap),
		DailyBuckets: dailyMapToSlice(daily),
		Sparkline:    spark,
	}

	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return wr.Emit(result)
	}

	meta := &digestRender{
		spanLabel:  digestSpanLabel(flags.since, since),
		scopeLabel: scopeNamesLabel(scopeSet),
		startLabel: since.Format("Jan 2"),
		endLabel:   now.Format("Jan 2"),
	}
	w := cmd.OutOrStdout()
	clr := colorizer.New(interactive.ShouldStyle(cmd, w), w)
	return renderDigestText(cmd, w, clr, &result, flags.by, meta)
}

// digestParseFlags validates and parses the --by, --since, and --scope flags.
// It returns a silentError on any validation failure so the caller never prints
// a second message.
func digestParseFlags(
	cmd *cobra.Command,
	flags *digestFlags,
	logger *zap.Logger,
) (since time.Time, scopeSet kernel.ScopeSet, err error) {
	switch flags.by {
	case "", digestByWriter, digestByKind:
		// valid
	default:
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(),
			"digest: --by %q is not valid (want writer|kind)\n", flags.by); werr != nil {
			logger.Warn("digest: write stderr", zap.Error(werr))
		}
		return time.Time{}, kernel.ScopeSet{},
			silentError{code: 1, err: fmt.Errorf("invalid --by value %q", flags.by)}
	}

	since, err = ParseSince(flags.since)
	if err != nil {
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "digest: --since: %v\n", err); werr != nil {
			logger.Warn("digest: write stderr", zap.Error(werr))
		}
		return time.Time{}, kernel.ScopeSet{}, silentError{code: 1, err: err}
	}

	scopeSet = kernel.DefaultReadSet()
	if len(flags.scopes) > 0 {
		ss, serr := kernel.ParseScopeSet(flags.scopes)
		if serr != nil {
			if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "digest: --scope: %v\n", serr); werr != nil {
				logger.Warn("digest: write stderr", zap.Error(werr))
			}
			return time.Time{}, kernel.ScopeSet{}, silentError{code: 1, err: serr}
		}
		scopeSet = ss
	}
	return since, scopeSet, nil
}

// digestReadEvents opens the event log, reads events since the given time,
// and filters them by scope. Returns the collected slice.
func digestReadEvents(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	since time.Time,
	scopeSet kernel.ScopeSet,
	logger *zap.Logger,
) ([]kernel.Event, error) {
	eventLog, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return nil, wrapDigestFatal(cmd, fmt.Errorf("open event log: %w", err))
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("digest: close event log", zap.Error(cerr))
		}
	}()

	events := make([]kernel.Event, 0, 64) // capacity hint; iterator length unknown
	for ev, rerr := range eventLog.Read(ctx, kernel.ReadOpts{Since: since}) {
		if rerr != nil {
			return nil, wrapDigestFatal(cmd, fmt.Errorf("read event log: %w", rerr))
		}
		if !scopeSet.Contains(ev.Scope) {
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}

// computeDigestStats processes a slice of events into per-writer, per-kind,
// and per-day counts. This is a pure function for testability.
func computeDigestStats(events []kernel.Event) (
	writerMap map[string]*writerStat,
	kindMap map[string]int,
	daily map[string]int, // key: "2006-01-02"
) {
	writerMap = make(map[string]*writerStat)
	kindMap = make(map[string]int)
	daily = make(map[string]int)

	for i := range events {
		ev := &events[i]

		kindMap[string(ev.Kind)]++

		day := ev.CreateTime.UTC().Format("2006-01-02")
		daily[day]++

		uid := ev.Author.PrincipalUID
		if uid == "" {
			uid = "local" // events predating the author field
		}
		ws, ok := writerMap[uid]
		if !ok {
			ws = &writerStat{
				PrincipalUID: uid,
				ByKind:       make(map[string]int),
			}
			writerMap[uid] = ws
		}
		ws.Total++
		ws.ByKind[string(ev.Kind)]++
	}
	return writerMap, kindMap, daily
}

// resolveWriters converts the writerMap (keyed by principalUID) to a sorted
// []writerStat with display names resolved from the principals SQLite table.
// Failures are soft (name stays as the UID so output is always complete).
func resolveWriters(
	ctx context.Context,
	db *sql.DB,
	writerMap map[string]*writerStat,
	logger *zap.Logger,
) []writerStat {
	result := make([]writerStat, 0, len(writerMap))
	for uid, ws := range writerMap {
		name, err := lookupPrincipalName(ctx, db, uid)
		if err != nil {
			logger.Warn("digest: resolve principal name", zap.String("uid", uid), zap.Error(err))
		}
		if name == "" {
			name = uid // fall back to UID when no display name recorded
		}
		ws.DisplayName = name
		result = append(result, *ws)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Total != result[j].Total {
			return result[i].Total > result[j].Total
		}
		return result[i].PrincipalUID < result[j].PrincipalUID
	})
	return result
}

// lookupPrincipalName queries the principals table for a display name. Returns
// "" when the UID is not present (non-fatal — the caller falls back to the UID).
// This mirrors Dispatcher.PrincipalDisplayName without importing the heavyweight
// dispatcher package.
func lookupPrincipalName(ctx context.Context, db *sql.DB, principalUID string) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT display_name FROM principals WHERE principal_uid = ?`, principalUID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query principal display name: %w", err)
	}
	return name, nil
}

// queryEntityKindCounts returns a count of entities created since `since`,
// grouped by kind, filtered to the given scopes.
func queryEntityKindCounts(
	ctx context.Context,
	db *sql.DB,
	since time.Time,
	scopeSet kernel.ScopeSet,
) ([]entityKindCount, error) {
	scopes := scopeSet.Scopes()
	if len(scopes) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(scopes))
	args := make([]any, 0, len(scopes)+1)
	args = append(args, since.UTC().Format(time.RFC3339))
	for i, sc := range scopes {
		placeholders[i] = "?"
		args = append(args, string(sc))
	}

	q := fmt.Sprintf(
		`SELECT kind, COUNT(*) AS cnt FROM entities
		 WHERE create_time >= ?
		 AND scope IN (%s)
		 GROUP BY kind
		 ORDER BY cnt DESC`,
		strings.Join(placeholders, ","),
	)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("entity kind counts: %w", err)
	}
	defer rows.Close()

	var counts []entityKindCount
	for rows.Next() {
		var c entityKindCount
		if err := rows.Scan(&c.Kind, &c.Count); err != nil {
			return nil, fmt.Errorf("scan entity kind count: %w", err)
		}
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entity kind counts: %w", err)
	}
	if counts == nil {
		counts = []entityKindCount{}
	}
	return counts, nil
}

// kindMapToSlice converts the kind frequency map to a sorted slice (desc count, asc kind).
func kindMapToSlice(m map[string]int) []kindStat {
	out := make([]kindStat, 0, len(m))
	for k, n := range m {
		out = append(out, kindStat{Kind: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// dailyMapToSlice converts the daily count map to a chronologically-sorted slice.
func dailyMapToSlice(m map[string]int) []dailyBucket {
	out := make([]dailyBucket, 0, len(m))
	for d, n := range m {
		out = append(out, dailyBucket{Date: d, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Date < out[j].Date
	})
	return out
}

// sparklineCounts builds a []int of per-day counts over the [since, now] window
// with one slot per calendar day. Days with no events contribute a zero.
func sparklineCounts(daily map[string]int, since, now time.Time) []int {
	start := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	var counts []int
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		counts = append(counts, daily[d.Format("2006-01-02")])
	}
	return counts
}

// sparklineStr converts a slice of daily counts to a six-level Unicode block
// sparkline string (▁▂▃▅▇█). Zero counts render as a space. The maximum count
// maps to '█'; all others are normalised proportionally.
func sparklineStr(counts []int) string {
	if len(counts) == 0 {
		return ""
	}
	maxVal := 0
	for _, n := range counts {
		if n > maxVal {
			maxVal = n
		}
	}
	var sb strings.Builder
	for _, n := range counts {
		if n == 0 {
			sb.WriteByte(' ')
			continue
		}
		if maxVal == 0 {
			sb.WriteRune(sparkLevels[0])
			continue
		}
		// Map [1, maxVal] onto [0, len(sparkLevels)-1] using ceiling integer
		// division so that n==maxVal always lands on the top-most level █.
		nLevels := len(sparkLevels)
		idx := (n*(nLevels-1) + maxVal - 1) / maxVal
		if idx >= nLevels {
			idx = nLevels - 1
		}
		sb.WriteRune(sparkLevels[idx])
	}
	return sb.String()
}

// renderDigestText renders the digest in human-readable form.
// ByMode is digestByWriter (writer section only), digestByKind (kind section
// only), or "" (all sections).
func renderDigestText(
	cmd *cobra.Command,
	w io.Writer,
	clr *colorizer.Colorizer,
	result *digestResult,
	byMode string,
	meta *digestRender,
) error {
	styled := interactive.ShouldStyle(cmd, w)
	if err := renderDigestHeader(w, clr, meta); err != nil {
		return err
	}

	showAll := byMode == ""
	if showAll {
		if err := renderSparklineSection(w, clr, result, meta); err != nil {
			return err
		}
	}
	if showAll || byMode == digestByWriter {
		if err := renderWritersSection(w, clr, result.Writers, styled); err != nil {
			return err
		}
	}
	if showAll {
		if err := renderNewEntitiesSection(w, clr, result.NewEntities); err != nil {
			return err
		}
	}
	if showAll || byMode == digestByKind {
		return renderKindSection(w, clr, result.KindCounts)
	}
	return nil
}

// renderDigestHeader prints the "Memory digest · last 7 days · project, personal"
// banner from the derived render labels.
func renderDigestHeader(w io.Writer, clr *colorizer.Colorizer, meta *digestRender) error {
	parts := []string{"Memory digest"}
	if meta.spanLabel != "" {
		parts = append(parts, meta.spanLabel)
	}
	if meta.scopeLabel != "" {
		parts = append(parts, meta.scopeLabel)
	}
	if _, err := fmt.Fprintln(w, clr.Accent(strings.Join(parts, "  ·  "))); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// digestWriterBarWidth is the cell count of the per-writer magnitude bar.
const digestWriterBarWidth = 14

// writerBar renders a ▓/░ magnitude bar for a writer's event count relative to
// the busiest writer, tinted with the writer's stable agent hue when styled.
func writerBar(count, maxCount int, name string, styled bool) string {
	width := digestWriterBarWidth
	if maxCount > 0 {
		width = int(float64(count) / float64(maxCount) * float64(digestWriterBarWidth))
	}
	if width < 1 && count > 0 {
		width = 1
	}
	if width > digestWriterBarWidth {
		width = digestWriterBarWidth
	}
	bar := strings.Repeat("▓", width) + strings.Repeat("░", digestWriterBarWidth-width)
	if styled {
		return lipgloss.NewStyle().Foreground(style.AgentColor(name)).Render(bar)
	}
	return bar
}

// renderWritersSection prints the "by writer" breakdown with per-writer bars,
// or nothing when empty.
func renderWritersSection(w io.Writer, clr *colorizer.Colorizer, writers []writerStat, styled bool) error {
	if len(writers) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, clr.Dim("by writer")); err != nil {
		return err
	}
	maxTotal := 0
	for _, ws := range writers {
		if ws.Total > maxTotal {
			maxTotal = ws.Total
		}
	}
	for _, ws := range writers {
		dot := renderAgentDot(ws.DisplayName, styled)
		namePadded := fmt.Sprintf("%-16s", truncateString(ws.DisplayName, 16))
		bar := writerBar(ws.Total, maxTotal, ws.DisplayName, styled)
		kinds := topKindsStr(ws.ByKind, 3)
		if _, err := fmt.Fprintf(w, "  %s %s  %4d  %s  %s\n",
			dot,
			clr.Ident(namePadded),
			ws.Total,
			bar,
			clr.Dim(kinds),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderNewEntitiesSection prints the "New entities" breakdown, or nothing when empty.
func renderNewEntitiesSection(w io.Writer, clr *colorizer.Colorizer, entities []entityKindCount) error {
	if len(entities) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, clr.Dim("new entities")); err != nil {
		return err
	}
	for _, ec := range entities {
		if _, err := fmt.Fprintf(w, "  %-24s  %s%d\n",
			clr.Meta(ec.Kind),
			clr.Added("+"),
			ec.Count,
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderKindSection prints the "By event kind" table, or nothing when empty.
func renderKindSection(w io.Writer, clr *colorizer.Colorizer, kinds []kindStat) error {
	if len(kinds) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, clr.Dim("by event kind")); err != nil {
		return err
	}
	for _, ks := range kinds {
		if _, err := fmt.Fprintf(w, "  %-28s  %d\n", clr.Meta(ks.Kind), ks.Count); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// sparklineLabel is the leading column for the top sparkline line. Its visible
// width sets the indent for the date-range axis beneath the sparkline.
const sparklineLabel = "writes"

// renderSparklineSection prints the top "writes <spark>  N events" line and a
// dim "<start> → <end>" date-range axis aligned under the sparkline. Renders
// nothing when the sparkline is empty.
func renderSparklineSection(w io.Writer, clr *colorizer.Colorizer, result *digestResult, meta *digestRender) error {
	if result.Sparkline == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "%s  %s   %s\n",
		clr.Dim(sparklineLabel),
		clr.Accent(result.Sparkline),
		clr.Dim(fmt.Sprintf("%d events", result.TotalEvents))); err != nil {
		return err
	}
	if meta.startLabel != "" && meta.endLabel != "" {
		indent := strings.Repeat(" ", len(sparklineLabel)+2)
		if _, err := fmt.Fprintf(w, "%s%s\n", indent,
			clr.Dim(meta.startLabel+" → "+meta.endLabel)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderAgentDot returns a colored "●" dot using the writer's identity hue,
// or a plain "●" when styled is false (NO_COLOR / non-TTY).
func renderAgentDot(name string, styled bool) string {
	if !styled {
		return "●"
	}
	return lipgloss.NewStyle().Foreground(style.AgentColor(name)).Render("●")
}

// topKindsStr returns the N most common kinds from a ByKind map as a compact
// comma-separated string, e.g. "episode(12), entity(3)".
func topKindsStr(byKind map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(byKind))
	for k, v := range byKind {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	if len(pairs) > n {
		pairs = pairs[:n]
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s(%d)", p.k, p.v)
	}
	return strings.Join(parts, ", ")
}

// truncateString shortens s to at most n runes (not bytes). Used for alignment.
func truncateString(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// wrapDigestFatal reports an unexpected digest failure and returns exit code 2.
func wrapDigestFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "digest", err)
}
