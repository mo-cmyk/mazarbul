package contextbrief

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// fakeCardReader is a configurable CardReader for the entity-dossier tests. It
// records the bi-temporal filters it received so a test can assert the `at`
// anchor reached the relation + fact sub-queries (ADR-0001).
type fakeCardReader struct {
	entity        navigate.Entity
	getErr        error
	relations     []navigate.Relation
	relErr        error
	facts         []navigate.Fact
	factErr       error
	gotRelFilter  navigate.RelationFilter
	gotFactFilter navigate.FactFilter
}

func (f *fakeCardReader) GetEntity(_ context.Context, _ string) (navigate.Entity, error) {
	if f.getErr != nil {
		return navigate.Entity{}, f.getErr
	}
	return f.entity, nil
}

func (f *fakeCardReader) EntityRelations(_ context.Context, _ string, opts navigate.RelationFilter) ([]navigate.Relation, error) {
	f.gotRelFilter = opts
	return f.relations, f.relErr
}

func (f *fakeCardReader) FactsForSubject(_ context.Context, _ string, opts navigate.FactFilter) ([]navigate.Fact, error) {
	f.gotFactFilter = opts
	return f.facts, f.factErr
}

// seededCardReader returns a reader over a small graph: Frodo works_at the Shire
// (outgoing) and is mentored_by Gandalf (incoming), with two facts.
func seededCardReader() *fakeCardReader {
	return &fakeCardReader{
		entity: navigate.Entity{UID: "frodo", Kind: "person", DisplayName: "Frodo Baggins", ResolutionStatus: "resolved"},
		relations: []navigate.Relation{
			{UID: "r1", SubjectUID: "frodo", Predicate: "works_at", ObjectUID: "shire", ObjectName: "The Shire", Status: "asserted"},
			{UID: "r2", SubjectUID: "gandalf", SubjectName: "Gandalf", Predicate: "mentors", ObjectUID: "frodo", Status: "asserted"},
		},
		facts: []navigate.Fact{
			{UID: "f1", SubjectUID: "frodo", StateKey: "status", Value: "ring-bearer"},
			{UID: "f2", SubjectUID: "frodo", StateKey: "home", Value: "Bag End"},
		},
	}
}

func TestEntityCard_AssemblesAllSectionsAndFooter(t *testing.T) {
	t.Parallel()
	reader := seededCardReader()
	rec := &fakeRecaller{hits: []kernel.Hit{
		{HitType: kernel.HitTypeEpisode, Text: "Frodo set out from the Shire", Citations: []string{"ep1"}},
	}}
	a := New(nil, rec)

	brief, err := a.EntityCard(context.Background(), reader, CardRequest{UID: "frodo", BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("EntityCard: %v", err)
	}
	md := brief.Markdown

	// Header + the three packable section titles must all be present.
	for _, want := range []string{
		"# Frodo Baggins", "entities/frodo", "resolved",
		"## Connections", "works_at", "The Shire", "mentors",
		"## What I know", "status: ring-bearer", "home: Bag End",
		"## Mentioned in", "Frodo set out from the Shire",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("dossier missing %q\n---\n%s", want, md)
		}
	}

	// Footer counts must match the seeded graph: 2 relations, 2 facts.
	if !strings.Contains(md, "2 relations, 2 facts · budget 4000 · used ") {
		t.Errorf("footer counts/budget wrong:\n%s", md)
	}

	// The dossier must never exceed its budget. Tokens is the package's
	// sum-of-per-piece-ceils estimate (the same convention pack uses), so it is a
	// conservative over-estimate of tokensFor over the whole string.
	if brief.Tokens > brief.Budget {
		t.Errorf("tokens %d exceed budget %d", brief.Tokens, brief.Budget)
	}
	if brief.Tokens < tokensFor(brief.Markdown) {
		t.Errorf("Tokens %d under-counts the final markdown estimate %d", brief.Tokens, tokensFor(brief.Markdown))
	}
	// The footer's "used" figure must equal the reported Tokens (fixed point).
	if !strings.Contains(md, fmt.Sprintf("used %d_", brief.Tokens)) {
		t.Errorf("footer 'used' must equal Tokens=%d:\n%s", brief.Tokens, md)
	}
}

func TestEntityCard_IncludeFiltersSections(t *testing.T) {
	t.Parallel()
	reader := seededCardReader()
	a := New(nil, &fakeRecaller{}) // recall present but mentions not requested

	brief, err := a.EntityCard(context.Background(), reader, CardRequest{
		UID:     "frodo",
		Include: []string{CardSectionFacts},
	})
	if err != nil {
		t.Fatalf("EntityCard: %v", err)
	}
	md := brief.Markdown
	if !strings.Contains(md, "## What I know") {
		t.Errorf("include=[facts] must keep What-I-know:\n%s", md)
	}
	if strings.Contains(md, "## Connections") || strings.Contains(md, "## Mentioned in") {
		t.Errorf("include=[facts] must drop other sections:\n%s", md)
	}
	// Connections were not fetched, so the footer reports 0 relations but the true
	// fact count.
	if !strings.Contains(md, "0 relations, 2 facts ·") {
		t.Errorf("footer should report 0 relations, 2 facts:\n%s", md)
	}
}

// TestEntityCard_AtAnchorsSubqueriesNotHeader pins ADR-0001: the `at` valid-time
// anchor reaches the relation + fact sub-queries, while the entity-identity
// header row is unchanged.
func TestEntityCard_AtAnchorsSubqueriesNotHeader(t *testing.T) {
	t.Parallel()
	reader := seededCardReader()
	a := New(nil, nil)
	at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	brief, err := a.EntityCard(context.Background(), reader, CardRequest{UID: "frodo", At: &at})
	if err != nil {
		t.Fatalf("EntityCard: %v", err)
	}

	if reader.gotRelFilter.At == nil || !reader.gotRelFilter.At.Equal(at) {
		t.Errorf("relation filter At = %v, want %v", reader.gotRelFilter.At, at)
	}
	if reader.gotFactFilter.At == nil || !reader.gotFactFilter.At.Equal(at) {
		t.Errorf("fact filter At = %v, want %v", reader.gotFactFilter.At, at)
	}
	// The header (identity) is exempt from the anchor: the display name still renders.
	if !strings.Contains(brief.Markdown, "# Frodo Baggins") {
		t.Errorf("header must be unchanged by the at anchor:\n%s", brief.Markdown)
	}
}

// TestEntityCard_NilRecallOmitsMentions verifies an Assembler built without a
// recall lane simply omits the Mentioned-in section (no panic), like the brief's
// relevant lane.
func TestEntityCard_NilRecallOmitsMentions(t *testing.T) {
	t.Parallel()
	a := New(nil, nil)
	brief, err := a.EntityCard(context.Background(), seededCardReader(), CardRequest{UID: "frodo"})
	if err != nil {
		t.Fatalf("EntityCard: %v", err)
	}
	if strings.Contains(brief.Markdown, "## Mentioned in") {
		t.Errorf("nil recall must omit Mentioned-in:\n%s", brief.Markdown)
	}
}

// TestEntityCard_SkipsSelfMention verifies the entity's own bare-entity recall
// hit is not listed as a mention of itself.
func TestEntityCard_SkipsSelfMention(t *testing.T) {
	t.Parallel()
	reader := seededCardReader()
	rec := &fakeRecaller{hits: []kernel.Hit{
		{HitType: kernel.HitTypeEntity, Text: "Frodo Baggins", Entities: []kernel.EntityRef{{UID: "frodo"}}, Citations: []string{"entities/frodo"}},
		{HitType: kernel.HitTypeEpisode, Text: "Bilbo gave Frodo the ring", Citations: []string{"ep9"}},
	}}
	a := New(nil, rec)

	brief, err := a.EntityCard(context.Background(), reader, CardRequest{UID: "frodo"})
	if err != nil {
		t.Fatalf("EntityCard: %v", err)
	}
	md := brief.Markdown
	if !strings.Contains(md, "Bilbo gave Frodo the ring") {
		t.Errorf("episode mention should render:\n%s", md)
	}
	// The self-hit's bare display name must not appear as a Mentioned-in bullet.
	if strings.Contains(md, "- Frodo Baggins —") || strings.Contains(md, "- Frodo Baggins\n") {
		t.Errorf("self bare-entity hit must be skipped as a mention:\n%s", md)
	}
}

// TestEntityCard_NeverExceedsTinyBudget pins the never-exceed invariant for an
// absurdly small budget: the body is clamped and the footer reservation holds.
func TestEntityCard_NeverExceedsTinyBudget(t *testing.T) {
	t.Parallel()
	a := New(nil, nil)
	brief, err := a.EntityCard(context.Background(), seededCardReader(), CardRequest{UID: "frodo", BudgetTokens: 30})
	if err != nil {
		t.Fatalf("EntityCard: %v", err)
	}
	if brief.Tokens > brief.Budget {
		t.Errorf("tokens %d exceed tiny budget %d:\n%s", brief.Tokens, brief.Budget, brief.Markdown)
	}
}

// TestEntityCard_GetEntityNotFound surfaces the reader's ErrNotFound verbatim so
// the tool layer can map it to E_NOT_FOUND.
func TestEntityCard_GetEntityNotFound(t *testing.T) {
	t.Parallel()
	a := New(nil, nil)
	reader := &fakeCardReader{getErr: navigate.ErrNotFound}
	_, err := a.EntityCard(context.Background(), reader, CardRequest{UID: "ghost"})
	if !errors.Is(err, navigate.ErrNotFound) {
		t.Fatalf("err = %v, want navigate.ErrNotFound", err)
	}
}
