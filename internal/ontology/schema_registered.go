package ontology

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindSchemaRegistered is the ontology kind under which SchemaRegisteredEvent
// is registered. A schemaRegistered event carries an arbitrary runtime
// JSON-Schema registration into the event log: the typed property schema
// attached to an entity kind (keyed entityKind:<name>), a relation's
// attribute schema, or a fully custom event/entity shape supplied by an agent
// (defineSchema MCP tool) or a schema pack.
//
// It is the general-purpose, reflexive escape hatch that lets the ontology
// grow new validated shapes at runtime without a code change. The wrapped
// definition mirrors kernel.Schema (Kind, Version, Definition, Provenance);
// the dispatcher materializes it into the ontology_schemas projection so
// kernel.Ontology can Validate against it after a restart or maz rebuild, and
// the render pipeline writes it to the git-synced ontology/schemas/ tree. See
// agent-docs/designs/dynamic-ontology.md.
const KindSchemaRegistered = "schemaRegistered"

// VersionSchemaRegistered is the semver-shaped schema version for
// SchemaRegisteredEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionSchemaRegistered = "v0.1.0"

// SchemaRegisteredEvent is the payload of a schemaRegistered event: one
// runtime schema registration. The Go struct is the source of truth; the JSON
// Schema is reflected from it at first access and cached.
type SchemaRegisteredEvent struct {
	// SchemaKind is the lookup key the registered schema validates against,
	// e.g. "entityKind:vehicle" for a kind's property schema, or a custom
	// event kind. Required. Named SchemaKind (not Kind) to avoid colliding
	// with the enclosing event's own kind on the wire.
	//
	// The key grammar is frozen by pattern: a bare camelCase event kind
	// (myCustomEvent), optionally colon-namespaced into one or more segments
	// (entityKind:vehicle, entityKind:user:deal). Spaces, path separators, and
	// other punctuation are rejected so the key is safe as a registry lookup
	// key. The grammar deliberately permits the namespace colon, which is not a
	// legal Windows path component; the render pipeline therefore does NOT use
	// the raw key as a filename. The frozen render-path contract
	// (SchemaKindRenderFilename) maps the colon to a hyphen for a lowercased,
	// length-bounded readable stem and appends the full SHA-256 hex digest of
	// the exact raw key, so every accepted key is materializable, the filename
	// is collision-free on every filesystem (including for case-only variants
	// on case-insensitive Windows/macOS, since the digest is over raw bytes and
	// rendered as lowercase hex), and the basename is length-bounded. The key
	// is also capped at 128 chars (maxLength) as defense in depth so a durable
	// event can never carry a key that no filesystem could materialize.
	SchemaKind string `json:"schemaKind" jsonschema:"required,minLength=1,maxLength=128,pattern=^[a-z][a-zA-Z0-9_]*(:[A-Za-z0-9_]+)*$,description=Lookup key the registered schema validates against: bare camelCase event kind (myCustomEvent) or colon-namespaced (entityKind:vehicle, entityKind:user:deal). No spaces or path separators; the namespace colon is mapped to a hyphen for the render filename; max 128 chars."`

	// Version is the semver-shaped version of the registered schema.
	// Required. Higher versions replace lower ones per the kernel.Ontology
	// contract; same version with a different definition is a conflict.
	Version string `json:"version" jsonschema:"required,pattern=^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$,description=Semver version of the registered schema, e.g. v0.1.0. Leading-zero components (v01.0.0) are rejected to match golang.org/x/mod/semver.IsValid."`

	// Definition is the JSON Schema document (Draft 2020-12) being
	// registered. Required. Opaque to this event's own schema; compiled and
	// enforced by kernel.Ontology.Validate for SchemaKind.
	Definition json.RawMessage `json:"definition" jsonschema:"required,description=The JSON Schema document being registered. Draft 2020-12."`

	// Provenance identifies the namespace lane: core, extension:<name>, or
	// user. Required.
	Provenance string `json:"provenance" jsonschema:"required,pattern=^(core|user|extension:[a-z][a-z0-9_-]*)$,description=Namespace lane: core, user, or extension:<name>."`

	// AddedBy identifies the agent that registered this schema. Required.
	// Enforced by EntityKindAddedBy.JSONSchema() (shared enum).
	AddedBy EntityKindAddedBy `json:"addedBy" jsonschema:"required,description=Agent that registered the schema: seed, llm, user, or extension."`

	// AddedAt is the instant the schema was registered, in UTC. Required.
	// Serialized as RFC 3339.
	AddedAt time.Time `json:"addedAt" jsonschema:"required,format=date-time,description=When the schema was registered. RFC 3339 UTC."`
}

// schemaRegisteredSchema lazily builds the SchemaRegisteredEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var schemaRegisteredSchema = sync.OnceValue(buildSchemaRegisteredSchema)

// buildSchemaRegisteredSchema reflects SchemaRegisteredEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildSchemaRegisteredSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&SchemaRegisteredEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal schemaRegistered schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindSchemaRegistered,
		Version:    VersionSchemaRegistered,
		Definition: raw,
		Provenance: "core",
	}
}

// SchemaRegisteredSchema returns the cached kernel.Schema for
// SchemaRegisteredEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func SchemaRegisteredSchema() kernel.Schema {
	return schemaRegisteredSchema()
}

// SchemaKindRenderFilename maps a SchemaKind to the relative filename the
// render pipeline writes under the git-synced ontology/schemas/ tree. The
// SchemaKind grammar deliberately permits the namespace colon (entityKind:
// vehicle), which is not a legal path component on Windows, so the raw key
// cannot be used verbatim as a filename. This is the frozen render-path
// contract: a lowercased, hyphen-mapped, length-bounded readable stem is
// followed by the full SHA-256 hex digest of the exact raw key, closed by a
// .json suffix (entityKind:vehicle -> entitykind-vehicle-<64 hex>.json).
//
// The digest makes the mapping collision-free on EVERY filesystem, including
// the case-insensitive defaults (Windows NTFS, macOS APFS): it is computed
// over the exact raw bytes (so case-only variants such as entityKind:vehicle
// vs entityKind:Vehicle diverge), it is the full untruncated 256-bit digest
// (so there is no truncation collision surface — the same content-addressing
// guarantee git relies on), and it is rendered as lowercase hex (so it is
// already case-folded). The readable stem is purely cosmetic and contributes
// nothing to uniqueness.
//
// The filename length is bounded and platform-portable: the stem is capped at
// schemaKindStemMax and the digest is a constant 64 hex chars, so the basename
// never exceeds schemaKindStemMax + 1 + 64 + len(".json") bytes, well under
// the 255-byte single-component limit common to ext4/APFS/NTFS. The SchemaKind
// jsonschema tag additionally caps the raw key length as defense in depth.
func SchemaKindRenderFilename(schemaKind string) string {
	stem := strings.ReplaceAll(strings.ToLower(schemaKind), ":", "-")
	if len(stem) > schemaKindStemMax {
		stem = stem[:schemaKindStemMax]
	}
	sum := sha256.Sum256([]byte(schemaKind))
	return stem + "-" + hex.EncodeToString(sum[:]) + ".json"
}

// schemaKindStemMax caps the cosmetic readable stem of a rendered schema
// filename. The stem carries no uniqueness (the SHA-256 digest does), so
// truncating it is safe; the cap keeps the basename comfortably under the
// 255-byte single-path-component limit common to ext4, APFS, and NTFS.
const schemaKindStemMax = 80

// RegisterSchemaRegistered installs the SchemaRegisteredEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology
// contract specifies "first registration wins": a second call without a
// higher Version returns kernel.ErrSchemaConflict.
func RegisterSchemaRegistered(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, schemaRegisteredSchema())
}
