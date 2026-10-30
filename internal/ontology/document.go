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

// KindDocumentRevision is the ontology kind under which DocumentRevision is
// registered. It matches the EventKind emitted by the document write pipeline
// when a DocumentRevision payload is appended to the event log. Each
// meaningful save of a vault document produces one such event carrying the
// verbatim body, so the event log is the single source of truth for every
// revision of every document.
const KindDocumentRevision = "documentRevision"

// KindDocumentDeleted is the ontology kind for the soft-delete tombstone event.
// The event sets deleted=1 in the derived store but keeps the documents row so
// Rebuild can replay the tombstone deterministically and arrive at the same
// end-state. The JSONL log retains the original revisions and the deleted event
// for full provenance.
const KindDocumentDeleted = "documentDeleted"

// VersionDocumentRevision is the semver-shaped schema version for
// DocumentRevision. V0.x.y signals that breaking changes are allowed before
// v1.0 and that the full migration story is pending. Must be a full
// MAJOR.MINOR.PATCH triplet because kernel.MemoryOntology.Register validates
// with golang.org/x/mod/semver.IsValid, which rejects shorter forms like "v0.1".
const VersionDocumentRevision = "v0.1.0"

// VersionDocumentDeleted is the semver-shaped schema version for
// DocumentDeleted. Mirrors VersionDocumentRevision versioning convention.
const VersionDocumentDeleted = "v0.1.0"

// WikiLink represents one [[wikilink]] extracted from a document body.
// The Raw field carries the full wikilink literal (e.g. "[[Rosie Cotton]]") so
// the extractor can round-trip the original text. Target is the resolved or
// raw display name; Section, Alias, RelType, and Confidence are optional typed
// extensions for the richer graph (see plan §3 for the typed wikilink syntax).
//
// WikiLinks are stored in DocumentRevision.Wikilinks; P1 resolves them to
// canonical entities via the resolver.matchMention pipeline and emits
// relation events.
type WikiLink struct {
	// Raw is the full wikilink literal, e.g. "[[Rosie Cotton]]". Required.
	Raw string `json:"raw"`

	// Target is the resolved or raw display name of the link target.
	// Required. When the link is unresolved, Target holds the raw display
	// name; the resolver back-fills the entity UID in P1.
	Target string `json:"target"`

	// Section is the in-document section anchor, e.g. "## Goals". Optional.
	// Present when the wikilink syntax is [[Target#Section]].
	Section string `json:"section,omitempty"`

	// Alias is the display override, e.g. in [[Target|Alias]]. Optional.
	Alias string `json:"alias,omitempty"`

	// RelType is the typed relation extension, e.g. "relates_to" or
	// "contradicts" from [[relates_to::Target]] syntax. Optional. Empty for
	// standard [[Target]] wikilinks.
	RelType string `json:"relType,omitempty"`

	// Confidence is the stated confidence for typed wikilinks, in the closed
	// interval 0..1. Optional (zero value means "not stated"). Used when the
	// typed extension carries a confidence, e.g. [contradicts:0.8] [[Y]].
	Confidence float64 `json:"confidence,omitempty"`
}

// Observation represents a structured fact extracted from a document body
// using the de-facto `- [category] text #tag (context)` convention pioneered
// by basic-memory and claude-obsidian. Observations become relation/fact events
// in P6 once the parser for typed observation lines lands.
type Observation struct {
	// Category is the observation type, e.g. "note", "fact", "action". Required.
	Category string `json:"category"`

	// Text is the observation content. Required.
	Text string `json:"text"`

	// Tags is the set of #-prefixed tags associated with this observation.
	// Optional. Order is not significant.
	Tags []string `json:"tags,omitempty"`

	// Context is a free-form contextual note attached to the observation,
	// e.g. an in-line parenthetical. Optional.
	Context string `json:"context,omitempty"`
}

// DocumentRevision is the event payload for one meaningful save of a vault
// document. The Go struct is the source of truth; the JSON Schema is reflected
// from it at first access via invopop/jsonschema, so the wire shape and the
// type cannot drift.
//
// Design axioms (from plan §A and §"Ontology & storage"):
//
//   - Body is stored VERBATIM (never normalised) so the vault projector can
//     materialise a byte-stable local file and the content-hash round-trip
//     holds. I1 of the bidirectional watcher invariants depends on this.
//   - DocumentUID is the stable ULID identity of the underlying entity across
//     revisions; it maps to the EntityKindDocument entity row. WikiLinks survive
//     document renames because they resolve to the entity UID, not the path.
//   - UID (the event UID, = revision UID) chains revisions: PreviousRevisionUID
//     links to the event UID of the immediately preceding revision.
//
// Field naming follows Google AIP: createTime (not createdAt), camelCase
// throughout. Optional fields carry omitempty so absent fields are omitted.
type DocumentRevision struct {
	// Name is the AIP resource name in the form "documentRevisions/{uid}".
	// Output-only; assigned by the writer after the event UID is known.
	// Validated by pattern when present, so a caller who supplies it must
	// supply the canonical form.
	Name string `json:"name,omitempty" jsonschema:"pattern=^documentRevisions/[0-9A-HJKMNP-TV-Z]{26}$,description=Resource name in the form documentRevisions/{uid}. Output-only; assigned by the writer after the UID is known."`

	// UID is the ULID identifier for this revision event. Output-only.
	// Mirrors the outer Event.UID. The ULID embeds a timestamp so revision
	// chains are sortable chronologically by UID alone.
	UID string `json:"uid,omitempty" jsonschema:"pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identifier for this revision. Output-only; mirrors the outer Event.UID."`

	// CreateTime is the instant this revision was first observed, in UTC.
	// Output-only. Serialized as an RFC 3339 string when present.
	CreateTime time.Time `json:"createTime,omitempty" jsonschema:"format=date-time,description=Time the revision was first observed. RFC 3339 UTC. Output-only."`

	// DocumentUID is the stable ULID identity of the document across all its
	// revisions. Required. Maps to the EntityKindDocument entity row. Immutable:
	// even if the document is moved to a new path, DocumentUID stays constant so
	// wikilinks that resolve to this document survive the rename.
	DocumentUID string `json:"documentUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=Stable ULID identity of the document across all revisions. Immutable; maps to the EntityKindDocument entity row."`

	// Path is the vault-relative POSIX path of the document, e.g.
	// "notes/project-alpha.md". Required. Non-empty. The path is frozen at
	// first creation (slug is derived from it); moveDocument (P2) updates path
	// while keeping DocumentUID constant.
	Path string `json:"path" jsonschema:"required,minLength=1,description=Vault-relative POSIX path of the document, e.g. notes/project-alpha.md. Required."`

	// Title is the document title, extracted from the first top-level heading
	// or frontmatter title field. Optional. Empty for documents without a
	// discernible title.
	Title string `json:"title,omitempty" jsonschema:"description=Document title from the first heading or frontmatter title. Optional."`

	// Body is the VERBATIM file body, stored opaquely and never normalised.
	// Required. The dispatcher stores this byte-for-byte so the vault projector
	// can reproduce an on-disk file that hashes to ContentHash. Callers must
	// not normalise line endings or strip trailing newlines before setting Body.
	Body string `json:"body" jsonschema:"required,description=Verbatim file body. Stored opaquely and never normalised. Required."`

	// Frontmatter is the parsed YAML frontmatter as a JSON object. Optional.
	// Absent when the document has no frontmatter block. The raw bytes are
	// preserved separately in FrontmatterRaw for byte-stable round-trips;
	// this field carries the decoded key-value map for downstream queries.
	Frontmatter json.RawMessage `json:"frontmatter,omitempty" jsonschema:"description=Parsed YAML frontmatter as a JSON object. Optional; absent when no frontmatter block is present."`

	// FrontmatterRaw holds the exact frontmatter bytes (including the
	// opening/closing '---' delimiters) for byte-stable round-trips. Optional.
	// Empty when the document has no frontmatter block.
	FrontmatterRaw string `json:"frontmatterRaw,omitempty" jsonschema:"description=Exact frontmatter bytes including --- delimiters. Optional; empty when no frontmatter block is present."`

	// ContentHash is the SHA-256 hex digest of the exact file bytes (before
	// any encoding). Required. The dispatcher uses this for the content-hash
	// idempotency check (I1): a revision is only appended when the on-disk hash
	// differs from the latest revision's ContentHash.
	ContentHash string `json:"contentHash" jsonschema:"required,pattern=^[0-9a-f]{64}$,description=SHA-256 hex digest of the exact file bytes. Required."`

	// Wikilinks holds every [[wikilink]] extracted from the body. Optional.
	// Empty until the wikilink scanner runs (P1). Populated by the ingest
	// pipeline; wikilink resolution to canonical entities emits relation events
	// alongside the document revision event.
	Wikilinks []WikiLink `json:"wikilinks,omitempty" jsonschema:"description=Wikilinks extracted from the body. Optional; populated by the wikilink scanner in P1."`

	// Observations holds structured observations extracted from the body using
	// the '- [category] text #tag' convention. Optional. Empty until the
	// observation parser runs (P6). Each observation becomes a relation/fact
	// event in P6.
	Observations []Observation `json:"observations,omitempty" jsonschema:"description=Structured observations extracted from the body. Optional; populated by the observation parser in P6."`

	// PreviousRevisionUID links this revision to the immediately preceding
	// revision by its event UID. Optional. Absent for the first revision of a
	// document (when no prior revision exists). Forms the revision chain used by
	// 'maz doc log <path>' to enumerate the full history.
	PreviousRevisionUID *string `json:"previousRevisionUid,omitempty" jsonschema:"pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=Event UID of the immediately preceding revision. Optional; absent for the first revision."`

	// Pinned marks the document as always included in the context brief (Section
	// C of the plan). Optional. Defaults to false when absent. Corresponds to
	// frontmatter 'pinned: true' or the '--pin' flag on 'maz doc new'.
	Pinned bool `json:"pinned,omitempty" jsonschema:"description=When true, this document is always included in the context brief. Optional."`

	// Tags is a free-form set of string labels attached to the document.
	// Optional. Order is not significant; populated from frontmatter 'tags:' or
	// inline #-tags in the body.
	Tags []string `json:"tags,omitempty" jsonschema:"description=Free-form string labels attached to the document. Optional."`
}

// DocumentDeleted is the event payload for a soft-delete tombstone. Soft-
// deletion sets deleted=1 in the derived store and strips the document from
// FTS5 + vec lanes, but the documents row is preserved so Rebuild can replay
// the tombstone deterministically. The JSONL log retains the original revisions
// and this event for full provenance.
//
// Field naming follows Google AIP: camelCase, no *_at suffixes.
type DocumentDeleted struct {
	// DocumentUID is the stable ULID identity of the document being deleted.
	// Required. Maps to the documents.document_uid row that will be soft-deleted.
	DocumentUID string `json:"documentUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=Stable ULID identity of the document to soft-delete. Required."`

	// Path is the vault-relative path of the document at deletion time. Optional.
	// Included for human-readable audit logs; the dispatcher keys the operation
	// on DocumentUID, not path.
	Path string `json:"path,omitempty" jsonschema:"description=Vault-relative path of the document at deletion time. Optional; for audit purposes only."`

	// Reason is a free-form human-readable explanation for the deletion. Optional.
	// Examples: "duplicate of notes/foo.md", "superseded by structured facts".
	Reason string `json:"reason,omitempty" jsonschema:"description=Free-form explanation for the deletion. Optional."`
}

// documentRevisionSchema lazily builds the DocumentRevision JSON Schema
// document once and caches it for the process lifetime. Invopop reflection is
// not free; sync.OnceValue makes the cost pay-once regardless of how many
// callers invoke RegisterDocumentRevision or DocumentRevisionSchema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var documentRevisionSchema = sync.OnceValue(buildDocumentRevisionSchema)

// buildDocumentRevisionSchema reflects the DocumentRevision struct into a
// kernel.Schema. Runs once at first call to DocumentRevisionSchema or
// RegisterDocumentRevision; panics on failure because invopop cannot fail on a
// well-formed in-tree struct and a startup-time encoding error is unrecoverable.
func buildDocumentRevisionSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&DocumentRevision{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal documentRevision schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindDocumentRevision,
		Version:    VersionDocumentRevision,
		Definition: raw,
		Provenance: "core",
	}
}

// DocumentRevisionSchema returns the cached kernel.Schema document for
// DocumentRevision. Useful for tests and callers that need to inspect the
// schema without registering it.
func DocumentRevisionSchema() kernel.Schema {
	return documentRevisionSchema()
}

// RegisterDocumentRevision installs the DocumentRevision schema with o. Call
// once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict; this function does not retry or suppress
// that error.
//
// Named per-kind (RegisterDocumentRevision rather than the generic Register) so
// sibling kinds (Episode, Entity, Mention, Relation, etc.) coexist in this
// package without a function-name collision.
func RegisterDocumentRevision(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, documentRevisionSchema())
}

// documentDeletedSchema lazily builds the DocumentDeleted JSON Schema document
// once and caches it for the process lifetime. Mirrors documentRevisionSchema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var documentDeletedSchema = sync.OnceValue(buildDocumentDeletedSchema)

// buildDocumentDeletedSchema reflects the DocumentDeleted struct into a
// kernel.Schema. Runs once at first call to DocumentDeletedSchema or
// RegisterDocumentDeleted; panics on failure because invopop cannot fail on a
// well-formed in-tree struct and a startup-time encoding error is unrecoverable.
func buildDocumentDeletedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&DocumentDeleted{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal documentDeleted schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindDocumentDeleted,
		Version:    VersionDocumentDeleted,
		Definition: raw,
		Provenance: "core",
	}
}

// DocumentDeletedSchema returns the cached kernel.Schema document for
// DocumentDeleted. Useful for tests and callers that need to inspect the
// schema without registering it.
func DocumentDeletedSchema() kernel.Schema {
	return documentDeletedSchema()
}

// RegisterDocumentDeleted installs the DocumentDeleted schema with o. Call
// once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict; this function does not retry or suppress
// that error.
//
// Named per-kind (RegisterDocumentDeleted rather than the generic Register) so
// sibling kinds coexist without a function-name collision.
func RegisterDocumentDeleted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, documentDeletedSchema())
}
