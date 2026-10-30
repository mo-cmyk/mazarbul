// Package enrichment defines the contracts for Mazarbul's enrichment
// subsystem (sparse-record fill, alias resolution, stale-fact refresh).
// Concrete implementations land starting in M2; this file holds only
// the interfaces and a Noop fallback.
//
// Operating principle: "enrichment proposes, the user (or policy)
// disposes". Every Proposal carries full provenance, a confidence
// score, and a path back to the source so nothing is silently promoted
// to truth.
package enrichment

import (
	"context"
	"errors"
	"time"
)

// Kind classifies an enrichment operation by the five categories from
// the architecture spec.
type Kind string

const (
	// KindResolution maps a mention to a canonical entity ID. Cheap,
	// mostly deterministic, runs via the entity.Resolver.
	KindResolution Kind = "resolution"

	// KindLookup fills missing fields from an external source (Crunchbase,
	// OpenRegister, Wikidata, etc.). API calls, sometimes paid.
	KindLookup Kind = "lookup"

	// KindComputation derives a value deterministically from existing
	// fields (geohash from lat/lng, age from DOB, domain from email).
	// Free, code-only.
	KindComputation Kind = "computation"

	// KindInference reasons new facts from existing ones via an LLM
	// ("if A works at B and B is owned by C, A indirectly serves C").
	// LLM tokens.
	KindInference Kind = "inference"

	// KindCrossReference links this store's record to one in an external
	// system via MCP (Gmail thread, LinkedIn URL, calendar event).
	KindCrossReference Kind = "cross_reference"
)

// Mode selects when an enrichment runs.
type Mode string

const (
	// ModeSync runs during ingest, in the ingestion's critical path.
	// Reserved for cheap deterministic enrichments only.
	ModeSync Mode = "sync"

	// ModeBackground runs in the background worker pool, rate-limited
	// and budget-gated.
	ModeBackground Mode = "background"

	// ModeLazy runs at query time when a recall discovers a missing
	// field the answer needs.
	ModeLazy Mode = "lazy"

	// ModeScheduled runs on a cron schedule (Crunchbase every 30 days,
	// Wikidata weekly, government registries every 90 days).
	ModeScheduled Mode = "scheduled"
)

// Proposal is a single enrichment candidate. The Enricher returns
// proposals; the policy engine accepts, rejects, or queues for review.
// Nothing in the proposal lands as ground truth without going through
// disposition (or user confirmation via `maz review`).
type Proposal struct {
	UID           string
	Kind          Kind
	TargetUID     string
	FieldPath     string
	ProposedValue any
	Source        string
	SourceTime    time.Time
	Confidence    float64
	Cost          float64
	Reasoning     string
	CreateTime    time.Time
}

// Enricher is the contract for a single enrichment source (Crunchbase
// connector, Wikidata lookup, geocoder, etc.). The pipeline runs many
// enrichers in parallel and the policy engine merges their proposals.
type Enricher interface {
	// Name is the stable identifier ("crunchbase", "wikidata", "geohash",
	// "gmail-cross-ref").
	Name() string

	// Kind reports which enrichment category this enricher implements.
	Kind() Kind

	// SupportedModes lists which run modes are valid for this enricher.
	// A geocoder supports ModeSync; a Crunchbase lookup is ModeBackground +
	// ModeScheduled.
	SupportedModes() []Mode

	// Propose generates zero or more proposals for the given target. The
	// pipeline calls Propose with a budget; the enricher must stop early
	// when ctx is cancelled or the budget is exhausted.
	Propose(ctx context.Context, target string) ([]Proposal, error)
}

// Policy is the rule set that disposes proposals. M2+ ships
// with a declarative YAML policy file at .mazarbul/enrichment.yaml that
// users edit; this interface is what the runtime consumes.
type Policy interface {
	// ShouldEnrich reports whether the named target is eligible for
	// enrichment from a given source under the current policy and budget.
	ShouldEnrich(ctx context.Context, target, source string) (bool, error)

	// OnProposal disposes a fresh proposal: accept, reject, or queue for
	// human review.
	OnProposal(ctx context.Context, proposal Proposal) (Disposition, error)
}

// Disposition is the policy's verdict on a single proposal.
type Disposition string

// Proposal dispositions. Accept lands the proposal as a new event; reject
// drops it silently; review queues it for `maz review`.
const (
	DispositionAccept Disposition = "accept"
	DispositionReject Disposition = "reject"
	DispositionReview Disposition = "review"
)

// ErrNotImplemented is returned by the Noop placeholders.
var ErrNotImplemented = errors.New("enrichment: not yet implemented")

// NoopEnricher returns no proposals. Used until concrete enrichers land
// in M2+.
type NoopEnricher struct{}

// Name implements Enricher.
func (NoopEnricher) Name() string { return "noop" }

// Kind implements Enricher.
func (NoopEnricher) Kind() Kind { return KindComputation }

// SupportedModes implements Enricher.
func (NoopEnricher) SupportedModes() []Mode { return nil }

// Propose implements Enricher. It returns ErrNotImplemented so an unwired
// enricher cannot be mistaken for a real "no proposals" outcome.
func (NoopEnricher) Propose(_ context.Context, _ string) ([]Proposal, error) {
	return nil, ErrNotImplemented
}

// NoopPolicy is the placeholder policy. Returns ErrNotImplemented from every
// operation so an unwired policy cannot silently dispose proposals.
type NoopPolicy struct{}

// ShouldEnrich implements Policy. It returns ErrNotImplemented so an unwired
// policy cannot be mistaken for a real "deny" decision.
func (NoopPolicy) ShouldEnrich(_ context.Context, _, _ string) (bool, error) {
	return false, ErrNotImplemented
}

// OnProposal implements Policy. It returns ErrNotImplemented so an unwired
// policy cannot be mistaken for a real "queue for review" decision.
func (NoopPolicy) OnProposal(_ context.Context, _ Proposal) (Disposition, error) {
	return "", ErrNotImplemented
}
