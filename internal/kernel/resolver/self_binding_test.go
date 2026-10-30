package resolver

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestBindCandidateEndpoints_SelfSubjectBindsToSeededUID covers the
// Contract: a RelationCandidate whose SubjectMentionIdx is the
// SelfSubjectSentinel (-1) — emitted by the lexical self-statement
// detector — must bind to the canonical seeded 'self' entity rather
// than dereferencing the mention batch. Without this binding the
// supersede pipeline can't close a prior "I changed my name to X"
// row when a new one arrives.
func TestBindCandidateEndpoints_SelfSubjectBindsToSeededUID(t *testing.T) {
	t.Parallel()
	r := &Resolver{logger: zap.NewNop()}
	mentions := []MatchResult{
		{EntityUID: "01LUCAS0000000000000000001"},
	}
	cand := ontology.RelationCandidate{
		SubjectMentionIdx: ontology.SelfSubjectSentinel,
		Predicate:         "has_name",
		ObjectMentionIdx:  0,
	}
	subjUID, objUID, ok := r.bindCandidateEndpoints(cand, mentions, SelfEntityUID, 0)
	if !ok {
		t.Fatal("bindCandidateEndpoints returned ok=false for valid self candidate")
	}
	if subjUID != SelfEntityUID {
		t.Errorf("subjUID = %q, want %q", subjUID, SelfEntityUID)
	}
	if objUID != "01LUCAS0000000000000000001" {
		t.Errorf("objUID = %q, want object mention's EntityUID", objUID)
	}
}

// TestBindCandidateEndpoints_RegularCandidateUnchanged confirms the
// non-self path still works after the refactor: a regular candidate
// with SubjectMentionIdx >= 0 must resolve subject + object via the
// MatchResult slice as before.
func TestBindCandidateEndpoints_RegularCandidateUnchanged(t *testing.T) {
	t.Parallel()
	r := &Resolver{logger: zap.NewNop()}
	mentions := []MatchResult{
		{EntityUID: "01FRODO000000000000000001"},
		{EntityUID: "01EREBOR00000000000000000001"},
	}
	cand := ontology.RelationCandidate{
		SubjectMentionIdx: 0,
		Predicate:         "works_at",
		ObjectMentionIdx:  1,
	}
	subjUID, objUID, ok := r.bindCandidateEndpoints(cand, mentions, SelfEntityUID, 0)
	if !ok {
		t.Fatal("ok=false for valid regular candidate")
	}
	if subjUID != "01FRODO000000000000000001" {
		t.Errorf("subjUID = %q, want subject mention's EntityUID", subjUID)
	}
	if objUID != "01EREBOR00000000000000000001" {
		t.Errorf("objUID = %q, want object mention's EntityUID", objUID)
	}
}

// TestBindCandidateEndpoints_OutOfRangeObjectIsSkipped covers the
// defensive path: a candidate with a bogus object index returns
// ok=false so the caller logs and skips. Important because the
// detector + LLM extractor each contribute to the same batch and a
// downstream filter (e.g. grounding threshold) can drop mentions
// after the indices were assigned.
func TestBindCandidateEndpoints_OutOfRangeObjectIsSkipped(t *testing.T) {
	t.Parallel()
	r := &Resolver{logger: zap.NewNop()}
	mentions := []MatchResult{
		{EntityUID: "01ONLYONE000000000000000001"},
	}
	cand := ontology.RelationCandidate{
		SubjectMentionIdx: ontology.SelfSubjectSentinel,
		Predicate:         "has_name",
		ObjectMentionIdx:  42, // beyond mentions[]
	}
	_, _, ok := r.bindCandidateEndpoints(cand, mentions, SelfEntityUID, 0)
	if ok {
		t.Error("ok=true for out-of-range object index; want false")
	}
}

// TestBindCandidateEndpoints_EmptyEntityUIDIsSkipped guards against
// candidates whose mention resolved to an empty EntityUID (a match
// failure upstream). These would otherwise insert "self -> has_name
// -> <empty>" rows into the relations table.
func TestBindCandidateEndpoints_EmptyEntityUIDIsSkipped(t *testing.T) {
	t.Parallel()
	r := &Resolver{logger: zap.NewNop()}
	mentions := []MatchResult{
		{EntityUID: ""}, // unresolved
	}
	cand := ontology.RelationCandidate{
		SubjectMentionIdx: ontology.SelfSubjectSentinel,
		Predicate:         "has_name",
		ObjectMentionIdx:  0,
	}
	_, _, ok := r.bindCandidateEndpoints(cand, mentions, SelfEntityUID, 0)
	if ok {
		t.Error("ok=true for unresolved object mention; want false")
	}
}

// fakeSelfBinder is a SelfBinder whose result is fixed at construction: bound
// returns (principalUID, true); unbound returns ("", false).
type fakeSelfBinder struct {
	principalUID string
	bound        bool
}

func (b fakeSelfBinder) SelfPrincipal(context.Context) (string, bool) {
	return b.principalUID, b.bound
}

const testResolverPrincipalUID = "01PRINCIPAL000000000000001"

// TestResolveSelfSubject_BindsToPrincipalThroughBinder is the F4 end-to-end
// guard the mechanical tests above miss: with a REAL bound binder,
// resolveSelfSubject returns the principal UID, and that subject flows through
// bindCandidateEndpoints to anchor a first-person statement on the principal —
// the full self->principal rebind chain the daemon wires, not the seeded-UID
// passthrough.
func TestResolveSelfSubject_BindsToPrincipalThroughBinder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &Resolver{logger: zap.NewNop(), binder: fakeSelfBinder{principalUID: testResolverPrincipalUID, bound: true}}

	selfUID := r.resolveSelfSubject(ctx)
	if selfUID != testResolverPrincipalUID {
		t.Fatalf("resolveSelfSubject = %q, want bound principal %q", selfUID, testResolverPrincipalUID)
	}

	mentions := []MatchResult{{EntityUID: "01LUCAS0000000000000000001"}}
	cand := ontology.RelationCandidate{
		SubjectMentionIdx: ontology.SelfSubjectSentinel,
		Predicate:         "has_name",
		ObjectMentionIdx:  0,
	}
	subjUID, objUID, ok := r.bindCandidateEndpoints(cand, mentions, selfUID, 0)
	if !ok {
		t.Fatal("bindCandidateEndpoints ok=false for valid self candidate")
	}
	if subjUID != testResolverPrincipalUID {
		t.Errorf("subjUID = %q, want principal %q (rebound via binder)", subjUID, testResolverPrincipalUID)
	}
	if objUID != "01LUCAS0000000000000000001" {
		t.Errorf("objUID = %q, want object mention EntityUID", objUID)
	}
}

// TestResolveSelfSubject_FallsBackToSeedWhenUnboundOrNil confirms pre-identity
// behavior: a nil binder OR an unbound device leaves first-person subjects on
// the seeded SelfEntityUID.
func TestResolveSelfSubject_FallsBackToSeedWhenUnboundOrNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := map[string]*Resolver{
		"nil binder":     {logger: zap.NewNop()},
		"unbound device": {logger: zap.NewNop(), binder: fakeSelfBinder{bound: false}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if got := r.resolveSelfSubject(ctx); got != SelfEntityUID {
				t.Errorf("resolveSelfSubject = %q, want %q", got, SelfEntityUID)
			}
		})
	}
}
