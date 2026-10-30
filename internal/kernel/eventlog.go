// Package kernel exposes the contracts that bind Mazarbul's core: the event
// log, the four-layer storage, the ontology registry, and the query composer.
// Real implementations land in M1 issues; this file holds only the interfaces,
// the data shapes they exchange, and Noop fallbacks that downstream code can
// compile against until concrete writers and readers exist.
//
// AIP naming applies throughout: every wire-shaped field uses createTime
// (not createdAt), every resource path is plural lowercase, every method
// name is a CRUD verb where one fits. See agent-docs/synthesis.md section 3.
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/hlc"
)

// EffectiveHLC returns the hybrid-logical-clock stamp to use for total-order
// sorting. If the event was stamped at write time (all events written after the
// HLC wave), e.HLC is returned verbatim. For legacy events that predate the
// wave, the implicit HLC — Encode(e.CreateTime.UnixMilli(), 0) — is derived
// from the immutable CreateTime field. It is a pure function: every machine
// computes the same implicit stamp for the same event, keeping cross-machine
// sort order convergent for pre-wave logs.
func (e *Event) EffectiveHLC() string {
	if e.HLC != "" {
		return e.HLC
	}
	return hlc.Implicit(e.CreateTime)
}

// EventKind identifies the shape of an event's payload. Open enum: providers
// and connectors can register new kinds via the ontology registry. M1 ships
// with the seed kind "episode" (see internal/ontology/episode.go); later
// kinds (recall_hit, supersede, forget, merge, ...) Join as the relevant
// ontology schemas register.
type EventKind string

// Scope identifies the sync / encryption boundary an event belongs to.
// Closed enum: every event must be in exactly one scope. The default
// (when callers leave Event.Scope empty) is ScopePersonal so existing
// JSONL files written before scope tagging existed coerce to private
// per-device state, never accidentally leaking into a shared scope.
type Scope string

// Scopes recognised by the kernel. Project = in-repo, plaintext (already
// public via the git repo); personal = per-user, encrypted, syncs across
// the user's paired devices; team = org-scoped, encrypted, ACL'd by
// repo collaborators (UI-disabled until M5). Draft = device-local staging
// for speculative writes: deliberately excluded from both DefaultReadSet
// and AllScopes (so it never reaches default recall OR graph navigation)
// and never synced, so unreviewed memory cannot pollute recall. It is
// reachable only via an explicit NewScopeSet(ScopeDraft) at the dedicated
// `maz scope draft` call sites; promote re-appends as personal.
const (
	ScopeProject  Scope = "project"
	ScopePersonal Scope = "personal"
	ScopeTeam     Scope = "team"
	ScopeDraft    Scope = "draft"
)

// DefaultScope is what prepare() backfills onto events whose Scope is
// left empty by the caller. Personal is the safe default: never
// accidentally publishes data into a shared scope.
const DefaultScope = ScopePersonal

// Valid reports whether s is a recognised scope. Empty scope returns
// false so callers reading raw JSONL can distinguish "unset" (which
// the kernel will default-to-personal) from "explicitly invalid".
func (s Scope) Valid() bool {
	switch s {
	case ScopeProject, ScopePersonal, ScopeTeam, ScopeDraft:
		return true
	default:
		return false
	}
}

// Event is the unit of writes across Mazarbul. The event log is content-
// addressed and append-only; every Event is immutable once Append returns.
//
// Field naming follows Google AIP: createTime not createdAt, uid as the
// stable opaque identifier (a ULID at runtime), name as the AIP-style
// resource name (here "events/{uid}").
type Event struct {
	// Name is the AIP-style resource name: "events/{uid}". Output-only; the
	// writer assigns it from the uid.
	Name string `json:"name"`

	// UID is a ULID. Sortable, opaque, immutable. Assigned by the writer.
	UID string `json:"uid"`

	// CreateTime is the time the event was appended, in UTC. Output-only.
	CreateTime time.Time `json:"createTime"`

	// Kind classifies the event payload. The ontology registry knows the
	// schema for each kind.
	Kind EventKind `json:"kind"`

	// Source identifies where the event originated: a user note, a
	// connector pull, an enrichment proposal, etc. Free-form during M1;
	// becomes typed in M2 when the source taxonomy lands.
	Source string `json:"source"`

	// Confidence is the writer's stated confidence in the payload. 1.0 for
	// direct user notes; lower for inference, enrichment, or low-quality
	// extraction.
	Confidence float64 `json:"confidence"`

	// Payload is the kind-specific event body. Validated against the
	// ontology's JSON Schema for Kind when the schema exists.
	Payload json.RawMessage `json:"payload"`

	// Scope binds the event to a sync / encryption boundary
	// (project | personal | team). Optional at construction time;
	// FileEventLog.prepare() defaults an empty Scope to
	// DefaultScope (personal) so existing JSONL files round-trip
	// without modification. omitempty keeps the on-disk shape
	// backwards-compatible: events from pre-scope builds have no
	// "scope" key and reload as the zero value, which prepare()
	// then upgrades to personal.
	Scope Scope `json:"scope,omitempty"`

	// HLC is the sortable hybrid-logical-clock stamp assigned by
	// FileEventLog.prepare() at write time. Encoding: 16 lowercase hex chars =
	// 12-hex wall-ms + 4-hex logical counter (see internal/kernel/hlc).
	// Lexicographic order equals numeric order, so HLC-ordered events sort
	// correctly as strings. omitempty keeps the on-disk shape
	// backwards-compatible: events written before the HLC wave have no "hlc"
	// key and round-trip with HLC == "", which EffectiveHLC() transparently
	// maps to hlc.Implicit(CreateTime). Grafted peer events carry their
	// original HLC verbatim (preserved by graftPersistOne); local events are
	// always stamped before signing so CanonicalBytes v2 covers the HLC.
	HLC string `json:"hlc,omitempty"`

	// Author is the mandatory signed authorship block: who (principal)
	// on which device produced this event, plus an ed25519 signature
	// over the event's canonical bytes. Stamped by the injected Signer
	// in prepare(); verified for team-scope events during Rebuild. There
	// is no omitempty — every event carries an Author (events predating
	// the field are re-stamped once by the sign-events migration). See
	// event_author.go for the canonical-bytes spec and the Signer /
	// Verifier contracts.
	Author EventAuthor `json:"author"`
}

// Receipt is what Append returns when an event has landed in durable storage.
// The caller can wait on Synced() to confirm the four-layer dispatchers have
// also caught up, but most callers only need the UID.
type Receipt struct {
	UID        string
	CreateTime time.Time
}

// ProvenanceRecord captures the source/confidence/decided-at triple that the
// dispatcher writes alongside an Episode event. The fields mirror the columns
// in the `provenance` table created by migration 0001 and constrained to
// one row per event by migration 0009.
//
// Derived records (mentions, relations) inherit their provenance from the
// source Episode via source_event_uid; callers that need provenance for a
// mention or relation walk back to the episode and then call Provenance.
type ProvenanceRecord struct {
	// EventUID identifies the Episode event whose provenance is recorded.
	EventUID string `json:"eventUid"`

	// Source mirrors Event.Source: where the event originated (user note,
	// connector pull, enrichment proposal, etc.).
	Source string `json:"source"`

	// Confidence mirrors Event.Confidence: the writer's stated confidence
	// in the payload, 0.0 to 1.0 inclusive.
	Confidence float64 `json:"confidence"`

	// DecidedAt is the wall-clock instant the provenance was recorded.
	// In M1 this equals Event.CreateTime; later milestones may distinguish
	// the two when enrichment pipelines write provenance asynchronously.
	DecidedAt time.Time `json:"decidedAt"`
}

// ReadOpts narrow what the Read iterator returns. Empty value reads
// everything in append order.
type ReadOpts struct {
	// Since is the inclusive lower bound on CreateTime. Zero value means "no
	// lower bound".
	Since time.Time

	// Until is the exclusive upper bound on CreateTime. Zero value means "no
	// upper bound".
	Until time.Time

	// Kinds restricts the iterator to the listed event kinds. Empty means
	// "all kinds".
	Kinds []EventKind

	// Limit caps the number of events returned. Zero means "no limit".
	Limit int
}

// EventLog is the immutable, append-only source of truth for every record
// Mazarbul holds. The on-disk layout is JSONL files in events/YYYY/MM/DD.jsonl
// per the architecture spec; this interface hides the layout from callers.
//
// Implementations must guarantee:
//   - Append is atomic per event. A partial write must not be observable.
//   - Read returns events in chronological order by CreateTime, with ULID
//     monotonic ordering as the within-millisecond tiebreaker. For events
//     appended with strictly monotonic CreateTime, chronological order
//     coincides with append order; for backfill or replay batches whose
//     CreateTime is older than wall-clock, persisted order follows
//     CreateTime, not insertion order. The iterator is single-pass.
//   - At returns the event identified by uid, or ErrNotFound if it does not
//     exist.
//   - Rebuild reconstructs every derived store (graph, vector, FTS) from
//     the log alone; the log is the only durable artifact.
type EventLog interface {
	Append(ctx context.Context, events []Event) ([]Receipt, error)
	Read(ctx context.Context, opts ReadOpts) iter.Seq2[Event, error]
	At(ctx context.Context, uid string) (Event, error)
	Rebuild(ctx context.Context, into Storage, opts ...RebuildOption) ([]ApplyError, error)

	// Provenance returns the provenance records associated with an
	// Episode event UID. Returns an empty slice (not error) when the
	// event has no recorded provenance, which is the case for events
	// that pre-date migration 0009 or for non-Episode events. Callers
	// fetching provenance for a Mention or Relation should walk back to
	// the source Episode via source_event_uid and pass that UID here.
	Provenance(ctx context.Context, eventUID string) ([]ProvenanceRecord, error)
}

// ErrNotFound is returned by At and by other lookup helpers when the
// requested resource does not exist.
var ErrNotFound = errors.New("kernel: not found")

// ErrNotImplemented is returned by Noop placeholders and by partial
// implementations whose remaining methods land in follow-up issues.
var ErrNotImplemented = errors.New("kernel: not yet implemented")

// ApplyError records a single event that failed to Apply during Rebuild.
// In non-strict mode Rebuild collects these and continues; in strict mode
// the first ApplyError aborts the rebuild and is wrapped in the returned
// error so callers can branch on errors.As.
type ApplyError struct {
	UID string
	Err error
}

// Error implements error.
func (e ApplyError) Error() string { return fmt.Sprintf("apply uid=%s: %v", e.UID, e.Err) }

// Unwrap exposes the underlying Apply failure so errors.Is / errors.As
// can match against dispatcher-layer sentinels.
func (e ApplyError) Unwrap() error { return e.Err }

// RebuildOption configures a Rebuild call. Options compose via functional
// arguments so the interface signature stays stable as new knobs land.
type RebuildOption func(*rebuildConfig)

// JobReporter is the subset of jobs.Tracker the rebuild loop uses.
// Declared here (rather than importing internal/kernel/jobs) to avoid
// a circular import: the jobs package may eventually import the kernel.
type JobReporter interface {
	// Update records fractional progress (0..1) and an optional detail string.
	Update(progress float64, detail string)
	// Done marks the job succeeded.
	Done()
	// Fail marks the job failed.
	Fail(err error)
}

// JobStarter opens a new tracked job and returns its reporter plus a
// child context that is canceled when the job is canceled externally.
// The signature matches jobs.Registry.Begin so callers can pass the
// registry directly via WithJobReporter.
type JobStarter interface {
	Begin(ctx context.Context, kind, title string) (JobReporter, context.Context)
}

// rebuildConfig is the merged option set Rebuild consults. Unexported so
// callers cannot construct one directly; all configuration goes through
// the With* helpers below. Exported fields would invite drift between
// the public surface and what Rebuild actually reads.
type rebuildConfig struct {
	progress      io.Writer
	progressEvery int
	strict        bool
	jobs          JobStarter
}

// defaultProgressEvery is the cadence at which Rebuild emits progress
// lines when WithProgressEvery is not supplied. 100 events is small
// enough to keep an interactive operator updated and large enough that
// fast rebuilds do not drown the terminal in output.
const defaultProgressEvery = 100

// WithProgress installs an io.Writer that receives one human-readable
// line per progress tick plus a terminal summary. A nil writer is
// silently ignored so callers can wire it conditionally without a guard.
// The format is plain ASCII: "rebuilt {n} events  skipped={s}
// elapsed={d}  rate={r}/s\n". The total event count is not known in
// advance (the Read iterator is streaming), so the line reports counts
// and rate rather than a percentage or ETA. A summary line with the
// same shape is always emitted at the end when progress is non-nil so
// the caller sees a terminal count even for short rebuilds.
func WithProgress(w io.Writer) RebuildOption {
	return func(c *rebuildConfig) {
		if w != nil {
			c.progress = w
		}
	}
}

// WithProgressEvery overrides the default cadence at which progress
// lines are written. A non-positive value reverts to the default.
func WithProgressEvery(n int) RebuildOption {
	return func(c *rebuildConfig) {
		if n > 0 {
			c.progressEvery = n
		}
	}
}

// WithStrict aborts Rebuild at the first ApplyError. The returned error
// wraps the offending ApplyError so callers can errors.As to recover the
// UID. Use for replays that must reach a known-good state before
// continuing (test setup, recovery scripts); operational rebuilds prefer
// the default best-effort mode where every event gets a chance.
func WithStrict() RebuildOption {
	return func(c *rebuildConfig) { c.strict = true }
}

// WithJobReporter wires a JobStarter into the rebuild loop. When set,
// Rebuild calls Begin at the start of the loop, Update on every progress
// tick, Done on success, and Fail on error or cancellation. A nil starter
// is silently ignored. The *jobs.Registry satisfies JobStarter because its
// Begin method returns (*Tracker, context.Context) and *Tracker implements
// JobReporter -- the adapter in the jobs package bridges the two.
func WithJobReporter(s JobStarter) RebuildOption {
	return func(c *rebuildConfig) {
		if s != nil {
			c.jobs = s
		}
	}
}
