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

// KindRelationPromoted is the ontology kind under which RelationPromotedEvent
// is registered. A relationPromoted event is appended by the promoteRelation
// tool (and `maz relation promote`) to lift the trust quarantine on a relation
// an untrusted connector source proposed: it flips relations.status from
// "proposed" to "asserted".
//
// Promotion is the human-in-the-loop counterpart to the resolver's automatic
// trust gate (#211, #212). A connector-ingested fact lands status=proposed —
// it ranks on its own relevance (no confidence penalty) but is excluded from
// supersession and predicate minting. Promoting it asserts it: from that point
// the edge can supersede and be superseded like any user-authored relation.
// The event also stamps the promoter as the relation's signed asserter, so the
// promotion is auditable and a team-scope edge gains a concrete authority.
//
// The projection is idempotent: the dispatcher's UPDATE is guarded on
// status='proposed', so replaying a promotion (or promoting an
// already-asserted edge) is a no-op. The JSONL log + the relations table let
// `maz rebuild` reach the same end-state deterministically.
const KindRelationPromoted = "relationPromoted"

// VersionRelationPromoted is the semver-shaped schema version for
// RelationPromotedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionRelationPromoted = "v0.1.0"

// RelationPromotedEvent is the payload of a relationPromoted event. It records
// which Relation was promoted out of the proposed quarantine and when. The Go
// struct is the source of truth; the JSON Schema is reflected from it at first
// access and cached.
type RelationPromotedEvent struct {
	// RelationUID is the ULID of the Relation being promoted (proposed →
	// asserted). Required. The dispatcher flips relations.status for this uid
	// only when it is currently "proposed", so the projection is idempotent.
	RelationUID ULID `json:"relationUid" jsonschema:"required,description=ULID of the Relation being promoted from proposed to asserted. Required."`

	// PromotedAt is the instant the promotion was applied, in UTC. Required.
	// Serialized as RFC 3339.
	PromotedAt time.Time `json:"promotedAt" jsonschema:"required,format=date-time,description=When the promotion was applied. RFC 3339 UTC. Required."`
}

// relationPromotedSchema lazily builds the RelationPromotedEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var relationPromotedSchema = sync.OnceValue(buildRelationPromotedSchema)

// buildRelationPromotedSchema reflects RelationPromotedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildRelationPromotedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&RelationPromotedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal relationPromoted schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindRelationPromoted,
		Version:    VersionRelationPromoted,
		Definition: raw,
		Provenance: "core",
	}
}

// RelationPromotedSchema returns the cached kernel.Schema for
// RelationPromotedEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func RelationPromotedSchema() kernel.Schema {
	return relationPromotedSchema()
}

// RegisterRelationPromoted installs the RelationPromotedEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology
// contract specifies "first registration wins": a second call without a higher
// Version returns kernel.ErrSchemaConflict.
func RegisterRelationPromoted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, relationPromotedSchema())
}
