package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// Timeline event kinds — the chronological history of one entity's
// relationships. Each kind maps to a distinct glyph in the text rendering and
// is a stable string in the JSON / NDJSON shape.
//
// The four end-states are derived from the bi-temporal navigator's FOLDED
// relation row (see internal/kernel/navigate appendRelationFilter +
// ontology.ReduceRetractions); timeline re-uses that fold rather than
// re-implementing any time logic:
//   - supersededByUid set                     -> superseded (a newer edge replaced it)
//   - validUntil set, retracted=true          -> retracted  (tombstone close via unrelate / resolver)
//   - validUntil set, retracted=false         -> closed     (born-bounded: valid_until set at creation)
//   - neither validUntil nor superseder       -> still open (only an `opened` event)
const (
	timelineKindOpened     = "opened"
	timelineKindClosed     = "closed"
	timelineKindSuperseded = "superseded"
	timelineKindRetracted  = "retracted"
)

// Glyphs for the text rendering, one per end-state, kept visually distinct so a
// human scanning a timeline tells an opening from each kind of close at a glance.
const (
	timelineGlyphOpened     = "+"
	timelineGlyphClosed     = "⊗" // ⊗ born-bounded close (valid_until set at creation)
	timelineGlyphSuperseded = "⟳" // ⟳ superseded by a newer edge
	timelineGlyphRetracted  = "⊘" // ⊘ tombstone close (relationRetracted event; explicit retraction)
)

// Edge directions and the relations collection segment, mirroring the wire
// vocabulary the relations tool emits (internal/mcp/tools): "outgoing" when the
// queried entity is the subject, "incoming" when it is the object.
const (
	timelineDirectionOutgoing = "outgoing"
	timelineDirectionIncoming = "incoming"
	timelineRelationsPrefix   = "relations/"
)

// timelineEvent is one entry in an entity's relationship history: a relation
// edge either opening or ending, anchored at the instant it occurred. The shape
// is stable across --output text|json|ndjson.
type timelineEvent struct {
	// Time is the RFC 3339 instant the event occurred: an `opened` event sits at
	// the edge's valid_from (falling back to its create_time when the extractor
	// set no validity start); an end event sits at the edge's valid_until.
	Time string `json:"time"`
	// Kind is one of opened|closed|superseded.
	Kind string `json:"kind"`
	// Predicate is the relation's typed predicate, e.g. works_at.
	Predicate string `json:"predicate"`
	// Relation is the relations/{uid} resource name of the edge this event is about.
	Relation string `json:"relation"`
	// Direction is the edge orientation relative to the queried entity:
	// "outgoing" when it is the subject, "incoming" when it is the object.
	Direction string `json:"direction,omitempty"`
	// Subject / Object carry the full edge so the JSON shape is self-contained.
	Subject     string `json:"subject"`
	SubjectName string `json:"subjectName,omitempty"`
	Object      string `json:"object"`
	ObjectName  string `json:"objectName,omitempty"`
	// SupersededBy is the relations/{uid} of the replacing edge; set only on a
	// superseded event (omitted otherwise).
	SupersededBy string `json:"supersededBy,omitempty"`
}

// timeline is the stable JSON envelope for an entity's relationship history:
// the resolved anchor entity plus its events in chronological order.
type timeline struct {
	Entity string          `json:"entity"`
	Events []timelineEvent `json:"events"`
}

// counterparty returns the display label of the OTHER endpoint relative to the
// queried entity (the object on an outgoing edge, the subject on an incoming
// one), preferring the hydrated display name and falling back to the resource
// name when the name is out of scope.
func (e *timelineEvent) counterparty() string {
	if e.Direction == timelineDirectionIncoming {
		return firstNonEmpty(e.SubjectName, e.Subject)
	}
	return firstNonEmpty(e.ObjectName, e.Object)
}

func newTimelineCmd() *cobra.Command {
	var predicates []string
	var scopes []string
	var asOf string
	var limit int
	cmd := &cobra.Command{
		Use:   "timeline <entity>",
		Short: "Chronological history of an entity's temporal relation states (LLM-free)",
		Long: `Print the chronological history of one entity's RELATIONSHIPS: when each
typed edge opened and, if it is no longer live, how it ended. Unlike "maz
relations" (which shows the current slice by default), timeline always walks the
FULL history via the bi-temporal navigator's include-inactive read, so closed,
superseded, and time-bounded edges all appear in the order they happened.

TEMPORAL STATE vs TRUST STATUS — timeline shows TEMPORAL state only:

  Temporal state events shown here:
    + opened     the edge came into being (at its valid_from / create_time)
    ⊗ closed     the edge expired at a born-bounded valid_until (set at creation)
    ⊘ retracted  the edge was closed by an explicit tombstone (unrelate / resolver)
    ⟳ superseded a newer edge replaced it (carries supersededBy)

  Trust status is a separate axis (NOT shown here):
    proposed    extracted by a connector, awaiting human verification
    asserted    verified by a human (or authored directly)
  To see the trust queue use 'maz review proposed'; to verify an edge use
  'maz promote relations/{uid}'. Trust status and temporal state are independent.

The entity is an entities/{uid} resource name, a bare uid, or a display name
(resolved like "maz get" / "maz relations"; an unknown or ambiguous name returns
a pointed error naming the candidates).

--predicate restricts the history to one relation type; --as-of bounds it to
what had been recorded by an instant (transaction time).

JSON shape (--output json): {"entity": "entities/{uid}", "events": [ {time, kind,
predicate, relation, direction, subject, subjectName, object, objectName,
supersededBy?}, ... ]}. NDJSON emits one event object per line.

Examples:
  maz timeline "Mira Stonebrook"
  maz timeline entities/01ARZ3NDEKTSV4RRFFQ69G5FAV --predicate works_at
  maz timeline "Mira Stonebrook" --output json
  maz timeline 01ARZ3... --as-of 2025-01-01T00:00:00Z --output ndjson`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTimeline(cmd, args[0], predicates, scopes, asOf, limit)
		},
	}
	cmd.Flags().StringArrayVar(&predicates, "predicate", nil,
		"restrict to this predicate (repeatable), e.g. --predicate works_at")
	cmd.Flags().StringVar(&asOf, "as-of", "",
		"transaction-time anchor (RFC 3339 UTC); show history as it had been recorded by this instant")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum relations to scan for history")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	return cmd
}

// runTimeline fetches the entity's full relation history via the relations tool
// and renders it chronologically. Factored out of the RunE closure to keep the
// command constructor's cognitive complexity under the gate.
func runTimeline(cmd *cobra.Command, entity string, predicates, scopes []string, asOf string, limit int) error {
	if err := validateScopeFlag(cmd, "timeline", scopes); err != nil {
		return err
	}
	reqBody, err := json.Marshal(timelineInput(entity, predicates, scopes, asOf, limit))
	if err != nil {
		return wrapNavFatal(cmd, "timeline", err)
	}
	// timeline is a thin chronological re-projection of the relations tool; it
	// reuses the navigator's fold rather than touching the event log.
	raw, err := invokeNavTool(cmd, "relations", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "timeline", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr on the unavailable path
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}
	tl, err := buildTimeline(entity, raw)
	if err != nil {
		return respondFatal(cmd, "timeline", err)
	}
	return renderTimeline(cmd, tl, limit)
}

// timelineInput projects the flags into the relations tool-input map. Timeline
// ALWAYS reads full history: includeInactive bypasses the valid-window +
// superseded gates so ended edges are returned. It does NOT bypass asOf
// (transaction time stays a hard recording bound), so --as-of composes cleanly.
// --at is deliberately not exposed: a valid-time anchor is ignored under
// includeInactive and would be a no-op flag.
func timelineInput(entity string, predicates, scopes []string, asOf string, limit int) map[string]any {
	in := map[string]any{"entity": entity, "includeInactive": true}
	if len(predicates) > 0 {
		in["predicates"] = predicates
	}
	if len(scopes) > 0 {
		in["scopes"] = scopes
	}
	if asOf != "" {
		in["asOf"] = asOf
	}
	if limit > 0 {
		in["limit"] = limit
	}
	return in
}

// renderTimeline dispatches the built history to the format-appropriate renderer.
func renderTimeline(cmd *cobra.Command, tl timeline, limit int) error {
	switch output.From(cmd).Format() {
	case output.FormatJSON:
		return renderTimelineJSON(cmd, tl)
	case output.FormatNDJSON:
		return renderTimelineNDJSON(cmd, tl)
	default:
		return renderTimelineCLI(cmd, tl, limit)
	}
}

// timelineRelation is the subset of the relations-tool wire shape timeline
// consumes. The temporal fields (validFrom/validUntil/supersededByUid) are the
// already-folded canonical values the navigator materializes, so timeline does
// no time arithmetic of its own. Retracted mirrors the navigator's Relation.Retracted
// field: true when a relationRetracted tombstone closed the edge (unrelate or
// resolver supersession), false when any valid_until was set at creation.
type timelineRelation struct {
	Name            string `json:"name"`
	Subject         string `json:"subject"`
	Object          string `json:"object"`
	Direction       string `json:"direction"`
	SubjectName     string `json:"subjectName"`
	ObjectName      string `json:"objectName"`
	Predicate       string `json:"predicate"`
	CreateTime      string `json:"createTime"`
	ValidFrom       string `json:"validFrom"`
	ValidUntil      string `json:"validUntil"`
	SupersededByUID string `json:"supersededByUid"`
	Retracted       bool   `json:"retracted,omitempty"`
}

// buildTimeline turns the relations tool's response into a chronologically
// ordered event list. Arg is the entity argument as typed; the returned
// timeline.Entity is the resolved anchor resource name when derivable from the
// rows (every row of an entity-anchored listing shares one anchor), else arg.
func buildTimeline(arg string, raw json.RawMessage) (timeline, error) {
	var resp struct {
		Relations []timelineRelation `json:"relations"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return timeline{}, fmt.Errorf("parse relations: %w", err)
	}
	events := make([]timelineEvent, 0, len(resp.Relations)*2)
	anchor := ""
	for i := range resp.Relations {
		r := &resp.Relations[i]
		if anchor == "" {
			anchor = anchorFromRelation(r)
		}
		events = append(events, relationTimelineEvents(r)...)
	}
	sortTimelineEvents(events)
	entity := arg
	if anchor != "" {
		entity = anchor
	}
	return timeline{Entity: entity, Events: events}, nil
}

// anchorFromRelation returns the queried entity's resource name for a row: the
// subject on an outgoing edge, the object on an incoming one. Empty when the
// direction is absent (not expected from an entity-anchored listing).
func anchorFromRelation(r *timelineRelation) string {
	switch r.Direction {
	case timelineDirectionOutgoing:
		return r.Subject
	case timelineDirectionIncoming:
		return r.Object
	default:
		return ""
	}
}

// relationTimelineEvents expands one relation row into its events: always an
// `opened`, plus exactly one end event when the edge is no longer live. A
// superseded edge emits a single `superseded` event (NOT also a `closed`): the
// supersession IS its end, even though the fold also stamps a valid_until.
func relationTimelineEvents(r *timelineRelation) []timelineEvent {
	base := timelineEvent{
		Predicate:   r.Predicate,
		Relation:    r.Name,
		Direction:   r.Direction,
		Subject:     r.Subject,
		SubjectName: r.SubjectName,
		Object:      r.Object,
		ObjectName:  r.ObjectName,
	}
	opened := base
	opened.Kind = timelineKindOpened
	opened.Time = firstNonEmpty(r.ValidFrom, r.CreateTime)
	out := []timelineEvent{opened}

	switch {
	case r.SupersededByUID != "":
		end := base
		end.Kind = timelineKindSuperseded
		end.Time = firstNonEmpty(r.ValidUntil, r.CreateTime)
		end.SupersededBy = timelineRelationsPrefix + r.SupersededByUID
		out = append(out, end)
	case r.ValidUntil != "" && r.Retracted:
		// A tombstone-closed edge: at least one relationRetracted event targeted
		// it (via unrelate or resolver). Rendered as ⊘ retracted, distinct from
		// a born-bounded edge whose valid_until was set at creation.
		end := base
		end.Kind = timelineKindRetracted
		end.Time = r.ValidUntil
		out = append(out, end)
	case r.ValidUntil != "":
		// Born-bounded: valid_until set at creation, no tombstone. The edge
		// simply expired on schedule with no explicit retraction event.
		end := base
		end.Kind = timelineKindClosed
		end.Time = r.ValidUntil
		out = append(out, end)
	}
	return out
}

// sortTimelineEvents orders events oldest-first by their RFC 3339 instant, with
// a deterministic tie-break (relation uid, then kind rank so an `opened` sorts
// before its end at a shared instant) for a stable, diffable rendering.
func sortTimelineEvents(events []timelineEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		ti, tj := parseTimelineTime(events[i].Time), parseTimelineTime(events[j].Time)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if events[i].Relation != events[j].Relation {
			return events[i].Relation < events[j].Relation
		}
		return timelineKindRank(events[i].Kind) < timelineKindRank(events[j].Kind)
	})
}

// parseTimelineTime parses an RFC 3339 instant for ordering; an empty or
// malformed value sorts to the zero time (earliest) rather than failing the
// whole listing.
func parseTimelineTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// timelineKindRank orders the kinds at a shared instant: an opening precedes any
// end so a born-closed edge (valid_from == valid_until) reads open-then-close.
func timelineKindRank(kind string) int {
	switch kind {
	case timelineKindOpened:
		return 0
	default:
		return 1
	}
}

// timelineGlyph returns the distinct text-mode marker for an event kind.
func timelineGlyph(kind string) string {
	switch kind {
	case timelineKindOpened:
		return timelineGlyphOpened
	case timelineKindClosed:
		return timelineGlyphClosed
	case timelineKindSuperseded:
		return timelineGlyphSuperseded
	case timelineKindRetracted:
		return timelineGlyphRetracted
	default:
		return "?"
	}
}

// renderTimelineJSON emits the full {entity, events} envelope as one indented
// JSON document, matching the framework's 2-space indent.
func renderTimelineJSON(cmd *cobra.Command, tl timeline) error {
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return fmt.Errorf("timeline: marshal json: %w", err)
	}
	_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(b))
	return werr
}

// renderTimelineNDJSON emits one JSON object per event to cmd's stdout, matching
// the --output ndjson contract used by neighbors/relations/entities.
func renderTimelineNDJSON(cmd *cobra.Command, tl timeline) error {
	w := cmd.OutOrStdout()
	for i := range tl.Events {
		b, err := json.Marshal(tl.Events[i])
		if err != nil {
			return fmt.Errorf("timeline: marshal ndjson event: %w", err)
		}
		if _, werr := fmt.Fprintln(w, string(b)); werr != nil {
			return werr
		}
	}
	return nil
}

// renderTimelineCLI prints the history as an aligned, glyph-marked table,
// oldest-first.
func renderTimelineCLI(cmd *cobra.Command, tl timeline, limit int) error {
	w := cmd.OutOrStdout()
	if len(tl.Events) == 0 {
		_, err := fmt.Fprintln(w, "no relationship history")
		return err
	}
	c := cmdColorizer(cmd)
	wr := output.From(cmd)
	if perr := output.MaybePage(cmd, func(pw io.Writer) error {
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, []output.Column{
			{Title: "when"},
			{Title: "event"},
			{Title: "predicate"},
			{Title: "dir"},
			{Title: "with", MaxWidth: 30, Truncate: true},
		})
		for i := range tl.Events {
			ev := &tl.Events[i]
			event := timelineGlyph(ev.Kind) + " " + ev.Kind
			tbl.Row(ev.Time, event, ev.Predicate, ev.Direction, ev.counterparty())
		}
		return tbl.Render()
	}); perr != nil {
		return perr
	}
	// --limit caps RELATIONS scanned, not events (each edge yields up to two
	// events), so the truncation footer must count relations — exactly the number
	// of `opened` events (one per edge) — or it would falsely warn "limit reached"
	// when a sub-limit edge set merely produced more than `limit` events.
	output.ShowingNofM(wr, c, countOpenedEvents(tl.Events), 0, limit, "raise --limit")
	return nil
}

// countOpenedEvents returns the number of distinct relations in the timeline:
// relationTimelineEvents emits exactly one `opened` event per edge, so the
// opened-event count is the relation count the --limit cap applies to.
func countOpenedEvents(events []timelineEvent) int {
	n := 0
	for i := range events {
		if events[i].Kind == timelineKindOpened {
			n++
		}
	}
	return n
}
