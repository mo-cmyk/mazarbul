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

// KindInsightDismissed is the ontology kind under which InsightDismissedEvent
// is registered. An insightDismissed event is appended when a USER explicitly
// dismisses a synthesized insight from the Insight Inbox (e.g. "this pattern is
// wrong / not useful"). It is the user-initiated, event-sourced counterpart to
// the projection-side decay sweep.
//
// The two MUST NOT be conflated. The decay sweep (internal/kernel/cognitive/
// decay.go) lowers current_confidence and flips state to 'demoted' purely as a
// recomputable projection — it appends no event, and `maz rebuild` resets a
// demoted insight back to 'hot'. Dismissal is the opposite: it is a durable,
// auditable user decision carried in the event log, so the dispatcher projects
// it to the DISTINCT terminal state 'dismissed'. A dismissed insight is never
// re-promoted by reinforce-on-access (which only lifts 'demoted') and survives
// `maz rebuild` because the dismissal event replays after the synthesis event.
//
// Recall continues to surface only state='hot' insights, so a dismissed insight
// (like a demoted one) drops out of hot recall — but for a different, explicit
// reason that the log preserves.
const KindInsightDismissed = "insightDismissed"

// VersionInsightDismissed is the semver-shaped schema version for
// InsightDismissedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionInsightDismissed = "v0.1.0"

// InsightDismissedEvent is the payload of an insightDismissed event — a user's
// explicit dismissal of a synthesized insight. The Go struct is the source of
// truth; the JSON Schema is reflected from it at first access and cached.
type InsightDismissedEvent struct {
	// InsightUID is the ULID of the insight being dismissed. Required. It is
	// the insights row primary key (the same uid carried by the originating
	// insightSynthesized event), so the projection updates exactly that row's
	// state to 'dismissed'. Dismissing a uid that does not (yet) exist is a
	// no-op on the projection — the dispatcher does not fabricate a row.
	InsightUID ULID `json:"insightUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the insight being dismissed. Required. The insights row primary key."`

	// Reason is the user's optional free-text justification for dismissing the
	// insight (e.g. "incorrect inference", "no longer relevant"). Optional;
	// retained on the event for audit but not projected into a queryable column
	// in this version.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional free-text reason for the dismissal, retained for audit. Optional."`

	// DismissedAt is the instant the user dismissed the insight, in UTC.
	// Required. Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	DismissedAt time.Time `json:"dismissedAt" jsonschema:"required,format=date-time,description=When the user dismissed the insight. RFC 3339 UTC. Required."`
}

// insightDismissedSchema lazily builds the InsightDismissedEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var insightDismissedSchema = sync.OnceValue(buildInsightDismissedSchema)

// buildInsightDismissedSchema reflects InsightDismissedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildInsightDismissedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&InsightDismissedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal insightDismissed schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindInsightDismissed,
		Version:    VersionInsightDismissed,
		Definition: raw,
		Provenance: "core",
	}
}

// InsightDismissedSchema returns the cached kernel.Schema for
// InsightDismissedEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func InsightDismissedSchema() kernel.Schema {
	return insightDismissedSchema()
}

// RegisterInsightDismissed installs the InsightDismissedEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterInsightDismissed(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, insightDismissedSchema())
}
