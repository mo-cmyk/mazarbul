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

// KindInsightSynthesized is the ontology kind under which
// InsightSynthesizedEvent is registered. An insightSynthesized event is
// appended by the autonomous reflection job: it clusters a connected
// neighborhood of entities (1-2 hop graph traversal) and LLM-synthesizes a
// durable insight from them (issue #231).
//
// The event MUST carry a non-empty SourceUids list — the records the insight
// was derived from. This is the auditable-provenance contract that
// distinguishes Mazarbul's derived facts from the competitor's opaque LLM
// blobs: every synthesized insight cryptographically cites its sources, and
// the dispatcher rejects an empty SourceUids list as a hard error.
//
// The dispatcher projects each event into the insights table (migration 0038)
// keyed by InsightUID with INSERT OR IGNORE idempotency, so `maz rebuild`
// reconstructs the projection deterministically from the JSONL log.
// Confidence DECAYS via a periodic projection-side sweep and a low-confidence
// insight is DEMOTED out of hot recall (state='demoted') — never deleted. The
// event stays in the log, so `maz rebuild` (which replays only the synthesis
// event and resets current_confidence to synthesized_confidence, before any
// sweep runs) RESTORES the insight to hot recall.
const KindInsightSynthesized = "insightSynthesized"

// VersionInsightSynthesized is the semver-shaped schema version for
// InsightSynthesizedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionInsightSynthesized = "v0.1.0"

// InsightSynthesizedEvent is the payload of an insightSynthesized event — a
// reflection-job-derived insight over a cluster of related entities. The Go
// struct is the source of truth; the JSON Schema is reflected from it at first
// access and cached.
type InsightSynthesizedEvent struct {
	// InsightUID is the insight's own ULID identity. Required. It is the
	// insights row primary key. Unlike a SourceRef's derived uid, an insight's
	// uid is a fresh random ULID minted by the reflection job (insights are not
	// deduped by a key); the dispatcher upserts via INSERT OR IGNORE keyed on
	// this uid so a Rebuild replay of the same event is a no-op.
	InsightUID ULID `json:"insightUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identity of this insight. Required. Fresh random ULID minted by the reflection job."`

	// Statement is the LLM-synthesized insight prose. Required, non-empty.
	Statement string `json:"statement" jsonschema:"required,minLength=1,description=The synthesized insight prose. Required and non-empty."`

	// InsightType is the kind of insight, drawn from an OPEN taxonomy
	// (free-text, intentionally not an enum). Required. Examples: "pattern",
	// "preference", "summary".
	InsightType string `json:"insightType" jsonschema:"required,minLength=1,description=Kind of insight. Open taxonomy free-text e.g. pattern|preference|summary. Required."`

	// SourceUids is the set of record ULIDs this insight was synthesized from
	// (auditable provenance — every derived fact cites its sources). Required
	// and NON-EMPTY: the dispatcher rejects an empty list as a hard error, and
	// the reflection job rejects any uid the LLM did not draw from the offered
	// candidate set (no hallucinated provenance).
	SourceUids []ULID `json:"sourceUids" jsonschema:"required,minItems=1,description=Record ULIDs this insight was synthesized from. Required and non-empty (auditable provenance)."`

	// SeedEntityUID is the entity whose 1-2 hop neighborhood seeded this
	// reflection. Required. Provenance for the cluster and the grouping key for
	// the decay sweep.
	SeedEntityUID ULID `json:"seedEntityUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=Entity whose neighborhood seeded this reflection. Required."`

	// SynthesizedConfidence is the writer's confidence in the insight at
	// synthesis time, in the closed interval 0..1. Required. It is the IMMUTABLE
	// decay baseline: the projection's current_confidence starts here and is
	// lowered by the periodic decay sweep; `maz rebuild` resets current_confidence
	// to this value. Distinct from the write-time groundedness Confidence on
	// entities/relations, which never decays.
	SynthesizedConfidence float64 `json:"synthesizedConfidence" jsonschema:"required,minimum=0,maximum=1,description=Confidence in the insight at synthesis time. Range 0..1. Required. Immutable decay baseline."`

	// SynthesizedAt is the instant the insight was synthesized, in UTC.
	// Required. Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	SynthesizedAt time.Time `json:"synthesizedAt" jsonschema:"required,format=date-time,description=When the insight was synthesized. RFC 3339 UTC. Required."`
}

// insightSynthesizedSchema lazily builds the InsightSynthesizedEvent JSON
// Schema document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var insightSynthesizedSchema = sync.OnceValue(buildInsightSynthesizedSchema)

// buildInsightSynthesizedSchema reflects InsightSynthesizedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildInsightSynthesizedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&InsightSynthesizedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal insightSynthesized schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindInsightSynthesized,
		Version:    VersionInsightSynthesized,
		Definition: raw,
		Provenance: "core",
	}
}

// InsightSynthesizedSchema returns the cached kernel.Schema for
// InsightSynthesizedEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func InsightSynthesizedSchema() kernel.Schema {
	return insightSynthesizedSchema()
}

// RegisterInsightSynthesized installs the InsightSynthesizedEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterInsightSynthesized(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, insightSynthesizedSchema())
}
