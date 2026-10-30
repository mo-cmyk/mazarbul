package tools

// share_to_team.go implements the shareToTeam MCP tool: it re-publishes a
// personal-scope record (entity, fact, or relation) as an ordinary team-scoped
// event batch so the record becomes readable by all admitted team members.
//
// Design: RE-PUBLISH, not re-scope. The sealer seals events by scope; the
// personal original is never touched and remains visible only on this device.
// Both copies coexist: the personal original lives under personal scope, the
// team copy under team scope.
//
// Entity dedup: before minting a new team entity the tool checks if the team
// scope already holds an entity with the same display name + kind. Strong
// identifiers are matched if a *entitymatch.Lookup is wired (WithMatcher);
// without it only the exact-name check runs.
//
// Scope stamping: every appended event carries Scope: kernel.ScopeTeam so the
// dispatcher's AuthorizeTeamWrite gate applies. A reader-role caller therefore
// surfaces E_PERMISSION from mapAppendError.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const shareToTeamInputSchema = `{
  "type": "object",
  "properties": {
    "ref": {
      "type": "string",
      "minLength": 1,
      "description": "The record to share. Accepts an AIP resource name (entities/{uid}, facts/{uid}, relations/{uid}) or a bare ULID (treated as entity). Entity names are resolved case-insensitively when not a uid."
    },
    "withFacts": {
      "type": "boolean",
      "description": "Also share the entity's currently-open facts. Only meaningful when ref resolves to an entity. Defaults to false."
    },
    "withRelations": {
      "type": "boolean",
      "description": "Also share the entity's active relations. Only meaningful when ref resolves to an entity. Defaults to false."
    }
  },
  "required": ["ref"],
  "additionalProperties": false
}`

const shareToTeamOutputSchema = `{
  "type": "object",
  "properties": {
    "shared": {
      "type": "array",
      "description": "Per-item share outcomes.",
      "items": {
        "type": "object",
        "properties": {
          "name":     {"type": "string", "description": "AIP resource name of the original personal-scope record."},
          "teamName": {"type": "string", "description": "AIP resource name of the team-scope copy or of the reused existing team entity. Absent when status=skipped."},
          "type":     {"type": "string", "enum": ["entity", "fact", "relation"], "description": "Record kind."},
          "status":   {"type": "string", "enum": ["shared", "reused", "skipped"], "description": "shared = new team-scope event appended; reused = existing team entity matched by name; skipped = an endpoint entity has no team counterpart (share the entity first or use withFacts/withRelations on the entity)."},
          "reason":   {"type": "string", "description": "Set when status=skipped; explains which endpoint is missing a team-scope counterpart."}
        },
        "required": ["name", "type", "status"],
        "additionalProperties": false
      }
    },
    "status": {"type": "string", "enum": ["shared"], "description": "Always \"shared\" on success."}
  },
  "required": ["shared", "status"],
  "additionalProperties": false
}`

// sharedItem is one entry in the shareToTeam output.
type sharedItem struct {
	Name     string `json:"name"`
	TeamName string `json:"teamName,omitempty"` // absent when status=skipped
	Type     string `json:"type"`
	Status   string `json:"status"`           // shared | reused | skipped
	Reason   string `json:"reason,omitempty"` // set when status=skipped
}

type shareToTeamInput struct {
	Ref           string `json:"ref"`
	WithFacts     bool   `json:"withFacts,omitempty"`
	WithRelations bool   `json:"withRelations,omitempty"`
}

type shareToTeamOutput struct {
	Shared []sharedItem `json:"shared"`
	Status string       `json:"status"`
}

// ShareToTeam re-publishes a personal-scope record to the team scope. It
// reads the source record from personal scope and appends team-scoped events
// of the same kinds, so all admitted team members can read the record. The
// personal original is unchanged. Both copies — personal and team — coexist;
// scopes are separate lenses, and team-to-personal downscope is not provided.
type ShareToTeam struct {
	appender Appender
	nav      Navigator
	matcher  *entitymatch.Lookup // optional; nil → name-only dedup
}

// NewShareToTeam constructs a ShareToTeam tool. Nav is used for both
// reading the personal-scope source and checking for existing team-scope
// entities. Matcher may be nil; when provided it enables identifier-based
// dedup against team scope.
func NewShareToTeam(a Appender, nav Navigator) *ShareToTeam {
	return &ShareToTeam{appender: a, nav: nav}
}

// WithMatcher attaches an optional entity-match lookup for identifier-based
// team dedup. When nil (the default) only exact-name dedup runs.
func (s *ShareToTeam) WithMatcher(m *entitymatch.Lookup) *ShareToTeam {
	s.matcher = m
	return s
}

// Name implements mcp.Tool.
func (*ShareToTeam) Name() string { return "shareToTeam" }

// Description implements mcp.Tool.
func (*ShareToTeam) Description() string {
	return "Re-publish a personal-scope record (entity, fact, or relation) to the team scope so all admitted team members can read it. The personal original is unchanged — both copies coexist as separate scope lenses. Requires admitted team membership (else E_PERMISSION). Entities are deduplicated against the team scope by exact name before minting; an existing match returns status=reused. Facts and relations remap their endpoint UIDs to the team-scope counterparts: for a bare fact/relation share, all referenced entities must already be in team scope (else E_VALIDATION); for a subtree share (withFacts/withRelations), relations whose object has no team counterpart are skipped (status=skipped, reason set) rather than appended with dangling references. Use withFacts/withRelations to share an entity subtree in one call."
}

// InputSchema implements mcp.Tool.
func (*ShareToTeam) InputSchema() json.RawMessage { return json.RawMessage(shareToTeamInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ShareToTeam) OutputSchema() json.RawMessage { return json.RawMessage(shareToTeamOutputSchema) }

// Invoke implements mcp.Tool.
func (s *ShareToTeam) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in shareToTeamInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Ref == "" {
		return nil, mcp.NewValidationError("ref is required", nil)
	}

	personalNav := navWithScopes(s.nav, kernel.NewScopeSet(kernel.ScopePersonal))
	teamNav := navWithScopes(s.nav, kernel.NewScopeSet(kernel.ScopeTeam))

	// Determine the record type from the ref.
	var items []sharedItem
	var shareErr error

	if looksLikeAIPResourceName(in.Ref) {
		collection, uid, err := parseResourceName(in.Ref)
		if err != nil {
			return nil, err
		}
		switch collection {
		case collectionFacts:
			item, err := s.shareFact(ctx, uid, personalNav, teamNav, nil)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		case collectionRelations:
			item, err := s.shareRelation(ctx, uid, personalNav, teamNav, nil)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		case collectionEntities:
			items, shareErr = s.shareEntitySubtree(ctx, uid, in, personalNav, teamNav)
			if shareErr != nil {
				return nil, shareErr
			}
		default:
			return nil, mcp.NewValidationError(
				fmt.Sprintf("shareToTeam: unsupported collection %q; only entities, facts, relations are supported", collection), nil)
		}
	} else {
		// Treat as entity ref: bare ULID or display name.
		uid, err := resolveEntityArg(ctx, personalNav, nil, in.Ref)
		if err != nil {
			return nil, err
		}
		items, shareErr = s.shareEntitySubtree(ctx, uid, in, personalNav, teamNav)
		if shareErr != nil {
			return nil, shareErr
		}
	}

	return marshalOutput(shareToTeamOutput{Shared: items, Status: "shared"})
}

// shareEntitySubtree shares an entity and optionally its facts + relations.
// It builds a remap of personal→team UIDs so shareFact/shareRelation can
// rewrite endpoint references to valid team-scope UIDs rather than dangling
// personal UIDs that teammates' devices cannot resolve.
func (s *ShareToTeam) shareEntitySubtree(ctx context.Context, uid string, in shareToTeamInput, personalNav, teamNav Navigator) ([]sharedItem, error) {
	item, err := s.shareEntity(ctx, uid, personalNav, teamNav)
	if err != nil {
		return nil, err
	}
	items := []sharedItem{item}

	// Build the remap: personal entity uid → team-scope uid (minted or reused).
	// TrimPrefix strips the "entities/" collection segment from item.TeamName.
	teamUID := strings.TrimPrefix(item.TeamName, collectionEntities+"/")
	remap := map[string]string{uid: teamUID}

	if in.WithFacts {
		facts, err := personalNav.FactsForSubject(ctx, uid, navigate.FactFilter{})
		if err != nil {
			return nil, readErr("shareToTeam: list facts", err)
		}
		for _, f := range facts {
			fi, ferr := s.shareFact(ctx, f.UID, personalNav, teamNav, remap)
			if ferr != nil {
				return nil, ferr
			}
			items = append(items, fi)
		}
	}

	if in.WithRelations {
		rels, err := personalNav.EntityRelations(ctx, uid, navigate.RelationFilter{})
		if err != nil {
			return nil, readErr("shareToTeam: list relations", err)
		}
		for _, r := range rels {
			ri, rerr := s.shareRelation(ctx, r.UID, personalNav, teamNav, remap)
			if rerr != nil {
				return nil, rerr
			}
			items = append(items, ri)
		}
	}

	return items, nil
}

// shareEntity reads a personal-scope entity and mints a team-scope copy,
// deduplicating against the team scope by identifier then display name.
func (s *ShareToTeam) shareEntity(ctx context.Context, uid string, personalNav, teamNav Navigator) (sharedItem, error) {
	ent, err := personalNav.GetEntity(ctx, uid)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return sharedItem{}, mcp.NewNotFoundError("entities/" + uid + " not found in personal scope")
		}
		return sharedItem{}, readErr("shareToTeam: get entity", err)
	}

	// Dedup: check team scope for an existing entity before minting.
	teamUID, found, ferr := s.findTeamEntity(ctx, uid, ent, personalNav, teamNav)
	if ferr != nil {
		// A dedup read failure must NOT fall through to minting — that would
		// silently create a duplicate team entity when a match may exist.
		return sharedItem{}, readErr("shareToTeam: dedup lookup", ferr)
	}
	if found {
		return sharedItem{
			Name:     collectionEntities + "/" + uid,
			TeamName: collectionEntities + "/" + teamUID,
			Type:     "entity",
			Status:   "reused",
		}, nil
	}

	return s.mintTeamEntity(ctx, uid, ent, personalNav)
}

// findTeamEntity checks whether a team-scope entity already exists that
// corresponds to the personal entity with the given uid + ent. Returns the
// team-scope UID and true on a match; empty and false on a clean miss. A
// non-nil error means the dedup lookup itself failed — the caller must treat
// that as fatal, never as a miss, so a transient read error cannot mint a
// duplicate.
func (s *ShareToTeam) findTeamEntity(ctx context.Context, uid string, ent navigate.Entity, personalNav, teamNav Navigator) (string, bool, error) {
	// Lane 1: strong-identifier match (when matcher wired). Alias kind is a
	// weak signal so it is excluded; only typed identifiers (email, username,
	// phone, domain lanes) are treated as strong.
	if s.matcher != nil {
		if teamUID, ok := s.findTeamEntityByIdentifier(ctx, uid, ent.Kind, personalNav, teamNav); ok {
			return teamUID, true, nil
		}
	}

	// Lane 2: exact-name match in team scope.
	existing, lerr := teamNav.ListEntities(ctx, navigate.EntityFilter{
		NameExact: ent.DisplayName,
		Kind:      ent.Kind,
		Limit:     1,
	})
	if lerr != nil {
		return "", false, lerr
	}
	if len(existing) > 0 {
		return existing[0].UID, true, nil
	}
	return "", false, nil
}

// findTeamEntityByIdentifier iterates the personal entity's strong identifiers
// and returns the first that resolves to a team-scoped entity.
func (s *ShareToTeam) findTeamEntityByIdentifier(ctx context.Context, uid, kind string, personalNav, teamNav Navigator) (string, bool) {
	ids, err := personalNav.EntityIdentifiers(ctx, uid)
	if err != nil {
		return "", false // fail open on read error
	}
	for _, id := range ids {
		if id.Type == string(ontology.IdentifierKindAlias) {
			continue // alias is weak; skip
		}
		mUID, _, merr := s.matcher.IdentifierMatch(ctx, id.Value, kind)
		if merr != nil || mUID == "" {
			continue
		}
		// Verify the matched entity is in team scope.
		if _, teamErr := teamNav.GetEntity(ctx, mUID); teamErr == nil {
			return mUID, true
		}
	}
	return "", false
}

// mintTeamEntity appends a fresh KindEntity event (+ identifier events) to
// team scope and returns the resulting shared item.
func (s *ShareToTeam) mintTeamEntity(ctx context.Context, uid string, ent navigate.Entity, personalNav Navigator) (sharedItem, error) {
	teamUID, uerr := newShareULID()
	if uerr != nil {
		return sharedItem{}, mcp.NewInternalError(fmt.Errorf("shareToTeam: generate uid: %w", uerr))
	}

	teamEnt := ontology.Entity{
		Name:             collectionEntities + "/" + teamUID,
		UID:              teamUID,
		Kind:             ontology.EntityKind(ent.Kind),
		DisplayName:      ent.DisplayName,
		Confidence:       ent.Confidence,
		ResolutionStatus: ontology.ResolutionStatus(ent.ResolutionStatus),
		CreateTime:       time.Now().UTC(),
		Properties:       ent.Properties,
	}

	entPayload, merr := json.Marshal(teamEnt)
	if merr != nil {
		return sharedItem{}, mcp.NewInternalError(fmt.Errorf("shareToTeam: marshal entity: %w", merr))
	}

	events := []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "shared:" + uid,
		Confidence: ent.Confidence,
		Scope:      kernel.ScopeTeam,
		Payload:    entPayload,
	}}

	events = append(events, s.buildIdentifierEvents(ctx, uid, teamUID, personalNav)...)

	if _, appErr := s.appender.Append(ctx, events); appErr != nil {
		if errors.Is(appErr, kernel.ErrInvalidEvent) {
			return sharedItem{}, mcp.NewValidationError(fmt.Sprintf("shareToTeam append entity: %v", appErr), nil)
		}
		return sharedItem{}, mapAppendError("shareToTeam append entity", appErr)
	}

	return sharedItem{
		Name:     collectionEntities + "/" + uid,
		TeamName: collectionEntities + "/" + teamUID,
		Type:     "entity",
		Status:   "shared",
	}, nil
}

// buildIdentifierEvents mirrors a personal entity's identifiers as team-scoped
// entityIdentifierAdded events for the new teamUID. Errors per identifier are
// silently dropped (best-effort; a failed identifier marshal is non-fatal).
func (s *ShareToTeam) buildIdentifierEvents(ctx context.Context, srcUID, teamUID string, personalNav Navigator) []kernel.Event {
	ids, err := personalNav.EntityIdentifiers(ctx, srcUID)
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	events := make([]kernel.Event, 0, len(ids))
	for _, id := range ids {
		idPayload, pidErr := json.Marshal(ontology.EntityIdentifierAddedEvent{
			EntityUID:  teamUID,
			Identifier: id.Value,
			Kind:       ontology.IdentifierKind(id.Type),
			Source:     ontology.IdentifierSourceUser,
			AddedAt:    now,
		})
		if pidErr != nil {
			continue
		}
		events = append(events, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
			Source:     "shared:" + srcUID,
			Confidence: 1.0,
			Scope:      kernel.ScopeTeam,
			Payload:    idPayload,
		})
	}
	return events
}

// shareFact reads a personal-scope fact and appends a team-scoped copy with
// remapped endpoint UIDs. The remap parameter maps personal entity UIDs to team
// entity UIDs for the current subtree pass; nil means a bare (non-subtree) call.
//
// Endpoint resolution:
//   - Subtree call (remap != nil): subject resolved via remap then dedup lanes;
//     if unresolvable the fact is returned as status=skipped.
//   - Bare call (remap == nil): subject must have a team counterpart (else E_VALIDATION).
func (s *ShareToTeam) shareFact(ctx context.Context, uid string, personalNav, teamNav Navigator, remap map[string]string) (sharedItem, error) {
	f, err := personalNav.GetFact(ctx, uid)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return sharedItem{}, mcp.NewNotFoundError("facts/" + uid + " not found in personal scope")
		}
		return sharedItem{}, readErr("shareToTeam: get fact", err)
	}

	// Resolve the subject entity to its team-scope counterpart.
	teamSubjUID, found, rerr := s.resolveEndpoint(ctx, f.SubjectUID, personalNav, teamNav, remap)
	if rerr != nil {
		return sharedItem{}, readErr("shareToTeam: resolve fact subject", rerr)
	}
	if !found {
		if remap != nil {
			// Subtree: skip rather than append a dangling reference.
			return sharedItem{
				Name:   collectionFacts + "/" + uid,
				Type:   "fact",
				Status: "skipped",
				Reason: fmt.Sprintf("subject entity %q has no team-scope counterpart; share the entity first", f.SubjectUID),
			}, nil
		}
		return sharedItem{}, mcp.NewValidationError(
			fmt.Sprintf("shareToTeam: subject entity %q is not in team scope; share the entity first or use withFacts on the entity ref", f.SubjectUID), nil)
	}

	teamUID, uerr := newShareULID()
	if uerr != nil {
		return sharedItem{}, mcp.NewInternalError(fmt.Errorf("shareToTeam: generate uid: %w", uerr))
	}

	eventStart := parseOptionalRFC3339(f.EventStart)

	now := time.Now().UTC()
	factPayload, merr := json.Marshal(ontology.Fact{
		Name:           collectionFacts + "/" + teamUID,
		UID:            teamUID,
		SubjectUID:     teamSubjUID, // remapped to team-scope uid
		StateKey:       f.StateKey,
		Value:          f.Value,
		EventStart:     eventStart,
		CreateTime:     now,
		SourceEventUID: teamUID,
	})
	if merr != nil {
		return sharedItem{}, mcp.NewInternalError(fmt.Errorf("shareToTeam: marshal fact: %w", merr))
	}

	sourceRef := f.SourceEventUID
	if sourceRef == "" {
		sourceRef = uid
	}

	if _, appErr := s.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindFact),
		Source:     "shared:" + sourceRef,
		Confidence: f.Confidence,
		Scope:      kernel.ScopeTeam,
		Payload:    factPayload,
	}}); appErr != nil {
		if errors.Is(appErr, kernel.ErrInvalidEvent) {
			return sharedItem{}, mcp.NewValidationError(fmt.Sprintf("shareToTeam append fact: %v", appErr), nil)
		}
		return sharedItem{}, mapAppendError("shareToTeam append fact", appErr)
	}

	return sharedItem{
		Name:     collectionFacts + "/" + uid,
		TeamName: collectionFacts + "/" + teamUID,
		Type:     "fact",
		Status:   "shared",
	}, nil
}

// shareRelation reads a personal-scope relation and appends a team-scoped copy
// with remapped endpoint UIDs. The remap parameter maps personal entity UIDs to
// team entity UIDs for the current subtree pass; nil means a bare (non-subtree) call.
//
// Endpoint resolution for both subject and object:
//   - Check remap first, then fall back to dedup lanes (resolveEndpoint).
//   - Subtree call (remap != nil): either endpoint unresolvable → status=skipped.
//   - Bare call (remap == nil): either endpoint unresolvable → E_VALIDATION.
func (s *ShareToTeam) shareRelation(ctx context.Context, uid string, personalNav, teamNav Navigator, remap map[string]string) (sharedItem, error) {
	r, err := personalNav.GetRelation(ctx, uid)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return sharedItem{}, mcp.NewNotFoundError("relations/" + uid + " not found in personal scope")
		}
		return sharedItem{}, readErr("shareToTeam: get relation", err)
	}

	teamSubjUID, subjFound, subjErr := s.resolveEndpoint(ctx, r.SubjectUID, personalNav, teamNav, remap)
	if subjErr != nil {
		return sharedItem{}, readErr("shareToTeam: resolve relation subject", subjErr)
	}
	teamObjUID, objFound, objErr := s.resolveEndpoint(ctx, r.ObjectUID, personalNav, teamNav, remap)
	if objErr != nil {
		return sharedItem{}, readErr("shareToTeam: resolve relation object", objErr)
	}

	if !subjFound || !objFound {
		return unresolvedRelationEndpoint(uid, r.SubjectUID, r.ObjectUID, subjFound, objFound, remap != nil)
	}

	teamUID, uerr := newShareULID()
	if uerr != nil {
		return sharedItem{}, mcp.NewInternalError(fmt.Errorf("shareToTeam: generate uid: %w", uerr))
	}

	now := time.Now().UTC()
	relPayload, merr := json.Marshal(ontology.Relation{
		Name:           collectionRelations + "/" + teamUID,
		UID:            teamUID,
		SubjectUID:     teamSubjUID, // remapped to team-scope uid
		Predicate:      r.Predicate,
		ObjectUID:      teamObjUID, // remapped to team-scope uid
		Confidence:     r.Confidence,
		SourceEventUID: teamUID,
		CreateTime:     now,
		ValidFrom:      parseOptionalRFC3339(r.ValidFrom), // preserve the validity window
		ValidUntil:     parseOptionalRFC3339(r.ValidUntil),
		Attributes:     r.Attributes,
		Provenance:     r.Provenance,
	})
	if merr != nil {
		return sharedItem{}, mcp.NewInternalError(fmt.Errorf("shareToTeam: marshal relation: %w", merr))
	}

	sourceRef := r.SourceEventUID
	if sourceRef == "" {
		sourceRef = uid
	}

	if _, appErr := s.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "shared:" + sourceRef,
		Confidence: r.Confidence,
		Scope:      kernel.ScopeTeam,
		Payload:    relPayload,
	}}); appErr != nil {
		if errors.Is(appErr, kernel.ErrInvalidEvent) {
			return sharedItem{}, mcp.NewValidationError(fmt.Sprintf("shareToTeam append relation: %v", appErr), nil)
		}
		return sharedItem{}, mapAppendError("shareToTeam append relation", appErr)
	}

	return sharedItem{
		Name:     collectionRelations + "/" + uid,
		TeamName: collectionRelations + "/" + teamUID,
		Type:     "relation",
		Status:   "shared",
	}, nil
}

// resolveEndpoint maps a personal entity UID to its team-scope counterpart.
// It checks remap first (from an active subtree pass), then falls back to
// resolveTeamUID which runs the same dedup lanes as findTeamEntity. A non-nil
// error means the lookup itself failed (distinct from a clean "no counterpart"
// miss) and must be surfaced rather than treated as a skip.
func (s *ShareToTeam) resolveEndpoint(ctx context.Context, personalUID string, personalNav, teamNav Navigator, remap map[string]string) (string, bool, error) {
	if remap != nil {
		if teamUID, ok := remap[personalUID]; ok {
			return teamUID, true, nil
		}
	}
	return s.resolveTeamUID(ctx, personalUID, personalNav, teamNav)
}

// resolveTeamUID finds the team-scope counterpart of personalUID by reading
// the personal entity and running the same dedup lanes as findTeamEntity
// (identifier match when a matcher is wired, then exact display-name match).
// Returns ("", false, nil) when the entity is genuinely absent or has no team
// counterpart; a non-nil error means a read failed and the caller must not
// treat it as a clean miss.
func (s *ShareToTeam) resolveTeamUID(ctx context.Context, personalUID string, personalNav, teamNav Navigator) (string, bool, error) {
	ent, err := personalNav.GetEntity(ctx, personalUID)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return "", false, nil // genuinely absent → no team counterpart
		}
		return "", false, err // real read error → surface, never mint/skip silently
	}
	return s.findTeamEntity(ctx, personalUID, ent, personalNav, teamNav)
}

// unresolvedRelationEndpoint builds the outcome for a relation whose subject
// or object has no team-scope counterpart. In a subtree pass the relation is
// skipped (status=skipped + reason) rather than appended with a dangling
// reference; a bare share fails with E_VALIDATION naming the missing endpoint.
func unresolvedRelationEndpoint(uid, subjUID, objUID string, subjFound, objFound, subtree bool) (sharedItem, error) {
	if subtree {
		var reason string
		switch {
		case !subjFound && !objFound:
			reason = fmt.Sprintf("subject %q and object %q have no team-scope counterpart", subjUID, objUID)
		case !subjFound:
			reason = fmt.Sprintf("subject entity %q has no team-scope counterpart", subjUID)
		default:
			reason = fmt.Sprintf("object entity %q has no team-scope counterpart", objUID)
		}
		return sharedItem{
			Name:   collectionRelations + "/" + uid,
			Type:   "relation",
			Status: "skipped",
			Reason: reason,
		}, nil
	}
	if !subjFound {
		return sharedItem{}, mcp.NewValidationError(
			fmt.Sprintf("shareToTeam: subject entity %q is not in team scope; share the entity first", subjUID), nil)
	}
	return sharedItem{}, mcp.NewValidationError(
		fmt.Sprintf("shareToTeam: object entity %q is not in team scope; share the entity first", objUID), nil)
}

// newShareULID generates a fresh ULID for a team-scope copy of a record.
func newShareULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return "", fmt.Errorf("ulid: %w", err)
	}
	return id.String(), nil
}

// parseOptionalRFC3339 parses an optional RFC 3339 timestamp string into a
// *time.Time (UTC), returning nil for an empty or unparseable value so a
// shared fact/relation preserves its temporal bounds without ever failing the
// share on malformed source data.
func parseOptionalRFC3339(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
