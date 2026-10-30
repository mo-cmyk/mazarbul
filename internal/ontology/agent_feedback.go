package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindAgentFeedbackRecorded is the ontology kind under which one agent feedback
// or lesson capture is recorded. It is the intake of the dynamic lessons +
// feedback loop: an agent (or a human, via `maz feedback`) reports a durable
// friction point or a distilled rule it learned while using Mazarbul. Each
// capture is a DISTINCT occurrence — the dispatcher projects one agent_feedback
// row per event keyed by the carrying event uid — so the loop can count how
// often a cluster recurs and auto-promote the recurring ones into the
// per-session guidance ("## Known gotchas & lessons").
//
// Captured text is UNTRUSTED. It never rewrites the trusted base operating
// manual: promotion distills a single one-line rule into a capped, fenced,
// last-placed guidance section, and a human can pin / edit / veto any promoted
// rule. The export surface is allowlist-redacted and never emits user memory,
// principal identities, or team scope.
const KindAgentFeedbackRecorded = "agentFeedbackRecorded"

// VersionAgentFeedbackRecorded is the semver-shaped schema version for
// AgentFeedbackRecordedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionAgentFeedbackRecorded = "v0.1.0"

// AgentFeedbackRecordedEvent is the payload of an agentFeedbackRecorded event:
// one recorded friction report or lesson. The Go struct is the source of truth;
// the JSON Schema is reflected from it at first access and cached.
type AgentFeedbackRecordedEvent struct {
	// Category classifies the feedback. confusing|missing|wrong_result|slow|
	// tool_error|suggestion are friction reports; lesson is a distilled durable
	// rule the agent wants to remember. Required.
	Category string `json:"category" jsonschema:"required,enum=confusing,enum=missing,enum=wrong_result,enum=slow,enum=tool_error,enum=suggestion,enum=lesson,description=Kind of feedback. confusing|missing|wrong_result|slow|tool_error|suggestion are friction; lesson is a durable rule. Required."`

	// Severity is the reporter's sense of impact. Safety-relevant or high-impact
	// items promote faster (lower recurrence threshold). Required.
	Severity string `json:"severity" jsonschema:"required,enum=low,enum=medium,enum=high,description=Impact. high promotes at a lower recurrence threshold than low/medium. Required."`

	// Surface is the tool, CLI command, or instruction the feedback is about,
	// e.g. "recall", "maz load", "instructions". Optional but strongly
	// encouraged — it is part of the recurrence cluster key.
	Surface string `json:"surface,omitempty" jsonschema:"description=Tool / CLI / instruction the feedback is about e.g. recall|maz load. Optional; part of the cluster key."`

	// Summary is a short one-line description of the friction or rule. Required.
	Summary string `json:"summary" jsonschema:"required,minLength=1,description=One-line description of the friction or rule. Required."`

	// Detail is optional longer context (reproduction, expectation vs reality).
	Detail string `json:"detail,omitempty" jsonschema:"description=Optional longer context: reproduction or expected-vs-actual. Optional."`

	// Rule is the distilled one-line rule a lesson expresses, phrased as durable
	// guidance ("prefer X over Y because Z"). Optional for friction categories;
	// when present it is what gets promoted into the guidance section verbatim
	// (after fencing). Keep it imperative and self-contained.
	Rule string `json:"rule,omitempty" jsonschema:"description=Distilled one-line durable rule (for lessons). Promoted verbatim into the guidance section after fencing. Optional."`

	// Keywords are optional tags that aid clustering and search.
	Keywords []string `json:"keywords,omitempty" jsonschema:"description=Optional tags aiding clustering + search. Optional."`

	// Version is the Mazarbul version at capture time. Optional; auto-filled by
	// the tool. Emitted in the redacted export so diagnostics are version-keyed.
	Version string `json:"version,omitempty" jsonschema:"description=Mazarbul version at capture. Auto-filled. Optional."`

	// ActivePreset is the active steering preset at capture time, if any.
	// Optional; auto-filled by the tool.
	ActivePreset string `json:"activePreset,omitempty" jsonschema:"description=Active steering preset at capture if any. Auto-filled. Optional."`

	// ClusterKey is the recurrence-grouping key. Optional: the tool may compute
	// a semantic-assisted key (collapsing paraphrases onto one cluster); when
	// empty the dispatcher derives a deterministic LEXICAL key so replay is
	// reproducible regardless of embedder availability.
	ClusterKey string `json:"clusterKey,omitempty" jsonschema:"description=Recurrence-grouping key. Optional; derived lexically by the projection when empty so rebuild is deterministic."`
}

// KindAgentFeedbackReviewed is the ontology kind for a human review action on a
// promoted (or promotable) feedback cluster — the "+ review" half of
// auto-promote-and-review. Auto-promotion is DERIVED from recurrence; review
// events are the durable override layered on top: pin protects a rule from cap
// eviction, veto suppresses it (and stops it re-promoting), edit overrides the
// distilled text, and the un* actions reverse pin/veto. Keyed by cluster, so
// each event upserts the one review row for its cluster.
const KindAgentFeedbackReviewed = "agentFeedbackReviewed"

// VersionAgentFeedbackReviewed is the semver-shaped schema version for
// AgentFeedbackReviewedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionAgentFeedbackReviewed = "v0.1.0"

// AgentFeedbackReviewedEvent is the payload of an agentFeedbackReviewed event.
type AgentFeedbackReviewedEvent struct {
	// ClusterKey identifies the feedback cluster being reviewed (the cluster_key
	// surfaced by `maz feedback gotchas`). Required.
	ClusterKey string `json:"clusterKey" jsonschema:"required,minLength=1,description=Cluster being reviewed (the cluster_key from feedback gotchas). Required."`

	// Action is the review verb. pin|unpin toggle eviction protection; veto|
	// unveto toggle suppression; edit sets EditedRule. Required.
	Action string `json:"action" jsonschema:"required,enum=pin,enum=unpin,enum=veto,enum=unveto,enum=edit,description=Review verb: pin|unpin (eviction protection) veto|unveto (suppression) edit (override text). Required."`

	// EditedRule is the override one-liner for action=edit. Ignored otherwise.
	EditedRule string `json:"editedRule,omitempty" jsonschema:"description=Override one-liner for action=edit. Ignored for other actions. Optional."`
}

// LexicalClusterKey derives a deterministic, embedder-free recurrence key from a
// feedback item's category, surface, and salient text. It is the projection's
// fallback when an event carries no tool-computed ClusterKey, and the floor the
// tool can build a semantic-assisted key on top of. Pure function: identical
// inputs always yield the identical key, so the agent_feedback projection
// converges under `maz rebuild`.
//
// The key is "category|surface|<first few normalized alnum tokens>" — stable
// against trailing punctuation and case, coarse enough that near-identical
// reports collapse, specific enough that distinct surfaces stay distinct.
func LexicalClusterKey(category, surface, text string) string {
	const maxTokens = 6
	tokens := make([]string, 0, maxTokens)
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(f) < 3 { // drop stop-word-ish short tokens for a coarser cluster
			continue
		}
		tokens = append(tokens, f)
		if len(tokens) == maxTokens {
			break
		}
	}
	return strings.ToLower(category) + "|" + strings.ToLower(surface) + "|" + strings.Join(tokens, " ")
}

// KindAgentFeedbackPromoted is the ontology kind for the auto-promotion audit
// event appended the first time a feedback cluster crosses its recurrence
// threshold. It is the durable, append-only record that promotion happened:
// "the event log IS the changelog". Subsequent recurrences do NOT append a new
// event (idempotency is enforced by the agent_feedback_promotions table).
const KindAgentFeedbackPromoted = "agentFeedbackPromoted"

// VersionAgentFeedbackPromoted is the semver-shaped schema version for
// AgentFeedbackPromotedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionAgentFeedbackPromoted = "v0.1.0"

// AgentFeedbackPromotedEvent is the payload of an agentFeedbackPromoted event.
// One event is appended the first time a cluster crosses the threshold; the
// event is NOT repeated on subsequent recurrences (idempotent by clusterKey).
type AgentFeedbackPromotedEvent struct {
	// ClusterKey is the recurrence-grouping key that just crossed the threshold.
	ClusterKey string `json:"clusterKey" jsonschema:"required,minLength=1,description=Recurrence cluster that just crossed its promotion threshold. Required."`

	// Rule is the representative distilled rule text at promotion time.
	Rule string `json:"rule,omitempty" jsonschema:"description=Distilled rule text at promotion time. Optional."`

	// Recurrence is the count that triggered promotion.
	Recurrence int `json:"recurrence" jsonschema:"required,minimum=1,description=Recurrence count that triggered promotion. Required."`

	// Severity is the cluster's highest observed severity at promotion time.
	Severity string `json:"severity" jsonschema:"required,enum=low,enum=medium,enum=high,description=Highest observed severity at promotion time. Required."`
}

// agentFeedbackRecordedSchema / agentFeedbackReviewedSchema lazily build + cache
// the reflected JSON Schemas for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var agentFeedbackRecordedSchema = sync.OnceValue(func() kernel.Schema {
	return reflectAgentFeedbackSchema(&AgentFeedbackRecordedEvent{}, KindAgentFeedbackRecorded, VersionAgentFeedbackRecorded)
})

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var agentFeedbackReviewedSchema = sync.OnceValue(func() kernel.Schema {
	return reflectAgentFeedbackSchema(&AgentFeedbackReviewedEvent{}, KindAgentFeedbackReviewed, VersionAgentFeedbackReviewed)
})

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var agentFeedbackPromotedSchema = sync.OnceValue(func() kernel.Schema {
	return reflectAgentFeedbackSchema(&AgentFeedbackPromotedEvent{}, KindAgentFeedbackPromoted, VersionAgentFeedbackPromoted)
})

// reflectAgentFeedbackSchema reflects an in-tree event struct into a
// kernel.Schema. Panics on failure because invopop cannot fail on a well-formed
// in-tree struct.
func reflectAgentFeedbackSchema(v any, kind, version string) kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(v)
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal %s schema: %v", kind, err))
	}
	return kernel.Schema{
		Kind:       kind,
		Version:    version,
		Definition: raw,
		Provenance: "core",
	}
}

// AgentFeedbackRecordedSchema returns the cached schema for inspection/tests.
func AgentFeedbackRecordedSchema() kernel.Schema { return agentFeedbackRecordedSchema() }

// AgentFeedbackReviewedSchema returns the cached schema for inspection/tests.
func AgentFeedbackReviewedSchema() kernel.Schema { return agentFeedbackReviewedSchema() }

// AgentFeedbackPromotedSchema returns the cached schema for inspection/tests.
func AgentFeedbackPromotedSchema() kernel.Schema { return agentFeedbackPromotedSchema() }

// RegisterAgentFeedbackRecorded installs the AgentFeedbackRecordedEvent schema.
func RegisterAgentFeedbackRecorded(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, agentFeedbackRecordedSchema())
}

// RegisterAgentFeedbackReviewed installs the AgentFeedbackReviewedEvent schema.
func RegisterAgentFeedbackReviewed(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, agentFeedbackReviewedSchema())
}

// RegisterAgentFeedbackPromoted installs the AgentFeedbackPromotedEvent schema.
func RegisterAgentFeedbackPromoted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, agentFeedbackPromotedSchema())
}
