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

// KindRelation is the ontology kind under which Relation is registered.
// Relation is a typed edge between two referents in the knowledge graph:
// Entity-to-Entity (the common case) or Entity-to-Episode (e.g.
// `mentions`). The schema lands here at M1 so downstream extractors and
// the graph writer (Kuzu, M2) have a contract to validate against;
// extraction itself is out of scope until M2.
const KindRelation = "relation"

// VersionRelation is the semver-shaped schema version for Relation. Bumped
// to v0.2.0 to add validityScore and supersededByUid (P0 schema
// additions). V0.x.y signals that breaking changes are allowed before
// v1.0; the M2 epic introduces the migration story (#13). Must be a full
// MAJOR.MINOR.PATCH triplet because kernel.MemoryOntology.Register
// validates with golang.org/x/mod/semver.IsValid, which rejects shorter
// forms like "v0.1" (Greptile #82 P1).
//
// V0.3.0 (2026-05-27) adds Attributes: a JSON-shaped map of qualifying
// key/value pairs (most commonly role, title, employment_type) that the
// extractor populates from the source text. Without this field a triple
// like (Rosie, works_at, Erebor) loses the "CEO" qualifier present in the
// source. Additive only: callers that omit Attributes still validate.
const VersionRelation = "v0.3.0"

// Relation is the typed edge that connects two referents in Mazarbul's
// knowledge graph. The Go struct is the source of truth; the JSON
// Schema is reflected from it at first access via invopop/jsonschema
// and cached, so the wire shape and the Go type cannot drift.
//
// A Relation always carries provenance: SourceEventUID anchors the
// W3C PROV chain (the event-log row holds source, agent, session, and
// timestamp; see architecture spec section 08), and the optional
// Provenance string captures free-text rationale the extractor emits.
//
// ValidFrom and ValidUntil reserve the bi-temporal validity window
// (architecture spec section 06). Bi-temporal queries land in M2; M1
// only ships the contract.
//
// Field naming follows Google AIP: createTime (not createdAt), name as
// the resource name in the form "relations/{uid}", uid as the stable
// ULID identifier. Optional fields carry omitempty so absent fields
// are omitted from JSON rather than serialized as empty.
type Relation struct {
	// Name is the AIP resource name in the form "relations/{uid}".
	// Required. Output-only; assigned by the writer from UID.
	Name string `json:"name" jsonschema:"required,pattern=^relations/[0-9A-HJKMNP-TV-Z]{26}$,description=Resource name in the form relations/{uid}. Output-only."`

	// UID is the ULID identifier for this Relation. Required.
	// Output-only. Sortable lexicographically by creation time, opaque,
	// immutable.
	UID string `json:"uid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identifier. Output-only; assigned by the writer."`

	// SubjectUID is the ULID of the source of the edge, or the literal
	// sentinel "self" for relations whose subject is the seeded
	// canonical self entity (first-person self-statements emitted by
	// the lexical detector — see ontology.SelfSubjectSentinel). The
	// reference-target validation is the writer's job; the schema
	// enforces the lexical shape only. Required.
	SubjectUID string `json:"subjectUid" jsonschema:"required,pattern=^([0-9A-HJKMNP-TV-Z]{26}|self)$,description=ULID of the edge source, or the literal 'self' sentinel for the canonical self entity. References an Entity or Episode."`

	// Predicate names the relationship type, e.g. works_at, member_of,
	// mentions. Required. Open string at M1: the ontology does not
	// enforce a closed predicate list. Closed predicate enums land in
	// M3+ via plugin extensions.
	Predicate string `json:"predicate" jsonschema:"required,minLength=1,description=Relationship type. Open string in M1, e.g. works_at, member_of, mentions."`

	// ObjectUID is the ULID of the target of the edge, or the literal
	// sentinel "self" for relations whose object is the seeded canonical
	// self entity (e.g. a note with `entity: self` frontmatter that is
	// ABOUT the user, or a prose-extracted mention of the speaker as a
	// relation target). Symmetric with SubjectUID: both endpoints accept
	// the "self" sentinel alongside a 26-char ULID. Reference-target
	// validation is the writer's job; the schema enforces the lexical
	// shape only. Required.
	ObjectUID string `json:"objectUid" jsonschema:"required,pattern=^([0-9A-HJKMNP-TV-Z]{26}|self)$,description=ULID of the edge target, or the literal 'self' sentinel for the canonical self entity. References an Entity or Episode."`

	// Confidence is the writer's stated confidence in this Relation, in
	// the closed interval 0..1. Required. 1.0 for user-confirmed edges;
	// lower for inferred or low-evidence edges in the gray zone.
	Confidence float64 `json:"confidence" jsonschema:"required,minimum=0,maximum=1,description=Writer's confidence in the Relation. Range 0..1 inclusive."`

	// SourceEventUID is the ULID of the event-log entry that produced
	// this Relation. Required. Anchors the W3C PROV chain: the event-log
	// row holds source, agent, session, and timestamp, so structural
	// provenance is not duplicated on the Relation row itself.
	SourceEventUID string `json:"sourceEventUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the source event-log entry. Provenance anchor."`

	// CreateTime is the instant the Relation was first materialized in
	// UTC. Required. Output-only. Serialized as an RFC 3339 string.
	CreateTime time.Time `json:"createTime" jsonschema:"required,format=date-time,description=Time the Relation was first materialized. RFC 3339 UTC. Output-only."`

	// Provenance is free-form rationale for the Relation: an extractor
	// might write "inferred from co-mention in 3 emails", a user might
	// write "confirmed in maz review". Optional. Structural provenance
	// (source, agent, session, timestamp) lives on the event-log row
	// pointed at by SourceEventUID, not here.
	Provenance string `json:"provenance,omitempty" jsonschema:"description=Free-form rationale for the Relation. Optional."`

	// ValidFrom is the inclusive lower bound of the Relation's validity
	// window in UTC. Optional. Pointer so json:omitempty fires: a bare
	// time.Time would serialize as "0001-01-01T00:00:00Z" when absent.
	// Bi-temporal queries land in M2; M1 reserves the field.
	ValidFrom *time.Time `json:"validFrom,omitempty" jsonschema:"format=date-time,description=Inclusive lower bound of validity window. RFC 3339 UTC. Optional."`

	// ValidUntil is the inclusive upper bound of the Relation's
	// validity window in UTC. Optional. Pointer so json:omitempty fires
	// (see ValidFrom). Equality with ValidFrom is permitted: per
	// architecture spec section 06, instantaneous supersession produces
	// a zero-duration window where the prior fact gets
	// t_invalid == enrichment_date.
	ValidUntil *time.Time `json:"validUntil,omitempty" jsonschema:"format=date-time,description=Inclusive upper bound of validity window. RFC 3339 UTC. Optional."`

	// ValidityScore is the estimated probability that this Relation is
	// currently true, in the closed interval 0..1. Optional. Distinct
	// from Confidence: Confidence expresses the groundedness of the
	// original claim (how well-evidenced was the fact at extraction
	// time); ValidityScore expresses the probability the fact is still
	// true now (decays toward 0.0 after a supersede event, may be
	// updated by periodic re-evaluation). A Relation with high
	// Confidence but low ValidityScore was once well-evidenced but has
	// since been superseded or aged out. Optional.
	ValidityScore float64 `json:"validityScore,omitempty" jsonschema:"minimum=0,maximum=1,description=Probability this Relation is currently true. Distinct from Confidence (claim-groundedness at extraction time); decays on supersede. Range 0..1. Optional."`

	// SupersededByUID is the ULID of the Relation that invalidates
	// this one. Optional. Set by the resolver when a newer conflicting
	// Relation is appended for a functional predicate (or a non-functional
	// predicate where the LLM contradiction check returns contradictory).
	// Output-only: callers must not supply this field. The writer pipeline
	// materializes it as a deterministic fold over the relationRetracted
	// tombstone set (lowest superseding ULID), so the value converges across
	// paired devices regardless of replay order. Pointer so omitempty fires
	// on absence; matches the *time.Time pattern on ValidFrom/ValidUntil.
	SupersededByUID *ULID `json:"supersededByUid,omitempty" jsonschema:"description=ULID of the Relation that invalidates this one. Set on supersede. Output-only. Optional."`

	// Attributes carries qualifying key/value pairs the extractor pulled
	// from the source text, most commonly role, title, employment_type,
	// or rank. Optional. Without this field a triple like (Rosie,
	// works_at, Erebor) cannot distinguish "Rosie is a backend engineer
	// at Erebor" from "Rosie is the CEO of Erebor"; both collapse to the
	// same edge. The graph lane reads attributes during reasoning
	// queries (boss-of, member-of-with-role, transitive management
	// chains). Values are strings so the JSON Schema reflection lands
	// on a single shape; richer types belong on dedicated typed events.
	Attributes map[string]string `json:"attributes,omitempty" jsonschema:"description=Qualifying key/value pairs (role, title, employment_type). Optional."`

	// Status marks the trust posture of this Relation. RelationStatusAsserted
	// (the default, and the value for every user- or trusted-extractor-produced
	// edge) means the edge is authoritative: it can supersede and be superseded.
	// RelationStatusProposed marks an edge extracted from an UNTRUSTED external
	// source (a connector ingest, issue #211): it is confidence-capped, EXCLUDED
	// from supersession (it can neither close a user-asserted edge nor be closed
	// by one), and never mints a new entity kind or predicate. Optional on the
	// wire (empty = asserted); the projection stores the resolved non-empty value.
	Status string `json:"status,omitempty" jsonschema:"enum=asserted,enum=proposed,description=Trust posture: asserted (default authoritative) or proposed (untrusted external-source ingest excluded from supersession). Optional; empty = asserted."`
}

// relationSchema lazily builds the Relation JSON Schema document once
// and caches it for the process lifetime. Invopop reflection is not
// free; sync.OnceValue makes the cost pay-once regardless of how many
// callers invoke RegisterRelation or RelationSchema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var relationSchema = sync.OnceValue(buildRelationSchema)

// buildRelationSchema reflects the Relation struct into a kernel.Schema.
// Runs once at first call to RelationSchema or RegisterRelation; panics
// on failure because invopop cannot fail on a well-formed in-tree
// struct and a startup-time encoding error is unrecoverable.
func buildRelationSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&Relation{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal relation schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindRelation,
		Version:    VersionRelation,
		Definition: raw,
		Provenance: "core",
	}
}

// RelationSchema returns the cached kernel.Schema document for Relation.
// Useful for tests and callers that need to inspect the schema without
// registering it.
func RelationSchema() kernel.Schema {
	return relationSchema()
}

// RegisterRelation installs the Relation schema with o. Call once at
// process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher
// Version returns kernel.ErrSchemaConflict; this function does not
// retry or suppress that error.
//
// Named per-kind (RegisterRelation rather than the generic Register)
// so sibling kinds (#22 Episode, #34 Entity, #35 Mention) coexist in
// this package without a function-name collision.
func RegisterRelation(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, relationSchema())
}
