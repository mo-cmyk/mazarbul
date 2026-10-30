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

// KindFactRehomed is the ontology kind under which FactRehomed events are
// registered. A factRehomed event is appended when an observation fact that
// was initially homed on a document entity is moved to the document's
// about: subject.
//
// The event is an explicit, auditable, reversible operation: the event log
// carries the full (fromSubjectUID, toSubjectUID) pair so a rebuild can
// re-derive the same end-state by replaying the log. Idempotency is enforced
// by the dispatcher: applying a factRehomed event when the fact is already on
// ToSubjectUID is a no-op. Two paired devices that independently emit the same
// rehome converge to identical projection state regardless of replay order.
const KindFactRehomed = "factRehomed"

// VersionFactRehomed is the semver-shaped schema version for FactRehomed.
// Must be a full MAJOR.MINOR.PATCH triplet.
const VersionFactRehomed = "v0.1.0"

// FactRehomed is the payload of a factRehomed event. It records which Fact
// was moved, from which subject it was moved, and to which subject it was
// moved, plus provenance fields that anchor it to the revision that triggered
// the rehome (or to the rebuild sweep event). The Go struct is the source of
// truth; the JSON Schema is reflected from it at first access.
type FactRehomed struct {
	// FactUID is the ULID of the Fact being rehomed. Required.
	FactUID string `json:"factUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Fact being rehomed. Required."`

	// FromSubjectUID is the entity the Fact was previously attached to.
	// Required. The dispatcher uses this to reconcile the old (fromSubjectUID,
	// stateKey) slot after the move so its supersede chain stays consistent.
	FromSubjectUID string `json:"fromSubjectUid" jsonschema:"required,pattern=^([0-9A-HJKMNP-TV-Z]{26}|self)$,description=ULID (or 'self' sentinel) of the entity the Fact was previously attached to. Required."`

	// ToSubjectUID is the entity the Fact is being moved to. Required. Must
	// differ from FromSubjectUID for the move to be meaningful, although the
	// dispatcher is idempotent even when they happen to be equal (no-op path).
	ToSubjectUID string `json:"toSubjectUid" jsonschema:"required,pattern=^([0-9A-HJKMNP-TV-Z]{26}|self)$,description=ULID (or 'self' sentinel) of the entity the Fact is being moved to. Required."`

	// CreateTime is the instant the rehome event was recorded, in UTC.
	// Required. Serialized as RFC 3339.
	CreateTime time.Time `json:"createTime" jsonschema:"required,format=date-time,description=Time the rehome event was recorded. RFC 3339 UTC. Required."`

	// SourceEventUID is the ULID of the Event that triggered this rehome.
	// Required. Typically the documentRevision event whose about: frontmatter
	// resolved the target subject, or a synthetic event produced by the
	// retroactive rebuild sweep.
	SourceEventUID string `json:"sourceEventUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the triggering Event. PROV anchor. Required."`
}

// factRehomedSchema lazily builds the FactRehomed JSON Schema document once
// and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var factRehomedSchema = sync.OnceValue(buildFactRehomedSchema)

// buildFactRehomedSchema reflects FactRehomed into a kernel.Schema. Runs
// once at first call; panics on failure because invopop cannot fail on a
// well-formed in-tree struct.
func buildFactRehomedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&FactRehomed{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal factRehomed schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindFactRehomed,
		Version:    VersionFactRehomed,
		Definition: raw,
		Provenance: "core",
	}
}

// FactRehomedSchema returns the cached kernel.Schema for FactRehomed.
// Useful for tests and consumers that need to inspect the schema without
// registering it.
func FactRehomedSchema() kernel.Schema {
	return factRehomedSchema()
}

// RegisterFactRehomed installs the FactRehomed schema with o. Call once at
// process startup (or once per test). The kernel.Ontology contract specifies
// "first registration wins": a second call without a higher Version returns
// kernel.ErrSchemaConflict.
func RegisterFactRehomed(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, factRehomedSchema())
}
