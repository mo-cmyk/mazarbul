// Package entity defines the contracts for Mazarbul's entity model
// (Mentions, canonical Entities, the Resolver). Concrete implementations
// land in M2 (entity resolution v1) and M3+ (multi-vector embeddings,
// community detection). This file holds only the interfaces and a Noop
// fallback.
//
// Core distinction:
//
//	Mention - a single observation from a single source. Immutable,
//	          content-addressed, signed. The raw record.
//	Entity  - the conceptual referent (a person, a company, a project).
//	          Derived from N mentions plus a merge decision. Reversible.
package entity

import (
	"context"
	"errors"
	"time"
)

// Kind identifies the entity type. Open enum: the six built-in values
// below are the canonical taxa; the ontology registry
// (kernel.SQLiteOntology entity_kinds table) accepts additional
// namespaced kinds such as "user:deal" or "extension:schema_org:Person".
// No code in this package enforces a closed set; callers must not switch
// exhaustively over these constants without a default branch.
type Kind string

// Built-in entity kinds. Extension kinds follow the "namespace:localname"
// pattern and are registered via kernelboot.SeedOntology or the
// entityKindRegistered event; see agent-docs/designs/dynamic-ontology.md.
const (
	KindPerson       Kind = "person"
	KindOrganization Kind = "organization"
	KindPlace        Kind = "place"
	KindProject      Kind = "project"
	KindEvent        Kind = "event"
	KindDocument     Kind = "document"
)

// Identifier is a typed external identifier for an entity. Strong
// identifiers (HRB, LinkedIn URN, Wikidata QID, GitHub numeric user ID)
// drive the auto-merge path of the resolver.
type Identifier struct {
	Type  string // "email", "github_user_id", "wikidata_qid", "hrb", etc.
	Value string
}

// Mention is one observation of an entity. Created during ingestion and
// during inference passes. Mentions are immutable; reclassification
// produces new events that point at the original mention by UID.
type Mention struct {
	UID               string
	Kind              Kind
	DisplayName       string
	Identifiers       []Identifier
	DisambiguationCtx string
	SourceEventUID    string
	CreateTime        time.Time
}

// Entity is the canonical resolved form. Derived from mentions; never
// stored directly without a merge or split event in the log to back it.
type Entity struct {
	UID              string
	Kind             Kind
	DisplayName      string
	AlternateNames   []string
	Identifiers      []Identifier
	MentionUIDs      []string
	MergeHistory     []MergeEvent
	Confidence       float64
	ResolutionStatus ResolutionStatus
}

// ResolutionStatus is the current state of the entity's identity belief.
type ResolutionStatus string

// Resolution status values. Confirmed entities are high-confidence and stable;
// tentative entities are visible but flagged; split candidates surface in
// `maz review` for human disposition.
const (
	ResolutionConfirmed      ResolutionStatus = "confirmed"
	ResolutionTentative      ResolutionStatus = "tentative"
	ResolutionSplitCandidate ResolutionStatus = "split_candidate"
)

// MergeEvent is one row of the entity's reversible merge history.
type MergeEvent struct {
	From       []string
	Into       string
	DecidedBy  string
	Confidence float64
	Reasoning  string
	CreateTime time.Time
}

// Resolver runs the three-stage resolution pipeline (blocking, scoring,
// LLM disambiguation) per architecture spec section 05. The concrete
// implementation lands in M2.
type Resolver interface {
	// Resolve attaches a new mention to an existing entity, merges into
	// an existing entity, or creates a new canonical entity. Returns the
	// entity UID the mention was bound to plus a flag indicating whether
	// the resolver made a high-confidence decision or queued the mention
	// for human review.
	Resolve(ctx context.Context, mention Mention) (uid string, pending bool, err error)

	// Get returns the entity by UID, or ErrNotFound if it does not exist.
	Get(ctx context.Context, uid string) (Entity, error)

	// Split reverses a prior merge. Emits a split event so the operation
	// is reversible.
	Split(ctx context.Context, uid string, reason string) (newUIDs []string, err error)

	// Merge fuses two entities into one. Emits a merge event.
	Merge(ctx context.Context, a, b string, reason string) (uid string, err error)

	// PendingReview returns the current backlog of mentions whose
	// confidence fell in the gray zone and were queued for human review.
	PendingReview(ctx context.Context) ([]Mention, error)
}

// ErrNotFound is returned when an entity does not exist.
var ErrNotFound = errors.New("entity: not found")

// ErrNotImplemented is returned by the Noop resolver until M2 lands.
var ErrNotImplemented = errors.New("entity: not yet implemented")

// NoopResolver is the placeholder resolver. Returns ErrNotImplemented
// from every operation. Used while M2 work proceeds.
type NoopResolver struct{}

// Resolve implements Resolver. It returns ErrNotImplemented.
func (NoopResolver) Resolve(_ context.Context, _ Mention) (string, bool, error) {
	return "", false, ErrNotImplemented
}

// Get implements Resolver. It returns ErrNotImplemented.
func (NoopResolver) Get(_ context.Context, _ string) (Entity, error) {
	return Entity{}, ErrNotImplemented
}

// Split implements Resolver. It returns ErrNotImplemented.
func (NoopResolver) Split(_ context.Context, _ string, _ string) ([]string, error) {
	return nil, ErrNotImplemented
}

// Merge implements Resolver. It returns ErrNotImplemented.
func (NoopResolver) Merge(_ context.Context, _, _ string, _ string) (string, error) {
	return "", ErrNotImplemented
}

// PendingReview implements Resolver. It returns ErrNotImplemented so an
// unwired resolver cannot be mistaken for a real empty review backlog.
func (NoopResolver) PendingReview(_ context.Context) ([]Mention, error) {
	return nil, ErrNotImplemented
}
