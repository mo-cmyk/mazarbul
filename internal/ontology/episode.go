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

// KindEpisode is the ontology kind under which Episode is registered.
// It matches the EventKind emitted by the remember MCP tool when an
// Episode payload is appended to the event log.
const KindEpisode = "episode"

// VersionEpisode is the semver-shaped schema version for Episode. The
// v0.x.y prefix indicates breaking changes are allowed before v1.0; M2
// introduces the migration story (epic #13). Must be a full
// MAJOR.MINOR.PATCH triplet because kernel.MemoryOntology.Register
// validates with golang.org/x/mod/semver.IsValid, which rejects shorter
// forms like "v0.1".
// V0.2.0: added optional Coordinates and CoordinatesProvenance fields
// to support the --coords flag on maz remember and resolver coordinate
// forwarding onto place Entities.
// V0.3.0: added optional Actor field (user|assistant|system) so the
// extractor can route assistant-actor episodes through a different
// prompt branch (Phase 1 of the mem0-parity plan).
const VersionEpisode = "v0.4.0"

// ulidPattern is the regular expression for Crockford-Base32 ULIDs:
// 26 characters drawn from 0-9 + A-Z minus I, L, O, U. Shared by every
// identifier field so a typo in one site cannot drift from the others.
// Struct tags cannot reference this const (tags must be literals), but
// the ULID type below and TestEpisodeSchema_PatternsConsistent both
// reference it.
const ulidPattern = "^[0-9A-HJKMNP-TV-Z]{26}$"

// ActorKind names the speaker an Episode is attributable to. Three
// values today: user (the person), assistant (the AI), system (the
// platform itself). Phase 1 of the mem0-parity plan introduces this
// distinction because mem0's +53.6pp lift on the
// single-session-assistant LongMemEval category comes entirely from
// running assistant-actor turns through a different extractor prompt
// branch instead of treating them as third-party narration.
//
// Default value when omitted is ActorUser, set explicitly in the
// remember tool so the database column carries a meaningful value
// rather than the empty string for legacy events.
type ActorKind string

// ActorUser, ActorAssistant, ActorSystem are the three legal ActorKind
// values. User is the default (people writing facts via remember()),
// assistant carries AI-generated facts (recommendations, confirmations,
// derived insights), system carries platform-level events.
const (
	ActorUser      ActorKind = "user"
	ActorAssistant ActorKind = "assistant"
	ActorSystem    ActorKind = "system"
)

// EpisodeClass classifies an Episode by memory type, mirroring mem0
// v3's semantic / episodic / procedural typology. The retrieval lane
// multiplies the fused score by a type_weight (default semantic 0.6,
// episodic 0.3, procedural 0.1) so context-stable facts surface above
// session chatter on questions that reach for facts.
//
// The default class is ClassEpisodic (most user turns are episodic);
// the extractor's heuristic classifier promotes self-statements to
// ClassSemantic and assistant-actor numbered traces to
// ClassProcedural.
type EpisodeClass string

// ClassSemantic, ClassEpisodic, ClassProcedural are the three legal
// EpisodeClass values. Semantic = context-stable facts ("I live in
// Berlin"); episodic = time-anchored events ("we went jogging
// today"); procedural = action recipes ("1. Open the file 2. Edit
// line 3...").
const (
	ClassSemantic   EpisodeClass = "semantic"
	ClassEpisodic   EpisodeClass = "episodic"
	ClassProcedural EpisodeClass = "procedural"
)

// ULID is a Crockford-Base32 ULID string. It exists so invopop applies
// the pattern constraint to slice elements (e.g. MentionUIDs); a bare
// []string would emit items: {type: string} with no pattern, leaving
// downstream indexing exposed to bogus identifiers. Implementations
// convert with `ontology.ULID(s)`; the type is a string under the hood.
type ULID string

// JSONSchema lets invopop reflect ULID into a string-with-pattern schema
// whether the ULID appears as a scalar field, a slice item, or a map
// value. Called by invopop during Reflect; do not call directly.
func (ULID) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:    "string",
		Pattern: ulidPattern,
	}
}

// Episode is the atomic memory unit Mazarbul writes on every remember
// MCP call and reads on every recall query. The Go struct is the source
// of truth; the JSON Schema is reflected from it at first access via
// invopop/jsonschema, so the wire shape and the type cannot drift.
//
// Field naming follows Google AIP: createTime (not createdAt), name as
// the resource name in the form "episodes/{uid}", uid as the stable
// ULID identifier. Optional fields carry omitempty so absent fields are
// omitted from JSON rather than serialized as empty.
type Episode struct {
	// Name is the AIP resource name in the form "episodes/{uid}".
	// Optional on input: output-only and assigned by the writer pipeline
	// after the Event UID is known. Validated by pattern when present, so
	// a caller who supplies it must supply the canonical form. Schema
	// excludes "required" so a realistic caller, who cannot know the UID
	// before Append, is not forced to fabricate one and risk drift between
	// Event.UID and Episode.UID.
	Name string `json:"name,omitempty" jsonschema:"pattern=^episodes/[0-9A-HJKMNP-TV-Z]{26}$,description=Resource name in the form episodes/{uid}. Output-only; assigned by the writer after the UID is known."`

	// UID is the ULID identifier for this Episode. Optional on input;
	// output-only. The writer assigns the canonical UID on the outer
	// Event; the consumer-facing Episode reconstruction copies it into
	// this field after the row is read back. Sortable, opaque, immutable.
	UID string `json:"uid,omitempty" jsonschema:"pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identifier. Output-only; mirrors the outer Event.UID."`

	// CreateTime is the instant the Episode was first observed, in UTC.
	// Optional on input; output-only. Mirrors the outer Event.CreateTime
	// in the persisted row. Serialized as an RFC 3339 string when present.
	CreateTime time.Time `json:"createTime,omitempty" jsonschema:"format=date-time,description=Time the Episode was first observed. RFC 3339 UTC. Output-only; mirrors the outer Event.CreateTime."`

	// Text is the natural-language record this Episode preserves.
	// Required. The verbatim payload of a remember() call.
	Text string `json:"text" jsonschema:"required,minLength=1,description=Natural-language record. The verbatim payload of remember()."`

	// Source identifies where the Episode originated. Required.
	// Free-form in M1 ("cli:remember"; "providers/anthropic"); the source
	// taxonomy becomes typed in M2.
	Source string `json:"source" jsonschema:"required,minLength=1,description=Where the Episode originated. Free-form in M1; typed in M2."`

	// Actor identifies the speaker the Episode text is attributable to:
	// the end user, the AI assistant, or the system itself. Defaults to
	// "user" when omitted so existing callers keep working without
	// migration. Phase 1 of the mem0-parity plan: assistant-actor episodes
	// run through a different extractor prompt branch (mem0's
	// AGENT_MEMORY_EXTRACTION_PROMPT) so assistant statements
	// ("I've booked your flight for March 3rd") become first-class facts
	// instead of being filtered as third-party narration. The +53.6pp
	// single-session-assistant lift mem0 reports comes from this
	// distinction.
	Actor ActorKind `json:"actor,omitempty" jsonschema:"enum=user,enum=assistant,enum=system,description=Who said it. Defaults to user when absent. user|assistant|system."`

	// Class classifies the Episode by memory type. Defaults to
	// "episodic" when absent so legacy events keep working without
	// migration. The extractor's heuristic classifier promotes
	// self-statements to "semantic" and assistant-actor numbered
	// traces to "procedural". The retrieval lane applies a type_weight
	// multiplier on the fused score (mem0 v3 parity).
	Class EpisodeClass `json:"class,omitempty" jsonschema:"enum=semantic,enum=episodic,enum=procedural,description=Memory class. Defaults to episodic. semantic|episodic|procedural."`

	// Confidence is the writer's stated confidence in this Episode, in
	// the closed interval 0..1. Required. 1.0 for direct user notes;
	// lower for inference, enrichment, or low-quality extraction.
	Confidence float64 `json:"confidence" jsonschema:"required,minimum=0,maximum=1,description=Writer's confidence in the Episode. Range 0..1 inclusive."`

	// MentionUIDs lists the Mention UIDs extracted from this Episode.
	// Optional. Empty until an extractor runs. AIP-clarified from the
	// issue body's `entities` (which was ambiguous between Entity and
	// Mention UIDs); the acceptance criterion explicitly says "mention
	// UIDs". Element type is ULID so the schema validates each entry
	// against the ULID pattern (Greptile #70 P1).
	MentionUIDs []ULID `json:"mentionUids,omitempty" jsonschema:"description=Mention UIDs extracted from text. Each is a ULID."`

	// RelationUIDs lists the Relation UIDs derived from this Episode.
	// Optional. Empty until an extractor runs. AIP-clarified from the
	// issue body's `relations` for symmetry with MentionUIDs.
	RelationUIDs []ULID `json:"relationUids,omitempty" jsonschema:"description=Relation UIDs derived from this Episode. Each is a ULID."`

	// Tags is a free-form set of string labels attached by the user or
	// an enricher. Optional. Order is not significant; a future
	// migration may deduplicate, but the schema does not enforce
	// uniqueness today.
	Tags []string `json:"tags,omitempty" jsonschema:"description=Free-form string labels. Order not significant."`

	// ParentEpisodeUID links this Episode to a parent Episode for
	// threaded memory (e.g., a reply to a prior note). Optional. Stored
	// as a ULID pointing at another Episode. AIP-corrected from the
	// issue body's parent_episode_uid (snake_case is forbidden on the
	// wire per AGENTS.md rule 2).
	ParentEpisodeUID *string `json:"parentEpisodeUid,omitempty" jsonschema:"pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=Parent Episode UID for threaded memory. ULID."`

	// Coordinates holds the geographic position associated with this
	// Episode in WGS84 decimal degrees. Optional. Set when the caller
	// supplies explicit coordinates (e.g. maz remember --coords). The
	// resolver forwards these onto any matched or created place Entity;
	// existing Entity coordinates are not overwritten by lower-provenance
	// sources (rank order: user_provided > calendar:google >
	// geocoded:nominatim).
	Coordinates *Coordinates `json:"coordinates,omitempty" jsonschema:"description=Geographic coordinates associated with this Episode. Both latitude and longitude required when present. Optional."`

	// CoordinatesProvenance records the origin of the Coordinates value.
	// Required when Coordinates is set; absent otherwise. Same provenance
	// rank as Entity.CoordinatesProvenance.
	CoordinatesProvenance string `json:"coordinatesProvenance,omitempty" jsonschema:"description=Origin of the Coordinates value, e.g. user_provided. Required when coordinates is set. Optional."`
}

// episodeSchema lazily builds the Episode JSON Schema document once and
// caches it for the process lifetime. Invopop reflection is not free;
// sync.OnceValue makes the cost pay-once regardless of how many callers
// invoke RegisterEpisode or EpisodeSchema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var episodeSchema = sync.OnceValue(buildEpisodeSchema)

// buildEpisodeSchema reflects the Episode struct into a kernel.Schema.
// It runs once at first call to EpisodeSchema or RegisterEpisode;
// panics on failure because invopop cannot fail on a well-formed
// in-tree struct and a startup-time encoding error is unrecoverable.
func buildEpisodeSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&Episode{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal episode schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEpisode,
		Version:    VersionEpisode,
		Definition: raw,
		Provenance: "core",
	}
}

// EpisodeSchema returns the cached kernel.Schema document for Episode.
// Useful for tests and callers that need to inspect the schema without
// registering it.
func EpisodeSchema() kernel.Schema {
	return episodeSchema()
}

// RegisterEpisode installs the Episode schema with o. Call once at
// process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher
// Version returns kernel.ErrSchemaConflict; this function does not
// retry or suppress that error.
//
// Named per-kind (RegisterEpisode rather than the generic Register) so
// sibling kinds (#34 Entity, #35 Mention, #36 Relation) can coexist in
// this package without a function-name collision.
func RegisterEpisode(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, episodeSchema())
}
