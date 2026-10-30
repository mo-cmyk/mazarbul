package contextbrief

// brief_pins_test.go covers the steering-pin merge (#15): free-text pins lead
// the Pinned section ahead of auto-surfaced items, corrections overlay a pin's
// text in place, corrections overlay a surfaced entity, the latest correction
// per target wins, and a nil reader leaves the brief unchanged.

import (
	"context"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// fakePinSource is an explicit ContextPinReader for the unscoped/test path
// (fakeReader does not implement ContextPins, so Build falls back to this).
type fakePinSource struct{ pins []navigate.ContextPin }

func (f *fakePinSource) ContextPins(_ context.Context) ([]navigate.ContextPin, error) {
	return f.pins, nil
}

func TestBuild_SteeringPinLeadsPinnedSection(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		pinned:   []navigate.Document{{DocumentUID: "P1", Path: "notes/pinned.md", Title: "Pinned Doc", Body: "body", Pinned: true}},
		entities: []navigate.Entity{{UID: "E1", Kind: "person", DisplayName: "Rosie"}},
	}
	pins := &fakePinSource{pins: []navigate.ContextPin{
		{UID: "PIN1", Type: "pin", Text: "Ship the wave Friday", Active: true, CreateTime: "2026-06-24T10:00:00Z"},
	}}
	a := New(reader, nil).WithContextPinReader(pins)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	for _, want := range []string{"## Pinned", "📌 Ship the wave Friday", "## Key entities"} {
		if !strings.Contains(md, want) {
			t.Errorf("brief missing %q\n---\n%s", want, md)
		}
	}
	// The steering pin must lead the Pinned section: before the pinned document
	// AND before the auto-surfaced Key entities section.
	if pinAt, docAt := strings.Index(md, "📌 Ship the wave Friday"), strings.Index(md, "notes/pinned.md"); pinAt > docAt {
		t.Errorf("steering pin should precede the pinned doc (pin@%d doc@%d)\n%s", pinAt, docAt, md)
	}
	if pinAt, entAt := strings.Index(md, "📌 Ship the wave Friday"), strings.Index(md, "## Key entities"); pinAt > entAt {
		t.Errorf("steering pin should precede Key entities (pin@%d ent@%d)\n%s", pinAt, entAt, md)
	}
}

func TestBuild_CorrectionOverlaysPinText(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{}
	pins := &fakePinSource{pins: []navigate.ContextPin{
		// newest-first ordering, as ContextPins returns.
		{UID: "C1", Type: "correction", TargetRef: "PIN1", Text: "Ships next Friday instead", Active: true, CreateTime: "2026-06-24T12:00:00Z"},
		{UID: "PIN1", Type: "pin", Text: "Ship the wave today", Active: true, CreateTime: "2026-06-24T10:00:00Z"},
	}}
	a := New(reader, nil).WithContextPinReader(pins)
	brief, err := a.Build(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	if !strings.Contains(md, "Ships next Friday instead") {
		t.Errorf("correction text not rendered over the pin:\n%s", md)
	}
	if strings.Contains(md, "Ship the wave today") {
		t.Errorf("original pin text should be overlaid by the correction:\n%s", md)
	}
	if !strings.Contains(md, "_(corrected)_") {
		t.Errorf("corrected pin should be tagged:\n%s", md)
	}
}

func TestBuild_CorrectionOverlaysEntity(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		entities: []navigate.Entity{{UID: "E1", Kind: "person", DisplayName: "Rosie"}},
	}
	pins := &fakePinSource{pins: []navigate.ContextPin{
		{UID: "C1", Type: "correction", TargetRef: "entities/E1", Text: "Left Acme in May", Active: true, CreateTime: "2026-06-24T12:00:00Z"},
	}}
	a := New(reader, nil).WithContextPinReader(pins)
	brief, err := a.Build(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	if !strings.Contains(md, "✏️ Left Acme in May") {
		t.Errorf("entity correction overlay missing:\n%s", md)
	}
	// The entity anchor is retained for traceability.
	if !strings.Contains(md, "entities/E1") {
		t.Errorf("entity anchor lost under correction:\n%s", md)
	}
}

func TestBuild_LatestCorrectionPerTargetWins(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{}
	pins := &fakePinSource{pins: []navigate.ContextPin{
		// Two corrections for the same pin; newest-first, so the first wins.
		{UID: "C2", Type: "correction", TargetRef: "PIN1", Text: "newest", Active: true, CreateTime: "2026-06-24T13:00:00Z"},
		{UID: "C1", Type: "correction", TargetRef: "PIN1", Text: "older", Active: true, CreateTime: "2026-06-24T12:00:00Z"},
		{UID: "PIN1", Type: "pin", Text: "original", Active: true, CreateTime: "2026-06-24T10:00:00Z"},
	}}
	a := New(reader, nil).WithContextPinReader(pins)
	brief, err := a.Build(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	if !strings.Contains(md, "newest") {
		t.Errorf("latest correction should win:\n%s", md)
	}
	if strings.Contains(md, "older") || strings.Contains(md, "original") {
		t.Errorf("only the latest correction should render:\n%s", md)
	}
}

func TestBuild_NilPinReader_UnchangedBrief(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		entities: []navigate.Entity{{UID: "E1", Kind: "person", DisplayName: "Rosie"}},
	}
	a := New(reader, nil) // no WithContextPinReader
	brief, err := a.Build(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(brief.Markdown, "📌") {
		t.Errorf("no pin reader → no 📌 section:\n%s", brief.Markdown)
	}
	// The entity line stays in its plain (uncorrected) form.
	if !strings.Contains(brief.Markdown, "- **Rosie** (person) — `entities/E1`\n") {
		t.Errorf("entity line should render plain without a pin reader:\n%s", brief.Markdown)
	}
}

func TestContextCorrections_SkipsEmptyTargetAndPins(t *testing.T) {
	t.Parallel()
	got := contextCorrections([]navigate.ContextPin{
		{UID: "C1", Type: "correction", TargetRef: "PIN1", Text: "a"},
		{UID: "C2", Type: "correction", TargetRef: "", Text: "no-target"}, // skipped
		{UID: "PIN1", Type: "pin", TargetRef: "PIN1", Text: "not a correction"},
	})
	if len(got) != 1 || got["PIN1"] != "a" {
		t.Errorf("corrections = %v, want {PIN1: a}", got)
	}
}
