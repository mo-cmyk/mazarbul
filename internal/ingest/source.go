// Package ingest provides Mazarbul's pull-based external-source ingestion: a
// narrow Source contract and an orchestrator that owns the universal half of
// connect-and-ingest (dedup, content-hash change detection, normalisation into
// ontology.Episode, atomic SourceRef emission, watermark advance, job-layer
// backfill bounding). It feeds the EXISTING extractor + resolver pipeline; it
// does not re-implement extraction or correlation.
//
// The contract is deliberately NOT an extension of the dormant
// plugin.Connector: Source is born sized to its single consumer (the
// orchestrator) with the first impl, LocalGitSource (issue #211).
package ingest

import (
	"context"
	"encoding/json"
	"time"
)

// Source is a pull-only external ingestion source. Implementations read an
// external system (a local git repo, a GitHub API, …) and yield normalised
// Items that the orchestrator turns into Episodes + SourceRefs.
//
// A Source may expose more than one Stream (e.g. local-git has "commits" and
// "pull-requests"); the orchestrator drives each Stream independently with its
// own resume Watermark.
type Source interface {
	// Name is the stable source identifier, e.g. "local-git". It tags every
	// ingested Event.Source as "connector:<Name>" (which trips the #204 privacy
	// leak gate and the #211 resolver trust gate) and is the connector_watermarks
	// projection key.
	Name() string

	// Streams returns the sub-channels this source pulls, in a stable order.
	// Each is driven with its own watermark. A single-stream source returns one
	// element (may be "").
	Streams() []string

	// Validate checks source-specific configuration (the repo path exists and is
	// a git repo, the remote is reachable, …) WITHOUT pulling any items. It powers
	// the dryRun preview and the connectSource consent gate.
	Validate(ctx context.Context) error

	// Pull returns the next batch of items for stream strictly after wm.Cursor,
	// bounded by limit (the JOB layer sets limit; the Source must honour it). The
	// returned Batch carries the advanced cursor/sequence to persist iff the batch
	// fully applies.
	Pull(ctx context.Context, stream string, wm Watermark, limit int) (Batch, error)
}

// Watermark is the resume position for one (connector, stream) pair. Cursor is
// Source-defined opaque JSON; the orchestrator never interprets it, only
// validates it (size cap + JSON-object shape + monotone Sequence). A zero
// Watermark (nil Cursor, Sequence 0) means "from the beginning" — a first
// connect / full backfill.
type Watermark struct {
	Connector string
	Stream    string
	Cursor    json.RawMessage
	Sequence  uint64
}

// Batch is one Pull's worth of items plus the advanced cursor. Done is true when
// the Source has no more items after this batch (the stream is fully drained).
type Batch struct {
	Items        []Item
	NextCursor   json.RawMessage
	NextSequence uint64
	Done         bool
}

// Item is one externally-sourced unit pre-normalisation. The orchestrator turns
// each Item into exactly one Episode + one SourceRef.
type Item struct {
	// DedupKey is the natural key for source_refs UNIQUE(dedup_key) — the
	// idempotency anchor, e.g. "local-git:<repo>:<sha>". Required.
	DedupKey string
	// ExternalID is the source-system opaque id, e.g. a commit sha or PR number.
	ExternalID string
	// ExternalURL is the dereferenceable location (omitted from plaintext render
	// by #207). Optional.
	ExternalURL string
	// SourceType is the open-taxonomy kind of this item, e.g. "git_commit" or
	// "git_pull_request". Required; stored on the SourceRef.
	SourceType string
	// ContentHash is the SHA-256 hex of the verbatim external body. It drives
	// change detection: a matching hash on re-ingest is skipped; a changed hash
	// produces a new revision. Required.
	ContentHash string
	// Body is the verbatim untrusted text (commit message / PR prose). It is
	// scrubbed of secret-shaped strings and prompt-delimited before becoming an
	// Episode. Required.
	Body string
	// FetchTime is when the item was read. Required.
	FetchTime time.Time
	// FreshnessToken is the optional HMAC binding (dedupKey + contentHash +
	// summary length) carried through to the SourceRef for caller-hash integrity.
	// Set by agent-mediated capture (captureExternal); empty for the pull
	// connectors, which compute the hash server-side and need no binding.
	FreshnessToken string
}
