package cli

// timeline_test.go — tests for `maz timeline`, the chronological relationship
// history command. They pin the cli-developer two-mode contract:
//
//   1. --help is non-empty and teaches (carries Examples + the glyph legend).
//   2. CLI (text) mode produces stable, glyph-marked output.
//   3. --output json / ndjson produce parseable, stable JSON.
//   4. Exit codes match the documented taxonomy (0 ok, 1 expected failure).
//
// The history-building + ordering + end-state classification is exercised
// hermetically against synthetic relations-tool JSON (no backend, always runs);
// the open/close/open acceptance scenario is exercised end-to-end against a live
// in-process backend (CI only, skipped under -short like the other e2e tests).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ---- hermetic: history building, ordering, classification ----

// relationsJSON returns the relations-tool response shape timeline consumes,
// with the temporal fields already folded the way the navigator materializes
// them. Each row is a raw JSON object string.
func relationsJSON(rows ...string) json.RawMessage {
	return json.RawMessage(`{"relations":[` + strings.Join(rows, ",") + `]}`)
}

// TestBuildTimelineChronologicalOrder pins the acceptance scenario's shape:
// open works_at Y, close it, open works_at Z renders opened/closed/opened in
// time order with the right kinds.
func TestBuildTimelineChronologicalOrder(t *testing.T) {
	t.Parallel()
	raw := relationsJSON(
		// works_at Z, opened latest — listed first (relations come create_time DESC).
		`{"name":"relations/RZ","subject":"entities/X","object":"entities/Z","direction":"outgoing","subjectName":"Mira","objectName":"Gondor","predicate":"works_at","createTime":"2025-03-01T00:00:00Z","validFrom":"2025-03-01T00:00:00Z"}`,
		// works_at Y, opened first then closed.
		`{"name":"relations/RY","subject":"entities/X","object":"entities/Y","direction":"outgoing","subjectName":"Mira","objectName":"Mithril","predicate":"works_at","createTime":"2025-01-01T00:00:00Z","validFrom":"2025-01-01T00:00:00Z","validUntil":"2025-02-01T00:00:00Z"}`,
	)
	tl, err := buildTimeline("Mira", raw)
	if err != nil {
		t.Fatalf("buildTimeline: %v", err)
	}
	if tl.Entity != "entities/X" {
		t.Errorf("entity = %q, want resolved anchor entities/X", tl.Entity)
	}
	gotKinds := make([]string, len(tl.Events))
	for i, ev := range tl.Events {
		gotKinds[i] = ev.Kind
	}
	wantKinds := []string{timelineKindOpened, timelineKindClosed, timelineKindOpened}
	if strings.Join(gotKinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("kinds in time order = %v, want %v", gotKinds, wantKinds)
	}
	// The events must be non-decreasing in time.
	for i := 1; i < len(tl.Events); i++ {
		if tl.Events[i].Time < tl.Events[i-1].Time {
			t.Errorf("event %d time %q precedes event %d time %q (not chronological)",
				i, tl.Events[i].Time, i-1, tl.Events[i-1].Time)
		}
	}
	// The closed event is the works_at Y edge.
	if tl.Events[1].Relation != "relations/RY" || tl.Events[1].Predicate != "works_at" {
		t.Errorf("closed event = %+v, want the relations/RY works_at edge", tl.Events[1])
	}
}

// TestBuildTimelineDistinctEndMarkers verifies the three end-states render with
// DISTINCT kinds/glyphs and are NOT collapsed: a temporal close (valid_until,
// no superseder), a supersession (superseded_by_uid set), and a still-open edge.
func TestBuildTimelineDistinctEndMarkers(t *testing.T) {
	t.Parallel()
	raw := relationsJSON(
		// closed: valid_until set, no superseder.
		`{"name":"relations/RC","subject":"entities/X","object":"entities/Y","direction":"outgoing","subjectName":"Mira","objectName":"Mithril","predicate":"works_at","createTime":"2025-01-01T00:00:00Z","validFrom":"2025-01-01T00:00:00Z","validUntil":"2025-06-01T00:00:00Z"}`,
		// superseded: superseded_by_uid set (the fold also stamps valid_until).
		`{"name":"relations/RS","subject":"entities/X","object":"entities/W","direction":"outgoing","subjectName":"Mira","objectName":"Rohan","predicate":"reports_to","createTime":"2025-02-01T00:00:00Z","validFrom":"2025-02-01T00:00:00Z","validUntil":"2025-07-01T00:00:00Z","supersededByUid":"01SUPERSEDER0000000000000Z"}`,
		// open: no end at all.
		`{"name":"relations/RO","subject":"entities/X","object":"entities/V","direction":"outgoing","subjectName":"Mira","objectName":"Arnor","predicate":"works_at","createTime":"2025-03-01T00:00:00Z","validFrom":"2025-03-01T00:00:00Z"}`,
	)
	tl, err := buildTimeline("entities/X", raw)
	if err != nil {
		t.Fatalf("buildTimeline: %v", err)
	}
	// 3 edges: 3 opened + 1 closed + 1 superseded = 5 events.
	if len(tl.Events) != 5 {
		t.Fatalf("event count = %d, want 5\nevents=%+v", len(tl.Events), tl.Events)
	}
	var closed, superseded *timelineEvent
	for i := range tl.Events {
		switch tl.Events[i].Kind {
		case timelineKindClosed:
			closed = &tl.Events[i]
		case timelineKindSuperseded:
			superseded = &tl.Events[i]
		}
	}
	if closed == nil || superseded == nil {
		t.Fatalf("want both a closed and a superseded event; got %+v", tl.Events)
	}
	// Distinct glyphs — the core "do not collapse the end kinds" guarantee.
	if timelineGlyph(closed.Kind) == timelineGlyph(superseded.Kind) {
		t.Errorf("closed and superseded share glyph %q; must be distinct", timelineGlyph(closed.Kind))
	}
	if timelineGlyph(closed.Kind) != timelineGlyphClosed {
		t.Errorf("closed glyph = %q, want %q", timelineGlyph(closed.Kind), timelineGlyphClosed)
	}
	if timelineGlyph(superseded.Kind) != timelineGlyphSuperseded {
		t.Errorf("superseded glyph = %q, want %q", timelineGlyph(superseded.Kind), timelineGlyphSuperseded)
	}
	// A superseded event carries the replacing edge; a closed one does not.
	if superseded.SupersededBy != "relations/01SUPERSEDER0000000000000Z" {
		t.Errorf("superseded.SupersededBy = %q, want relations/01SUPERSEDER0000000000000Z", superseded.SupersededBy)
	}
	if closed.SupersededBy != "" {
		t.Errorf("closed.SupersededBy = %q, want empty", closed.SupersededBy)
	}
}

// TestBuildTimelineRetractedDistinctFromClosedAndSuperseded pins the three-way
// distinctness contract: a retracted edge (retracted=true, no superseder), a
// superseded edge (supersededByUid set), and a born-bounded closed edge
// (validUntil set, retracted=false) each produce a DISTINCT kind and glyph.
// This is the core regression guard for the feature gap closed in this commit.
func TestBuildTimelineRetractedDistinctFromClosedAndSuperseded(t *testing.T) {
	t.Parallel()
	raw := relationsJSON(
		// retracted: validUntil set AND retracted=true (tombstone from unrelate/resolver).
		`{"name":"relations/RR","subject":"entities/X","object":"entities/A","direction":"outgoing","subjectName":"Mira","objectName":"Amon","predicate":"works_at","createTime":"2025-01-01T00:00:00Z","validFrom":"2025-01-01T00:00:00Z","validUntil":"2025-04-01T00:00:00Z","retracted":true}`,
		// closed (born-bounded): validUntil set but retracted=false (no tombstone; set at creation).
		`{"name":"relations/RC","subject":"entities/X","object":"entities/B","direction":"outgoing","subjectName":"Mira","objectName":"Bree","predicate":"works_at","createTime":"2025-02-01T00:00:00Z","validFrom":"2025-02-01T00:00:00Z","validUntil":"2025-05-01T00:00:00Z"}`,
		// superseded: supersededByUid set (fold also stamps validUntil).
		`{"name":"relations/RS","subject":"entities/X","object":"entities/C","direction":"outgoing","subjectName":"Mira","objectName":"Carn Dum","predicate":"reports_to","createTime":"2025-03-01T00:00:00Z","validFrom":"2025-03-01T00:00:00Z","validUntil":"2025-06-01T00:00:00Z","supersededByUid":"01SUPERSEDER0000000000000Z"}`,
	)
	tl, err := buildTimeline("entities/X", raw)
	if err != nil {
		t.Fatalf("buildTimeline: %v", err)
	}
	// 3 edges: 3 opened + 1 retracted + 1 closed + 1 superseded = 6 events.
	if len(tl.Events) != 6 {
		t.Fatalf("event count = %d, want 6\nevents=%+v", len(tl.Events), tl.Events)
	}
	var retracted, closed, superseded *timelineEvent
	for i := range tl.Events {
		switch tl.Events[i].Kind {
		case timelineKindRetracted:
			retracted = &tl.Events[i]
		case timelineKindClosed:
			closed = &tl.Events[i]
		case timelineKindSuperseded:
			superseded = &tl.Events[i]
		}
	}
	if retracted == nil {
		t.Fatal("no retracted event found; want one for the tombstone-closed edge")
	}
	if closed == nil {
		t.Fatal("no closed event found; want one for the born-bounded edge")
	}
	if superseded == nil {
		t.Fatal("no superseded event found; want one for the replaced edge")
	}
	// All three glyphs must be distinct from each other.
	glyths := map[string]string{
		"retracted":  timelineGlyph(retracted.Kind),
		"closed":     timelineGlyph(closed.Kind),
		"superseded": timelineGlyph(superseded.Kind),
	}
	for ka, ga := range glyths {
		for kb, gb := range glyths {
			if ka != kb && ga == gb {
				t.Errorf("kinds %q and %q share glyph %q; all three end-states must be distinct", ka, kb, ga)
			}
		}
	}
	// Glyph values must match the declared constants.
	if timelineGlyph(retracted.Kind) != timelineGlyphRetracted {
		t.Errorf("retracted glyph = %q, want %q", timelineGlyph(retracted.Kind), timelineGlyphRetracted)
	}
	if timelineGlyph(closed.Kind) != timelineGlyphClosed {
		t.Errorf("closed glyph = %q, want %q", timelineGlyph(closed.Kind), timelineGlyphClosed)
	}
	if timelineGlyph(superseded.Kind) != timelineGlyphSuperseded {
		t.Errorf("superseded glyph = %q, want %q", timelineGlyph(superseded.Kind), timelineGlyphSuperseded)
	}
	// Retracted edge carries no SupersededBy; superseded one does.
	if retracted.SupersededBy != "" {
		t.Errorf("retracted.SupersededBy = %q, want empty (pure tombstone close, no replacement)", retracted.SupersededBy)
	}
	if superseded.SupersededBy != "relations/01SUPERSEDER0000000000000Z" {
		t.Errorf("superseded.SupersededBy = %q, want relations/01SUPERSEDER0000000000000Z", superseded.SupersededBy)
	}
}

// TestBuildTimelineSupersededEmitsSingleEnd verifies a superseded edge emits
// exactly one end event (superseded), not also a separate closed event, even
// though the fold stamps both superseded_by_uid AND valid_until.
func TestBuildTimelineSupersededEmitsSingleEnd(t *testing.T) {
	t.Parallel()
	raw := relationsJSON(
		`{"name":"relations/RS","subject":"entities/X","object":"entities/W","direction":"outgoing","predicate":"reports_to","createTime":"2025-02-01T00:00:00Z","validFrom":"2025-02-01T00:00:00Z","validUntil":"2025-07-01T00:00:00Z","supersededByUid":"01SUP"}`,
	)
	tl, err := buildTimeline("entities/X", raw)
	if err != nil {
		t.Fatalf("buildTimeline: %v", err)
	}
	if len(tl.Events) != 2 {
		t.Fatalf("event count = %d, want 2 (opened + superseded)\nevents=%+v", len(tl.Events), tl.Events)
	}
	if tl.Events[0].Kind != timelineKindOpened || tl.Events[1].Kind != timelineKindSuperseded {
		t.Errorf("kinds = [%s,%s], want [opened,superseded]", tl.Events[0].Kind, tl.Events[1].Kind)
	}
}

// TestBuildTimelineOpenedFallsBackToCreateTime verifies an edge with no
// valid_from anchors its opened event at create_time rather than dropping out.
func TestBuildTimelineOpenedFallsBackToCreateTime(t *testing.T) {
	t.Parallel()
	raw := relationsJSON(
		`{"name":"relations/R1","subject":"entities/X","object":"entities/Y","direction":"outgoing","predicate":"knows","createTime":"2025-05-05T12:00:00Z"}`,
	)
	tl, err := buildTimeline("entities/X", raw)
	if err != nil {
		t.Fatalf("buildTimeline: %v", err)
	}
	if len(tl.Events) != 1 || tl.Events[0].Kind != timelineKindOpened {
		t.Fatalf("want a single opened event, got %+v", tl.Events)
	}
	if tl.Events[0].Time != "2025-05-05T12:00:00Z" {
		t.Errorf("opened time = %q, want create_time fallback 2025-05-05T12:00:00Z", tl.Events[0].Time)
	}
}

// TestBuildTimelineEmpty verifies an entity with no relations yields an empty
// (non-nil) event slice and echoes the queried arg as the entity.
func TestBuildTimelineEmpty(t *testing.T) {
	t.Parallel()
	tl, err := buildTimeline("Frodo", relationsJSON())
	if err != nil {
		t.Fatalf("buildTimeline: %v", err)
	}
	if tl.Entity != "Frodo" {
		t.Errorf("entity = %q, want echoed arg Frodo", tl.Entity)
	}
	if tl.Events == nil {
		t.Error("events is nil; want a non-nil empty slice for a stable JSON []")
	}
	if len(tl.Events) != 0 {
		t.Errorf("event count = %d, want 0", len(tl.Events))
	}
}

// TestSortTimelineEventsTieBreak verifies that at a shared instant an opening
// sorts before its end (a born-closed edge reads open-then-close).
func TestSortTimelineEventsTieBreak(t *testing.T) {
	t.Parallel()
	events := []timelineEvent{
		{Time: "2025-01-01T00:00:00Z", Kind: timelineKindClosed, Relation: "relations/R1"},
		{Time: "2025-01-01T00:00:00Z", Kind: timelineKindOpened, Relation: "relations/R1"},
	}
	sortTimelineEvents(events)
	if events[0].Kind != timelineKindOpened || events[1].Kind != timelineKindClosed {
		t.Errorf("tie-break order = [%s,%s], want [opened,closed]", events[0].Kind, events[1].Kind)
	}
}

// TestTimelineGlyphsDistinct verifies every kind maps to a unique, non-empty glyph.
func TestTimelineGlyphsDistinct(t *testing.T) {
	t.Parallel()
	kinds := []string{timelineKindOpened, timelineKindClosed, timelineKindRetracted, timelineKindSuperseded}
	seen := map[string]string{}
	for _, k := range kinds {
		g := timelineGlyph(k)
		if g == "" || g == "?" {
			t.Errorf("kind %q has no distinct glyph (got %q)", k, g)
		}
		if prev, dup := seen[g]; dup {
			t.Errorf("glyph %q shared by kinds %q and %q", g, prev, k)
		}
		seen[g] = k
	}
}

// TestCountOpenedEvents verifies the relation count (one opened event per edge)
// that the --limit truncation footer is based on — NOT the total event count.
func TestCountOpenedEvents(t *testing.T) {
	t.Parallel()
	// 3 edges → 3 opened + 1 closed + 1 superseded = 5 events, 3 relations.
	tl, err := buildTimeline("entities/X", relationsJSON(
		`{"name":"relations/RC","subject":"entities/X","object":"entities/Y","direction":"outgoing","predicate":"works_at","createTime":"2025-01-01T00:00:00Z","validUntil":"2025-06-01T00:00:00Z"}`,
		`{"name":"relations/RS","subject":"entities/X","object":"entities/W","direction":"outgoing","predicate":"reports_to","createTime":"2025-02-01T00:00:00Z","validUntil":"2025-07-01T00:00:00Z","supersededByUid":"01SUP"}`,
		`{"name":"relations/RO","subject":"entities/X","object":"entities/V","direction":"outgoing","predicate":"works_at","createTime":"2025-03-01T00:00:00Z"}`,
	))
	if err != nil {
		t.Fatalf("buildTimeline: %v", err)
	}
	if got := countOpenedEvents(tl.Events); got != 3 {
		t.Errorf("countOpenedEvents = %d, want 3 (relations), not %d (events)", got, len(tl.Events))
	}
}

// ---- rendering: json / ndjson / text ----

// timelineCmd returns a bare command writing to outBuf for exercising the pure
// render helpers without a full root (mirrors navigate_test's ndjsonCmd).
func timelineCmd(outBuf *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	cmd.SetOut(outBuf)
	return cmd
}

func sampleTimeline() timeline {
	tl, _ := buildTimeline("entities/X", relationsJSON(
		`{"name":"relations/RY","subject":"entities/X","object":"entities/Y","direction":"outgoing","subjectName":"Mira","objectName":"Mithril","predicate":"works_at","createTime":"2025-01-01T00:00:00Z","validFrom":"2025-01-01T00:00:00Z","validUntil":"2025-02-01T00:00:00Z"}`,
		`{"name":"relations/RZ","subject":"entities/X","object":"entities/Z","direction":"outgoing","subjectName":"Mira","objectName":"Gondor","predicate":"works_at","createTime":"2025-03-01T00:00:00Z","validFrom":"2025-03-01T00:00:00Z"}`,
	))
	return tl
}

// TestRenderTimelineJSONStable verifies --output json emits one parseable object
// with the documented {entity, events[]} shape and required per-event fields.
func TestRenderTimelineJSONStable(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	if err := renderTimelineJSON(timelineCmd(&outBuf), sampleTimeline()); err != nil {
		t.Fatalf("renderTimelineJSON: %v", err)
	}
	var got struct {
		Entity string `json:"entity"`
		Events []struct {
			Time      string `json:"time"`
			Kind      string `json:"kind"`
			Predicate string `json:"predicate"`
			Relation  string `json:"relation"`
		} `json:"events"`
	}
	if err := json.Unmarshal(outBuf.Bytes(), &got); err != nil {
		t.Fatalf("output is not parseable JSON: %v\nout=%q", err, outBuf.String())
	}
	if got.Entity != "entities/X" {
		t.Errorf("entity = %q, want entities/X", got.Entity)
	}
	if len(got.Events) != 3 {
		t.Fatalf("events = %d, want 3 (opened, closed, opened)", len(got.Events))
	}
	for i, ev := range got.Events {
		if ev.Time == "" || ev.Kind == "" || ev.Relation == "" {
			t.Errorf("event %d missing a required field: %+v", i, ev)
		}
	}
}

// TestRenderTimelineNDJSONOneObjectPerLine verifies --output ndjson emits one
// parseable JSON object per event, matching neighbors/relations/entities.
func TestRenderTimelineNDJSONOneObjectPerLine(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	if err := renderTimelineNDJSON(timelineCmd(&outBuf), sampleTimeline()); err != nil {
		t.Fatalf("renderTimelineNDJSON: %v", err)
	}
	lines := strings.Split(strings.TrimRight(outBuf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("ndjson: want 3 lines (one per event), got %d:\n%q", len(lines), outBuf.String())
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("ndjson line %d is not a JSON object: %v\nline=%q", i, err, line)
		}
	}
}

// TestRenderTimelineNDJSONEmptyEmitsNothing verifies an empty timeline emits no
// lines (not a wrapper), matching the ndjson contract.
func TestRenderTimelineNDJSONEmptyEmitsNothing(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	tl, _ := buildTimeline("Frodo", relationsJSON())
	if err := renderTimelineNDJSON(timelineCmd(&outBuf), tl); err != nil {
		t.Fatalf("renderTimelineNDJSON empty: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Errorf("empty timeline ndjson: want no output, got %q", outBuf.String())
	}
}

// TestRenderTimelineCLIGlyphs verifies text mode renders distinct glyphs and the
// kind labels for each event.
func TestRenderTimelineCLIGlyphs(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	if err := renderTimelineCLI(timelineCmd(&outBuf), sampleTimeline(), 50); err != nil {
		t.Fatalf("renderTimelineCLI: %v", err)
	}
	out := outBuf.String()
	for _, want := range []string{timelineGlyphOpened, timelineGlyphClosed, "opened", "closed", "works_at"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\nout=%q", want, out)
		}
	}
}

// TestRenderTimelineCLIEmpty verifies the empty-history text line.
func TestRenderTimelineCLIEmpty(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	tl, _ := buildTimeline("Frodo", relationsJSON())
	if err := renderTimelineCLI(timelineCmd(&outBuf), tl, 50); err != nil {
		t.Fatalf("renderTimelineCLI empty: %v", err)
	}
	if !strings.Contains(outBuf.String(), "no relationship history") {
		t.Errorf("empty text output = %q, want 'no relationship history'", outBuf.String())
	}
}

// ---- help: non-empty and teaching ----

// TestTimelineHelp verifies --help is non-empty, carries runnable Examples, and
// teaches the glyph legend so a human (or agent) learns the surface.
func TestTimelineHelp(t *testing.T) {
	t.Parallel()
	cmd := newTimelineCmd()
	long := cmd.Long
	if strings.TrimSpace(long) == "" {
		t.Fatal("timeline Long help is empty")
	}
	for _, want := range []string{"Examples:", "maz timeline", "--predicate", "--output json"} {
		if !strings.Contains(long, want) {
			t.Errorf("help missing %q", want)
		}
	}
	// The glyph legend must teach all four end-states.
	for _, g := range []string{timelineGlyphOpened, timelineGlyphClosed, timelineGlyphRetracted, timelineGlyphSuperseded} {
		if !strings.Contains(long, g) {
			t.Errorf("help missing glyph %q from the legend", g)
		}
	}
	if strings.TrimSpace(cmd.Short) == "" {
		t.Error("timeline Short help is empty")
	}
}

// ---- end-to-end: live in-process backend (CI only; skipped under -short) ----

// makeTimelineRoot wires a minimal root with the commands the timeline e2e
// drives: entity (create), relate, unrelate, and timeline itself.
func makeTimelineRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newEntityCmd())
	root.AddCommand(newRelateCmd())
	root.AddCommand(newUnrelateCmd())
	root.AddCommand(newTimelineCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// runTimelineJSON runs `maz timeline <entity> --output json [extra...]` against
// dataDir and returns the decoded envelope, failing the test on a non-zero exit.
func runTimelineJSON(t *testing.T, dataDir, entity string, extra ...string) timeline {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeTimelineRoot(&outBuf, &errBuf)
	args := append([]string{"--data-dir", dataDir, "--output", "json", "timeline", entity}, extra...)
	root.SetArgs(args)
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("timeline --output json: %v (stderr=%q)", err, errBuf.String())
	}
	var tl timeline
	if err := json.Unmarshal(outBuf.Bytes(), &tl); err != nil {
		t.Fatalf("timeline json not parseable: %v\nout=%q", err, outBuf.String())
	}
	return tl
}

// TestTimelineE2EOpenCloseOpen is the brief's acceptance scenario end-to-end:
// open X works_at Y, close it, open X works_at Z; the timeline shows
// opened/closed/opened in time order with the right kinds.
func TestTimelineE2EOpenCloseOpen(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir) // skips under -short; boots the live in-process backend

	x := runEntityCreateCmd(t, dir, "person", "Timeline Mira")
	y := runEntityCreateCmd(t, dir, "organization", "Timeline Mithril")
	z := runEntityCreateCmd(t, dir, "organization", "Timeline Gondor")

	// 1) open works_at Y, 2) close it (valid_until = now), 3) open works_at Z.
	runRelateCmd(t, dir, x, "works_at", y)
	runUnrelateCmd(t, dir, x, "works_at", y)
	runRelateCmd(t, dir, x, "works_at", z)

	tl := runTimelineJSON(t, dir, x)
	gotKinds := make([]string, len(tl.Events))
	for i, ev := range tl.Events {
		gotKinds[i] = ev.Kind
	}
	// unrelate appends a relationRetracted tombstone, so the close is a retraction,
	// not a born-bounded closed.
	want := strings.Join([]string{timelineKindOpened, timelineKindRetracted, timelineKindOpened}, ",")
	if strings.Join(gotKinds, ",") != want {
		t.Fatalf("timeline kinds = %v, want [opened retracted opened]\nevents=%+v", gotKinds, tl.Events)
	}
	// Chronological: times must be non-decreasing.
	for i := 1; i < len(tl.Events); i++ {
		if tl.Events[i].Time < tl.Events[i-1].Time {
			t.Errorf("event %d (%s) precedes event %d (%s) in time", i, tl.Events[i].Time, i-1, tl.Events[i-1].Time)
		}
	}

	// --predicate works_at keeps every event (all edges are works_at); a
	// non-matching predicate empties the history.
	if other := runTimelineJSON(t, dir, x, "--predicate", "reports_to"); len(other.Events) != 0 {
		t.Errorf("--predicate reports_to: want 0 events, got %d", len(other.Events))
	}
}

// runUnrelateCmd closes the open <subject> <predicate> <object> edge in dataDir.
func runUnrelateCmd(t *testing.T, dataDir, subject, predicate, object string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeTimelineRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--output", "text", "--data-dir", dataDir, "unrelate", subject, predicate, object})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("unrelate: %v (stderr=%q)", err, errBuf.String())
	}
}

// TestTimelineE2EUnknownEntityExitsOne pins the exit-code contract: an unknown
// entity is an expected failure (E_NOT_FOUND) and exits 1, not 2.
func TestTimelineE2EUnknownEntityExitsOne(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir) // skips under -short

	var outBuf, errBuf bytes.Buffer
	root := makeTimelineRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "timeline", "No Such Person"})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("timeline on an unknown entity exited 0; want a non-zero exit")
	}
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T, want silentError", err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1 (expected failure)", se.code)
	}
	if !strings.Contains(outBuf.String(), "E_NOT_FOUND") {
		t.Errorf("json error envelope = %q, want an E_NOT_FOUND code", outBuf.String())
	}
}
