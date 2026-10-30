package ontology

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindFact is the ontology kind under which Fact is registered. Facts
// are the scalar / preference / state slot of the four-layer storage:
// "user.preference.coffee.milk = oat" is a Fact, while
// "Rosie works_at Erebor" stays a Relation. Phase 3 of the mem0-parity
// plan introduces this kind additively — Relations and Facts coexist;
// no migration of historical relations data.
const KindFact = "fact"

// VersionFact is the semver-shaped schema version for Fact. V0.1.0 is
// the initial introduction; bump on incompatible changes. Must be a
// full MAJOR.MINOR.PATCH triplet because kernel.MemoryOntology.Register
// validates with golang.org/x/mod/semver.IsValid.
const VersionFact = "v0.1.0"

// stateKeyPattern is the regular expression for a Fact's state_key: a
// dotted lowercase path of one or more segments, each composed of
// alphanumerics or underscores. Examples:
//
//	preference.coffee.milk
//	state.location.current
//	event.flight.booked
//	relationship.spouse.name
//
// The dotted-prefix convention mirrors mem0 v3's seven categories
// (event, state, relationship, preference, plan, absence, timeless).
// Struct tags cannot reference this const (tags must be literals) —
// the Fact struct's jsonschema tag duplicates the literal regex.
const stateKeyPattern = "^[a-z][a-z0-9_]*(\\.[a-z][a-z0-9_]*)*$"

// Fact is the atomic state-slot memory unit. Unlike Relation (a typed
// edge between two entities), a Fact carries a scalar value addressed
// by a hierarchical state_key. The (subject_uid, state_key) pair is the
// natural unique key for supersede: a new write to the same state_key
// closes the prior open Fact by setting its event_end, mirroring the
// functional-predicate supersede semantics on Relation but without
// requiring a pre-registered predicate.
//
// Bi-temporal model matches Relation: EventStart / EventEnd describe
// when the fact was true in the world; CreateTime captures when it was
// recorded in Mazarbul. An open Fact has EventEnd == nil.
//
// Field naming follows Google AIP: createTime (not createdAt), name as
// the resource name in the form "facts/{uid}", uid as the stable ULID.
type Fact struct {
	// Name is the AIP resource name in the form "facts/{uid}".
	// Required. Output-only; assigned by the writer from UID after the
	// outer Event UID is known.
	Name string `json:"name" jsonschema:"required,pattern=^facts/[0-9A-HJKMNP-TV-Z]{26}$,description=Resource name in the form facts/{uid}. Output-only."`

	// UID is the ULID identifier for this Fact. Required. Output-only.
	// Sortable lexicographically by creation time, opaque, immutable.
	UID string `json:"uid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identifier. Output-only; assigned by the writer."`

	// SubjectUID is the entity the Fact is about. Defaults to the
	// canonical self entity ("self") for first-person state slots;
	// other subjects are valid (e.g. "facts about Rosie's coffee
	// preference"). References an Entity UID; same lexical shape as
	// Relation.SubjectUID so the resolver can reuse its binding pass.
	SubjectUID string `json:"subjectUid" jsonschema:"required,pattern=^([0-9A-HJKMNP-TV-Z]{26}|self)$,description=ULID of the subject the Fact is about, or the literal 'self' sentinel. References an Entity."`

	// StateKey is the hierarchical address of the slot the Fact occupies.
	// Required. Lowercase, dot-separated, alphanumeric+underscore segments.
	// Mem0 v3's seven categories (event, state, relationship, preference,
	// plan, absence, timeless) map to the top-level segment; deeper
	// segments name the specific slot. Examples: preference.coffee.milk,
	// state.location.current, plan.travel.next_trip.
	StateKey string `json:"stateKey" jsonschema:"required,pattern=^[a-z][a-z0-9_]*(\\.[a-z][a-z0-9_]*)*$,description=Hierarchical address of the slot. Lowercase dot-separated path; segments are [a-z][a-z0-9_]*. The first segment names the mem0-style category (event|state|relationship|preference|plan|absence|timeless)."`

	// Value is the scalar value the slot currently holds. Required.
	// Free-form string in v0.1.0; future versions may add a typed-value
	// variant for booleans / numbers / dates without breaking existing
	// data.
	Value string `json:"value" jsonschema:"required,minLength=1,description=The scalar value of the slot. Free-form string in v0.1.0."`

	// EventStart is the inclusive lower bound of the validity window:
	// when the Fact began to be true in the world. Optional; when
	// absent the supersede pipeline treats the Fact as valid from
	// CreateTime.
	EventStart *time.Time `json:"eventStart,omitempty" jsonschema:"format=date-time,description=Inclusive lower bound of validity. RFC 3339 UTC. Optional; defaults to CreateTime."`

	// EventEnd is the exclusive upper bound of the validity window:
	// when the Fact ceased to be true. Optional; an open Fact has
	// EventEnd == nil. The supersede pipeline sets this to the
	// superseding Fact's EventStart (or CreateTime) when a same-
	// state_key write arrives.
	EventEnd *time.Time `json:"eventEnd,omitempty" jsonschema:"format=date-time,description=Exclusive upper bound of validity. RFC 3339 UTC. Optional; open Facts omit this."`

	// CreateTime is the instant the Fact was first recorded in
	// Mazarbul, in UTC. Required. Output-only; mirrors the outer
	// Event.CreateTime in the persisted row.
	CreateTime time.Time `json:"createTime" jsonschema:"required,format=date-time,description=Time the Fact was first recorded. RFC 3339 UTC. Output-only; mirrors Event.CreateTime."`

	// SourceEventUID is the UID of the Event that produced this Fact.
	// Required. Anchors the W3C PROV chain back to the originating
	// Episode (or other producer event).
	SourceEventUID string `json:"sourceEventUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the producing Event. PROV anchor."`

	// ContentHash is a stable hash of (subject_uid, state_key, value)
	// used for MD5-dedup on insert. Optional on input; the writer
	// computes it when absent. Mem0 v3 introduced content-hash dedup;
	// Mazarbul follows the same pattern so the extractor can re-emit a
	// Fact verbatim without creating a duplicate row.
	ContentHash string `json:"contentHash,omitempty" jsonschema:"description=Stable hash of (subjectUid, stateKey, value). Computed by the writer when absent."`

	// SupersededByUID points at the Fact UID that closed this one, when
	// EventEnd is non-nil. Optional. Same shape as
	// Relation.SupersededByUID; tools can walk the supersede chain to
	// reconstruct the history of a slot without joining across the
	// event log.
	SupersededByUID string `json:"supersededByUid,omitempty" jsonschema:"pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Fact that closed this one. Set in lockstep with EventEnd by the supersede pipeline."`

	// SourcePriority biases the recall lane: lower values demote the
	// Fact relative to higher-priority facts when both match a query.
	// Mem0 v3 uses this so digest / rollup facts (Phase 5) surface
	// only when no direct evidence is available. Optional. Range [0, 1];
	// zero is treated as the default 1.0 by the recall scorer.
	SourcePriority float32 `json:"sourcePriority,omitempty" jsonschema:"minimum=0,maximum=1,description=Recall-lane priority for this Fact. Range 0..1; 0 is treated as 1.0 default. Lower priority surfaces only when no higher-priority Fact matches."`
}

// factSchema lazily builds the Fact JSON Schema document once and caches
// it for the process lifetime. Invopop reflection is not free;
// sync.OnceValue makes the cost pay-once regardless of how many callers
// invoke RegisterFact or FactSchema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var factSchema = sync.OnceValue(buildFactSchema)

// buildFactSchema reflects the Fact struct into a kernel.Schema. It
// runs once at first call; panics on failure because invopop cannot
// fail on a well-formed in-tree struct and a startup-time encoding
// error is unrecoverable.
func buildFactSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&Fact{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal fact schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindFact,
		Version:    VersionFact,
		Definition: raw,
		Provenance: "core",
	}
}

// FactSchema returns the cached kernel.Schema document for Fact. Useful
// for tests and callers that need to inspect the schema without
// registering it.
func FactSchema() kernel.Schema {
	return factSchema()
}

// RegisterFact installs the Fact schema with o. Call once at process
// startup. The kernel.Ontology contract specifies "first registration
// wins"; a second call without a higher Version returns
// kernel.ErrSchemaConflict.
func RegisterFact(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, factSchema())
}

// StateKeyPattern returns the regex literal used to validate Fact.StateKey.
// Exposed for tests and for the SQLite CHECK constraint generator (the
// migration duplicates the pattern as a literal in SQL).
func StateKeyPattern() string { return stateKeyPattern }

// ObservationContentHash computes the content-hash dedup key for a Fact derived
// from a document observation: SHA-256 of (subjectUID, stateKey, value) with NUL
// separators, truncated to 16 bytes (32 hex chars). This is the single source
// of truth for the observation dedup key. Both the document importer (when first
// materialising an observation Fact) and the dispatcher (when a factRehomed
// event moves the Fact to a new subject) call it, so a freshly re-imported fact
// and a re-homed fact land on byte-identical hashes and dedup against the same
// open slot instead of duplicating it.
func ObservationContentHash(subjectUID, stateKey, value string) string {
	h := sha256.New()
	h.Write([]byte(subjectUID))
	h.Write([]byte{0})
	h.Write([]byte(stateKey))
	h.Write([]byte{0})
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil)[:16])
}
