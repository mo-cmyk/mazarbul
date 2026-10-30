package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// createEntityDefaultConfidence is applied when the caller omits confidence: a
// direct structured mint is treated as user-confirmed.
const createEntityDefaultConfidence = 1.0

// createEntityKindPattern mirrors ontology.entityKindPattern so a malformed kind
// surfaces as E_VALIDATION here rather than as an opaque E_INTERNAL when the
// entity schema rejects the append downstream.
var createEntityKindPattern = regexp.MustCompile(`^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$`)

const createEntityInputSchema = `{
  "type": "object",
  "properties": {
    "kind": {
      "type": "string",
      "pattern": "^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$",
      "description": "Entity kind. A core kind (person, organization, place, project, event, document) or a registered extension kind (e.g. user:vehicle). Register a new kind with defineEntityKind first; introspect lists what exists."
    },
    "displayName": {
      "type": "string",
      "minLength": 1,
      "description": "Human-readable label, e.g. \"Mithril Corp\" or \"Eowyn\"."
    },
    "confidence": {
      "type": "number",
      "minimum": 0,
      "maximum": 1,
      "description": "Writer confidence in the entity's identity. Defaults to 1.0 (user-confirmed)."
    },
    "identifiers": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "type": {"type": "string", "minLength": 1, "description": "Identifier type in snake_case, e.g. email, username, phone, alias, or a domain type like hrb, lei, tax_id. Domain types are preserved as their own lookup lane (not collapsed into a generic id), so the same value under different types never collides."},
          "value": {"type": "string", "minLength": 1, "description": "Identifier value, e.g. eowyn@mithril.example."}
        },
        "required": ["type", "value"],
        "additionalProperties": false
      },
      "description": "Strong/weak external identifiers to anchor the entity so the resolver attaches future mentions instead of minting a duplicate. Optional."
    },
    "alternateNames": {
      "type": "array",
      "items": {"type": "string", "minLength": 1},
      "description": "Other surface forms for this entity. Optional."
    },
    "properties": {
      "type": "object",
      "description": "Typed attribute values matching the kind's propertySchema (P3), e.g. {\"vin\":\"…\",\"make\":\"…\"}. Validated on write when strict_attributes is enabled. Optional."
    },
    "scope": {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope to write into — pick by who should see it. Defaults to personal (private to the user, syncs to their devices). Use \"project\" for records tied to the current repo/codebase (shared with anyone who has repo access). Use \"team\" for the shared team brain (requires admitted membership, else E_PERMISSION)."},
    "force": {"type": "boolean", "description": "Skip the duplicate check and mint a new entity even when one with the same name already exists. Use to record a genuinely distinct entity that shares a name with an existing one (e.g. a second real person named the same). Defaults to false."}
  },
  "required": ["kind", "displayName"],
  "additionalProperties": false
}`

const createEntityOutputSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "pattern": "^entities/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the entity (set on status created/reused). Pass to relate or get(entities/{uid})."},
    "uid": {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "Bare ULID of the entity (set on status created/reused)."},
    "kind": {"type": "string", "description": "The entity's kind (set on status created/reused)."},
    "status": {"type": "string", "enum": ["created", "reused", "duplicate_candidates"], "description": "created = a new entity was minted; reused = an existing entity matched by identifier and is returned in uid/name; duplicate_candidates = NOT created because same-name or near-duplicate entities exist (see candidates) — reuse one, merge with mergeEntities, or retry with force=true."},
    "candidates": {
      "type": "array",
      "description": "Existing entities that match by name or embedding. On duplicate_candidates the create was refused; on created these are advisory near-matches you may want to merge.",
      "items": {
        "type": "object",
        "properties": {
          "uid": {"type": "string"},
          "displayName": {"type": "string"},
          "kind": {"type": "string"},
          "similarity": {"type": "number", "description": "Cosine similarity (1.0 for an exact-name or identifier match)."},
          "reason": {"type": "string", "description": "Which lane surfaced this: exact_name, name_emb, or identifier."},
          "relationCount": {"type": "integer", "description": "Active relations on the candidate; higher usually means the more canonical entity to keep."}
        },
        "required": ["uid", "displayName", "kind", "similarity", "reason"],
        "additionalProperties": false
      }
    }
  },
  "required": ["status"],
  "additionalProperties": false
}`

// linkReconciler is the narrow reconcile surface CreateEntity uses to wire back
// dangling document wikilinks after a new entity is minted. Satisfied by
// *document.Importer; nil disables the reconcile pass (safe degradation).
type linkReconciler interface {
	// ReconcileForEntity runs a targeted reconcile for the named entity: it
	// scans documents with dangling links whose target matches one of the
	// supplied names and emits the now-resolvable relation edges. Returns the
	// number of new edges emitted.
	ReconcileForEntity(ctx context.Context, entityUID string, names []string) (int, error)
}

// CreateEntity mints a structured entity of a registered kind directly, without
// an LLM extraction round-trip. It is the write-side complement to relate: when
// you need an entity to hang a relation off — and you already hold its kind and
// name — createEntity returns its entities/{uid} in one call.
//
// It appends a KindEntity event the dispatcher applies straight to the entities
// table, plus one entityIdentifierAdded event per supplied identifier so the
// resolver anchors future mentions to this entity rather than splitting them
// into a duplicate. Typed properties are validated against the kind's
// propertySchema on the write path when [ontology] strict_attributes is enabled.
//
// After a successful mint, CreateEntity runs a best-effort targeted reconcile
// via the wired linkReconciler (if any): documents that carry a dangling
// [[DisplayName]] wikilink or about: frontmatter referencing this entity's name
// will have their pending edge emitted immediately, without waiting for the next
// document import. A reconcile failure is logged and does not fail the tool call
// — the entity is already durably created.
type CreateEntity struct {
	appender   Appender
	kinds      kindLister          // may be nil → kind-existence check skipped
	reconciler linkReconciler      // may be nil → reconcile pass skipped
	matcher    *entitymatch.Lookup // may be nil → create-time dedup skipped
	logger     *zap.Logger         // may be nil → fail-open dedup degradation is silent
}

// WithLinkReconciler attaches an optional post-create reconcile surface.
// When set, Invoke triggers a targeted wikilink reconcile after each successful
// entity mint so dangling [[Name]] links in existing documents resolve
// immediately. Returns c so calls can be chained:
//
//	tools.NewCreateEntity(a, kinds).WithLinkReconciler(importer)
func (c *CreateEntity) WithLinkReconciler(r linkReconciler) *CreateEntity {
	c.reconciler = r
	return c
}

// WithDedup attaches the shared entity-dedup lookup so Invoke refuses to mint a
// duplicate of an existing entity (exact-name or high name-embedding match) and
// instead returns candidates for the caller to reuse or merge. A nil matcher
// (the default, or degraded boot) skips the gate entirely. Returns c so calls
// can be chained.
func (c *CreateEntity) WithDedup(m *entitymatch.Lookup) *CreateEntity {
	c.matcher = m
	return c
}

// WithLogger attaches a logger so the dedup gate's fail-open degradation (a
// matcher error allows the write rather than blocking it) is observable at
// debug level instead of vanishing silently. A nil logger (the default) keeps
// the degradation silent. Returns c so calls can be chained.
func (c *CreateEntity) WithLogger(l *zap.Logger) *CreateEntity {
	c.logger = l
	return c
}

// kindLister is the read-side dependency CreateEntity uses to confirm the
// target kind is registered before minting, narrowed from Navigator. A nil
// lister (e.g. minimal boot) disables the check; a registry read error degrades
// to allow, so a transient DB fault never blocks a write.
type kindLister interface {
	RegisteredEntityKinds(ctx context.Context) ([]navigate.RegisteredEntityKind, error)
}

// NewCreateEntity constructs a CreateEntity tool bound to appender a. Kinds may
// be nil to skip the kind-existence check (minimal boot).
func NewCreateEntity(a Appender, kinds kindLister) *CreateEntity {
	return &CreateEntity{appender: a, kinds: kinds}
}

// Name implements mcp.Tool.
func (*CreateEntity) Name() string { return "createEntity" }

// Description implements mcp.Tool.
func (*CreateEntity) Description() string {
	return "Mint a structured entity of a registered kind directly, bypassing LLM extraction — the write-side complement to relate. Use it when you already hold an entity as (kind, displayName) and need its entities/{uid} to assert a relation, without a remember() round-trip. Inputs: kind (a core kind or a registered extension kind — register first with defineEntityKind, check with introspect), displayName, optional confidence (default 1.0), identifiers (anchor the entity so the resolver attaches future mentions instead of duplicating), alternateNames, and properties (typed attributes validated against the kind's propertySchema). Deduplicates before minting: a matching identifier returns the existing entity (status reused); an existing same-name or near-duplicate of the same kind is NOT created (status duplicate_candidates) — inspect candidates, then reuse a uid, merge with mergeEntities, or retry with force=true to record a genuinely distinct entity that happens to share a name. On a plain create (status created) any advisory near-matches are still listed in candidates. Returns E_VALIDATION on a malformed kind, empty displayName, out-of-range confidence, or non-object properties."
}

// InputSchema implements mcp.Tool.
func (*CreateEntity) InputSchema() json.RawMessage { return json.RawMessage(createEntityInputSchema) }

// OutputSchema implements mcp.Tool.
func (*CreateEntity) OutputSchema() json.RawMessage { return json.RawMessage(createEntityOutputSchema) }

type createEntityIdentifier struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type createEntityInput struct {
	Kind           string                   `json:"kind"`
	DisplayName    string                   `json:"displayName"`
	Confidence     *float64                 `json:"confidence,omitempty"`
	Identifiers    []createEntityIdentifier `json:"identifiers,omitempty"`
	AlternateNames []string                 `json:"alternateNames,omitempty"`
	Properties     json.RawMessage          `json:"properties,omitempty"`
	Scope          string                   `json:"scope,omitempty"`
	Force          bool                     `json:"force,omitempty"`
}

// entityCandidate is an existing entity surfaced as a possible duplicate of a
// requested create. It mirrors entitymatch.Candidate as the tool's wire shape.
type entityCandidate struct {
	UID           string  `json:"uid"`
	DisplayName   string  `json:"displayName"`
	Kind          string  `json:"kind"`
	Similarity    float64 `json:"similarity"`
	Reason        string  `json:"reason"`
	RelationCount int     `json:"relationCount"`
}

type createEntityOutput struct {
	Name       string            `json:"name,omitempty"`
	UID        string            `json:"uid,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Status     string            `json:"status"`
	Candidates []entityCandidate `json:"candidates,omitempty"`
}

// Invoke implements mcp.Tool. It builds an ontology.Entity, appends it as a
// KindEntity event, then appends one entityIdentifierAdded event per identifier,
// and returns the new entity's resource name.
func (c *CreateEntity) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in createEntityInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	confidence, verr := validateCreateEntityInput(in)
	if verr != nil {
		return nil, verr
	}
	scope, serr := parseWriteScope(in.Scope)
	if serr != nil {
		return nil, serr
	}
	resolvedKind, kerr := c.resolveKind(ctx, in.Kind)
	if kerr != nil {
		return nil, kerr
	}
	in.Kind = resolvedKind

	// Duplicate gate: refuse to mint a same-name / near-duplicate entity unless
	// the caller forces it. dedupGate runs the matcher (even under force, so a
	// forced mint still reports the near-matches it is forking from as advisory
	// candidates) and returns either a terminal decision to surface verbatim or
	// the advisory candidates to attach to the mint.
	decision, advisory := c.dedupGate(ctx, in)
	if decision != nil {
		return marshalOutput(*decision)
	}

	uid, err := newEntityULID()
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("generate entity uid: %w", err))
	}

	events, ent, berr := buildCreateEntityEvents(in, uid, confidence, scope)
	if berr != nil {
		return nil, berr
	}

	receipts, err := c.appender.Append(ctx, events)
	if err != nil {
		// A pre-write validation failure (ErrInvalidEvent) — most commonly a
		// properties map that violates the kind's propertySchema under
		// strict_attributes — is the caller's input mistake, so surface it as
		// E_VALIDATION (the agent can fix the input) rather than as an opaque
		// E_INTERNAL "server fault, retry/give up". Everything else routes
		// through mapAppendError so a team-scope write by a non-member surfaces
		// as E_PERMISSION (matching relate); genuine storage/wiring faults still
		// fall through to E_INTERNAL.
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append entity: %v", err), nil)
		}
		return nil, mapAppendError("append entity", err)
	}
	if len(receipts) == 0 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	// Best-effort targeted reconcile: resolve dangling document wikilinks that
	// name this entity. A failure here must not fail the tool call — the entity
	// is already durably written and the reconcile is an enhancement, not a
	// correctness requirement. The error is intentionally dropped: the Importer
	// logs any internal fault at warn before returning it, so the caller still
	// receives the successful create result.
	if c.reconciler != nil {
		names := append([]string{in.DisplayName}, in.AlternateNames...)
		_, _ = c.reconciler.ReconcileForEntity(ctx, uid, names) //nolint:errcheck // best-effort; entity already durable
	}

	return marshalOutput(createEntityOutput{
		Name:       ent.Name,
		UID:        uid,
		Kind:       in.Kind,
		Status:     "created",
		Candidates: advisory,
	})
}

// dedupGate runs the create-time dedup matcher and returns at most one of two
// outcomes for Invoke:
//
//   - decision != nil: a terminal status (reused / duplicate_candidates) to
//     return to the caller verbatim — the caller did not force and the matcher
//     found a blocking match.
//   - advisory != nil: near-match candidates to attach to a fresh mint (a forced
//     create, or a sub-reuse-threshold advisory band).
//
// A nil matcher (degraded boot) or a matcher error both yield (nil, nil): dedup
// is best-effort and an infra fault must never block a write. The fail-open path
// is logged at debug so the silent degradation is observable (a spike there means
// dedup is effectively off and duplicates can slip back in). Pulling this out of
// Invoke keeps Invoke's cyclomatic complexity under the gocyclo budget.
func (c *CreateEntity) dedupGate(ctx context.Context, in createEntityInput) (*createEntityOutput, []entityCandidate) {
	if c.matcher == nil {
		return nil, nil
	}
	decision, cands, derr := c.dedupCheck(ctx, in.DisplayName, in.Kind, in.Identifiers)
	if derr != nil {
		if c.logger != nil {
			c.logger.Debug("createEntity: dedup matcher error, allowing write (fail-open, no dedup)",
				zap.String("displayName", in.DisplayName),
				zap.String("kind", in.Kind),
				zap.Error(derr))
		}
		return nil, nil
	}
	if !in.Force && decision != nil {
		return decision, nil
	}
	return nil, cands
}

// dedupCheck runs the create-time duplicate gate against the shared matcher.
// It returns a (decision, candidates) pair:
//
//   - decision != nil → the action to take when the caller did NOT force: status
//     "reused" (an identifier match — a deliberate anchor, so reuse is confident)
//     or status "duplicate_candidates" (an exact-name or >= reuse-threshold
//     match; the caller must reuse a candidate, merge, or retry with force).
//   - decision == nil → no blocking match; mint a fresh entity.
//   - candidates is ALWAYS the full set of near/identical matches found,
//     independent of the decision. The caller attaches them as advisory on a
//     plain create AND on a forced mint, so a forced fork still reports what it
//     is duplicating; only the reuse/refuse decision is suppressed under force.
//
// kind is the already-resolved canonical kind. Any error is the caller's signal
// to degrade to an unconditional mint (never block a write on an infra fault).
func (c *CreateEntity) dedupCheck(ctx context.Context, displayName, kind string, ids []createEntityIdentifier) (*createEntityOutput, []entityCandidate, error) {
	// Lane 1: a matching strong identifier is a deliberate anchor → confident reuse.
	for _, id := range ids {
		uid, matchedKind, err := c.matcher.IdentifierMatch(ctx, id.Value, kind)
		if err != nil {
			return nil, nil, err
		}
		if uid == "" {
			continue
		}
		// The strong-id lane is kind-blind: an identifier match can resolve to an
		// entity of a different kind than the caller declared. Report the matched
		// entity's STORED kind (Describe refines it further when available) rather
		// than echoing the caller's kind, so a cross-kind reuse isn't mislabelled.
		reuseKind := matchedKind
		if reuseKind == "" {
			reuseKind = kind
		}
		out := &createEntityOutput{Status: "reused", UID: uid, Name: collectionEntities + "/" + uid, Kind: reuseKind}
		var cands []entityCandidate
		if cand, ok, derr := c.matcher.Describe(ctx, uid); derr == nil && ok {
			out.Kind = cand.Kind
			cands = []entityCandidate{{
				UID: uid, DisplayName: cand.DisplayName, Kind: cand.Kind,
				Similarity: 1.0, Reason: "identifier", RelationCount: cand.RelationCount,
			}}
		}
		return out, cands, nil
	}

	// Lane 2: name matches (exact + embedding KNN), kind-gated, deduped by UID.
	cands, refuse, err := c.nameCandidates(ctx, displayName, kind)
	if err != nil {
		return nil, nil, err
	}
	if len(cands) == 0 {
		return nil, nil, nil
	}
	if refuse {
		return &createEntityOutput{Status: "duplicate_candidates", Candidates: cands}, cands, nil
	}
	return nil, cands, nil
}

// nameCandidates gathers same-kind duplicate candidates for displayName: an
// exact-name hit (similarity 1.0) plus name-embedding neighbours at or above
// the advisory threshold. Refuse is true when any candidate is at or above the
// reuse threshold (an exact-name hit always is), meaning the create must be
// refused rather than minted.
func (c *CreateEntity) nameCandidates(ctx context.Context, displayName, kind string) (cands []entityCandidate, refuse bool, err error) {
	seen := make(map[string]struct{})

	exactUID, err := c.matcher.ExactNameMatch(ctx, displayName, kind)
	if err != nil {
		return nil, false, err
	}
	if exactUID != "" {
		cand, ok, derr := c.matcher.Describe(ctx, exactUID)
		if derr != nil {
			return nil, false, derr
		}
		if ok {
			seen[exactUID] = struct{}{}
			cands = append(cands, entityCandidate{
				UID: exactUID, DisplayName: cand.DisplayName, Kind: cand.Kind,
				Similarity: 1.0, Reason: "exact_name", RelationCount: cand.RelationCount,
			})
			refuse = true
		}
	}

	knn, err := c.matcher.NameKNN(ctx, displayName, kind, 5)
	if err != nil {
		return nil, false, err
	}
	for _, k := range knn {
		if k.Similarity < c.matcher.AdvisoryThreshold {
			continue
		}
		if _, dup := seen[k.UID]; dup {
			continue
		}
		seen[k.UID] = struct{}{}
		count, cerr := c.matcher.CountActiveRelations(ctx, k.UID)
		if cerr != nil {
			return nil, false, cerr
		}
		cands = append(cands, entityCandidate{
			UID: k.UID, DisplayName: k.DisplayName, Kind: k.Kind,
			Similarity: k.Similarity, Reason: k.Reason, RelationCount: count,
		})
		if k.Similarity >= c.matcher.ReuseThreshold {
			refuse = true
		}
	}
	return cands, refuse, nil
}

// resolveKind confirms the target kind exists in the ontology registry and
// returns the canonical registered name to mint under. A typo like
// "user:vehcile" fails fast with E_NOT_FOUND instead of persisting an entity
// under a kind describeType can never find.
//
// It also closes the define->create name mismatch: defineEntityKind/`kinds add`
// auto-namespace a bare name ("vehicle" -> "user:vehicle"), so the most natural
// agent flow — define "vehicle", then createEntity kind "vehicle" — would
// otherwise fail. When a bare (un-namespaced, non-core) name is not registered
// verbatim but its "user:" form is, we resolve to "user:<name>", mirroring the
// mint side. An explicitly namespaced name (any colon) is matched verbatim.
//
// The seven bare core kinds are always accepted as-is. The check is skipped when
// no lister is wired (minimal boot); a registry read error degrades to allow
// the verbatim kind so a transient DB fault never blocks a structured write.
func (c *CreateEntity) resolveKind(ctx context.Context, kind string) (string, error) {
	if c.kinds == nil || isCoreKind(kind) {
		return kind, nil
	}
	kinds, err := c.kinds.RegisteredEntityKinds(ctx)
	if err != nil {
		return kind, nil //nolint:nilerr // documented degrade path: never block a write on a registry read error
	}
	userForm := ""
	if !strings.Contains(kind, ":") {
		userForm = "user:" + kind
	}
	userFormExists := false
	for i := range kinds {
		switch kinds[i].Name {
		case kind:
			return kind, nil // verbatim match wins
		case userForm:
			userFormExists = true
		}
	}
	if userForm != "" && userFormExists {
		return userForm, nil // bare name resolves to its minted user: form
	}
	return "", mcp.NewNotFoundError(
		fmt.Sprintf("entity kind %q is not registered; register it with defineEntityKind first, or check introspect/describeType for the exact name", kind))
}

// validateCreateEntityInput checks the kind pattern, non-empty displayName,
// confidence range, and that properties (when present) is a JSON object. It
// returns the resolved confidence (default 1.0) or an E_VALIDATION error.
func validateCreateEntityInput(in createEntityInput) (float64, error) {
	if !createEntityKindPattern.MatchString(in.Kind) {
		return 0, mcp.NewValidationError(
			fmt.Sprintf("kind %q must be a core kind or a namespaced extension kind (e.g. user:vehicle); register it with defineEntityKind first", in.Kind), nil)
	}
	// `document` is reserved for the vault-note write path (writeDocument /
	// `maz doc write`), which also creates the backing documents-table row, body,
	// and vault path. A bare `document` entity minted here would strand a
	// body-less "ghost document" that resolves as a links_to target yet 404s in
	// every doc-oriented surface (recall doc views, `maz doc show`, the explorer),
	// so the direct create path refuses it — the resolver remaps the same kind on
	// extraction (resolver.reservedExtractionKinds). --batch inherits this guard,
	// so a JSONL file can never mint ghost documents en masse. The kind pattern
	// above already normalised case/whitespace, so an exact match is sufficient;
	// a namespaced "user:document" is a distinct extension kind and is allowed.
	if in.Kind == string(ontology.EntityKindDocument) {
		return 0, mcp.NewValidationError(
			"kind \"document\" is reserved for vault notes — create one with writeDocument (`maz doc write`), not createEntity", nil)
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		return 0, mcp.NewValidationError("displayName is required and must be non-empty (not just whitespace)", nil)
	}
	confidence := createEntityDefaultConfidence
	if in.Confidence != nil {
		if *in.Confidence < 0 || *in.Confidence > 1 {
			return 0, mcp.NewValidationError(
				fmt.Sprintf("confidence must be between 0 and 1, got %v", *in.Confidence), nil)
		}
		confidence = *in.Confidence
	}
	// Properties, when present, must be a JSON object — the kind's propertySchema
	// is an object schema, and a scalar would fail validation downstream with a
	// less actionable error.
	if len(in.Properties) > 0 {
		var probe map[string]any
		if err := json.Unmarshal(in.Properties, &probe); err != nil {
			return 0, mcp.NewValidationError(
				fmt.Sprintf("properties must be a JSON object: %v", err), nil)
		}
	}
	return confidence, nil
}

// buildCreateEntityEvents assembles the KindEntity event plus one
// entityIdentifierAdded event per identifier, returning them together with the
// constructed entity (for its resource name). An empty identifier type/value is
// E_VALIDATION; a marshal failure is E_INTERNAL. The scope (validated by the
// caller, empty for the default) is stamped onto both the entity event and every
// entityIdentifierAdded event so a team-scope mint keeps all its anchor metadata
// in the same scope envelope rather than accidentally falling into personal scope.
func buildCreateEntityEvents(in createEntityInput, uid string, confidence float64, scope kernel.Scope) ([]kernel.Event, ontology.Entity, error) {
	now := time.Now().UTC()
	ent := ontology.Entity{
		Name:             collectionEntities + "/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKind(in.Kind),
		DisplayName:      in.DisplayName,
		Confidence:       confidence,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       now,
		AlternateNames:   in.AlternateNames,
		Properties:       in.Properties,
	}
	idKinds := make([]ontology.IdentifierKind, 0, len(in.Identifiers))
	for _, id := range in.Identifiers {
		if id.Type == "" || id.Value == "" {
			return nil, ontology.Entity{}, mcp.NewValidationError("each identifier requires a non-empty type and value", nil)
		}
		idKind, kerr := identifierKindFor(id.Type)
		if kerr != nil {
			return nil, ontology.Entity{}, mcp.NewValidationError(kerr.Error(), nil)
		}
		idKinds = append(idKinds, idKind)
		ent.Identifiers = append(ent.Identifiers, ontology.Identifier{Type: id.Type, Value: id.Value})
	}

	payload, err := json.Marshal(ent)
	if err != nil {
		return nil, ontology.Entity{}, mcp.NewInternalError(fmt.Errorf("marshal entity: %w", err))
	}
	events := make([]kernel.Event, 0, 1+len(in.Identifiers))
	events = append(events, kernel.Event{
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "mcp/createEntity",
		Confidence: confidence,
		Scope:      scope,
		Payload:    payload,
	})
	// One entityIdentifierAdded event per identifier so the resolver's alias map
	// anchors future mentions to this entity instead of minting a duplicate.
	for i, id := range in.Identifiers {
		idPayload, merr := json.Marshal(ontology.EntityIdentifierAddedEvent{
			EntityUID:  uid,
			Identifier: id.Value,
			Kind:       idKinds[i],
			Source:     ontology.IdentifierSourceUser,
			AddedAt:    now,
		})
		if merr != nil {
			return nil, ontology.Entity{}, mcp.NewInternalError(fmt.Errorf("marshal entityIdentifierAdded: %w", merr))
		}
		events = append(events, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
			Source:     "mcp/createEntity",
			Confidence: 1.0,
			Scope:      scope,
			Payload:    idPayload,
		})
	}
	return events, ent, nil
}

// identifierKindShape validates normalized identifier types against the open
// snake_case pattern the entityIdentifierAdded v0.2.0 schema enforces.
var identifierKindShape = regexp.MustCompile(ontology.IdentifierKindPattern)

// identifierKindFor maps a free-form identifier type onto an IdentifierKind.
// Recognised human/machine surface forms map to their canonical kind; any
// other snake_case type (hrb, lei, tax_id, github_user_id, …) is preserved
// verbatim as its own domain lane — collapsing them into the generic "id"
// kind would merge unrelated identifier namespaces into one (identifier,
// kind) lookup key and let the resolver anchor a mention to the wrong
// entity. A type that does not normalize to snake_case is an error.
func identifierKindFor(t string) (ontology.IdentifierKind, error) {
	norm := strings.ToLower(strings.TrimSpace(t))
	switch norm {
	case "email":
		return ontology.IdentifierKindEmail, nil
	case "username":
		return ontology.IdentifierKindUsername, nil
	case "phone":
		return ontology.IdentifierKindPhone, nil
	case "alias":
		return ontology.IdentifierKindAlias, nil
	case "id":
		return ontology.IdentifierKindID, nil
	}
	if !identifierKindShape.MatchString(norm) {
		return "", fmt.Errorf(
			"identifier type %q must be snake_case (lowercase letter followed by lowercase letters, digits, or underscores), e.g. email, hrb, github_user_id", t)
	}
	return ontology.IdentifierKind(norm), nil
}

// newEntityULID returns a fresh ULID in Crockford base32, matching the CLI's
// entity-minting path and relate's relation UIDs.
func newEntityULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return "", fmt.Errorf("ulid: %w", err)
	}
	return id.String(), nil
}
