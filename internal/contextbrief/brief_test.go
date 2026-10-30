package contextbrief

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
)

type fakeReader struct {
	pinned   []navigate.Document
	recent   []navigate.Document
	entities []navigate.Entity
	// mentionOrdered is returned instead of entities when SortByMentions is
	// true, letting tests assert that the assembler requests mention-count
	// ordering for the no-ranker fallback path.
	mentionOrdered []navigate.Entity
}

func (f *fakeReader) ListDocuments(_ context.Context, opts navigate.DocumentFilter) ([]navigate.Document, error) {
	docs := f.recent
	if opts.PinnedOnly {
		docs = f.pinned
	}
	// Mirror the real Navigator: the SQL LIMIT is applied by the store before
	// the assembler dedupes, so honoring Limit here exercises the over-fetch.
	if opts.Limit > 0 && len(docs) > opts.Limit {
		docs = docs[:opts.Limit]
	}
	return docs, nil
}

//nolint:gocritic // hugeParam: EntityFilter is passed by value to satisfy the Reader interface (mirrors *Navigator.ListEntities).
func (f *fakeReader) ListEntities(_ context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	// Honor SortByMentions: the backfill lane requests mention-ranked entities.
	if opts.SortByMentions && f.mentionOrdered != nil {
		return f.mentionOrdered, nil
	}
	// Honor the Kind filter so the "Key entities" (no kind) and "Referenced
	// externally" (kind=external_ref) lanes partition faithfully, like the real
	// Navigator and the tools fakeNavigator.
	if opts.Kind == "" {
		return f.entities, nil
	}
	out := make([]navigate.Entity, 0, len(f.entities))
	for _, e := range f.entities {
		if e.Kind == opts.Kind {
			out = append(out, e)
		}
	}
	return out, nil
}

type fakeRecaller struct{ hits []kernel.Hit }

// Recall matches kernel.Query.Recall, which takes RecallRequest by value.
//
//nolint:gocritic // signature must match the kernel.Query interface (value receiver param)
func (f *fakeRecaller) Recall(_ context.Context, _ kernel.RecallRequest) iter.Seq2[kernel.Hit, error] {
	return func(yield func(kernel.Hit, error) bool) {
		for i := range f.hits {
			if !yield(f.hits[i], nil) {
				return
			}
		}
	}
}

func TestBuild_AssemblesPrioritizedSections(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		pinned: []navigate.Document{{DocumentUID: "P1", Path: "notes/pinned.md", Title: "Pinned One", Body: "pinned body", Pinned: true}},
		recent: []navigate.Document{
			{DocumentUID: "P1", Path: "notes/pinned.md", Title: "Pinned One", Body: "pinned body", Pinned: true}, // dup of pinned
			{DocumentUID: "R1", Path: "notes/recent.md", Title: "Recent One", Body: "recent body"},
		},
		entities: []navigate.Entity{{UID: "E1", Kind: "person", DisplayName: "Rosie"}},
	}
	a := New(reader, nil)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	for _, want := range []string{"# Context", "## Pinned", "notes/pinned.md", "## Recently updated", "notes/recent.md", "## Key entities", "Rosie", "entities/E1"} {
		if !strings.Contains(md, want) {
			t.Errorf("brief missing %q\n---\n%s", want, md)
		}
	}
	// The pinned doc must NOT be repeated in the recent section.
	if strings.Count(md, "notes/pinned.md") != 1 {
		t.Errorf("pinned doc duplicated across sections:\n%s", md)
	}
	if brief.Tokens > brief.Budget {
		t.Errorf("brief tokens %d exceed budget %d", brief.Tokens, brief.Budget)
	}
}

// TestBuild_ReferencedExternallySection asserts external_ref entities render
// under their own "Referenced externally" section (#208) and are NOT
// double-listed under "Key entities".
func TestBuild_ReferencedExternallySection(t *testing.T) {
	t.Parallel()
	const refURI = "https://github.com/org/repo/issues/42"
	reader := &fakeReader{
		entities: []navigate.Entity{
			{UID: "E1", Kind: "person", DisplayName: "Rosie"},
			{UID: "X1", Kind: "external_ref", DisplayName: refURI},
		},
	}
	a := New(reader, nil)
	brief, err := a.Build(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	for _, want := range []string{"## Referenced externally", refURI, "entities/X1"} {
		if !strings.Contains(md, want) {
			t.Errorf("brief missing %q\n---\n%s", want, md)
		}
	}
	// The person belongs to Key entities; the external_ref must not leak there.
	keyIdx := strings.Index(md, "## Key entities")
	refIdx := strings.Index(md, "## Referenced externally")
	if keyIdx < 0 || refIdx < 0 || keyIdx >= refIdx {
		t.Fatalf("expected Key entities before Referenced externally:\n%s", md)
	}
	keySection := md[keyIdx:refIdx]
	if !strings.Contains(keySection, "Rosie") {
		t.Errorf("person missing from Key entities:\n%s", keySection)
	}
	if strings.Contains(keySection, "entities/X1") {
		t.Errorf("external_ref leaked into Key entities:\n%s", keySection)
	}
}

func TestBuild_QueryLaneOnlyWithRecaller(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{recent: []navigate.Document{{DocumentUID: "R1", Path: "a.md", Title: "A", Body: "b"}}}
	rec := &fakeRecaller{hits: []kernel.Hit{
		{UID: "EV1", Text: "Rosie leads the Q3 launch", Source: &kernel.SourceLocation{Path: "notes/launch.md", Heading: "## Goals"}},
	}}
	a := New(reader, rec)

	// With a query, the Relevant section appears and cites the source path›heading.
	withQuery, err := a.Build(context.Background(), Request{Query: "Q3 launch"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(withQuery.Markdown, "Relevant to \"Q3 launch\"") {
		t.Errorf("relevant section missing:\n%s", withQuery.Markdown)
	}
	if !strings.Contains(withQuery.Markdown, "notes/launch.md › ## Goals") {
		t.Errorf("hit source label missing:\n%s", withQuery.Markdown)
	}

	// Without a query, no Relevant section even though a recaller is present.
	noQuery, err := a.Build(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(noQuery.Markdown, "Relevant to") {
		t.Errorf("relevant section should be absent without a query:\n%s", noQuery.Markdown)
	}
}

func TestBuild_RecentSectionSurvivesPinnedDominatingNewestRows(t *testing.T) {
	t.Parallel()
	// The vault's newest defaultRecentLimit documents are all pinned; an older
	// unpinned document exists further down the recency order. ListDocuments
	// applies its SQL LIMIT first, so a naive fetch-exactly-limit would dedupe
	// every pinned row away and leave an empty recent section. The over-fetch
	// must still surface the older unpinned doc.
	pinned := make([]navigate.Document, defaultRecentLimit)
	recent := make([]navigate.Document, 0, defaultRecentLimit+1)
	for i := range pinned {
		d := navigate.Document{
			DocumentUID: "PIN" + string(rune('A'+i)),
			Path:        "notes/pin.md",
			Title:       "Pinned",
			Body:        "pinned body",
			Pinned:      true,
		}
		pinned[i] = d
		recent = append(recent, d) // pinned docs are also the newest recent rows
	}
	older := navigate.Document{DocumentUID: "OLD1", Path: "notes/older.md", Title: "Older", Body: "older unpinned body"}
	recent = append(recent, older) // oldest, unpinned

	a := New(&fakeReader{pinned: pinned, recent: recent}, nil)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 8000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(brief.Markdown, "## Recently updated") {
		t.Errorf("recent section dropped when newest rows are all pinned:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "notes/older.md") {
		t.Errorf("older unpinned doc missing from recent section:\n%s", brief.Markdown)
	}
}

func TestBuild_RespectsBudgetAndReportsSections(t *testing.T) {
	t.Parallel()
	// Many recent docs, a tiny budget → truncation + section stats reflect only
	// what was included.
	recent := make([]navigate.Document, 30)
	for i := range recent {
		recent[i] = navigate.Document{
			DocumentUID: "D" + string(rune('A'+i%26)) + string(rune('0'+i/26)),
			Path:        "notes/doc.md",
			Title:       "Doc",
			Body:        strings.Repeat("content ", 50),
		}
	}
	a := New(&fakeReader{recent: recent}, nil)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 200})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if brief.Tokens > 200 {
		t.Errorf("brief tokens %d exceed budget 200", brief.Tokens)
	}
	if brief.Dropped == 0 {
		t.Errorf("expected truncation with a 200-token budget over 30 docs")
	}
	if !strings.Contains(brief.Markdown, "omitted to fit") {
		t.Errorf("truncation marker missing:\n%s", brief.Markdown)
	}
}

// fakeRanker is a simple EntityRanker stub for assembler-level tests.
type fakeRanker struct {
	entities []navigate.Entity
}

func (f *fakeRanker) TopEntities(_ context.Context, n int) ([]navigate.Entity, error) {
	if n < len(f.entities) {
		return f.entities[:n], nil
	}
	return f.entities, nil
}

func TestBuild_EntityRanker_OrdersKeyEntitiesByRecallCount(t *testing.T) {
	t.Parallel()
	// When a ranker is installed the brief must list entities in the order the
	// ranker returns them (recallCount DESC), NOT the order the reader's
	// ListEntities would return them.
	reader := &fakeReader{
		// ListEntities on this reader would return Theoden first (alphabetical
		// accident here; the ranker must override this).
		entities: []navigate.Entity{
			{UID: "uid-zara", Kind: "person", DisplayName: "Theoden"},
			{UID: "uid-alice", Kind: "person", DisplayName: "Merry"},
		},
	}
	// Ranker returns Merry first (higher recallCount).
	ranker := &fakeRanker{entities: []navigate.Entity{
		{UID: "uid-alice", Kind: "person", DisplayName: "Merry"},
		{UID: "uid-zara", Kind: "person", DisplayName: "Theoden"},
	}}

	a := New(reader, nil).WithEntityRanker(ranker)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	alicePos := strings.Index(md, "Merry")
	zaraPos := strings.Index(md, "Theoden")
	if alicePos < 0 || zaraPos < 0 {
		t.Fatalf("entities missing from brief:\n%s", md)
	}
	if alicePos > zaraPos {
		t.Errorf("expected Merry (higher recall) before Theoden in brief:\n%s", md)
	}
}

// TestBuild_EntityRanker_BackfillsByMentions verifies that when the usage ranker
// returns fewer entities than the limit (a brain with little recall history),
// the Key-entities block is topped up with mention-ranked entities — keeping the
// usage-ranked ones first and not duplicating any. Regression for the unbiased
// finding that getContext surfaced only a handful of entities with most of the
// token budget unused.
func TestBuild_EntityRanker_BackfillsByMentions(t *testing.T) {
	t.Parallel()
	// Ranker knows only one recalled entity.
	ranker := &fakeRanker{entities: []navigate.Entity{
		{UID: "uid-used", Kind: "person", DisplayName: "Frodo"},
	}}
	// The reader's mention-ordered list has the used entity plus two more.
	reader := &fakeReader{
		mentionOrdered: []navigate.Entity{
			{UID: "uid-used", Kind: "person", DisplayName: "Frodo"},
			{UID: "uid-m1", Kind: "person", DisplayName: "Samwise"},
			{UID: "uid-m2", Kind: "place", DisplayName: "Rivendell"},
		},
	}
	a := New(reader, nil).WithEntityRanker(ranker)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	for _, want := range []string{"Frodo", "Samwise", "Rivendell"} {
		if !strings.Contains(md, want) {
			t.Errorf("brief missing backfilled entity %q:\n%s", want, md)
		}
	}
	// Frodo (usage-ranked) must precede the mention-only backfill entities.
	if strings.Index(md, "Frodo") > strings.Index(md, "Samwise") {
		t.Errorf("usage-ranked Frodo should precede backfilled Samwise:\n%s", md)
	}
	// Dedup by UID: Frodo appears exactly once despite being in both lists.
	if got := strings.Count(md, "uid-used"); got != 1 {
		t.Errorf("Frodo (uid-used) should appear exactly once, got %d:\n%s", got, md)
	}
}

func TestBuild_EntityRanker_ZeroStatsFallsBackToReaderOrder(t *testing.T) {
	t.Parallel()
	// When the stats store is empty the StatsRanker falls back to recency.
	// The brief must still show entities (not an empty section).
	navFake := &fakeEntityNav{
		fallback: []navigate.Entity{
			{UID: "uid-r1", Kind: "person", DisplayName: "Recency Person"},
		},
	}
	statsFake := &fakeStatsLister{items: nil} // cold store

	reader := &fakeReader{} // reader.ListEntities never reached when ranker is wired
	ranker := NewStatsRanker(statsFake, navFake, stats.Last7Days)

	a := New(reader, nil).WithEntityRanker(ranker)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(brief.Markdown, "## Key entities") {
		t.Errorf("Key entities section missing on cold store:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "Recency Person") {
		t.Errorf("fallback entity missing from brief:\n%s", brief.Markdown)
	}
}

func TestBuild_EntityRanker_Determinism(t *testing.T) {
	t.Parallel()
	// Two successive Build calls with identical state must produce bit-for-bit
	// identical markdown (the brief is used as document content for index.md
	// and byte-stability avoids spurious git diffs).
	ranker := &fakeRanker{entities: []navigate.Entity{
		{UID: "uid-1", Kind: "person", DisplayName: "Merry"},
		{UID: "uid-2", Kind: "org", DisplayName: "Mithril"},
	}}
	reader := &fakeReader{
		recent: []navigate.Document{
			{DocumentUID: "d1", Path: "notes/a.md", Title: "A", Body: "body a"},
		},
	}
	a := New(reader, nil).WithEntityRanker(ranker)

	first, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	second, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if first.Markdown != second.Markdown {
		t.Errorf("Build not deterministic:\nfirst:\n%s\nsecond:\n%s", first.Markdown, second.Markdown)
	}
}

func TestBuild_WithoutRanker_UsesReaderEntities(t *testing.T) {
	t.Parallel()
	// Without a ranker the assembler falls back to reader.ListEntities, so the
	// existing recency behaviour is preserved unchanged.
	reader := &fakeReader{
		entities: []navigate.Entity{
			{UID: "uid-e1", Kind: "person", DisplayName: "Legacy Entity"},
		},
	}
	a := New(reader, nil) // no WithEntityRanker call
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(brief.Markdown, "Legacy Entity") {
		t.Errorf("reader entity missing from brief without ranker:\n%s", brief.Markdown)
	}
}

// TestBuild_NoRanker_RequestsMentionOrder verifies that when no EntityRanker is
// wired the assembler requests SortByMentions=true from the reader, so the most-
// referenced entities appear first in the "Key entities" section instead of the
// most-recently-created ones. The fakeReader returns mentionOrdered when the flag
// is set, simulating a Navigator that would run the mention-count subquery.
func TestBuild_NoRanker_RequestsMentionOrder(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		// entities would be returned by a recency sort; a real brain might show
		// a recently-created but isolated entity here.
		entities: []navigate.Entity{
			{UID: "uid-new", Kind: "person", DisplayName: "New Person"},
		},
		// mentionOrdered simulates what SortByMentions=true returns: the entity
		// with the highest mention count, which may be older but more central.
		mentionOrdered: []navigate.Entity{
			{UID: "uid-central", Kind: "org", DisplayName: "Central Org"},
			{UID: "uid-new", Kind: "person", DisplayName: "New Person"},
		},
	}
	a := New(reader, nil) // no WithEntityRanker
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(brief.Markdown, "Central Org") {
		t.Errorf("most-mentioned entity missing from brief:\n%s", brief.Markdown)
	}
	centralPos := strings.Index(brief.Markdown, "Central Org")
	newPos := strings.Index(brief.Markdown, "New Person")
	if centralPos < 0 || newPos < 0 {
		t.Fatalf("entities missing from brief:\n%s", brief.Markdown)
	}
	if centralPos > newPos {
		t.Errorf("expected most-mentioned entity (Central Org) before newer entity (New Person):\n%s", brief.Markdown)
	}
}

// TestBuild_SelfPrincipalCollapsedToOneRow proves that when both the local self
// anchor (uid="self") and the bound principal ULID are eligible "Key entities"
// candidates they are collapsed into exactly ONE row, mirroring `maz self`.
//
// Two sub-cases are exercised:
//   - explicit injection via WithSelfUIDs (used by the CLI and test callers)
//   - unbound: only the anchor present, no principal — anchor must still render
func TestBuild_SelfPrincipalCollapsedToOneRow(t *testing.T) {
	t.Parallel()

	const anchorUID = "self"
	const principalUID = "01HPRINCIPAL000000000000001"

	t.Run("both self and principal collapse to one row", func(t *testing.T) {
		t.Parallel()
		reader := &fakeReader{
			entities: []navigate.Entity{
				{UID: anchorUID, Kind: "person", DisplayName: "self"},
				{UID: principalUID, Kind: "person", DisplayName: "mo-cmyk"},
				{UID: "uid-other", Kind: "org", DisplayName: "Acme Corp"},
			},
		}
		a := New(reader, nil).WithSelfUIDs(anchorUID, principalUID)
		brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := brief.Markdown

		// The principal row must appear.
		if !strings.Contains(md, principalUID) {
			t.Errorf("principal UID %q missing from brief:\n%s", principalUID, md)
		}
		// The anchor row must NOT appear as a separate entry.
		if strings.Count(md, "entities/"+anchorUID) > 0 {
			t.Errorf("self anchor leaked as separate Key entities row:\n%s", md)
		}
		// Unrelated entity must still appear.
		if !strings.Contains(md, "Acme Corp") {
			t.Errorf("unrelated entity missing from brief:\n%s", md)
		}
		// Exactly one self-related row: the principal.
		selfRows := strings.Count(md, principalUID)
		if selfRows != 1 {
			t.Errorf("expected exactly 1 principal row, got %d:\n%s", selfRows, md)
		}
	})

	t.Run("anchor only when unbound renders the anchor", func(t *testing.T) {
		t.Parallel()
		reader := &fakeReader{
			entities: []navigate.Entity{
				{UID: anchorUID, Kind: "person", DisplayName: "self"},
				{UID: "uid-other", Kind: "org", DisplayName: "Acme Corp"},
			},
		}
		// WithSelfUIDs with empty principal = unbound; anchor should render.
		a := New(reader, nil).WithSelfUIDs(anchorUID, "")
		brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		md := brief.Markdown
		if !strings.Contains(md, "entities/"+anchorUID) {
			t.Errorf("unbound self anchor missing from brief:\n%s", md)
		}
	})
}
