package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindScopePromoted is the ontology kind under which ScopePromotedEvent is
// registered. A scopePromoted event moves a draft-scoped projection row out of
// the staging area and into a durable scope (personal, project, or team).
//
// Security contract: the dispatcher projection enforces fromScope == "draft"
// before any UPDATE is applied. The MCP tool layer also enforces this at the
// boundary, so a scopePromoted event can only ever move a row OUT of draft,
// never demote from team to personal or rewrite any non-draft scope.
const KindScopePromoted = "scopePromoted"

// VersionScopePromoted is the semver-shaped schema version for ScopePromotedEvent.
const VersionScopePromoted = "v0.1.0"

// ScopePromotedEvent is the payload of a scopePromoted event. The dispatcher
// applies this by updating the scope column on the episodes/entities/relations/
// facts/mentions rows identified by TargetUID from FromScope to ToScope,
// cascading to derived rows whose source_event_uid is TargetUID.
type ScopePromotedEvent struct {
	// TargetUID is the UID of the draft episode (or entity/relation/fact) to
	// promote. Required.
	TargetUID string `json:"targetUid" jsonschema:"required,minLength=1,description=UID of the draft record to promote. Required."`

	// FromScope must always be \"draft\". Required. The dispatcher enforces this
	// and ignores the event when FromScope is not \"draft\" (security gate).
	FromScope string `json:"fromScope" jsonschema:"required,enum=draft,description=Source scope. Must be \"draft\". The dispatcher enforces this and ignores non-draft events. Required."`

	// ToScope is the durable target scope. Required. Only "personal" or "project"
	// are accepted: the dispatcher refuses promotion into "team" (a silent no-op)
	// because pushing unreviewed draft data into a shared scope is the highest-risk
	// leak; team writes must go through the explicit team grant/membership path.
	ToScope string `json:"toScope" jsonschema:"required,enum=personal,enum=project,description=Destination scope: personal or project. Promotion into team is refused. Required."`
}

// scopePromotedSchema lazily builds and caches the ScopePromotedEvent JSON Schema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var scopePromotedSchema = sync.OnceValue(buildScopePromotedSchema)

func buildScopePromotedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&ScopePromotedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal scopePromoted schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindScopePromoted,
		Version:    VersionScopePromoted,
		Definition: raw,
		Provenance: "core",
	}
}

// ScopePromotedSchema returns the cached kernel.Schema for ScopePromotedEvent.
func ScopePromotedSchema() kernel.Schema { return scopePromotedSchema() }

// RegisterScopePromoted installs the ScopePromotedEvent schema with o.
func RegisterScopePromoted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, scopePromotedSchema())
}
