package tools

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// capturePrimarySourcePredicate is the seed predicate (genesis) that grounds the
// captured summary episode in its external_ref. CaptureExternal mints the edge
// with a DETERMINISTIC uid (captureRelationUID) so a later revision can retract
// exactly the prior episode's edge without a navigator lookup.
const capturePrimarySourcePredicate = "has_primary_source"

// captureExternal action verbs (the output `action`). They are a stable wire
// contract the preset and tests key off.
const (
	captureActionCaptured   = "captured"   // first snapshot of a new item
	captureActionSkipped    = "skipped"    // unchanged + within TTL: idempotent no-op
	captureActionRevision   = "revision"   // content changed: new episode, old one superseded
	captureActionExpired    = "expired"    // unchanged but past TTL: fetch-time refreshed
	captureActionTombstoned = "tombstoned" // item reported gone/inaccessible
)

const captureExternalInputSchema = `{
  "type": "object",
  "properties": {
    "uri":         {"type": "string", "minLength": 1, "description": "Absolute URI of the external item (must carry a scheme), e.g. a Notion page URL, a Linear issue URL, a Drive doc link. The canonical identity of the external_ref pointer."},
    "connector":   {"type": "string", "minLength": 1, "description": "The connector the item came from: notion, linear, granola, drive, github. Drives the freshness budget and the untrusted-lineage trust tag. Ephemeral connectors (gmail, gcal, slack) are refused: read those live."},
    "sourceType":  {"type": "string", "minLength": 1, "description": "Open-taxonomy kind of the item, e.g. notion_page, linear_issue, meeting_notes, drive_doc."},
    "summary":     {"type": "string", "description": "Your faithful summary of the fetched content. Put the stable external id and key facts in the prose so recall can find it. Required unless you pass summaries[] or a tombstone status."},
    "summaries":   {"type": "array", "items": {"type": "string", "minLength": 1}, "description": "Chunked summary: one episode per element, all sharing one external_ref. Use for long items; otherwise pass summary."},
    "contentHash": {"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "SHA-256 hex of the item's canonical content subset (the freshness ETag). Recapturing with the same hash within the connector's TTL is a no-op. OMIT it if you cannot compute a real SHA-256 over the fetched content; do NOT fabricate one (any non-64-hex value is rejected). Defaults to the hash of your summary text when omitted."},
    "externalId":  {"type": "string", "description": "The source-system stable id (Notion page id, Linear issue key). Used to mint the dedupKey when you omit it; hashed in any plaintext render."},
    "externalUrl": {"type": "string", "format": "uri", "description": "Dereferenceable location to re-fetch when stale. Defaults to uri."},
    "label":       {"type": "string", "description": "Human-readable display name for the external_ref. Defaults to the externalId or connector, NEVER the raw URI (which would leak into a shared render)."},
    "stream":      {"type": "string", "description": "Sub-channel within the connector, e.g. a Notion database id or a Linear team key. Optional."},
    "dedupKey":    {"type": "string", "description": "Idempotency key. Defaults to <connector>:<externalId>:<scope>. Recapturing the same item must reuse the same dedupKey (omit it and pass a stable externalId, or pass it explicitly)."},
    "fetchTime":   {"type": "string", "format": "date-time", "description": "When you fetched the item (RFC 3339 UTC). Defaults to now."},
    "scope":       {"type": "string", "enum": ["personal", "team"], "description": "Scope to capture into. Defaults to personal. Project is refused: captured external content is sensitive and project renders to shared git."},
    "status":      {"type": "string", "enum": ["active", "deleted", "access-revoked"], "description": "active (default) records a live snapshot. deleted (a 404 on re-fetch) or access-revoked (a 403) tombstones the prior capture so recall flags it access-unverified instead of dropping it."}
  },
  "required": ["uri", "connector", "sourceType"],
  "additionalProperties": false
}`

const captureExternalOutputSchema = `{
  "type": "object",
  "properties": {
    "action":          {"type": "string", "enum": ["captured", "skipped", "revision", "expired", "tombstoned"], "description": "captured = first snapshot; skipped = unchanged within TTL (no write); revision = content changed (new episode, prior superseded); expired = unchanged but past TTL (fetch-time refreshed); tombstoned = item reported gone."},
    "externalRefName": {"type": "string", "pattern": "^entities/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the external_ref pointer entity."},
    "episodeNames":    {"type": "array", "items": {"type": "string"}, "description": "Resource names of the summary episode(s) this capture is grounded in (one per chunk)."},
    "sourceRefNames":  {"type": "array", "items": {"type": "string"}, "description": "Resource names of the SourceRef provenance rows."},
    "freshness":       {"type": "string", "enum": ["fresh", "stale", "access-unverified", "unknown"], "description": "The resulting freshness verdict for this item, the same value recall will report."}
  },
  "required": ["action", "externalRefName", "episodeNames", "sourceRefNames", "freshness"],
  "additionalProperties": false
}`

// CaptureExternal is the agent-mediated external-data capture verb. The agent
// (which holds the live MCP connection to Notion/Linear/Drive/...) Fetches an
// item and makes ONE captureExternal call that stores a POINTER plus the agent's
// summary plus a freshness anchor. Maz never holds the body, tokens, or fetches
// anything: it is a thin, searchable, freshness-aware index over the external
// world, not a second copy of it.
//
// Trust follows lineage, not authorship: the summary episode is tagged
// connector:<name>, so it and any facts extracted from it land status=proposed
// (untrusted), exactly like the pull connectors (#211/#212), never the trusted
// posture a plain remember() would give. Recall surfaces each hit's freshness so
// the agent reuses the cached summary when fresh and re-fetches live when stale.
type CaptureExternal struct {
	appender  Appender
	nav       Navigator
	secrets   secret.Provider
	freshness config.FreshnessPolicy
	// now overrides the clock for TTL math in tests; nil means time.Now.
	now func() time.Time
}

// NewCaptureExternal constructs the captureExternal tool. Nav resolves prior
// captures and external_ref existence (nil disables those reads, always minting);
// secrets holds the per-install HMAC key (nil degrades to TTL-only freshness);
// freshness is the resolved per-connector policy.
func NewCaptureExternal(a Appender, nav Navigator, secrets secret.Provider, freshness config.FreshnessPolicy) *CaptureExternal {
	return &CaptureExternal{appender: a, nav: nav, secrets: secrets, freshness: freshness}
}

// Name implements mcp.Tool.
func (*CaptureExternal) Name() string { return "captureExternal" }

// Description implements mcp.Tool.
func (*CaptureExternal) Description() string {
	return "Index an external item you fetched from a live connector (Notion, Linear, Granola, Drive, GitHub) WITHOUT copying its body into maz. One call stores a pointer + YOUR summary + a freshness anchor; maz never fetches or holds the content. recall later returns the summary with an inline freshness verdict so you reuse it when fresh and re-fetch live when stale. The capture is UNTRUSTED (lineage connector:<name>): facts land proposed, never superseding your own. Idempotent by contentHash within the connector's TTL. Pass status=deleted|access-revoked to tombstone an item you can no longer reach. Refuses project scope and ephemeral connectors (gmail/gcal/slack; read those live). recall FIRST before capturing, to avoid duplicates."
}

// InputSchema implements mcp.Tool.
func (*CaptureExternal) InputSchema() json.RawMessage {
	return json.RawMessage(captureExternalInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*CaptureExternal) OutputSchema() json.RawMessage {
	return json.RawMessage(captureExternalOutputSchema)
}

type captureExternalInput struct {
	URI         string   `json:"uri"`
	Connector   string   `json:"connector"`
	SourceType  string   `json:"sourceType"`
	Summary     string   `json:"summary,omitempty"`
	Summaries   []string `json:"summaries,omitempty"`
	ContentHash string   `json:"contentHash,omitempty"`
	ExternalID  string   `json:"externalId,omitempty"`
	ExternalURL string   `json:"externalUrl,omitempty"`
	Label       string   `json:"label,omitempty"`
	Stream      string   `json:"stream,omitempty"`
	DedupKey    string   `json:"dedupKey,omitempty"`
	FetchTime   string   `json:"fetchTime,omitempty"`
	Scope       string   `json:"scope,omitempty"`
	Status      string   `json:"status,omitempty"`
}

type captureExternalOutput struct {
	Action          string   `json:"action"`
	ExternalRefName string   `json:"externalRefName"`
	EpisodeNames    []string `json:"episodeNames"`
	SourceRefNames  []string `json:"sourceRefNames"`
	Freshness       string   `json:"freshness"`
}

// captureRequest is the validated, normalised form of one captureExternal call,
// shared by the live and tombstone paths so each works from the same resolved
// scope, dedupKey, fetch time, and chunk set.
type captureRequest struct {
	uri         string
	connector   string
	sourceType  string
	stream      string
	externalID  string
	externalURL string
	label       string
	contentHash string
	dedupKey    string
	scope       kernel.Scope
	fetchTime   time.Time
	summaries   []string
}

// Invoke implements mcp.Tool. It validates and normalises the request, then
// routes to the tombstone path (status deleted/access-revoked) or the live
// capture path (status active).
func (c *CaptureExternal) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in captureExternalInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	cr, tomb, verr := c.validate(in)
	if verr != nil {
		return nil, verr
	}
	if tomb != "" {
		return c.captureTombstone(ctx, cr, tomb)
	}
	return c.captureLive(ctx, cr)
}

// validate checks required fields and normalises the request. It returns the
// resolved captureRequest, the tombstone token ("" for a live capture), or an
// E_VALIDATION error. Re-validated here (not just in the JSON schema) because the
// CLI/in-process path bypasses the schema validator.
func (c *CaptureExternal) validate(in captureExternalInput) (captureRequest, string, error) {
	uri, verr := validateReferenceURI(in.URI)
	if verr != nil {
		return captureRequest{}, "", verr
	}
	if in.Connector == "" {
		return captureRequest{}, "", mcp.NewValidationError("connector is required", nil)
	}
	if in.SourceType == "" {
		return captureRequest{}, "", mcp.NewValidationError("sourceType is required", nil)
	}
	tomb, terr := parseCaptureStatus(in.Status)
	if terr != nil {
		return captureRequest{}, "", terr
	}
	scope, serr := parseIngestScope(in.Scope) // refuses project, defaults personal
	if serr != nil {
		return captureRequest{}, "", serr
	}
	// The ephemeral write-gate fires only for a live capture: a tombstone of an
	// ephemeral connector simply finds no prior row and reports not-found.
	if tomb == "" && c.freshness.Ephemeral(in.Connector) {
		return captureRequest{}, "", mcp.NewValidationError(fmt.Sprintf(
			"connector %q is ephemeral (real-time): a cached summary would mislead; read it live each time instead of capturing it", in.Connector), nil)
	}
	dedupKey, derr := captureDedupKey(in.DedupKey, in.Connector, in.ExternalID, string(scope))
	if derr != nil {
		return captureRequest{}, "", derr
	}
	fetchTime, ferr := parseRecordFetchTime(in.FetchTime)
	if ferr != nil {
		return captureRequest{}, "", ferr
	}
	summaries := normaliseSummaries(in.Summary, in.Summaries)
	if tomb == "" && len(summaries) == 0 {
		return captureRequest{}, "", mcp.NewValidationError("summary (or summaries[]) is required for a live capture", nil)
	}
	contentHash := in.ContentHash
	if contentHash == "" {
		// Fall back to a hash of the summary so idempotency still works when the
		// caller has no stable external ETag.
		sum := sha256.Sum256([]byte(joinSummaries(summaries)))
		contentHash = hex.EncodeToString(sum[:])
	}
	externalURL := in.ExternalURL
	if externalURL == "" {
		externalURL = uri
	}
	return captureRequest{
		uri:         uri,
		connector:   in.Connector,
		sourceType:  in.SourceType,
		stream:      in.Stream,
		externalID:  in.ExternalID,
		externalURL: externalURL,
		label:       captureLabel(in.Label, in.ExternalID, in.Connector),
		contentHash: contentHash,
		dedupKey:    dedupKey,
		scope:       scope,
		fetchTime:   fetchTime,
		summaries:   summaries,
	}, tomb, nil
}

// captureLive records (or refreshes) a live snapshot: it decides captured /
// skipped / revision / expired against any prior SourceRef and appends the
// resulting atomic batch.
func (c *CaptureExternal) captureLive(ctx context.Context, cr captureRequest) (json.RawMessage, error) {
	externalRefUID := string(ontology.ExternalRefUID(string(cr.scope), cr.uri))
	prior, found, perr := c.priorSourceRef(ctx, cr.scope, captureChunkKey(cr.dedupKey, 0))
	if perr != nil {
		return nil, perr
	}
	token := c.freshnessToken(ctx, cr.dedupKey, cr.contentHash, summariesLen(cr.summaries))
	action := c.decideAction(found, prior, cr, token)

	if action == captureActionSkipped {
		return c.skippedResponse(ctx, cr, externalRefUID)
	}

	events, episodeNames, sourceRefNames, berr := c.buildLiveEvents(ctx, cr, externalRefUID, token, action)
	if berr != nil {
		return nil, berr
	}
	if _, err := c.appender.Append(ctx, events); err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("capture external: %v", err), nil)
		}
		return nil, mapAppendError("capture external", err)
	}
	return marshalOutput(captureExternalOutput{
		Action:          action,
		ExternalRefName: collectionEntities + "/" + externalRefUID,
		EpisodeNames:    episodeNames,
		SourceRefNames:  sourceRefNames,
		Freshness:       c.freshness.Classify(cr.connector, cr.fetchTime, c.clock()),
	})
}

// decideAction resolves the live-capture verb from the prior SourceRef. No prior
// → captured. Content changed (hash differs, or the HMAC binding no longer holds,
// e.g. a forged/replayed hash or a changed summary) → revision. A prior tombstone
// that the agent is now re-capturing live → expired (re-stamp clears it). Unchanged
// within the connector TTL → skipped. Unchanged but past TTL → expired (refresh only).
func (c *CaptureExternal) decideAction(found bool, prior navigate.SourceRef, cr captureRequest, token string) string {
	if !found {
		return captureActionCaptured
	}
	if prior.ContentHash != cr.contentHash {
		return captureActionRevision
	}
	if prior.FreshnessToken != "" && token != "" && prior.FreshnessToken != token {
		return captureActionRevision
	}
	// A live re-capture of a previously tombstoned item proves it is reachable
	// again. Never skip: the expired re-stamp path rewrites the SourceRef with a
	// cleared tombstone (and a fresh fetch time), so recall stops reporting
	// access-unverified even when the content is unchanged and still within TTL.
	if prior.Tombstone != "" {
		return captureActionExpired
	}
	if c.withinTTL(prior.FetchTime, cr.connector) {
		return captureActionSkipped
	}
	return captureActionExpired
}

// buildLiveEvents assembles the atomic event batch for a captured/revision/
// expired action and returns it with the resulting episode and SourceRef names.
// Captured/revision mint a fresh episode per chunk (revision also retracts the
// prior episode's primary-source edge so it supersedes); expired keeps the prior
// episode and only re-stamps the SourceRef's fetch time.
func (c *CaptureExternal) buildLiveEvents(ctx context.Context, cr captureRequest, externalRefUID, token, action string) (events []kernel.Event, episodeNames, sourceRefNames []string, err error) {
	connectorSource := ontology.ConnectorSourceColonPrefix + cr.connector

	// Mint the external_ref pointer if it does not already exist in this scope.
	refEvents, rerr := c.externalRefEvents(ctx, cr, externalRefUID)
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	events = append(events, refEvents...)

	scrubber := ingest.NewScrubber()
	for i, summary := range cr.summaries {
		chunkKey := captureChunkKey(cr.dedupKey, i)
		sourceRefNames = append(sourceRefNames, "sourceRefs/"+string(ontology.SourceRefUID(chunkKey)))

		chunkEvents, epUID, cerr := c.buildChunkEvents(ctx, cr, summary, chunkKey, externalRefUID, connectorSource, token, action, scrubber)
		if cerr != nil {
			return nil, nil, nil, cerr
		}
		events = append(events, chunkEvents...)
		episodeNames = append(episodeNames, "episodes/"+epUID)
	}
	return events, episodeNames, sourceRefNames, nil
}

// buildChunkEvents builds the events for one chunk. On an expired action whose
// prior chunk still exists, it re-stamps that episode's SourceRef fetch time
// without minting a new episode (content unchanged, only the cache age moved past
// the TTL). Otherwise (captured/revision, or an expired action whose chunk count
// grew past the prior capture so there is no prior chunk to re-stamp) it mints a
// fresh episode + SourceRef + primary-source edge. Returns the chunk's events and
// the resulting episode uid.
func (c *CaptureExternal) buildChunkEvents(ctx context.Context, cr captureRequest, summary, chunkKey, externalRefUID, connectorSource, token, action string, scrubber *ingest.Scrubber) ([]kernel.Event, string, error) {
	if action == captureActionExpired {
		prior, found, perr := c.priorSourceRef(ctx, cr.scope, chunkKey)
		if perr != nil {
			return nil, "", perr
		}
		if found {
			// Re-stamp the existing episode's SourceRef; no new episode or edge.
			ev, eerr := c.sourceRefEvent(cr, chunkKey, prior.SubjectUID, connectorSource, token, cr.fetchTime, "")
			if eerr != nil {
				return nil, "", eerr
			}
			return []kernel.Event{ev}, prior.SubjectUID, nil
		}
		// The chunk count grew past the prior capture: this index has no prior
		// SourceRef to re-stamp, so mint it in full below rather than emitting a
		// SourceRef whose subject_uid points at an episode that never exists.
	}
	return c.mintChunkEvents(ctx, cr, summary, chunkKey, externalRefUID, connectorSource, token, action, scrubber)
}

// mintChunkEvents mints a fresh episode + SourceRef + primary-source edge for one
// chunk. On a revision it also retracts the prior episode's edge so the superseded
// summary stops ranking. Returns the chunk's events and the new episode uid.
func (c *CaptureExternal) mintChunkEvents(ctx context.Context, cr captureRequest, summary, chunkKey, externalRefUID, connectorSource, token, action string, scrubber *ingest.Scrubber) ([]kernel.Event, string, error) {
	epUID, uerr := newCaptureULID()
	if uerr != nil {
		return nil, "", uerr
	}
	item := &ingest.Item{
		DedupKey:       chunkKey,
		ExternalID:     cr.externalID,
		ExternalURL:    cr.externalURL,
		SourceType:     cr.sourceType,
		ContentHash:    cr.contentHash,
		Body:           summary,
		FetchTime:      cr.fetchTime,
		FreshnessToken: token,
	}
	pair, berr := ingest.BuildConnectorEpisodeEvents(scrubber, cr.connector, cr.stream, cr.scope, epUID, item)
	if berr != nil {
		return nil, "", mcp.NewInternalError(fmt.Errorf("build capture episode: %w", berr))
	}
	edge, eerr := c.primarySourceEdgeEvent(cr, epUID, externalRefUID, connectorSource)
	if eerr != nil {
		return nil, "", eerr
	}
	events := append(pair, edge)
	if action == captureActionRevision {
		retract, rterr := c.retractPriorEdge(ctx, cr, chunkKey)
		if rterr != nil {
			return nil, "", rterr
		}
		events = append(events, retract...)
	}
	return events, epUID, nil
}

// externalRefEvents returns the createEntity events for the external_ref pointer
// when it does not yet exist in cr.scope, or an empty slice when it does (the
// pointer is mint-or-return, idempotent within scope).
func (c *CaptureExternal) externalRefEvents(ctx context.Context, cr captureRequest, externalRefUID string) ([]kernel.Event, error) {
	if c.entityExists(ctx, cr.scope, externalRefUID) {
		return nil, nil
	}
	events, _, berr := buildCreateEntityEvents(createEntityInput{
		Kind:        string(ontology.EntityKindExternalRef),
		DisplayName: cr.label,
		Identifiers: []createEntityIdentifier{{Type: referenceIdentifierType, Value: cr.uri}},
	}, externalRefUID, createEntityDefaultConfidence, cr.scope)
	if berr != nil {
		return nil, berr
	}
	return events, nil
}

// captureTombstone re-records the prior capture's SourceRef(s) with a tombstone
// marker so recall keeps surfacing the item flagged access-unverified rather than
// dropping it or telling the agent to re-fetch content it can no longer reach.
func (c *CaptureExternal) captureTombstone(ctx context.Context, cr captureRequest, tomb string) (json.RawMessage, error) {
	externalRefUID := string(ontology.ExternalRefUID(string(cr.scope), cr.uri))
	connectorSource := ontology.ConnectorSourceColonPrefix + cr.connector

	var events []kernel.Event
	var episodeNames, sourceRefNames []string
	for i := 0; ; i++ {
		chunkKey := captureChunkKey(cr.dedupKey, i)
		prior, found, perr := c.priorSourceRef(ctx, cr.scope, chunkKey)
		if perr != nil {
			return nil, perr
		}
		if !found {
			break
		}
		// Preserve the prior content fields; only the tombstone + fetch time change.
		tcr := cr
		tcr.contentHash = prior.ContentHash
		tcr.externalURL = prior.ExternalURL
		tcr.sourceType = prior.SourceType
		tcr.externalID = prior.ExternalID
		tcr.stream = prior.Stream
		ev, eerr := c.sourceRefEvent(tcr, chunkKey, prior.SubjectUID, connectorSource, "", c.clock(), tomb)
		if eerr != nil {
			return nil, eerr
		}
		events = append(events, ev)
		episodeNames = append(episodeNames, "episodes/"+prior.SubjectUID)
		sourceRefNames = append(sourceRefNames, "sourceRefs/"+string(ontology.SourceRefUID(chunkKey)))
	}
	if len(events) == 0 {
		return nil, mcp.NewNotFoundError(fmt.Sprintf(
			"nothing captured under dedupKey %q to tombstone; capture it first or check the connector/externalId/scope", cr.dedupKey))
	}
	if _, err := c.appender.Append(ctx, events); err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("tombstone external: %v", err), nil)
		}
		return nil, mapAppendError("tombstone external", err)
	}
	return marshalOutput(captureExternalOutput{
		Action:          captureActionTombstoned,
		ExternalRefName: collectionEntities + "/" + externalRefUID,
		EpisodeNames:    episodeNames,
		SourceRefNames:  sourceRefNames,
		Freshness:       config.FreshnessAccessUnverified,
	})
}

// skippedResponse returns the existing names for an idempotent no-op capture,
// reconstructed from the deterministic uids (no write).
func (c *CaptureExternal) skippedResponse(ctx context.Context, cr captureRequest, externalRefUID string) (json.RawMessage, error) {
	var episodeNames []string
	sourceRefNames := make([]string, 0, len(cr.summaries))
	for i := range cr.summaries {
		chunkKey := captureChunkKey(cr.dedupKey, i)
		sourceRefNames = append(sourceRefNames, "sourceRefs/"+string(ontology.SourceRefUID(chunkKey)))
		prior, found, perr := c.priorSourceRef(ctx, cr.scope, chunkKey)
		if perr != nil {
			return nil, perr
		}
		if found {
			episodeNames = append(episodeNames, "episodes/"+prior.SubjectUID)
		}
	}
	return marshalOutput(captureExternalOutput{
		Action:          captureActionSkipped,
		ExternalRefName: collectionEntities + "/" + externalRefUID,
		EpisodeNames:    episodeNames,
		SourceRefNames:  sourceRefNames,
		Freshness:       config.FreshnessFresh,
	})
}

// primarySourceEdgeEvent builds the has_primary_source relation event grounding
// the episode in the external_ref. The relation uid is DETERMINISTIC in the
// episode uid (captureRelationUID) so a later revision can retract this exact
// edge; SourceEventUID is the episode uid so retracting the edge marks the
// episode superseded via the kernel's markSuperseded pass.
func (c *CaptureExternal) primarySourceEdgeEvent(cr captureRequest, episodeUID, externalRefUID, connectorSource string) (kernel.Event, error) {
	rel := ontology.Relation{
		Name:           "relations/" + captureRelationUID(episodeUID),
		UID:            captureRelationUID(episodeUID),
		SubjectUID:     episodeUID,
		Predicate:      capturePrimarySourcePredicate,
		ObjectUID:      externalRefUID,
		Confidence:     1.0,
		SourceEventUID: episodeUID,
		CreateTime:     c.clock(),
	}
	payload, err := json.Marshal(rel)
	if err != nil {
		return kernel.Event{}, mcp.NewInternalError(fmt.Errorf("marshal primary-source edge: %w", err))
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     connectorSource,
		Confidence: 1.0,
		Scope:      cr.scope,
		Payload:    payload,
	}, nil
}

// retractPriorEdge builds a relationRetracted closing the prior episode's
// primary-source edge, so that episode supersedes when its replacement is
// written. Returns an empty slice when there is no prior chunk to retract.
func (c *CaptureExternal) retractPriorEdge(ctx context.Context, cr captureRequest, chunkKey string) ([]kernel.Event, error) {
	prior, found, perr := c.priorSourceRef(ctx, cr.scope, chunkKey)
	if perr != nil {
		return nil, perr
	}
	if !found {
		return nil, nil
	}
	payload, err := json.Marshal(ontology.RelationRetractedEvent{
		RelationUID: ontology.ULID(captureRelationUID(prior.SubjectUID)),
		RetractedAt: c.clock(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal edge retraction: %w", err))
	}
	return []kernel.Event{{
		Kind:       ontology.KindRelationRetracted,
		Source:     ontology.ConnectorSourceColonPrefix + cr.connector,
		Confidence: 1.0,
		Scope:      cr.scope,
		Payload:    payload,
	}}, nil
}

// sourceRefEvent builds a standalone sourceRefRecorded event reusing an existing
// subject episode (the expired-refresh and tombstone paths, which never mint a
// new episode). Tombstone is "" for a live refresh.
func (c *CaptureExternal) sourceRefEvent(cr captureRequest, chunkKey, subjectUID, connectorSource, token string, fetchTime time.Time, tombstone string) (kernel.Event, error) {
	payload, err := json.Marshal(ontology.SourceRefRecordedEvent{
		UID:            ontology.SourceRefUID(chunkKey),
		SubjectUID:     ontology.ULID(subjectUID),
		SourceType:     cr.sourceType,
		Connector:      cr.connector,
		Stream:         cr.stream,
		ExternalID:     cr.externalID,
		ExternalURL:    cr.externalURL,
		ContentHash:    cr.contentHash,
		FetchTime:      fetchTime,
		DedupKey:       chunkKey,
		FreshnessToken: token,
		Tombstone:      tombstone,
	})
	if err != nil {
		return kernel.Event{}, mcp.NewInternalError(fmt.Errorf("marshal source ref: %w", err))
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindSourceRefRecorded),
		Source:     connectorSource,
		Confidence: 1.0,
		Scope:      cr.scope,
		Payload:    payload,
	}, nil
}

// priorSourceRef reads the SourceRef for a chunk key in scope, reporting found =
// false on a clean miss. A nil navigator (minimal boot) always reports not-found.
func (c *CaptureExternal) priorSourceRef(ctx context.Context, scope kernel.Scope, chunkKey string) (navigate.SourceRef, bool, error) {
	if c.nav == nil {
		return navigate.SourceRef{}, false, nil
	}
	uid := string(ontology.SourceRefUID(chunkKey))
	sr, err := navWithScopes(c.nav, kernel.NewScopeSet(scope)).GetSourceRef(ctx, uid)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return navigate.SourceRef{}, false, nil
		}
		return navigate.SourceRef{}, false, mcp.NewInternalError(fmt.Errorf("read prior source ref: %w", err))
	}
	return sr, true, nil
}

// entityExists reports whether the external_ref uid already exists in scope. A
// nil navigator reports false so the caller mints.
func (c *CaptureExternal) entityExists(ctx context.Context, scope kernel.Scope, uid string) bool {
	if c.nav == nil {
		return false
	}
	_, err := navWithScopes(c.nav, kernel.NewScopeSet(scope)).GetEntity(ctx, uid)
	return err == nil
}

// freshnessToken computes the HMAC binding (dedupKey + contentHash + summary
// length) under the per-install secret, or "" when no secret is available (the
// binding degrades to TTL-only freshness, which is still safe).
func (c *CaptureExternal) freshnessToken(ctx context.Context, dedupKey, contentHash string, summaryLen int) string {
	key := c.installSecret(ctx)
	if key == nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(dedupKey))
	mac.Write([]byte{0})
	mac.Write([]byte(contentHash))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.Itoa(summaryLen)))
	return hex.EncodeToString(mac.Sum(nil))
}

// installSecret returns the per-install HMAC key, generating and persisting a
// fresh 32-byte secret on first use. It returns nil (degrade to TTL-only) when
// there is no provider, the provider errors, or the secret cannot be persisted
// (a stored secret that cannot be re-read later would make verification
// impossible, so an unpersistable secret is treated as absent).
func (c *CaptureExternal) installSecret(ctx context.Context) []byte {
	if c.secrets == nil {
		return nil
	}
	v, err := c.secrets.Get(ctx, secret.NameCaptureHMACKey)
	switch {
	case err == nil && v != "":
		if b, derr := hex.DecodeString(v); derr == nil {
			return b
		}
		return nil
	case err != nil && !errors.Is(err, secret.ErrNotFound):
		return nil
	}
	buf := make([]byte, 32)
	if _, rerr := rand.Read(buf); rerr != nil {
		return nil
	}
	if serr := c.secrets.Set(ctx, secret.NameCaptureHMACKey, hex.EncodeToString(buf)); serr != nil {
		return nil
	}
	return buf
}

// withinTTL reports whether a prior fetch (RFC 3339 string) is still within the
// connector's freshness budget as of the tool clock. An unparseable time is
// treated as expired so a corrupt row forces a re-capture rather than a stale
// skip.
func (c *CaptureExternal) withinTTL(fetchTime, connector string) bool {
	ft, err := time.Parse(time.RFC3339Nano, fetchTime)
	if err != nil {
		return false
	}
	return c.clock().Sub(ft) <= c.freshness.TTL(connector)
}

// clock returns the tool's notion of now (UTC), overridable in tests.
func (c *CaptureExternal) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now().UTC()
}

// parseCaptureStatus validates the status arg into a tombstone token: "" for a
// live capture (active/empty), or the ontology tombstone constant for
// deleted/access-revoked.
func parseCaptureStatus(raw string) (string, error) {
	switch raw {
	case "", "active":
		return "", nil
	case "deleted":
		return ontology.TombstoneDeleted, nil
	case "access-revoked":
		return ontology.TombstoneAccessRevoked, nil
	default:
		return "", mcp.NewValidationError(
			fmt.Sprintf("invalid status %q (allowed: active, deleted, access-revoked)", raw), nil)
	}
}

// captureDedupKey returns the explicit dedupKey, or mints the canonical
// <connector>:<externalId>:<scope> form. An omitted dedupKey with no externalId
// is an error: there would be no stable idempotency anchor and every recapture
// would duplicate.
func captureDedupKey(explicit, connector, externalID, scope string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if externalID == "" {
		return "", mcp.NewValidationError(
			"provide a dedupKey, or an externalId so one can be minted as <connector>:<externalId>:<scope>", nil)
	}
	return connector + ":" + externalID + ":" + scope, nil
}

// captureChunkKey derives the per-chunk SourceRef dedup key. Every chunk is
// suffixed (#0, #1, …) so the key scheme is identical for a single summary and a
// chunk array, so a later recapture probes #0 regardless of how the prior
// capture was chunked.
func captureChunkKey(dedupKey string, i int) string {
	return dedupKey + "#" + strconv.Itoa(i)
}

// captureLabel resolves the external_ref display name, NEVER defaulting to the
// raw URI (which would leak a third-party URL into a shared render). Falls back
// to the externalId, then a connector-qualified placeholder.
func captureLabel(label, externalID, connector string) string {
	switch {
	case label != "":
		return label
	case externalID != "":
		return externalID
	default:
		return connector + " item"
	}
}

// normaliseSummaries folds the singular summary and the summaries[] array into
// one non-empty slice (array wins when both are present), dropping empties.
func normaliseSummaries(summary string, summaries []string) []string {
	out := make([]string, 0, len(summaries)+1)
	for _, s := range summaries {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 && summary != "" {
		out = append(out, summary)
	}
	return out
}

// joinSummaries concatenates chunk summaries for the fallback content hash.
func joinSummaries(summaries []string) string {
	return strings.Join(summaries, "\n")
}

// summariesLen is the total summary byte length bound into the freshness token.
func summariesLen(summaries []string) int {
	n := 0
	for _, s := range summaries {
		n += len(s)
	}
	return n
}

// captureRelationUID derives the deterministic has_primary_source relation uid
// for an episode, mirroring ontology.SourceRefUID's SHA-256→ULID encoding so a
// revision can retract the prior episode's edge without a navigator lookup. The
// 16-byte truncation renders a valid Crockford ULID (the leading symbol is
// always 0–7).
func captureRelationUID(episodeUID string) string {
	sum := sha256.Sum256([]byte(capturePrimarySourcePredicate + ":" + episodeUID))
	var id ulid.ULID
	copy(id[:], sum[:len(id)])
	return id.String()
}

// newCaptureULID mints a fresh time-ordered ULID for a summary episode.
func newCaptureULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return "", mcp.NewInternalError(fmt.Errorf("generate episode uid: %w", err))
	}
	return id.String(), nil
}
