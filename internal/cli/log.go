package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// logColorizer returns a no-op colorizer when JSON mode is active or color is
// disabled, and a live colorizer otherwise. Keeping the factory here keeps
// streamLogPlain readable.
func logColorizer(cmd *cobra.Command, w io.Writer, jsonMode bool) *colorizer.Colorizer {
	if jsonMode {
		return colorizer.New(false, w)
	}
	return colorizer.New(interactive.ShouldStyle(cmd, w), w)
}

type logFlags struct {
	since   string
	until   string
	kinds   []string
	limit   int
	oneline bool
	follow  bool
	scopes  []string
	authors []string
}

func newLogCmd() *cobra.Command {
	flags := &logFlags{limit: 100}
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Tail the JSONL event log",
		Long: `Stream events from the JSONL log in chronological order.

The log is the source of truth; the SQLite + LadybugDB derived stores
reproject from it via maz rebuild.

Examples:
  # compact one-liner per event with relative timestamps
  maz log --oneline

  # stream new events live as they arrive (Ctrl-C to stop)
  maz log --follow

  # last 50 project-scope events in the past week
  maz log --scope project --since 7d --limit 50

  # machine-readable JSON array of all episode events
  maz log --kind episode --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLog(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.since, "since", "", "lower bound on createTime: RFC 3339 timestamp or duration (7d, 2h, 90m)")
	cmd.Flags().StringVar(&flags.until, "until", "", "upper bound on createTime: RFC 3339 timestamp, or a duration meaning that long ago (e.g. 2h = up to 2h ago)")
	cmd.Flags().StringSliceVar(&flags.kinds, "kind", nil, "filter by event kind (repeatable, e.g. --kind episode --kind entity)")
	cmd.Flags().IntVar(&flags.limit, "limit", 100, "maximum events to print (0 = no limit)")
	cmd.Flags().BoolVar(&flags.oneline, "oneline", false, "compact one-liner per event: relative-time · kind · scope · author · summary")
	cmd.Flags().BoolVar(&flags.follow, "follow", false, "stream new events live as they arrive; poll every second (Ctrl-C to stop). Emits up to --limit existing events first, then tails new arrivals (uncapped)")
	cmd.Flags().StringArrayVar(&flags.scopes, "scope", nil, "filter by scope (repeatable: --scope project --scope personal)")
	cmd.Flags().StringArrayVar(&flags.authors, "author", nil, "filter by author principal UID (repeatable)")
	return cmd
}

// newEventsCmd wires `maz events tail` as a noun-first alias for `maz log`.
// Agents that grep for AIP-style resource verbs find it here; the
// implementation reuses runLog so behavior is identical.
func newEventsCmd() *cobra.Command {
	tail := newLogCmd()
	tail.Use = "tail"
	tail.Short = "Tail the JSONL event log (alias for `maz log`)"

	events := &cobra.Command{
		Use:   "events",
		Short: "Read the JSONL event log",
		Long:  "Noun-first alias group for the event log surface. `maz events tail` is equivalent to `maz log`.",
	}
	events.AddCommand(tail)
	return events
}

func runLog(cmd *cobra.Command, flags *logFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	opts, err := buildReadOpts(flags)
	if err != nil {
		return wrapLogFatal(cmd, err)
	}

	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return wrapLogFatal(cmd, fmt.Errorf("open eventlog: %w", err))
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("log: close eventlog", zap.Error(cerr))
		}
	}()

	return streamLogPlain(ctx, cmd, log, &opts, flags)
}

// streamLogPlain dispatches to a format-specific helper based on --output,
// --follow, and --oneline.  The individual helpers own all iteration and
// filtering logic so this function stays shallow.
func streamLogPlain(ctx context.Context, cmd *cobra.Command, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	ndjsonMode := wr.Format() == output.FormatNDJSON
	jsonArrayMode := wr.Format() == output.FormatJSON
	jsonMode := jsonArrayMode || ndjsonMode
	clr := logColorizer(cmd, w, jsonMode)

	if flags.follow {
		// A live tail emits events incrementally and can never close a bounded
		// JSON array, so --output json would silently degrade to NDJSON. Refuse
		// the combination and point at the streaming-native format rather than
		// violate the --output json contract.
		if jsonArrayMode {
			return output.Errorf(output.ExitUsage, output.CodeInvalidArgument,
				"--follow streams events incrementally and cannot emit a bounded JSON array; use --output ndjson for a live JSON feed, or drop --follow")
		}
		return streamLogFollow(ctx, cmd, wr, clr, log, opts, flags)
	}
	if jsonArrayMode {
		return emitLogJSONArray(ctx, cmd, wr, log, opts, flags)
	}
	if ndjsonMode {
		return streamLogNDJSON(ctx, cmd, w, clr, log, opts, flags)
	}
	if flags.oneline {
		return streamLogOnelineText(ctx, cmd, clr, log, opts, flags)
	}
	return streamLogFullText(ctx, cmd, clr, log, opts, flags)
}

// emitLogJSONArray buffers all matching events and emits them as a single JSON
// array.  An empty log yields "[]" so `| jq` never sees null.
func emitLogJSONArray(ctx context.Context, cmd *cobra.Command, wr output.Writer, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags) error {
	buf := make([]kernel.Event, 0)
	if err := forEachMatchedLogEvent(ctx, log, opts, flags, func(ev *kernel.Event) error {
		buf = append(buf, *ev)
		return nil
	}); err != nil {
		return wrapLogFatal(cmd, err)
	}
	if err := wr.Emit(buf); err != nil {
		return wrapLogFatal(cmd, err)
	}
	return wr.Flush()
}

// streamLogNDJSON writes one JSON object per event to w.
func streamLogNDJSON(ctx context.Context, cmd *cobra.Command, w io.Writer, clr *colorizer.Colorizer, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags) error {
	if err := forEachMatchedLogEvent(ctx, log, opts, flags, func(ev *kernel.Event) error {
		return writeLogLine(w, ev, true, clr)
	}); err != nil {
		return wrapLogFatal(cmd, err)
	}
	return nil
}

// streamLogOnelineText renders a compact table (TIME · KIND · SCOPE · AUTHOR ·
// SUMMARY) through MaybePage so large histories are paged on a TTY.
func streamLogOnelineText(ctx context.Context, cmd *cobra.Command, clr *colorizer.Colorizer, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags) error {
	return output.MaybePage(cmd, func(out io.Writer) error {
		textW := output.New(output.FormatText, out, cmd.ErrOrStderr())
		tbl := output.NewTable(textW, clr, logOnelineColumns())
		if err := forEachMatchedLogEvent(ctx, log, opts, flags, func(ev *kernel.Event) error {
			addLogOnelineRow(tbl, ev, clr)
			return nil
		}); err != nil {
			return err
		}
		return tbl.Render()
	})
}

// streamLogFullText renders the full log in the classic four-column layout
// (timestamp UID kind payload-preview) through MaybePage.
func streamLogFullText(ctx context.Context, cmd *cobra.Command, clr *colorizer.Colorizer, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags) error {
	return output.MaybePage(cmd, func(out io.Writer) error {
		return forEachMatchedLogEvent(ctx, log, opts, flags, func(ev *kernel.Event) error {
			return writeLogLine(out, ev, false, clr)
		})
	})
}

// streamLogFollow polls the event log for new events until the context is
// cancelled. It emits all existing matching events first, then polls every
// second for new arrivals. JSON/NDJSON modes use EmitStream (NDJSON) so the
// stream is agent-safe without buffering an unbounded array.
func streamLogFollow(ctx context.Context, cmd *cobra.Command, wr output.Writer, clr *colorizer.Colorizer, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags) error {
	w := cmd.OutOrStdout()
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON

	lastSeen, err := followEmitExisting(ctx, w, wr, clr, log, opts, flags, jsonMode)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			pollOpts := buildFollowPollOpts(opts, lastSeen)
			lastSeen, err = followPollTick(ctx, w, wr, clr, log, &pollOpts, flags, lastSeen, jsonMode)
			if err != nil {
				return err
			}
		}
	}
}

// followEmitExisting reads and emits all events that already match opts+flags.
// Returns the CreateTime of the last event seen, used to anchor subsequent polls.
func followEmitExisting(ctx context.Context, w io.Writer, wr output.Writer, clr *colorizer.Colorizer, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags, jsonMode bool) (time.Time, error) {
	var lastSeen time.Time
	if err := forEachMatchedLogEvent(ctx, log, opts, flags, func(ev *kernel.Event) error {
		if err := emitFollowEvent(w, wr, ev, jsonMode, flags.oneline, clr); err != nil {
			return err
		}
		if ev.CreateTime.After(lastSeen) {
			lastSeen = ev.CreateTime
		}
		return nil
	}); err != nil {
		return lastSeen, wrapLogFatalErr("follow", err)
	}
	return lastSeen, nil
}

// followPollTick performs a single poll iteration, reading events newer than
// lastSeen and emitting each. Returns the updated lastSeen timestamp.
func followPollTick(ctx context.Context, w io.Writer, wr output.Writer, clr *colorizer.Colorizer, log kernel.EventLog, pollOpts *kernel.ReadOpts, flags *logFlags, lastSeen time.Time, jsonMode bool) (time.Time, error) {
	for ev, ferr := range log.Read(ctx, *pollOpts) {
		if ferr != nil {
			return lastSeen, wrapLogFatalErr("follow poll", ferr)
		}
		if !matchesLogFilter(&ev, flags) {
			continue
		}
		if err := emitFollowEvent(w, wr, &ev, jsonMode, flags.oneline, clr); err != nil {
			return lastSeen, wrapLogFatalErr("follow emit", err)
		}
		if ev.CreateTime.After(lastSeen) {
			lastSeen = ev.CreateTime
		}
	}
	return lastSeen, nil
}

// buildFollowPollOpts builds the ReadOpts for a single follow poll tick,
// advancing Since to one nanosecond past the last seen event.
func buildFollowPollOpts(base *kernel.ReadOpts, lastSeen time.Time) kernel.ReadOpts {
	pollOpts := *base
	if !lastSeen.IsZero() {
		pollOpts.Since = lastSeen.Add(time.Nanosecond)
	}
	pollOpts.Limit = 0 // no cap on tail: consume all new arrivals
	return pollOpts
}

// emitFollowEvent writes one event in the follow stream. JSON mode uses
// EmitStream (promotes to NDJSON); text modes write inline without buffering.
func emitFollowEvent(w io.Writer, wr output.Writer, ev *kernel.Event, jsonMode, oneline bool, clr *colorizer.Colorizer) error {
	if jsonMode {
		return wr.EmitStream(ev)
	}
	if oneline {
		_, err := fmt.Fprintln(w, formatLogOnelineEvent(ev, clr))
		return err
	}
	return writeLogLine(w, ev, false, clr)
}

// matchesLogFilter reports whether ev passes the --scope and --author filters.
// Empty filter slices mean "accept all events".
func matchesLogFilter(ev *kernel.Event, flags *logFlags) bool {
	if len(flags.scopes) > 0 && !slices.Contains(flags.scopes, string(ev.Scope)) {
		return false
	}
	if len(flags.authors) > 0 && !slices.Contains(flags.authors, ev.Author.PrincipalUID) {
		return false
	}
	return true
}

// postFilterActive reports whether --scope or --author is in effect. The kernel
// has no scope/author predicate, so those filters run in-process after the
// read (matchesLogFilter). When active, buildReadOpts clears the kernel-level
// Limit so the cap counts matched — not raw — events (see forEachMatchedLogEvent).
func (f *logFlags) postFilterActive() bool {
	return len(f.scopes) > 0 || len(f.authors) > 0
}

// forEachMatchedLogEvent reads events through opts, applies the --scope/--author
// post-filter, and invokes fn for each match. It stops after flags.limit matches
// (flags.limit == 0 means no cap). Because the kernel applies opts.Limit to the
// RAW stream — before matchesLogFilter runs — buildReadOpts clears opts.Limit
// whenever a post-filter is active; the cap is then enforced here on matched
// events. This prevents the silent under-delivery where, say, a log of 100
// personal events followed by 10 project events answers `--scope project
// --limit 50` with an empty list (the kernel stopped after 50 unmatched events).
func forEachMatchedLogEvent(ctx context.Context, log kernel.EventLog, opts *kernel.ReadOpts, flags *logFlags, fn func(*kernel.Event) error) error {
	matched := 0
	for ev, ferr := range log.Read(ctx, *opts) {
		if ferr != nil {
			return fmt.Errorf("read: %w", ferr)
		}
		if !matchesLogFilter(&ev, flags) {
			continue
		}
		if err := fn(&ev); err != nil {
			return err
		}
		matched++
		if flags.limit > 0 && matched >= flags.limit {
			return nil
		}
	}
	return nil
}

// logOnelineColumns returns the column definitions for --oneline table rendering.
func logOnelineColumns() []output.Column {
	return []output.Column{
		{Title: "TIME", MaxWidth: 12},
		{Title: "KIND", MaxWidth: 22},
		{Title: "SCOPE", MaxWidth: 10},
		{Title: "AUTHOR", MaxWidth: 28, Truncate: true},
		{Title: "SUMMARY", MaxWidth: 64, Truncate: true},
	}
}

// addLogOnelineRow appends one event as a compact oneline row to the table.
func addLogOnelineRow(tbl *output.Table, ev *kernel.Event, clr *colorizer.Colorizer) {
	ts := clr.Meta(RelativeTime(ev.CreateTime))
	kind := logVerbColor(clr, ev.Kind)(string(ev.Kind))
	scope := clr.Dim(string(ev.Scope))
	author := logAuthorDisplay(clr, ev.Author)
	summary := logEventSummary(ev)
	tbl.Row(ts, kind, scope, author, summary)
}

// formatLogOnelineEvent formats one event as a compact single line for
// streaming (--follow --oneline). It uses a middle-dot separator rather than
// table alignment so output is legible as a live stream.
func formatLogOnelineEvent(ev *kernel.Event, clr *colorizer.Colorizer) string {
	ts := clr.Meta(RelativeTime(ev.CreateTime))
	kind := logVerbColor(clr, ev.Kind)(string(ev.Kind))
	scope := clr.Dim(string(ev.Scope))
	author := logAuthorDisplay(clr, ev.Author)
	summary := logEventSummary(ev)
	return ts + " · " + kind + " · " + scope + " · " + author + " · " + summary
}

// logVerbColor returns the colorizer method that expresses the semantic
// meaning of the event kind. Extracted predicates (logIsAddedKind /
// logIsRemovedKind) keep this function's cyclomatic complexity under the
// project threshold.
//
// Removed takes priority over added: the "relation" prefix in logIsAddedKind
// also matches "relationRetracted", so a retraction must be tested as a removal
// first or it would be painted green (added) instead of red (removed).
func logVerbColor(clr *colorizer.Colorizer, kind kernel.EventKind) func(string) string {
	k := strings.ToLower(string(kind))
	switch {
	case logIsRemovedKind(k):
		return clr.Removed
	case logIsAddedKind(k):
		return clr.Added
	case strings.HasPrefix(k, "resolve") || k == "recall":
		return clr.Accent
	case k == diffKindEpisode:
		return clr.Dim
	default:
		return func(s string) string { return s }
	}
}

// logIsAddedKind reports whether the lowercased kind string belongs to the
// "new information recorded" family (assert / entity / fact / relate /
// relation / mention).
func logIsAddedKind(k string) bool {
	return strings.HasPrefix(k, "assert") ||
		k == "fact" || k == "relate" || k == "mention" ||
		strings.HasPrefix(k, "entity") || strings.HasPrefix(k, "relation")
}

// logIsRemovedKind reports whether k belongs to the "information removed"
// family (retract / forget / relationRetracted / episodeForgotten).
func logIsRemovedKind(k string) bool {
	return strings.HasPrefix(k, "retract") ||
		strings.HasPrefix(k, "forget") ||
		k == "relationretracted" ||
		strings.HasPrefix(k, "episodeforgotten")
}

// logAuthorDisplay formats the EventAuthor block for a compact oneline column.
// The local-sentinel principal ("local") marks unsigned/unverified events with
// the warning glyph ‼; all other principals are marked ✓ as signed.
// The principal UID is abbreviated to 12 characters to keep the column narrow.
func logAuthorDisplay(clr *colorizer.Colorizer, author kernel.EventAuthor) string {
	const localSentinel = "local"
	uid := author.PrincipalUID
	// Events predating the EventAuthor field carry an empty PrincipalUID; treat
	// them like the local sentinel (unsigned/unverified) so they render
	// "‼ local" rather than a misleading "✓ " with an empty label. Mirrors
	// computeDigestStats, which normalises "" → "local" for the same reason.
	if uid == "" {
		uid = localSentinel
	}
	abbrev := uid
	if len([]rune(abbrev)) > 12 {
		abbrev = string([]rune(abbrev)[:12])
	}
	if uid == localSentinel {
		return clr.Warn("‼ " + abbrev)
	}
	return clr.Success("✓ " + abbrev)
}

// logEventSummary extracts a short plain-text preview from the event payload.
// It prefers the "text" field when present (episode-family events carry one);
// otherwise it falls back to the raw JSON payload trimmed at 80 visible runes.
func logEventSummary(ev *kernel.Event) string {
	if len(ev.Payload) > 0 {
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err == nil {
			if txt, ok := m["text"].(string); ok && txt != "" {
				return truncateLogSummary(strings.ReplaceAll(strings.TrimSpace(txt), "\n", " "))
			}
		}
	}
	raw := strings.ReplaceAll(strings.TrimSpace(string(ev.Payload)), "\n", " ")
	return truncateLogSummary(raw)
}

// truncateLogSummary shortens s to at most 80 visible runes, appending an
// ellipsis when truncated.
func truncateLogSummary(s string) string {
	runes := []rune(s)
	if len(runes) <= 80 {
		return s
	}
	return string(runes[:79]) + "…"
}

// writeLogLine writes one event in either JSON Lines or plain table form.
// In plain mode: timestamp is dim, UID is amber (Ident), kind is cyan (Meta).
// Pre-padding is applied before colorizing to keep column alignment intact.
func writeLogLine(w io.Writer, ev *kernel.Event, jsonMode bool, clr *colorizer.Colorizer) error {
	if jsonMode {
		line, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
		}
		_, err = fmt.Fprintln(w, string(line))
		return err
	}
	preview := strings.TrimSpace(string(ev.Payload))
	if len(preview) > 160 {
		preview = preview[:160] + "..."
	}
	tsStr := ev.CreateTime.UTC().Format(time.RFC3339)
	uidStr := fmt.Sprintf("%-26s", ev.UID)
	kindStr := fmt.Sprintf("%-22s", string(ev.Kind))
	_, err := fmt.Fprintf(w, "%s  %s  %s  %s\n",
		clr.Dim(tsStr), clr.Ident(uidStr), clr.Meta(kindStr), preview)
	return err
}

// rfc3339Example is the canonical example shown in time-parse error messages.
// It uses the Z suffix (UTC) to keep the hint short and unambiguous.
const rfc3339Example = "2006-01-02T15:04:05Z"

func buildReadOpts(flags *logFlags) (kernel.ReadOpts, error) {
	opts := kernel.ReadOpts{Limit: flags.limit}
	if flags.since != "" {
		t, err := ParseSince(flags.since)
		if err != nil {
			return kernel.ReadOpts{}, fmt.Errorf("--since: %w (example: %s)", err, rfc3339Example)
		}
		opts.Since = t.UTC()
	}
	if flags.until != "" {
		t, err := ParseSince(flags.until)
		if err != nil {
			return kernel.ReadOpts{}, fmt.Errorf("--until: %w (example: %s)", err, rfc3339Example)
		}
		opts.Until = t.UTC()
	}
	for _, k := range flags.kinds {
		opts.Kinds = append(opts.Kinds, kernel.EventKind(k))
	}
	if flags.postFilterActive() {
		// Read uncapped from the kernel; forEachMatchedLogEvent enforces
		// flags.limit on matched events so a scoped query is not silently
		// truncated to fewer than --limit results.
		opts.Limit = 0
	}
	return opts, nil
}

// wrapLogFatal routes an unexpected failure through the shared respondFatal
// path with the "log" command prefix.
func wrapLogFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "log", err)
}

// wrapLogFatalErr wraps an error with a prefix for follow-mode error paths
// that do not have direct access to the cobra.Command.
func wrapLogFatalErr(prefix string, err error) error {
	return fmt.Errorf("log %s: %w", prefix, err)
}
