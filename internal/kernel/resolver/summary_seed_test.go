package resolver

// summary_seed_test.go pins the first-summary seeding behavior: a brand-new
// entity gets its summary direct-set from mention text with ZERO LLM calls
// (burst profiling showed the old path burning one Complete call per new
// entity — 80 calls / 24% of all LLM time on a 62-fact burst), while the
// SummaryRegen threshold still routes through the LLM for real synthesis.

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// captureLog is a minimal kernel.EventLog that records appended events.
type captureLog struct {
	events []kernel.Event
}

func (l *captureLog) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	l.events = append(l.events, events...)
	return make([]kernel.Receipt, len(events)), nil
}

func (l *captureLog) Read(context.Context, kernel.ReadOpts) iter.Seq2[kernel.Event, error] {
	return func(func(kernel.Event, error) bool) {}
}

func (l *captureLog) At(context.Context, string) (kernel.Event, error) {
	return kernel.Event{}, kernel.ErrNotFound
}

func (l *captureLog) Rebuild(context.Context, kernel.Storage, ...kernel.RebuildOption) ([]kernel.ApplyError, error) {
	return nil, nil
}

func (l *captureLog) Provenance(context.Context, string) ([]kernel.ProvenanceRecord, error) {
	return nil, nil
}

// countingChat counts Complete and ToolUse calls; Complete returns a fixed
// summary. Tests assert the counters to prove (or rule out) LLM round-trips.
type countingChat struct {
	completes atomic.Int64
	toolUses  atomic.Int64
	content   string
}

func (c *countingChat) Complete(context.Context, kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	c.completes.Add(1)
	return kernel.CompleteResponse{Content: c.content}, nil
}

func (c *countingChat) ToolUse(context.Context, kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	c.toolUses.Add(1)
	return kernel.ToolUseResponse{}, nil
}

// newSummaryTestResolver builds the minimal Resolver surface that
// regenSummariesIfNeeded touches: config, event log, chat, prompts, logger.
func newSummaryTestResolver(t *testing.T, log *captureLog, chat kernel.ChatProvider) *Resolver {
	t.Helper()
	cfg := Config{}
	cfg.ApplyDefaults()
	reg := prompts.New(nil)
	if err := RegisterPrompts(context.Background(), reg); err != nil {
		t.Fatalf("RegisterPrompts: %v", err)
	}
	return &Resolver{
		cfg:       cfg,
		eventLog:  log,
		chat:      chat,
		promptReg: reg,
		logger:    zap.NewNop(),
	}
}

// entityFromEvent unmarshals the EntityUpdated payload.
func entityFromEvent(t *testing.T, ev kernel.Event) ontology.Entity {
	t.Helper()
	var e ontology.Entity
	if err := json.Unmarshal(ev.Payload, &e); err != nil {
		t.Fatalf("unmarshal entity payload: %v", err)
	}
	return e
}

func TestRegenSummaries_NewEntitySeedsWithoutLLM(t *testing.T) {
	t.Parallel()
	log := &captureLog{}
	chat := &countingChat{content: "LLM MUST NOT RUN"}
	r := newSummaryTestResolver(t, log, chat)

	mentions := []ontology.Mention{{UID: "m1", ExtractionText: "Merry moved to Berlin in 2021."}}
	results := []MatchResult{{EntityUID: "e1", IsNew: true}}
	entities := []ontology.Entity{{UID: "e1", DisplayName: "Merry", Kind: ontology.EntityKindPerson}}

	r.regenSummariesIfNeeded(context.Background(), mentions, results, entities, map[string]int{"e1": 1})

	if got := chat.completes.Load(); got != 0 {
		t.Fatalf("first-mention summary must not call the LLM; got %d Complete calls", got)
	}
	if len(log.events) != 1 {
		t.Fatalf("want exactly 1 EntityUpdated event, got %d", len(log.events))
	}
	e := entityFromEvent(t, log.events[0])
	if e.Summary != "Merry moved to Berlin in 2021." {
		t.Errorf("seeded summary = %q, want the raw mention text", e.Summary)
	}
	if e.SummaryUpdateTime == nil {
		t.Error("SummaryUpdateTime must be stamped so the staleness clock starts")
	}
}

func TestRegenSummaries_EmptyMentionTextSeedsNothing(t *testing.T) {
	t.Parallel()
	log := &captureLog{}
	chat := &countingChat{content: "LLM MUST NOT RUN"}
	r := newSummaryTestResolver(t, log, chat)

	mentions := []ontology.Mention{{UID: "m1", ExtractionText: "   "}}
	results := []MatchResult{{EntityUID: "e1", IsNew: true}}
	entities := []ontology.Entity{{UID: "e1", DisplayName: "Merry", Kind: ontology.EntityKindPerson}}

	r.regenSummariesIfNeeded(context.Background(), mentions, results, entities, map[string]int{"e1": 1})

	if got := chat.completes.Load(); got != 0 {
		t.Fatalf("blank mention must not call the LLM; got %d Complete calls", got)
	}
	if len(log.events) != 0 {
		t.Fatalf("blank seed must append no events, got %d", len(log.events))
	}
}

func TestRegenSummaries_ThresholdStillUsesLLM(t *testing.T) {
	t.Parallel()
	log := &captureLog{}
	chat := &countingChat{content: "Merry is a Berlin-based engineer."}
	r := newSummaryTestResolver(t, log, chat)

	recent := time.Now().UTC().Add(-time.Hour) // staleness interval NOT elapsed
	mentions := []ontology.Mention{{UID: "m9", ExtractionText: "Merry now leads the platform team."}}
	results := []MatchResult{{EntityUID: "e1", IsNew: false}}
	entities := []ontology.Entity{{
		UID:               "e1",
		DisplayName:       "Merry",
		Kind:              ontology.EntityKindPerson,
		Summary:           "Merry moved to Berlin in 2021.",
		SummaryUpdateTime: &recent,
	}}

	// Mention count at the SummaryRegen threshold → real multi-mention
	// synthesis → the LLM path must fire exactly once.
	counts := map[string]int{"e1": r.cfg.SummaryRegen}
	r.regenSummariesIfNeeded(context.Background(), mentions, results, entities, counts)

	if got := chat.completes.Load(); got != 1 {
		t.Fatalf("threshold regen must call the LLM once, got %d", got)
	}
	if len(log.events) != 1 {
		t.Fatalf("want exactly 1 EntityUpdated event, got %d", len(log.events))
	}
	e := entityFromEvent(t, log.events[0])
	if e.Summary != "Merry is a Berlin-based engineer." {
		t.Errorf("summary = %q, want the LLM synthesis", e.Summary)
	}
}

func TestRegenSummaries_TwoMentionsSameEntitySeedOnce(t *testing.T) {
	t.Parallel()
	log := &captureLog{}
	chat := &countingChat{content: "LLM MUST NOT RUN"}
	r := newSummaryTestResolver(t, log, chat)

	// Two mentions of the same brand-new entity in one batch: the loop
	// visits the UID twice but must seed exactly once, with the FULL
	// batch window.
	mentions := []ontology.Mention{
		{UID: "m1", ExtractionText: "Merry moved to Berlin."},
		{UID: "m2", ExtractionText: "Merry works at Initech."},
	}
	results := []MatchResult{
		{EntityUID: "e1", IsNew: true},
		{EntityUID: "e1", IsNew: false},
	}
	entities := []ontology.Entity{
		{UID: "e1", DisplayName: "Merry", Kind: ontology.EntityKindPerson},
		{UID: "e1", DisplayName: "Merry", Kind: ontology.EntityKindPerson},
	}

	r.regenSummariesIfNeeded(context.Background(), mentions, results, entities, map[string]int{"e1": 2})

	if got := chat.completes.Load(); got != 0 {
		t.Fatalf("seed path must not call the LLM; got %d Complete calls", got)
	}
	if len(log.events) != 1 {
		t.Fatalf("same entity mentioned twice must seed ONCE, got %d events", len(log.events))
	}
	e := entityFromEvent(t, log.events[0])
	want := "Merry moved to Berlin.\nMerry works at Initech."
	if e.Summary != want {
		t.Errorf("seeded summary = %q, want full batch window %q", e.Summary, want)
	}
}

func TestRegenSummaries_LegacySummaryNilTimeRoutesToLLM(t *testing.T) {
	t.Parallel()
	log := &captureLog{}
	chat := &countingChat{content: "Synthesized legacy summary."}
	r := newSummaryTestResolver(t, log, chat)

	// Summary set but SummaryUpdateTime nil (legacy/half-written state):
	// the seed guard must NOT swallow it — shouldRegenSummary's nil-time
	// branch routes it through the LLM once, which stamps the clock.
	mentions := []ontology.Mention{{UID: "m1", ExtractionText: "Merry now lives in Hamburg."}}
	results := []MatchResult{{EntityUID: "e1", IsNew: false}}
	entities := []ontology.Entity{{
		UID:         "e1",
		DisplayName: "Merry",
		Kind:        ontology.EntityKindPerson,
		Summary:     "Merry is a person from an old import.",
	}}

	r.regenSummariesIfNeeded(context.Background(), mentions, results, entities, map[string]int{"e1": 1})

	if got := chat.completes.Load(); got != 1 {
		t.Fatalf("legacy summary with nil time must take the LLM path once, got %d", got)
	}
	if len(log.events) != 1 {
		t.Fatalf("want 1 EntityUpdated event, got %d", len(log.events))
	}
	e := entityFromEvent(t, log.events[0])
	if e.SummaryUpdateTime == nil {
		t.Error("LLM regen must stamp SummaryUpdateTime")
	}
}

func TestRegenSummaries_SeedRespectsMentionWindow(t *testing.T) {
	t.Parallel()
	log := &captureLog{}
	chat := &countingChat{content: "LLM MUST NOT RUN"}
	r := newSummaryTestResolver(t, log, chat)

	// More mentions than cfg.SummaryMentionWindow (default 20): the seed
	// must use only the trailing window, mirroring the LLM prompt input.
	n := r.cfg.SummaryMentionWindow + 5
	mentions := make([]ontology.Mention, n)
	results := make([]MatchResult, n)
	entities := make([]ontology.Entity, n)
	for i := range mentions {
		mentions[i] = ontology.Mention{
			UID:            fmt.Sprintf("m%02d", i+1),
			ExtractionText: fmt.Sprintf("fact-%02d about Merry.", i+1),
		}
		results[i] = MatchResult{EntityUID: "e1", IsNew: i == 0}
		entities[i] = ontology.Entity{UID: "e1", DisplayName: "Merry", Kind: ontology.EntityKindPerson}
	}

	r.regenSummariesIfNeeded(context.Background(), mentions, results, entities, map[string]int{"e1": n})

	if len(log.events) != 1 {
		t.Fatalf("want 1 seed event, got %d", len(log.events))
	}
	e := entityFromEvent(t, log.events[0])
	if !strings.Contains(e.Summary, fmt.Sprintf("fact-%02d", n)) {
		t.Errorf("summary must include the newest mention; got %q", e.Summary)
	}
	if strings.Contains(e.Summary, "fact-05") {
		t.Errorf("summary must drop mentions outside the trailing window of %d; got %q", r.cfg.SummaryMentionWindow, e.Summary)
	}
}
