package ontology

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindSourceRefRecorded is the ontology kind under which SourceRefRecordedEvent
// is registered. A sourceRefRecorded event records the external-source
// provenance of a Mazarbul record (W3C PROV wasDerivedFrom): the connector,
// stream, and external identity a subject was derived from, plus a content hash
// and a fetch timestamp.
//
// This is the single shared provenance substrate (issue #207): external
// references (#208) and the ingestion connectors (#211/#212) both record
// SourceRefs so the open source_type taxonomy and the field set never fork. It
// is a structured event/field, never a relate() endpoint — relate hard-rejects
// non-ULID endpoints, and provenance is metadata about a subject, not an edge
// between two entities.
//
// Provenance recorded here is SELF-ASSERTED and UNVERIFIED: there is no
// principal/signature trust for a SourceRef yet (that waits on the identity
// substrate, #209, which signs the carrying event's authorship but does not
// attest the external claim). The dispatcher projects each event into the
// source_refs table (migration 0030) keyed by the SourceRef's own ULID with a
// UNIQUE(dedup_key) idempotency constraint, so `maz rebuild` reconstructs the
// projection deterministically from the JSONL log.
const KindSourceRefRecorded = "sourceRefRecorded"

// VersionSourceRefRecorded is the semver-shaped schema version for
// SourceRefRecordedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionSourceRefRecorded = "v0.1.0"

// Tombstone tokens for SourceRefRecordedEvent.Tombstone. The set is narrow on
// purpose: captureExternal records one of these when the agent reports the
// external item can no longer be reached, so recall flags the cached summary
// access-unverified instead of dropping it or telling the agent to re-fetch
// content it cannot read.
const (
	// TombstoneDeleted marks the external item gone (a 404 on re-fetch).
	TombstoneDeleted = "deleted"
	// TombstoneAccessRevoked marks the external item inaccessible (a 403).
	TombstoneAccessRevoked = "access-revoked"
)

// SourceRefRecordedEvent is the payload of a sourceRefRecorded event. It is the
// typed ontology.SourceRef: the external-source provenance of a subject record
// (W3C PROV wasDerivedFrom). The Go struct is the source of truth; the JSON
// Schema is reflected from it at first access and cached.
type SourceRefRecordedEvent struct {
	// UID is the SourceRef's own ULID identity. Required. It is the source_refs
	// row primary key and the sourceRefs/{uid} resource name. Unlike a Relation's
	// random ULID, it is DERIVED deterministically from DedupKey (see
	// SourceRefUID): because source_refs carries a UNIQUE(dedup_key) constraint, a
	// random per-call uid would make recordSourceRef hand back an unresolvable
	// name whenever the dedupKey already exists (re-ingestion, or two consumers
	// citing the same source). Deriving uid from dedupKey makes PRIMARY KEY(uid)
	// and UNIQUE(dedup_key) 1:1, so re-recording the same item yields the same
	// uid, updates the row in place, and the returned name always resolves.
	// Producers should set this via SourceRefUID(DedupKey); the source_refs
	// projection and the render layer both re-derive it from DedupKey rather than
	// trusting this field, so a producer that stamps it wrong cannot poison the
	// projection — the field stays present so the event is self-describing.
	UID ULID `json:"uid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identity of this SourceRef. Derived deterministically from dedupKey (see SourceRefUID). Required."`

	// SubjectUID is the ULID of the record this external source was derived
	// from (PROV wasDerivedFrom) — an entity, episode, or fact. Required.
	SubjectUID ULID `json:"subjectUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the subject record this source was derived from (PROV wasDerivedFrom). Required."`

	// SourceType is the kind of external source, drawn from an OPEN taxonomy
	// (free-text, intentionally not an enum). Required. Examples: "email",
	// "slack_message", "notion_page", "web_page". Connectors and external
	// references share this taxonomy so it never forks.
	SourceType string `json:"sourceType" jsonschema:"required,minLength=1,description=Kind of external source. Open taxonomy free-text e.g. email|slack_message|notion_page. Required."`

	// Connector is the connector that produced this reference, e.g. "gmail",
	// "slack". Optional (a hand-appended SourceRef may omit it).
	Connector string `json:"connector,omitempty" jsonschema:"description=Connector that produced the reference e.g. gmail|slack. Optional."`

	// Stream is the sub-channel within the connector, e.g. a mailbox label or a
	// Slack channel id. Optional.
	Stream string `json:"stream,omitempty" jsonschema:"description=Sub-channel within the connector e.g. a mailbox label or channel id. Optional."`

	// ExternalID is the source system's opaque identifier for the item, e.g. a
	// message id. Optional. HASHED in any plaintext-scope render projection
	// (#207 render rule) — the raw value never reaches plaintext git.
	ExternalID string `json:"externalId,omitempty" jsonschema:"description=Source-system opaque identifier for the item e.g. a message id. Optional. Hashed in plaintext-scope render output."`

	// ExternalURL is the dereferenceable location of the source item. Optional.
	// OMITTED entirely from any plaintext-scope render projection (#207 render
	// rule); it is never emitted to plaintext git.
	ExternalURL string `json:"externalUrl,omitempty" jsonschema:"format=uri,description=Dereferenceable location of the source item. Optional. Omitted from plaintext-scope render output."`

	// ContentHash is the appender-supplied hash of the fetched content, lower
	// hex SHA-256. Optional — fetch/dereference is out of scope here (#207), so
	// it is never computed by the kernel.
	ContentHash string `json:"contentHash,omitempty" jsonschema:"pattern=^[0-9a-f]{64}$,description=Appender-supplied SHA-256 hex of the fetched content. Optional; never computed by the kernel."`

	// FetchTime is the instant the source content was fetched, in UTC.
	// Required. Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	FetchTime time.Time `json:"fetchTime" jsonschema:"required,format=date-time,description=When the source content was fetched. RFC 3339 UTC. Required."`

	// DedupKey is the idempotency key for the source_refs projection. Required,
	// non-empty. Two events sharing a DedupKey collapse to one row
	// (UNIQUE(dedup_key) upsert), so re-recording the same external item — on
	// replay or across devices — never duplicates the projection.
	DedupKey string `json:"dedupKey" jsonschema:"required,minLength=1,description=Idempotency key for the source_refs projection. Required and non-empty; UNIQUE upsert collapses re-records."`

	// FreshnessToken binds the appender-supplied ContentHash to the captured
	// summary so a replayed or forged hash cannot silently suppress a content
	// refresh. captureExternal sets it to HMAC(install secret, dedupKey +
	// contentHash + summary length); the kernel stores it verbatim and never
	// recomputes it (it holds no install secret and never fetches). On re-capture,
	// a token mismatch forces a write rather than an idempotent skip. Optional:
	// the pull connectors compute the hash server-side from the body they fetched
	// and omit it.
	FreshnessToken string `json:"freshnessToken,omitempty" jsonschema:"description=HMAC binding (dedupKey + contentHash + summary length) that detects a replayed or forged contentHash on re-capture. Optional; set by captureExternal, omitted by pull connectors."`

	// Tombstone marks a captured external item the agent reported gone or
	// inaccessible: "deleted" (a 404 on re-fetch) or "access-revoked" (a 403).
	// captureExternal re-records the SourceRef with this set (keeping the row so
	// recall still surfaces the item, flagged access-unverified). Empty for a live
	// snapshot. Open-but-narrow: only the two tokens above are meaningful.
	Tombstone string `json:"tombstone,omitempty" jsonschema:"description=Marks the external item gone/inaccessible: deleted (404) or access-revoked (403). Empty for a live snapshot."`
}

// sourceRefRecordedSchema lazily builds the SourceRefRecordedEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var sourceRefRecordedSchema = sync.OnceValue(buildSourceRefRecordedSchema)

// buildSourceRefRecordedSchema reflects SourceRefRecordedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildSourceRefRecordedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&SourceRefRecordedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal sourceRefRecorded schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindSourceRefRecorded,
		Version:    VersionSourceRefRecorded,
		Definition: raw,
		Provenance: "core",
	}
}

// SourceRefRecordedSchema returns the cached kernel.Schema for
// SourceRefRecordedEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func SourceRefRecordedSchema() kernel.Schema {
	return sourceRefRecordedSchema()
}

// SourceRefUID derives the canonical SourceRef ULID for a dedupKey. It is a pure
// function — the same dedupKey always yields the same ULID — so the source_refs
// projection (PRIMARY KEY uid, UNIQUE dedup_key) stays 1:1 and re-recording the
// same external item resolves to the same sourceRefs/{uid} resource name rather
// than minting an orphan. The 128-bit identity is the leading half of
// SHA-256(dedupKey), rendered via ulid.String(). That encoding maps any 16 bytes
// to a well-formed 26-char Crockford string and derives the first symbol from
// only the top 3 bits of the leading byte, so the symbol is always 0–7 (even
// byte 0xFF renders "7…"), well within ULID's range — ulid.Parse round-trips it
// unconditionally. The id is not time-ordered, which the projection does not
// rely on.
func SourceRefUID(dedupKey string) ULID {
	sum := sha256.Sum256([]byte(dedupKey))
	var id ulid.ULID
	copy(id[:], sum[:len(id)])
	return ULID(id.String())
}

// RegisterSourceRefRecorded installs the SourceRefRecordedEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterSourceRefRecorded(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, sourceRefRecordedSchema())
}
