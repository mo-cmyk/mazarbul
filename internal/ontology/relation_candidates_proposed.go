package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindRelationCandidatesProposed is the event kind appended by the extractor
// after each Episode is processed. The payload carries the full list of raw
// relation candidates extracted from that Episode; the resolver (P3) reads
// these events and promotes them to canonical Relation events by resolving
// mention indices to Entity UIDs.
const KindRelationCandidatesProposed = "relationCandidatesProposed"

// VersionRelationCandidatesProposed is the semver-shaped schema version for
// RelationCandidatesProposedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
//
// V0.2.0 (2026-05-26) added optional RelationCandidate.ValidUntil to carry
// the upper bound of validity parsed from source text ("until 2020", "stopped
// in 2024"). Additive only: deserialising a v0.1.0 payload still works.
//
// V0.3.0 (2026-05-27) added optional RelationCandidate.Attributes so the
// extractor can preserve qualifying role/title/employment-type metadata
// pulled from the source text. Additive only.
//
// V0.4.0 (2026-05-30) relaxes SubjectMentionIdx minimum from 0 to -1.
// The new -1 sentinel means "subject is the canonical 'self' entity" —
// emitted by the lexical self-statement detector for first-person
// assertions ("I changed my name to X", "I live in Y", "I work at Z")
// that the LLM extractor's GROUNDING CONTRACT excludes. The resolver
// recognises the sentinel and binds the subject to the seeded 'self'
// entity UID instead of dereferencing a mention index. Additive only:
// every pre-existing event has SubjectMentionIdx >= 0.
const VersionRelationCandidatesProposed = "v0.4.0"

// SelfSubjectSentinel is the SubjectMentionIdx value that signals the
// subject of a RelationCandidate is the canonical self entity rather
// than a mention in the same extraction batch. Emitted by the lexical
// self-statement detector for first-person assertions; recognised by
// the resolver, which binds the subject to the seeded 'self' UID
// before the supersede pipeline runs.
const SelfSubjectSentinel = -1

// RelationCandidate is one raw directed edge extracted from an Episode. The
// subject and object are identified by their zero-based index into the
// []Mention slice accompanying the same extraction batch; the resolver (P3)
// replaces these indices with canonical Entity UIDs before emitting a
// Relation event. ValidFrom carries the model's stated temporal lower bound
// for the relationship (e.g. "since 2022" parsed from the source text).
type RelationCandidate struct {
	// SubjectMentionIdx is the zero-based index of the subject Mention in
	// the batch returned by the same Extract call. Required.
	//
	// The sentinel value SelfSubjectSentinel (-1) means "subject is the
	// canonical self entity"; emitted by the lexical self-statement
	// detector for first-person assertions and bound by the resolver
	// to the seeded 'self' UID. Use IsSelfSubject() rather than
	// comparing to -1 directly so call sites stay readable.
	SubjectMentionIdx int `json:"subjectMentionIdx" jsonschema:"required,minimum=-1,description=Zero-based index of the subject Mention in the extraction batch, or -1 for the canonical self entity. Required."`

	// Predicate names the relationship type, e.g. works_at or member_of.
	// Required. Non-empty. Snake_case by convention; the resolver normalises
	// to the canonical seed list entry if a close variant is supplied.
	Predicate string `json:"predicate" jsonschema:"required,minLength=1,description=Relationship type in snake_case, e.g. works_at. Required."`

	// ObjectMentionIdx is the zero-based index of the object Mention in the
	// batch returned by the same Extract call. Required.
	ObjectMentionIdx int `json:"objectMentionIdx" jsonschema:"required,minimum=0,description=Zero-based index of the object Mention in the extraction batch. Required."`

	// ValidFrom is the optional inclusive lower bound of the relationship's
	// validity window, parsed from the source text. Pointer so omitempty
	// fires when absent. RFC 3339 UTC.
	ValidFrom *time.Time `json:"validFrom,omitempty" jsonschema:"format=date-time,description=Inclusive lower bound of validity, parsed from source text. RFC 3339 UTC. Optional."`

	// ValidUntil is the optional inclusive upper bound of the relationship's
	// validity window, parsed from the source text. Set when the model can
	// determine that a relation has ended (e.g. "stopped working at Erebor in
	// 2024", "lived in Berlin until 2020"). The resolver propagates this into
	// the Relation's ValidUntil field so bi-temporal queries return the right
	// row for past instants without waiting for a contradicting supersede.
	// Pointer so omitempty fires when absent. RFC 3339 UTC.
	ValidUntil *time.Time `json:"validUntil,omitempty" jsonschema:"format=date-time,description=Inclusive upper bound of validity, parsed from source text. RFC 3339 UTC. Optional."`

	// Attributes carries qualifying key/value pairs from the source text:
	// role, title, employment_type, rank. Optional. Propagated by the
	// resolver onto the Relation.Attributes field so the graph lane can
	// reason about role-qualified edges (boss-of, member-of-with-role).
	Attributes map[string]string `json:"attributes,omitempty" jsonschema:"description=Qualifying key/value pairs (role, title, employment_type). Optional."`
}

// IsSelfSubject reports whether SubjectMentionIdx is the
// SelfSubjectSentinel marker — i.e. the resolver should bind the
// subject to the canonical 'self' entity rather than dereferencing a
// mention index. Use this helper at call sites so the intent reads
// even when the sentinel value changes.
func (c RelationCandidate) IsSelfSubject() bool {
	return c.SubjectMentionIdx == SelfSubjectSentinel
}

// RelationCandidatesProposedEvent is the payload of a
// relationCandidatesProposed event. It bundles all raw relation candidates
// extracted from a single Episode so the resolver can process them as a
// cohesive batch, resolving mention indices in the context of the
// accompanying Mention events.
type RelationCandidatesProposedEvent struct {
	// EpisodeUID is the ULID of the Episode from which these candidates
	// were extracted. Required. Used by the resolver to join against the
	// accompanying mentionRecorded events.
	EpisodeUID string `json:"episodeUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the source Episode. Required."`

	// Candidates is the ordered slice of relation candidates. Index 0 is
	// the first relation the model proposed; order is preserved from the
	// LLM response so SubjectMentionIdx / ObjectMentionIdx point into the
	// Mention slice that the extractor emits alongside this event.
	// Required. May be empty if the model found no relations.
	Candidates []RelationCandidate `json:"candidates" jsonschema:"required,description=Ordered slice of raw relation candidates. Required."`
}

// relationCandidatesProposedSchema lazily builds the schema once and caches
// it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var relationCandidatesProposedSchema = sync.OnceValue(buildRelationCandidatesProposedSchema)

// buildRelationCandidatesProposedSchema reflects RelationCandidatesProposedEvent
// into a kernel.Schema. Runs once at first call; panics on failure because
// invopop cannot fail on a well-formed in-tree struct.
func buildRelationCandidatesProposedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&RelationCandidatesProposedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal relationCandidatesProposed schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindRelationCandidatesProposed,
		Version:    VersionRelationCandidatesProposed,
		Definition: raw,
		Provenance: "core",
	}
}

// RelationCandidatesProposedSchema returns the cached kernel.Schema for
// RelationCandidatesProposedEvent. Useful for tests that need to inspect the
// schema without registering it.
func RelationCandidatesProposedSchema() kernel.Schema {
	return relationCandidatesProposedSchema()
}

// RegisterRelationCandidatesProposed installs the
// RelationCandidatesProposedEvent schema with o. Call once at process
// startup (or once per test). The kernel.Ontology contract specifies
// "first registration wins": a second call without a higher Version returns
// kernel.ErrSchemaConflict.
func RegisterRelationCandidatesProposed(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, relationCandidatesProposedSchema())
}
