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

// KindPresetInstalled is the ontology kind under which PresetInstalledEvent is
// registered. A presetInstalled event is appended by `maz preset install`
// alongside the entityKindRegistered/predicateRegistered events that seed the
// preset's vocabulary — in one atomic batch — to mark that a preset is active
// and to carry its verbatim agent-steering instructions for brief composition.
//
// The event folds into the `presets` projection (migration 0029): install
// upserts a row, deactivation deletes it, so the table holds only ACTIVE
// presets. The minimal payload deliberately omits Priority/Activation/
// RegisteredKinds/Scope/AddedBy — the seeded kinds and predicates already
// record themselves via their own events, and a preset is simply active or not.
const KindPresetInstalled = "presetInstalled"

// VersionPresetInstalled is the semver-shaped schema version for
// PresetInstalledEvent. Must be a full MAJOR.MINOR.PATCH triplet. Bumped to
// v0.2.0 in #224, which added the optional provenance fields (SourceURL,
// PinnedRef, Checksum) for remotely-installed presets; the minor bump lets the
// richer schema re-register cleanly over a v0.1.0 store ("first registration
// wins" otherwise rejects a changed definition at the same version).
const VersionPresetInstalled = "v0.2.0"

// PresetInstalledEvent is the payload of a presetInstalled event. The Go struct
// is the source of truth; the JSON Schema is reflected from it at first access
// and cached.
type PresetInstalledEvent struct {
	// Name is the preset identifier, equal to the pack manifest name. Required.
	// Same lowercase pattern as the pack name so "preset:<name>" is well-formed.
	Name string `json:"name" jsonschema:"required,minLength=1,pattern=^[a-z][a-z0-9_-]*$,description=Preset identifier; equals the pack manifest name."`

	// Version is the pack manifest version, e.g. "0.1.0". Required.
	Version string `json:"version" jsonschema:"required,minLength=1,description=Pack manifest version, e.g. 0.1.0."`

	// Description is the one-sentence pack description. Required.
	Description string `json:"description" jsonschema:"required,minLength=1,description=One-sentence preset description from the manifest."`

	// Instructions is the VERBATIM instructions.md body, byte-capped at parse
	// time (schemapack.MaxInstructionsBytes). Required. It is carried here for
	// brief composition only and is NEVER rendered to plaintext git; the privacy
	// gate marks it sensitive on shared (non-personal) scopes.
	Instructions string `json:"instructions" jsonschema:"required,minLength=1,description=Verbatim instructions.md body. Never rendered to plaintext git."`

	// AddedAt is the instant of install, in UTC. Required. Serialized as RFC 3339.
	AddedAt time.Time `json:"addedAt" jsonschema:"required,format=date-time,description=When the preset was installed. RFC 3339 UTC. Required."`

	// SourceURL is the remote spec the preset was fetched from (the user-typed
	// github.com/... handle or full git URL). Empty for local-directory and
	// builtin installs. Optional (#224).
	SourceURL string `json:"sourceUrl,omitempty" jsonschema:"description=Remote git spec the preset was fetched from; empty for local or builtin installs."`

	// PinnedRef is the resolved commit SHA the bundle was pinned to at fetch
	// time. Empty for local/builtin installs. Optional (#224).
	PinnedRef string `json:"pinnedRef,omitempty" jsonschema:"description=Resolved commit SHA the preset bundle was pinned to."`

	// Checksum is the canonical bundle checksum ("sha256:..."), recorded for
	// every remote install regardless of whether a --checksum gate was supplied.
	// Empty for local/builtin installs. Optional (#224).
	Checksum string `json:"checksum,omitempty" jsonschema:"description=Canonical bundle content checksum, sha256:hex."`
}

// presetInstalledSchema lazily builds the PresetInstalledEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var presetInstalledSchema = sync.OnceValue(buildPresetInstalledSchema)

// buildPresetInstalledSchema reflects PresetInstalledEvent into a kernel.Schema.
// Runs once at first call; panics on failure because invopop cannot fail on a
// well-formed in-tree struct.
func buildPresetInstalledSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&PresetInstalledEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal presetInstalled schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindPresetInstalled,
		Version:    VersionPresetInstalled,
		Definition: raw,
		Provenance: "core",
	}
}

// PresetInstalledSchema returns the cached kernel.Schema for
// PresetInstalledEvent. Useful for tests and consumers that need to inspect the
// schema without registering it.
func PresetInstalledSchema() kernel.Schema {
	return presetInstalledSchema()
}

// RegisterPresetInstalled installs the PresetInstalledEvent schema with o. Call
// once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterPresetInstalled(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, presetInstalledSchema())
}
