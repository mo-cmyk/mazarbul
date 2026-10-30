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

// KindMention is the ontology kind under which Mention is registered.
// A Mention is one observation of an entity from a single source
// Episode: the immutable, content-addressed building block the resolver
// (M2) uses to derive a canonical Entity. Many Mentions point at the
// same Entity once the resolver has merged them; the Mention itself is
// never rewritten.
const KindMention = "mention"

// VersionMention is the semver-shaped schema version for Mention. Bumped
// to v0.3.0 alongside VersionEntity to reflect the open-enum wire change
// on EntityKind (P2 dynamic-ontology). V0.2.0 added extractionText,
// sourceSpan, and groundingScore (P0). V0.x.y signals that breaking
// changes are allowed before v1.0; the M2 epic introduces the migration
// story (#13). Must be a full MAJOR.MINOR.PATCH triplet because
// kernel.MemoryOntology.Register validates with golang.org/x/mod/semver.IsValid,
// which rejects shorter forms like "v0.1" (Greptile #82 P1).
const VersionMention = "v0.3.0"

// SourceSpan holds the inclusive-start, exclusive-end character offsets
// of an extracted mention inside the source Episode's text. Both bounds
// are required when the struct is present; the SourceSpan field on
// Mention is itself required in v0.2.0, so both StartChar and EndChar
// are always on the wire for v0.2.0 payloads.
type SourceSpan struct {
	// StartChar is the inclusive lower bound character offset of the
	// extracted span in the source Episode's text. Required. Must be
	// non-negative.
	StartChar int `json:"startChar" jsonschema:"required,minimum=0,description=Inclusive lower bound character offset into the source Episode text. Required."`

	// EndChar is the exclusive upper bound character offset of the
	// extracted span in the source Episode's text. Required. Must be
	// non-negative. Values equal to StartChar denote a zero-width
	// anchor point; values less than StartChar are invalid and the
	// extractor must not emit them.
	EndChar int `json:"endChar" jsonschema:"required,minimum=0,description=Exclusive upper bound character offset into the source Episode text. Required."`
}

// Mention is one observation of an entity inside a single Episode. The
// Go struct is the source of truth; the JSON Schema is reflected from
// it at first access via invopop/jsonschema and cached, so the wire
// shape and the Go type cannot drift.
//
// Mentions are immutable in M1: the resolver (M2) reads them and emits
// merge events that materialize canonical Entities; it never rewrites
// the Mention itself. Multi-vector embeddings on mentions are deferred
// to M3 (architecture spec section 05, "embedding strategy multi-vector").
//
// Field naming follows Google AIP: createTime (not createdAt), name as
// the resource name in the form "mentions/{uid}", uid as the stable
// ULID identifier. Optional fields carry omitempty so absent fields are
// omitted from JSON rather than serialized as empty.
type Mention struct {
	// Name is the AIP resource name in the form "mentions/{uid}".
	// Optional on input: output-only and assigned by the writer
	// pipeline after the Event UID is known. Validated by pattern when
	// present, so a caller who supplies it must supply the canonical
	// form. Schema excludes "required" because kernel.FileEventLog
	// validates ev.Payload alone, and a realistic caller (extractor
	// emitting a Mention event) cannot know the UID before Append
	// assigns one; mirrors Episode.Name. The earlier "required"
	// shape would have made every legitimate Append fail (Greptile #93 P1).
	Name string `json:"name,omitempty" jsonschema:"pattern=^mentions/[0-9A-HJKMNP-TV-Z]{26}$,description=Resource name in the form mentions/{uid}. Output-only; assigned by the writer after the UID is known."`

	// UID is the ULID identifier for this Mention. Optional on input;
	// output-only. The writer assigns the canonical UID on the outer
	// Event; the consumer-facing Mention reconstruction copies it
	// into this field after the row is read back. Sortable, opaque,
	// immutable. Mirrors Episode.UID.
	UID string `json:"uid,omitempty" jsonschema:"pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identifier. Output-only; mirrors the outer Event.UID."`

	// Kind is the high-level taxon, mirroring Entity.kind exactly:
	// one of the seven core kinds (person, organization, place, project,
	// event, document, external_ref) or a namespaced extension kind
	// registered via the ontology registry. Required. Pattern-validated by
	// EntityKind.JSONSchema() — the same open pattern used on Entity so
	// the wire shapes stay identical at compile time, not just by
	// convention.
	Kind EntityKind `json:"kind" jsonschema:"required,description=Mention kind. Mirrors Entity.kind: person, organization, place, project, event, document."`

	// DisplayName is the surface form of the mention as it appears in
	// the source Episode (e.g., "Frodo", "F. Baggins", "@FrodoB").
	// Required. Non-empty. The resolver uses this for blocking and
	// scoring; the canonical name on the resulting Entity is chosen
	// later from the consensus of all merged mentions.
	DisplayName string `json:"displayName" jsonschema:"required,minLength=1,description=Surface form of the mention as it appears in the source Episode."`

	// SourceEventUID is the ULID of the Episode this Mention was
	// observed in. Required. Typed as ULID so the schema pattern
	// constraint flows from ULID.JSONSchema() rather than being
	// repeated in the struct tag; matches the typing of Entity's
	// MentionUIDs []ULID. Episode's ParentEpisodeUID uses *string
	// because invopop drops the JSONSchema() method through a pointer;
	// no pointer here.
	SourceEventUID ULID `json:"sourceEventUid" jsonschema:"required,description=ULID of the Episode the Mention was observed in."`

	// CreateTime is the instant the Mention was extracted, in UTC.
	// Optional on input; output-only. Mirrors the outer Event.CreateTime
	// in the persisted row. Serialized as an RFC 3339 string when
	// present. See the Name docstring for why writer-owned fields are
	// optional on the payload schema.
	//
	// Pointer type so json.Marshal of a zero-value Mention actually
	// omits the field. time.Time is a struct, and Go's `omitempty`
	// only drops empty strings/slices/numbers/maps/pointers, not
	// zero structs, so a non-pointer would emit
	// "createTime":"0001-01-01T00:00:00Z" which the schema accepts
	// and FileEventLog then persists, masking the writer-assigned
	// time. Greptile #93 P1 "Zero time still persists".
	CreateTime *time.Time `json:"createTime,omitempty" jsonschema:"format=date-time,description=Time the Mention was extracted. RFC 3339 UTC. Output-only; mirrors the outer Event.CreateTime."`

	// Identifiers holds typed external identifiers observed alongside
	// this mention, e.g. an email next to the name or a GitHub user ID
	// from a commit. Optional. Per-type format validation is the
	// resolver's job in M2 (architecture spec section 05, "strong vs
	// weak identifiers"). Reuses the Entity-side Identifier struct so
	// the two kinds describe identifiers identically.
	Identifiers []Identifier `json:"identifiers,omitempty" jsonschema:"description=Typed external identifiers observed alongside this mention."`

	// DisambiguationCtx is free-text breadcrumbs the resolver scoring
	// uses to anchor the mention in context (e.g., "fundraising chat",
	// "Erebor engineer", "github commit message"). Optional. Free-text
	// in v0.1; structured cues are deferred until the resolver demands
	// them. See architecture spec section 05, "Mention.disambiguation_context".
	DisambiguationCtx string `json:"disambiguationCtx,omitempty" jsonschema:"description=Free-text breadcrumbs used by the resolver to score this mention. Optional."`

	// ExtractionText is the verbatim span from the source Episode's
	// text that produced this Mention. Required. Non-empty. Used by
	// the grounding pass (extractor/ground.go) to locate the span and
	// compute SourceSpan; kept on the record so downstream consumers
	// can display the originating text without re-reading the Episode.
	ExtractionText string `json:"extractionText" jsonschema:"required,minLength=1,description=Verbatim span from the source Episode text that produced this Mention. Required."`

	// SourceSpan holds the character offsets of ExtractionText inside
	// the source Episode's text. Required. Both StartChar and EndChar
	// are set by the grounding pass after ExtractionText is located.
	// A groundingScore below 1.0 indicates the offsets are approximate
	// (fuzzy match) rather than verbatim.
	SourceSpan SourceSpan `json:"sourceSpan" jsonschema:"required,description=Character offsets of ExtractionText within the source Episode text. Required."`

	// GroundingScore is the extractor's confidence that ExtractionText
	// was found verbatim in the source Episode. Range 0..1 inclusive.
	// Required. 1.0 means verbatim substring match; lower values
	// indicate fuzzy match; 0.0 means the text was confabulated by the
	// LLM and does not appear in the source at all. Payloads with
	// GroundingScore == 0.0 are filtered by the extractor pipeline and
	// never produce Mention events; the field is present so round-trip
	// tests and schema consumers can observe the full range.
	GroundingScore float64 `json:"groundingScore" jsonschema:"required,minimum=0,maximum=1,description=Verbatim-match confidence of ExtractionText in the source Episode. 1.0 = exact match; 0.0 = confabulation. Required."`
}

// mentionSchema lazily builds the Mention JSON Schema document once and
// caches it for the process lifetime. Invopop reflection is not free;
// sync.OnceValue makes the cost pay-once regardless of how many callers
// invoke RegisterMention or MentionSchema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var mentionSchema = sync.OnceValue(buildMentionSchema)

// buildMentionSchema reflects the Mention struct into a kernel.Schema.
// Runs once at first call to MentionSchema or RegisterMention; panics on
// failure because invopop cannot fail on a well-formed in-tree struct
// and a startup-time encoding error is unrecoverable.
func buildMentionSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&Mention{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal mention schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindMention,
		Version:    VersionMention,
		Definition: raw,
		Provenance: "core",
	}
}

// MentionSchema returns the cached kernel.Schema document for Mention.
// Useful for tests and callers that need to inspect the schema without
// registering it.
func MentionSchema() kernel.Schema {
	return mentionSchema()
}

// RegisterMention installs the Mention schema with o. Call once at
// process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher
// Version returns kernel.ErrSchemaConflict; this function does not
// retry or suppress that error.
//
// Named per-kind (RegisterMention rather than the generic Register) so
// sibling kinds (#22 Episode, #34 Entity, #36 Relation) coexist in this
// package without a function-name collision.
func RegisterMention(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, mentionSchema())
}
