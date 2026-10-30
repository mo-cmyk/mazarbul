package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// Deps bundles all collaborators required by a Resolver. All fields are
// required unless documented as optional.
type Deps struct {
	// EventLog is the append-only source of truth. Required.
	EventLog kernel.EventLog

	// Embedder produces dense vectors for name and summary matching. Required.
	Embedder kernel.Embedder

	// Chat is the LLM provider used for disambiguation, contradiction checks,
	// summary regeneration, and predicate classification. Required.
	Chat kernel.ChatProvider

	// Vec is the vector index used for nearest-neighbour searches over entity
	// name and summary embeddings. Required.
	Vec kernel.VectorConn

	// Reader is a read-only SQLite connection pool for entity lookups. Required.
	Reader *sql.DB

	// Writer is the single-writer SQLite pool for predicate inserts and other
	// writes that accompany event appends. Required.
	Writer *sql.DB

	// Prompts is the template registry. The four resolver templates must have
	// been registered (via RegisterPrompts) before the first Resolve call.
	// Required.
	Prompts prompts.Registry

	// Predicates is the in-memory predicate registry backed by the predicates
	// table. Required.
	Predicates *Registry

	// KindRegistry is the in-memory entity-kind registry backed by the
	// entity_kinds table. Required.
	KindRegistry *KindRegistry

	// Logger receives structured diagnostic output from the resolver pipeline.
	// Required.
	Logger *zap.Logger

	// SelfBinder rebinds a first-person ("self") subject to the concrete
	// principal UID so extracted self-statements sync as the shareable
	// principal rather than the local-only "self" literal. Optional: nil (or an
	// unbound device) leaves first-person subjects bound to SelfEntityUID.
	SelfBinder SelfBinder
}

// SelfBinder resolves the device-local self->principal binding. Satisfied by
// *config.SelfBinder; nil disables rebinding (first-person subjects stay on
// the seeded SelfEntityUID, preserving pre-identity behavior).
type SelfBinder interface {
	SelfPrincipal(ctx context.Context) (string, bool)
}

// Resolver is the entity resolution pipeline for Mazarbul's memory layer. It
// processes batches of Mentions and RelationCandidates from a single Episode,
// resolving each Mention to an existing or newly created Entity, promoting
// confirmed RelationCandidates to Relation events, handling supersession for
// functional predicates, and regenerating Entity summaries when thresholds are
// crossed.
//
// Safe for concurrent use across goroutines; the regenGuard sync.Map prevents
// thundering-herd summary regeneration during eval-harness backfill.
type Resolver struct {
	cfg          Config
	eventLog     kernel.EventLog
	embedder     kernel.Embedder
	chat         kernel.ChatProvider
	vec          kernel.VectorConn
	reader       *sql.DB
	writer       *sql.DB
	promptReg    prompts.Registry
	predicates   *Registry
	kindRegistry *KindRegistry
	logger       *zap.Logger
	binder       SelfBinder // nil = first-person subjects stay on SelfEntityUID

	// matcher is the shared entity-dedup primitive (exact-name, name-vector,
	// identifier lanes). The resolver's match methods delegate to it so the
	// extraction path and the direct-mint createEntity path apply identical
	// rules. See internal/kernel/entitymatch.
	matcher *entitymatch.Lookup

	// regenGuard prevents concurrent summary regeneration for the same entity.
	// Keys are entity UIDs; values are struct{}. A Resolve call that loses the
	// race skips the regen rather than stacking a duplicate LLM call.
	regenGuard sync.Map

	// proposedFlight de-duplicates governed-mode entity-kind proposals within
	// the process. Keys are kind names. FileEventLog.Append applies the
	// entity_kinds projection only after writing the JSONL event, so two
	// concurrent ingestion passes for the same unknown kind could both pass the
	// isKindActiveOrProposed check and both append a proposal. singleflight
	// coalesces concurrent calls for the same key into a single execution that
	// runs the check-then-append atomically; the key auto-clears when that
	// execution returns, so the guard covers only the in-flight append window
	// and never permanently suppresses a later remint of a rejected kind.
	proposedFlight singleflight.Group
}

// New constructs a Resolver from cfg and deps, applying defaults to cfg and
// validating that all required deps are non-nil.
func New(cfg Config, deps Deps) (*Resolver, error) {
	cfg.ApplyDefaults()

	switch {
	case deps.EventLog == nil:
		return nil, errors.New("resolver: deps.EventLog is required")
	case deps.Embedder == nil:
		return nil, errors.New("resolver: deps.Embedder is required")
	case deps.Chat == nil:
		return nil, errors.New("resolver: deps.Chat is required")
	case deps.Vec == nil:
		return nil, errors.New("resolver: deps.Vec is required")
	case deps.Reader == nil:
		return nil, errors.New("resolver: deps.Reader is required")
	case deps.Writer == nil:
		return nil, errors.New("resolver: deps.Writer is required")
	case deps.Prompts == nil:
		return nil, errors.New("resolver: deps.Prompts is required")
	case deps.Predicates == nil:
		return nil, errors.New("resolver: deps.Predicates is required")
	case deps.KindRegistry == nil:
		return nil, errors.New("resolver: deps.KindRegistry is required")
	case deps.Logger == nil:
		return nil, errors.New("resolver: deps.Logger is required")
	}

	return &Resolver{
		cfg:          cfg,
		eventLog:     deps.EventLog,
		embedder:     deps.Embedder,
		chat:         deps.Chat,
		vec:          deps.Vec,
		reader:       deps.Reader,
		writer:       deps.Writer,
		promptReg:    deps.Prompts,
		predicates:   deps.Predicates,
		kindRegistry: deps.KindRegistry,
		logger:       deps.Logger,
		binder:       deps.SelfBinder,
		matcher:      entitymatch.NewLookup(deps.Reader, deps.Vec, deps.Embedder, cfg.NameEmbThreshold, entitymatch.AdvisoryDefault),
	}, nil
}

// Resolve processes a batch of Mentions and RelationCandidates from a single
// Episode identified by episodeUID.
//
// For each Mention it runs the three-step matching pipeline (exact name,
// identifier, summary embedding) and appends EntityCreated or EntityUpdated
// events. For each RelationCandidate it resolves subject and object Entity UIDs,
// auto-extends the predicate vocabulary when unknown, runs a supersede check,
// and appends Relation and RelationRetracted events as needed. Finally it
// triggers Entity summary regeneration when thresholds are crossed.
//
// Derived events (EntityCreated, Relation) are written with kernel.DefaultScope.
// Callers that need to preserve a non-default scope (e.g. draft) should use
// resolveInScope directly.
func (r *Resolver) Resolve(ctx context.Context, episodeUID string, mentions []ontology.Mention, candidates []ontology.RelationCandidate) error {
	return r.resolveInScope(ctx, episodeUID, kernel.DefaultScope, mentions, candidates)
}

// resolveInScope is the scope-aware core of Resolve. Scope is stamped onto
// every NEW entity and relation event minted for this batch. Existing entities
// (EntityUpdated path) keep their current scope — promotion is an explicit
// operation and must never be a side-effect of a draft extraction match.
func (r *Resolver) resolveInScope(ctx context.Context, episodeUID string, scope kernel.Scope, mentions []ontology.Mention, candidates []ontology.RelationCandidate) error {
	// Per-ingest mint counter caps how many novel entity kinds are registered in
	// one Resolve call. Shared across all mentions in the batch.
	mintCounter := &ingestMintCounter{}

	// Trust posture (#211): facts from a connector-sourced (untrusted) episode
	// are quarantined — confidence-capped, status=proposed, excluded from
	// supersession, and barred from minting new kinds/predicates. The signal
	// travels with the episode (its source column), read once per batch.
	untrusted := r.episodeUntrusted(ctx, episodeUID)

	mentionResults, mentionEntities, newMentionCounts, err := r.resolveMentions(ctx, episodeUID, scope, mentions, mintCounter, untrusted)
	if err != nil {
		return err
	}
	r.processCandidates(ctx, episodeUID, scope, mentions, mentionResults, candidates, untrusted)
	r.regenSummariesIfNeeded(ctx, mentions, mentionResults, mentionEntities, newMentionCounts)
	return nil
}

// episodeUntrusted reports whether the episode identified by episodeUID was
// pulled by an external ingestion connector (its source matches the
// connector-source convention, ontology.IsConnectorSource). The episode row is
// projected to SQLite before resolution runs (applyToStorage precedes the
// background extraction+resolution worker), so this read always sees a
// connector-ingested episode's source.
//
// Fail-safe direction is TRUSTED: a missing row or a read error means this is
// not a connector path (every connector ingest writes a readable episode row in
// the same atomic batch), so defaulting to trusted preserves normal resolution
// for the overwhelmingly-common non-ingest case. Data confidentiality does not
// depend on this read — the privacy leak gate is the fail-closed surface; this
// gate governs graph authority only.
func (r *Resolver) episodeUntrusted(ctx context.Context, episodeUID string) bool {
	var src string
	if err := r.reader.QueryRowContext(ctx,
		`SELECT source FROM episodes WHERE uid = ?`, episodeUID,
	).Scan(&src); err != nil {
		return false
	}
	return ontology.IsConnectorSource(src)
}

// resolveMentions matches each Mention to an Entity, emits the corresponding
// EntityCreated or EntityUpdated event, and tracks the new-mention count per
// existing Entity (used by the summary regen threshold). MintCounter enforces
// the per-ingest kind-registration cap across all mentions in the batch.
// Scope is stamped on EntityCreated events for new entities; existing entities
// (EntityUpdated) keep their current scope.
func (r *Resolver) resolveMentions(
	ctx context.Context,
	episodeUID string,
	scope kernel.Scope,
	mentions []ontology.Mention,
	mintCounter *ingestMintCounter,
	untrusted bool,
) ([]MatchResult, []ontology.Entity, map[string]int, error) {
	mentionResults := make([]MatchResult, len(mentions))
	mentionEntities := make([]ontology.Entity, len(mentions))
	newMentionCounts := make(map[string]int)

	// One batched round trip for every embedding the loop below may need
	// (T0.1): under burst, 2N sequential embed calls each queue behind the
	// extraction workers; the batch pays that wait once. nil on failure or
	// when batching is unavailable — the loop then embeds per mention.
	pre := r.preEmbedMentions(ctx, mentions)

	for i, m := range mentions {
		// `document` is a structural kind owned by the document write path (a
		// document entity must back a documents-table row + body + path).
		// Extraction must never stamp it on a bare mention — that mints a
		// body-less "ghost document" that breaks every doc-oriented surface.
		// Remap reserved kinds (document → topic) before matching/emission,
		// for trusted and untrusted mentions alike.
		m.Kind = ontology.EntityKind(r.remapReservedExtractionKind(ctx, string(m.Kind), mintCounter))

		// Auto-register the entity kind when the mention carries a novel kind.
		// The returned canonical is used for this mention only; the slice element
		// is a copy so we update the local variable, not the caller's slice.
		//
		// Untrusted (connector-ingested) mentions are FORBIDDEN from minting new
		// kinds (#211): a novel kind keeps its raw label and resolves against the
		// existing taxonomy, but never extends it. This denies an injection
		// payload the ability to grow the ontology.
		if !untrusted && r.kindRegistry != nil && !r.isRegisteredEntityKind(string(m.Kind)) {
			canonical, err := r.autoRegisterEntityKind(ctx, string(m.Kind), m.DisambiguationCtx, mintCounter)
			if err != nil {
				r.logger.Warn("Resolver: entity kind auto-register failed; keeping original kind",
					zap.String("kind", string(m.Kind)),
					zap.Error(err),
				)
			} else {
				m.Kind = ontology.EntityKind(canonical)
			}
		}

		result, err := r.matchMention(ctx, m, pre)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolver: match mention %d (%q): %w", i, m.DisplayName, err)
		}

		entity, err := r.emitEntityEvent(ctx, episodeUID, scope, m, result)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolver: emit entity event for mention %d: %w", i, err)
		}
		// emitEntityEvent generates a fresh ULID for IsNew matches; copy it back
		// onto the MatchResult so downstream relation processing has a UID to
		// reference. Without this, processCandidates skips every brand-new
		// entity as "subject or object has no UID".
		if result.EntityUID == "" {
			result.EntityUID = entity.UID
		}
		mentionResults[i] = result
		mentionEntities[i] = entity

		if !result.IsNew {
			newMentionCounts[result.EntityUID]++
		}
	}
	return mentionResults, mentionEntities, newMentionCounts, nil
}

// processCandidates iterates each RelationCandidate, validates indices, and
// delegates to processCandidate. Errors are logged but never fail the batch:
// one bad candidate must not poison the rest of the Episode's resolution.
// Scope is forwarded to processCandidate so minted Relation events carry
// the same scope boundary as the originating episode.
func (r *Resolver) processCandidates(
	ctx context.Context,
	episodeUID string,
	scope kernel.Scope,
	mentions []ontology.Mention,
	mentionResults []MatchResult,
	candidates []ontology.RelationCandidate,
	untrusted bool,
) {
	// Resolve the first-person subject once per batch (the binder caches the
	// lookup): the concrete principal UID when the device is bound, else the
	// seeded SelfEntityUID.
	selfUID := r.resolveSelfSubject(ctx)
	for i, cand := range candidates {
		// Self-subject candidates carry SelfSubjectSentinel (-1) in
		// SubjectMentionIdx and were emitted by the lexical self-
		// statement detector for first-person assertions. Bind the
		// subject to the resolved self UID rather than dereferencing
		// mentionResults; the object index must still be a real mention.
		subjUID, objUID, ok := r.bindCandidateEndpoints(cand, mentionResults, selfUID, i)
		if !ok {
			continue
		}

		// Derive relation confidence from the underlying mention grounding
		// scores. Previously appendRelation hardcoded Confidence=1.0, which
		// made the recall composer's MinConfidence filter a no-op for every
		// resolver-produced relation. Take the minimum of subject and object
		// mention grounding so a single weakly-grounded endpoint pulls the
		// whole edge down.
		confidence := relationConfidence(mentions, cand)

		if err := r.processCandidate(ctx, episodeUID, scope, cand, subjUID, objUID, confidence, untrusted); err != nil {
			r.logger.Warn("Resolver: failed to process relation candidate",
				zap.Int("candidateIdx", i),
				zap.String("predicate", cand.Predicate),
				zap.Error(err),
			)
		}
	}
}

// SelfEntityUID is the canonical UID for the speaker. Seeded by
// migration 0012 in the entities table. Exposed as a const because
// the resolver's self-subject binding path needs it, and because
// future surfaces (UI labels, MCP tools, etc.) will want to refer to
// the same identifier without re-deriving it.
const SelfEntityUID = "self"

// resolveSelfSubject returns the subject UID a first-person candidate binds
// to: the concrete principal UID when a binder is wired and the device is
// bound, else the seeded SelfEntityUID (pre-identity behavior). The binder
// caches the lookup, so calling this once per batch is cheap.
func (r *Resolver) resolveSelfSubject(ctx context.Context) string {
	if r.binder != nil {
		if principalUID, ok := r.binder.SelfPrincipal(ctx); ok {
			return principalUID
		}
	}
	return SelfEntityUID
}

// bindCandidateEndpoints validates the mention indices for a
// RelationCandidate and returns the resolved (subject_uid, object_uid)
// pair. Self-subject candidates skip the SubjectMentionIdx lookup and
// bind directly to selfUID (the resolved self subject). The fourth return value reports
// whether the candidate is safe to process; false means it was
// already logged and the caller should continue.
func (r *Resolver) bindCandidateEndpoints(
	cand ontology.RelationCandidate,
	mentionResults []MatchResult,
	selfUID string,
	candidateIdx int,
) (string, string, bool) {
	// Object must always point into the mention batch.
	if cand.ObjectMentionIdx < 0 || cand.ObjectMentionIdx >= len(mentionResults) {
		r.logger.Warn("Resolver: candidate has out-of-range object index, skipping",
			zap.Int("candidateIdx", candidateIdx),
			zap.Int("objectIdx", cand.ObjectMentionIdx),
			zap.Int("mentionCount", len(mentionResults)),
		)
		return "", "", false
	}
	objUID := mentionResults[cand.ObjectMentionIdx].EntityUID

	var subjUID string
	if cand.IsSelfSubject() {
		subjUID = selfUID
	} else {
		if cand.SubjectMentionIdx < 0 || cand.SubjectMentionIdx >= len(mentionResults) {
			r.logger.Warn("Resolver: candidate has out-of-range subject index, skipping",
				zap.Int("candidateIdx", candidateIdx),
				zap.Int("subjectIdx", cand.SubjectMentionIdx),
				zap.Int("mentionCount", len(mentionResults)),
			)
			return "", "", false
		}
		subjUID = mentionResults[cand.SubjectMentionIdx].EntityUID
	}

	if subjUID == "" || objUID == "" {
		r.logger.Warn("Resolver: skipping candidate because subject or object entity has no UID",
			zap.Int("candidateIdx", candidateIdx),
			zap.String("predicate", cand.Predicate),
		)
		return "", "", false
	}
	return subjUID, objUID, true
}

// regenSummariesIfNeeded triggers Entity summary regeneration for any entity
// that crossed the N=5 or 24h threshold during this batch. Entities with no
// summary at all are seeded directly from their mention text instead — no
// LLM call (see seedSummaryDirect).
func (r *Resolver) regenSummariesIfNeeded(
	ctx context.Context,
	mentions []ontology.Mention,
	mentionResults []MatchResult,
	mentionEntities []ontology.Entity,
	newMentionCounts map[string]int,
) {
	// mentionEntities carries one entry per MENTION, so an entity referenced
	// by several mentions in this batch appears several times — and every
	// visit would see the same pre-update snapshot (the local copy, not the
	// store). Process each UID once: collectMentionsForEntity already
	// gathers the FULL batch window for the entity, so the first visit does
	// all the work and later visits would only duplicate the event (or,
	// worse, the LLM call on the regen path).
	processed := make(map[string]struct{}, len(mentionEntities))
	for i, entity := range mentionEntities {
		if entity.UID == "" {
			continue
		}
		if _, dup := processed[entity.UID]; dup {
			continue
		}
		processed[entity.UID] = struct{}{}
		count := newMentionCounts[entity.UID]
		if mentionResults[i].IsNew {
			count = 1
		}

		// First summary: "summarizing" the one mention a fresh entity has
		// is an identity transform, yet burst profiling showed this LLM
		// call firing for EVERY new entity — 80 calls, 24% of all LLM
		// time across a 62-fact burst. Seed the summary directly from the
		// mention text instead; the LLM earns its keep at the
		// SummaryRegen threshold, where multi-mention synthesis is real.
		if entity.SummaryUpdateTime == nil && strings.TrimSpace(entity.Summary) == "" {
			entityMentions := collectMentionsForEntity(mentions, mentionResults, entity.UID)
			r.seedSummaryDirect(ctx, entity, entityMentions)
			continue
		}

		if !r.shouldRegenSummary(entity, count) {
			continue
		}
		entityMentions := collectMentionsForEntity(mentions, mentionResults, entity.UID)
		newSummary, err := r.regenSummary(ctx, entity, entityMentions)
		if err != nil {
			r.logger.Warn("Resolver: summary regen failed",
				zap.String("entityUID", entity.UID),
				zap.Error(err),
			)
			continue
		}
		now := time.Now().UTC()
		entity.Summary = newSummary
		entity.SummaryUpdateTime = &now
		if err := r.appendEntityUpdated(ctx, entity); err != nil {
			r.logger.Warn("Resolver: failed to append EntityUpdated after regen",
				zap.String("entityUID", entity.UID),
				zap.Error(err),
			)
		}
	}
}

// seedSummaryDirect sets a fresh entity's first summary from its mention
// window WITHOUT an LLM call (same input window the regen prompt would have
// seen; with a single mention the "summary" is the text itself). Stamps
// SummaryUpdateTime so the staleness clock starts and the LLM path takes
// over from the SummaryRegen threshold onward.
func (r *Resolver) seedSummaryDirect(ctx context.Context, entity ontology.Entity, entityMentions []ontology.Mention) {
	seed := strings.TrimSpace(r.summaryWindowText(entityMentions))
	if seed == "" {
		return
	}
	now := time.Now().UTC()
	entity.Summary = seed
	entity.SummaryUpdateTime = &now
	if err := r.appendEntityUpdated(ctx, entity); err != nil {
		r.logger.Warn("Resolver: failed to append EntityUpdated after summary seed",
			zap.String("entityUID", entity.UID),
			zap.Error(err),
		)
	}
}

// collectMentionsForEntity returns the subset of mentions that resolved to the
// given Entity UID. Used to assemble the input window for the summary regen
// prompt.
func collectMentionsForEntity(mentions []ontology.Mention, results []MatchResult, uid string) []ontology.Mention {
	var out []ontology.Mention
	for j, res := range results {
		if res.EntityUID == uid {
			out = append(out, mentions[j])
		}
	}
	return out
}

// emitEntityEvent appends EntityCreated or EntityUpdated for a resolved Mention
// and returns the resulting Entity. Scope is stamped on EntityCreated events
// only — new entities minted from a draft episode must be draft-scoped.
// Existing entities (EntityUpdated path) keep their current scope: scope
// promotion is an explicit operator action, never a side-effect of a match.
func (r *Resolver) emitEntityEvent(ctx context.Context, episodeUID string, scope kernel.Scope, m ontology.Mention, result MatchResult) (ontology.Entity, error) {
	if result.IsNew {
		uid := result.EntityUID
		if uid == "" {
			generated, err := newEntityULID()
			if err != nil {
				return ontology.Entity{}, fmt.Errorf("generate entity UID: %w", err)
			}
			uid = generated
		}
		entity := buildNewEntity(m, uid, result.Status)
		if err := r.appendEntityCreated(ctx, scope, entity); err != nil {
			return ontology.Entity{}, fmt.Errorf("append entity created: %w", err)
		}
		// New entity: its identifier carries the originating episode's scope, so a
		// draft entity's alias is not leaked into personal scope.
		r.appendIdentifierAdded(ctx, scope, entity.UID, m.DisplayName)
		return entity, nil
	}

	// Existing entity: load from the reader and append an EntityUpdated.
	entity, err := r.loadEntity(ctx, result.EntityUID)
	if err != nil {
		// Tolerate load errors by returning an empty entity; the caller logs.
		r.logger.Warn("Resolver: could not load entity for update",
			zap.String("entityUID", result.EntityUID),
			zap.String("episodeUID", episodeUID),
			zap.Error(err),
		)
		return ontology.Entity{UID: result.EntityUID}, nil
	}

	// Merge the new mention's display name into alternateNames if it differs.
	priorAlts := len(entity.AlternateNames)
	entity = mergeAlternateName(entity, m.DisplayName)
	entity.MentionUIDs = append(entity.MentionUIDs, ontology.ULID(m.UID))
	addedAlias := len(entity.AlternateNames) > priorAlts

	// P10: naming canonicalisation. Only invoke the LLM when at least two
	// distinct surface forms have accumulated, and never on freshly-created
	// entities (which carry zero alternateNames).
	if len(entity.AlternateNames) >= namingCanonMinNames-1 {
		entity = r.canonicalNameForEntity(ctx, entity)
	}

	// Read the entity's current scope ONCE and reuse it for both the update and
	// any identifier write, so an existing draft entity stays draft (and a
	// personal entity stays personal) without two independent scope reads.
	entityScope := r.currentEntityScope(ctx, entity.UID)
	if err := r.appendEntityUpdatedWithScope(ctx, entity, entityScope); err != nil {
		return ontology.Entity{}, fmt.Errorf("append entity updated: %w", err)
	}
	if addedAlias {
		r.appendIdentifierAdded(ctx, entityScope, entity.UID, m.DisplayName)
	}
	return entity, nil
}

// relationConfidence derives the confidence of a Relation from its underlying
// mention grounding scores. Returns the minimum of the subject and object
// mention groundings so a single weakly-grounded endpoint pulls the whole
// edge down. Falls back to 1.0 when either mention is out-of-range (caller
// already filtered, but defensive) or grounding is zero (treat unknown
// grounding as confident-by-default; the caller logs the candidate).
func relationConfidence(mentions []ontology.Mention, cand ontology.RelationCandidate) float64 {
	// Self-subject candidates have no subject Mention; their confidence
	// is driven solely by the object's grounding score (the lexical
	// detector already gates on a real mention).
	if cand.IsSelfSubject() {
		if cand.ObjectMentionIdx >= len(mentions) {
			return 1.0
		}
		obj := mentions[cand.ObjectMentionIdx].GroundingScore
		if obj <= 0 {
			return 1.0
		}
		return obj
	}
	if cand.SubjectMentionIdx >= len(mentions) || cand.ObjectMentionIdx >= len(mentions) {
		return 1.0
	}
	subj := mentions[cand.SubjectMentionIdx].GroundingScore
	obj := mentions[cand.ObjectMentionIdx].GroundingScore
	if subj <= 0 || obj <= 0 {
		return 1.0
	}
	if subj < obj {
		return subj
	}
	return obj
}

// processCandidate validates, auto-registers, supersede-checks, and appends
// events for one RelationCandidate.
//
// When untrusted (the candidate came from a connector-sourced episode, #211),
// the edge is stamped status=proposed and two quarantines apply: the predicate
// vocabulary is NOT extended (an unknown predicate stays the non-functional
// default), and the supersede checks are skipped entirely (a proposed edge never
// closes an existing one). It is NOT confidence-penalised (#212): the status
// attribute carries the trust posture, and `promoteRelation` lifts the
// quarantine by flipping the edge to asserted.
// Scope is stamped on the minted Relation event so draft-episode relations stay
// in the draft scope boundary rather than defaulting to personal.
func (r *Resolver) processCandidate(ctx context.Context, episodeUID string, scope kernel.Scope, cand ontology.RelationCandidate, subjUID, objUID string, confidence float64, untrusted bool) error {
	// Auto-extend predicate vocabulary if unknown — but NEVER for untrusted
	// input: an injection payload must not be able to mint a new predicate. An
	// unknown predicate from an untrusted source is left unregistered and so is
	// treated as non-functional by supersedeCheck (which we also skip below).
	if !untrusted {
		if _, known := r.predicates.Kind(cand.Predicate); !known {
			if err := r.autoRegisterPredicate(ctx, cand.Predicate); err != nil {
				r.logger.Warn("Resolver: predicate auto-register failed, treating as non-functional",
					zap.String("predicate", cand.Predicate),
					zap.Error(err),
				)
			}
		}
	}

	// Trust posture (#211, #212): a connector-sourced claim is stamped
	// status=proposed and quarantined from supersession + predicate minting
	// (below). It is NOT confidence-penalised — trust is carried by the status
	// ATTRIBUTE, not by a depressed score, so a proposed fact ranks on its own
	// relevance and a `promoteRelation` can later flip it to asserted.
	relStatus := ontology.RelationStatusAsserted
	if untrusted {
		relStatus = ontology.RelationStatusProposed
	}

	// Fact-level dedup (Graphiti §1.4). If an open relation of the SAME trust
	// status with the exact (subject, predicate, object) triple already exists,
	// the candidate re-states a fact we have already recorded; appending another
	// row would inflate the graph and, for functional predicates, would also
	// trigger a self-supersede that closes the still-current fact. Skip both
	// writes and let the caller continue with the next candidate. Errors fall
	// through to the normal supersede + append path so a transient SQLite read
	// does not silently drop a relation. The status filter keeps a proposed
	// re-ingest idempotent against prior proposed rows while never letting a
	// proposed claim dedup away (and thereby suppress) a user-asserted fact.
	if existing, dup, err := r.openRelationExists(ctx, subjUID, cand.Predicate, objUID, relStatus); err != nil {
		r.logger.Warn("Resolver: fact-dedup query failed; continuing with append",
			zap.String("predicate", cand.Predicate),
			zap.Error(err),
		)
	} else if dup {
		r.logger.Debug("Resolver: relation already open, skipping duplicate append",
			zap.String("predicate", cand.Predicate),
			zap.String("subjectUid", subjUID),
			zap.String("objectUid", objUID),
			zap.String("existingRelationUid", existing),
		)
		return nil
	}

	// Supersede check — SKIPPED entirely for untrusted input (#211): a proposed
	// connector edge never closes an existing edge. It coexists as a candidate.
	var supersededUID string
	if !untrusted {
		var err error
		supersededUID, err = r.supersedeCheck(ctx, subjUID, cand.Predicate, cand)
		if err != nil {
			return fmt.Errorf("supersede check: %w", err)
		}
	}

	// Append the new Relation event first so we know its UID for the supersede event.
	newRelUID, err := r.appendRelation(ctx, episodeUID, scope, subjUID, objUID, cand, confidence, relStatus)
	if err != nil {
		return fmt.Errorf("append relation: %w", err)
	}

	if supersededUID != "" {
		if err := r.appendRelationRetracted(ctx, supersededUID, newRelUID, cand.ValidFrom); err != nil {
			// Non-fatal: the new Relation landed; the retraction event is best-effort.
			r.logger.Warn("Resolver: failed to append RelationRetracted",
				zap.String("supersededUID", supersededUID),
				zap.String("newRelUID", newRelUID),
				zap.Error(err),
			)
		}
	}

	// Cross-subject role supersede is also a supersession path, so it too is
	// skipped for untrusted input: a proposed connector edge must not unseat an
	// existing executive-slot incumbent.
	if !untrusted {
		r.applyCrossSubjectRoleSupersede(ctx, cand, subjUID, objUID, newRelUID, supersededUID)
	}
	return nil
}

// applyCrossSubjectRoleSupersede closes any existing open relations that
// share the same functional executive slot (object_uid, predicate,
// normalised role) with the new fact, even when the subject differs.
// Example: a new "Frodo is CEO of Erebor" must close an existing
// "Rosie is CEO of Erebor". The regular functionalSupersede only fires on
// (subject, predicate) and silently skips this case; the dedicated
// singularRoles allowlist gates the opt-in so multi-incumbent roles
// (engineer, member) never trigger.
func (r *Resolver) applyCrossSubjectRoleSupersede(ctx context.Context, cand ontology.RelationCandidate, subjUID, objUID, newRelUID, supersededUID string) {
	role := cand.Attributes["role"]
	if role == "" {
		return
	}
	crossUIDs, err := r.crossSubjectRoleSupersede(ctx, subjUID, cand.Predicate, objUID, role)
	if err != nil {
		r.logger.Warn("Resolver: cross-subject role supersede query failed",
			zap.String("predicate", cand.Predicate),
			zap.String("role", role),
			zap.Error(err),
		)
	}
	for _, prevUID := range crossUIDs {
		if prevUID == supersededUID {
			continue // already closed above; do not double-emit.
		}
		if err := r.appendRelationRetracted(ctx, prevUID, newRelUID, cand.ValidFrom); err != nil {
			r.logger.Warn("Resolver: failed to append cross-subject RelationRetracted",
				zap.String("supersededUID", prevUID),
				zap.String("newRelUID", newRelUID),
				zap.String("role", role),
				zap.Error(err),
			)
		}
	}
}

// appendEntityCreated marshals entity and appends an EntityCreated event.
// Scope is copied onto the event so new entities inherit the originating
// episode's scope boundary (e.g. draft episodes mint draft entities).
func (r *Resolver) appendEntityCreated(ctx context.Context, scope kernel.Scope, entity ontology.Entity) error {
	raw, err := json.Marshal(entity)
	if err != nil {
		return fmt.Errorf("marshal entity: %w", err)
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "resolver/v1",
		Scope:      scope,
		Confidence: entity.Confidence,
		Payload:    raw,
	}
	_, err = r.eventLog.Append(ctx, []kernel.Event{ev})
	return err
}

// appendEntityUpdated marshals entity and appends an EntityUpdated event,
// stamped with the entity's CURRENT scope. An update (summary regen, alternate-
// name merge, or a re-mention from a later episode) must never re-scope the
// entity: a draft entity stays draft and a personal entity stays personal, so
// promotion out of draft is only ever the explicit `scope draft promote` act,
// never a side effect of re-extraction. Leaving Scope empty here would default
// the event to personal and silently leak a draft entity into the default read
// set (the omnigraph draft-isolation bug).
func (r *Resolver) appendEntityUpdated(ctx context.Context, entity ontology.Entity) error {
	return r.appendEntityUpdatedWithScope(ctx, entity, r.currentEntityScope(ctx, entity.UID))
}

// appendEntityUpdatedWithScope appends an EntityUpdated event stamped with the
// caller-supplied scope. The existing-entity resolution path reads the entity's
// current scope ONCE (see emitEntityEvent) and threads it both here and into
// appendIdentifierAdded, so a single update never issues two independent
// `SELECT scope` reads — closing a TOCTOU window where a concurrent
// `scope draft promote` could land between them. Regen/seed callers that have
// no precomputed scope go through appendEntityUpdated, which reads it.
func (r *Resolver) appendEntityUpdatedWithScope(ctx context.Context, entity ontology.Entity, scope kernel.Scope) error {
	raw, err := json.Marshal(entity)
	if err != nil {
		return fmt.Errorf("marshal entity: %w", err)
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "resolver/v1",
		Scope:      scope,
		Confidence: entity.Confidence,
		Payload:    raw,
	}
	_, err = r.eventLog.Append(ctx, []kernel.Event{ev})
	return err
}

// currentEntityScope returns the scope the entity is currently stored under in
// the projection. Updates to an existing entity MUST preserve this scope (see
// appendEntityUpdated). Defaults to kernel.DefaultScope when the row is missing
// or unreadable — a brand-new entity is stamped by its creator via
// appendEntityCreated, so a miss here only ever falls back to the prior
// (personal) behaviour for genuinely-unknown entities.
func (r *Resolver) currentEntityScope(ctx context.Context, uid string) kernel.Scope {
	if r.reader == nil {
		return kernel.DefaultScope
	}
	var scope string
	if err := r.reader.QueryRowContext(ctx,
		`SELECT scope FROM entities WHERE uid = ?`, uid,
	).Scan(&scope); err != nil || scope == "" {
		return kernel.DefaultScope
	}
	return kernel.Scope(scope)
}

// appendIdentifierAdded appends an EntityIdentifierAdded event tagging
// alias as the canonical/extracted surface form for entityUID. Errors are
// non-fatal and logged; the entity itself is already written, and the
// identifier write is a quality optimisation for the resolver's future
// matches, not a correctness invariant for the current write.
func (r *Resolver) appendIdentifierAdded(ctx context.Context, scope kernel.Scope, entityUID, alias string) {
	if alias == "" || entityUID == "" {
		return
	}
	payload := ontology.EntityIdentifierAddedEvent{
		EntityUID:  entityUID,
		Identifier: alias,
		Kind:       ontology.IdentifierKindAlias,
		Source:     ontology.IdentifierSourceExtracted,
		AddedAt:    time.Now().UTC(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		r.logger.Warn("Resolver: marshal entityIdentifierAdded",
			zap.String("entityUID", entityUID),
			zap.String("identifier", alias),
			zap.Error(err),
		)
		return
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
		Source:     "resolver/v1",
		Scope:      scope,
		Confidence: 1.0,
		Payload:    raw,
	}
	if _, err := r.eventLog.Append(ctx, []kernel.Event{ev}); err != nil {
		r.logger.Warn("Resolver: append entityIdentifierAdded",
			zap.String("entityUID", entityUID),
			zap.String("identifier", alias),
			zap.Error(err),
		)
	}
}

// appendRelation marshals and appends a Relation event. Returns the relation's
// payload ULID (rel.UID) — the key the dispatcher uses for the relations-table
// row — NOT the event-envelope ULID the log mints on append. See the return
// site for why the distinction is load-bearing for supersededByUid.
// Scope is stamped on the event so relations from draft episodes stay in the
// draft scope boundary rather than defaulting to personal.
func (r *Resolver) appendRelation(ctx context.Context, episodeUID string, scope kernel.Scope, subjUID, objUID string, cand ontology.RelationCandidate, confidence float64, status string) (string, error) {
	uid, err := newEntityULID()
	if err != nil {
		return "", fmt.Errorf("generate relation UID: %w", err)
	}
	rel := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     subjUID,
		Predicate:      cand.Predicate,
		ObjectUID:      objUID,
		Confidence:     confidence,
		SourceEventUID: episodeUID,
		CreateTime:     time.Now().UTC(),
		ValidFrom:      cand.ValidFrom,
		ValidUntil:     cand.ValidUntil,
		Attributes:     cand.Attributes,
		Status:         status,
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		return "", fmt.Errorf("marshal relation: %w", err)
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "resolver/v1",
		Scope:      scope,
		Confidence: confidence,
		Payload:    raw,
	}
	receipts, err := r.eventLog.Append(ctx, []kernel.Event{ev})
	if err != nil {
		return "", err
	}
	if len(receipts) == 0 {
		return "", errors.New("append relation: no receipt returned")
	}
	// Return the relation's own ULID (the payload UID the dispatcher keys the
	// relations row on), NOT receipts[0].UID — the event log mints a fresh
	// event-envelope ULID on append that is distinct from the relation
	// identity. The caller feeds this back into appendRelationRetracted as the
	// supersededByUid, which must reference the replacement relation; returning
	// the event UID instead leaves superseded_by_uid dangling (it matches no
	// relations row). See cli/relate + mcp/relate, which likewise treat the
	// pre-minted payload UID (rel.UID) as the canonical relation identity.
	return uid, nil
}

// appendRelationRetracted appends a relationRetracted tombstone closing the
// prior relation. The retractedAt timestamp doubles as the validUntil bound on
// the prior relation (see ontology.RelationRetractedEvent.RetractedAt
// docstring); when the superseding candidate has a known ValidFrom, use that
// instead of wall-clock now so bi-temporal queries against past instants return
// the correct relation. Example: ingesting "Frodo started at Rivendell
// 2025-01-01" in 2026 should close the prior `works_at Erebor` at 2025-01-01,
// not at 2026 (otherwise an `at=2025-06` query still sees both as valid).
// Falls back to time.Now() when ValidFrom is unset.
//
// Retraction is a commutative tombstone: the dispatcher unions these by event
// UID and re-folds the prior relation's close columns deterministically, so two
// paired devices closing the same arm converge regardless of replay order.
func (r *Resolver) appendRelationRetracted(ctx context.Context, supersededUID, supersederUID string, newValidFrom *time.Time) error {
	// Best-effort live gate (#213, D3): do not emit a team-scope tombstone the
	// local principal is not authorized to close. The AUTHORITATIVE gate is the
	// replay-path authority filter in dispatcher.recomputeRelationClose — an
	// unauthorized team-scope tombstone is recorded for audit but never closes
	// the relation on any device. This skip only avoids writing a doomed
	// tombstone locally; it never affects the non-team path. Errors here are
	// non-fatal: on any uncertainty we fall through and emit, letting the replay
	// filter protect correctness.
	if !r.teamRetractionAllowed(ctx, supersededUID) {
		r.logger.Debug("Resolver: skipping unauthorized team-scope relationRetracted",
			zap.String("supersededUID", supersededUID),
		)
		return nil
	}
	retractedAt := time.Now().UTC()
	if newValidFrom != nil && !newValidFrom.IsZero() {
		retractedAt = newValidFrom.UTC()
	}
	payload := ontology.RelationRetractedEvent{
		RelationUID:     ontology.ULID(supersededUID),
		SupersededByUID: ontology.ULID(supersederUID),
		RetractedAt:     retractedAt,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal relation retracted: %w", err)
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindRelationRetracted),
		Source:     "resolver/v1",
		Confidence: 1.0,
		Payload:    raw,
	}
	_, err = r.eventLog.Append(ctx, []kernel.Event{ev})
	return err
}

// teamRetractionAllowed reports whether the local principal may close the
// relation identified by supersededUID. It returns true for every non-team
// relation (personal/project retraction is unrestricted) and for any case it
// cannot decide cleanly — the call is BEST-EFFORT, fail-open, because the
// authoritative gate is the replay-path filter in
// dispatcher.recomputeRelationClose. For a team-scope relation it returns true
// only when the local principal is the original asserter
// (relations.asserted_by_principal) or the team's genesis owner, mirroring the
// replay filter so a doomed tombstone is not written in the first place.
func (r *Resolver) teamRetractionAllowed(ctx context.Context, supersededUID string) bool {
	var (
		scope      string
		assertedBy sql.NullString
	)
	switch err := r.reader.QueryRowContext(ctx,
		`SELECT scope, asserted_by_principal FROM relations WHERE uid = ?`, supersededUID,
	).Scan(&scope, &assertedBy); {
	case errors.Is(err, sql.ErrNoRows):
		// Relation not materialized locally yet: cannot judge authority. Emit and
		// let the replay filter decide once the relation lands.
		return true
	case err != nil:
		r.logger.Debug("Resolver: team-retraction authority lookup failed; emitting",
			zap.String("supersededUID", supersededUID),
			zap.Error(err),
		)
		return true
	}
	if scope != string(kernel.ScopeTeam) {
		return true // non-team retraction is unrestricted.
	}

	local := r.resolveSelfSubject(ctx)
	if local == "" || local == SelfEntityUID {
		// No bound principal identity: cannot prove authority either way. Emit
		// best-effort; the replay filter remains the authoritative gate.
		return true
	}
	if assertedBy.Valid && local == assertedBy.String {
		return true // local principal is the original asserter.
	}

	var genesis string
	switch err := r.reader.QueryRowContext(ctx,
		`SELECT principal_uid FROM team_membership_acl WHERE is_genesis = 1 LIMIT 1`,
	).Scan(&genesis); {
	case errors.Is(err, sql.ErrNoRows):
		genesis = ""
	case err != nil:
		r.logger.Debug("Resolver: genesis-owner lookup failed; emitting",
			zap.String("supersededUID", supersededUID),
			zap.Error(err),
		)
		return true
	}
	return genesis != "" && local == genesis
}

// loadEntity queries the SQLite reader for the entity row identified by uid.
// Returns ErrNotFound (wrapped) when no row exists. Name is derived as
// "entities/<uid>" because the dispatcher does not persist the AIP resource
// name as a separate column; Summary and SummaryUpdateTime are populated
// only after a regeneration pass writes them via the event log (M2 work),
// so the loader reads them through nullable joins to keep working before
// that lands.
func (r *Resolver) loadEntity(ctx context.Context, uid string) (ontology.Entity, error) {
	var (
		displayName      string
		kind             string
		confidence       float64
		resolutionStatus string
		createTimeStr    string
	)
	err := r.reader.QueryRowContext(ctx,
		`SELECT display_name, kind, confidence, resolution_status, create_time
		   FROM entities WHERE uid = ?`, uid,
	).Scan(&displayName, &kind, &confidence, &resolutionStatus, &createTimeStr)
	if errors.Is(err, sql.ErrNoRows) {
		return ontology.Entity{}, fmt.Errorf("entity %q: %w", uid, kernel.ErrNotFound)
	}
	if err != nil {
		return ontology.Entity{}, fmt.Errorf("load entity %q: %w", uid, err)
	}

	var createTime time.Time
	if createTimeStr != "" {
		createTime, err = time.Parse(time.RFC3339Nano, createTimeStr)
		if err != nil {
			return ontology.Entity{}, fmt.Errorf("parse entity create_time: %w", err)
		}
	}
	return ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKind(kind),
		DisplayName:      displayName,
		Confidence:       confidence,
		ResolutionStatus: ontology.ResolutionStatus(resolutionStatus),
		CreateTime:       createTime,
	}, nil
}

// autoRegisterPredicate classifies an unknown predicate and registers it.
// The curated predicateLexicon is consulted first: lexicon hits classify
// deterministically with zero LLM calls (burst profiling showed ~15
// classify_predicate round-trips per cold 62-fact ingest, all for
// predicates a human can classify once at build time — and the LLM
// occasionally classified past-tense predicates as functional, wrongly
// auto-superseding one past employer with another). Only genuinely novel
// predicates fall through to the predicate-canon-v1 LLM prompt.
func (r *Resolver) autoRegisterPredicate(ctx context.Context, predicate string) error {
	if kind, desc, ok := lexiconLookup(predicate); ok {
		return r.registerPredicate(ctx, Predicate{
			Name:        predicate,
			Kind:        kind,
			Description: desc,
			AddedAt:     time.Now().UTC(),
			AddedBy:     addedBySeed,
		})
	}

	system, err := r.promptReg.Render(ctx, predicateCanonTemplateName, predicateCanonTemplateVersion, prompts.RenderVars{
		"predicate": predicate,
	})
	if err != nil {
		return fmt.Errorf("render predicate-canon prompt: %w", err)
	}

	schema := json.RawMessage(`{
		"type": "object",
		"required": ["kind", "description"],
		"properties": {
			"kind":        {"type": "string", "enum": ["functional", "non-functional"]},
			"description": {"type": "string"}
		}
	}`)

	resp, err := r.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:  r.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: "Classify the predicate: " + predicate},
		},
		Tool: kernel.ToolSchema{
			Name:        "classify_predicate",
			Description: "Classify a relation predicate as functional or non-functional.",
			InputSchema: schema,
		},
		MaxTokens: r.cfg.PredicateCanonMaxTokens,
	})
	if err != nil {
		return fmt.Errorf("predicate-canon tool-use: %w", err)
	}

	kind := NonFunctional // conservative default
	description := ""

	if resp.ToolUse != nil {
		var out struct {
			Kind        string `json:"kind"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(resp.ToolUse.Input, &out); err == nil {
			description = out.Description
			switch out.Kind {
			case string(Functional):
				kind = Functional
			case string(NonFunctional):
				kind = NonFunctional
			}
		}
	}

	return r.registerPredicate(ctx, Predicate{
		Name:        predicate,
		Kind:        kind,
		Description: description,
		AddedAt:     time.Now().UTC(),
		AddedBy:     addedByLLM,
	})
}

// registerPredicate persists p in the predicates table, appends a
// PredicateRegistered event, and logs the vocabulary growth. Delegates to the
// package-level WritePredicate so the definePredicate MCP tool can call the
// same writer logic without importing the Resolver type.
func (r *Resolver) registerPredicate(ctx context.Context, p Predicate) error {
	return WritePredicate(ctx, r.writer, r.eventLog, r.predicates, r.logger, p, "resolver/v1")
}

// newEntityULID returns a fresh ULID encoded in Crockford base32, suitable
// for the Entity name pattern "entities/[0-9A-HJKMNP-TV-Z]{26}". Errors
// from the entropy source are surfaced so the caller can fail the
// resolution batch rather than silently emit an invalid event.
func newEntityULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.DefaultEntropy())
	if err != nil {
		return "", fmt.Errorf("ulid: %w", err)
	}
	return id.String(), nil
}

// buildNewEntity constructs a fresh Entity from a Mention and the pre-generated
// UID. The caller is responsible for assigning the UID (e.g. from a ULID
// generator) before calling this helper.
//
// For the resolver, if result.EntityUID is empty (e.g. from a match that
// returned IsNew=true with no generated UID), the entity is created with a
// placeholder. Real production usage assigns UIDs via the event log's
// Append receipt; for now we use a placeholder so the type system is satisfied.
func buildNewEntity(m ontology.Mention, uid string, status ontology.ResolutionStatus) ontology.Entity {
	now := time.Now().UTC()
	return ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             m.Kind,
		DisplayName:      m.DisplayName,
		Confidence:       m.GroundingScore,
		ResolutionStatus: status,
		CreateTime:       now,
		Identifiers:      m.Identifiers,
		MentionUIDs:      []ontology.ULID{ontology.ULID(m.UID)},
	}
}

// mergeAlternateName adds name to entity.AlternateNames if it differs from
// entity.DisplayName and is not already listed.
func mergeAlternateName(entity ontology.Entity, name string) ontology.Entity {
	if name == entity.DisplayName {
		return entity
	}
	for _, n := range entity.AlternateNames {
		if n == name {
			return entity
		}
	}
	entity.AlternateNames = append(entity.AlternateNames, name)
	return entity
}
