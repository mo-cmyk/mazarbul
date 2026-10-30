package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// Navigator is the subset of *navigate.Navigator the LLM-free navigation tools
// (get/neighbors/relations/entities/introspect) consume. Declaring it as an
// interface here lets tests drive the projection + error-mapping layer with a
// fake that returns navigate.ErrNotFound, exactly as Recall fakes kernel.Query.
type Navigator interface {
	GetEntity(ctx context.Context, uid string) (navigate.Entity, error)
	GetEpisode(ctx context.Context, uid string) (navigate.Episode, error)
	GetRelation(ctx context.Context, uid string) (navigate.Relation, error)
	EntityRelations(ctx context.Context, uid string, opts navigate.RelationFilter) ([]navigate.Relation, error)
	Neighbors(ctx context.Context, seed string, depth int, opts navigate.RelationFilter) ([]navigate.Neighbor, error)
	ListEntities(ctx context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error)
	CountEntities(ctx context.Context, opts navigate.EntityFilter) (int64, error)
	AggregateEntities(ctx context.Context, spec navigate.AggregateSpec) ([]navigate.GroupCount, error)
	EpisodesCitingEntity(ctx context.Context, entityUID string, limit int) ([]navigate.Episode, error)
	EventProvenance(ctx context.Context, eventUID string) ([]navigate.Provenance, error)
	EntityProvenance(ctx context.Context, entityUID string) ([]navigate.Provenance, error)
	Predicates(ctx context.Context) ([]navigate.Predicate, error)
	EntityKinds(ctx context.Context) ([]navigate.KindCount, error)
	RegisteredEntityKinds(ctx context.Context) ([]navigate.RegisteredEntityKind, error)
	GetFact(ctx context.Context, uid string) (navigate.Fact, error)
	FactsForSubject(ctx context.Context, subjectUID string, opts navigate.FactFilter) ([]navigate.Fact, error)
	EntityIdentifiers(ctx context.Context, uid string) ([]navigate.EntityIdentifier, error)
	GetSourceRef(ctx context.Context, uid string) (navigate.SourceRef, error)
	GetInsight(ctx context.Context, uid string) (navigate.Insight, error)
	GetConsolidatedMemory(ctx context.Context, uid string) (navigate.ConsolidatedMemory, error)
	ListEpisodes(ctx context.Context, opts navigate.EpisodeFilter) ([]navigate.Episode, error)
}

// navWithScopes returns a scope-narrowed view of nav for a single request. It
// type-asserts nav to *navigate.Navigator (the only concrete implementation in
// production) and calls WithScopes; when nav is a test fake that does not carry
// scopes the original nav is returned unchanged. This pattern avoids adding a
// covariant WithScopes method to the Navigator interface (Go interfaces require
// exact return types, and *navigate.Navigator.WithScopes returns
// *navigate.Navigator, not tools.Navigator).
func navWithScopes(nav Navigator, scopes kernel.ScopeSet) Navigator {
	type scopable interface {
		WithScopes(kernel.ScopeSet) *navigate.Navigator
	}
	if s, ok := nav.(scopable); ok {
		return s.WithScopes(scopes)
	}
	return nav
}

// navQueryError maps a Navigator query error to a ToolError. A malformed
// AIP-160 filter (navigate.ErrInvalidFilter) is the caller's mistake → an
// E_VALIDATION carrying the parser's message so the agent can fix the
// expression; a transient DB lock/busy → a retryable E_UNAVAILABLE; anything
// else is an internal failure wrapped under op.
func navQueryError(op string, err error) error {
	if errors.Is(err, navigate.ErrInvalidFilter) {
		return mcp.NewValidationError(err.Error(), nil)
	}
	return readErr(op, err)
}

// readErr maps a read-path failure to a ToolError. An error that is already a
// classified ToolError keeps its code and message verbatim — re-wrapping would
// flatten the code (e.g. E_PERMISSION → E_INTERNAL) and double-prefix op. A
// transient DB lock/busy (the file is briefly held by another connection)
// becomes a retryable E_UNAVAILABLE so the caller knows a retry-with-backoff
// will likely succeed; anything else is an E_INTERNAL wrapped under op. This is
// the read-path analogue of mapAppendError's write-path mapping, so lock
// contention surfaces uniformly across reads and writes (see issue #5).
func readErr(op string, err error) error {
	var te *mcp.ToolError
	if errors.As(err, &te) {
		return te
	}
	if mcp.IsTransientLockErr(err) {
		return mcp.NewUnavailableError(fmt.Sprintf("%s: %v", op, err), err)
	}
	return mcp.NewInternalError(fmt.Errorf("%s: %w", op, err))
}

// Collection names for AIP resource names ("{collection}/{uid}").
const (
	collectionEntities             = "entities"
	collectionEpisodes             = "episodes"
	collectionRelations            = "relations"
	collectionFacts                = "facts"
	collectionInsights             = "insights"
	collectionConsolidatedMemories = "consolidatedMemories"
)

// Edge direction relative to the queried/seed entity, shared by the relations
// listing (projectRelations) and neighbor hops so both surfaces report the same
// wire vocabulary.
const (
	directionOutgoing = "outgoing"
	directionIncoming = "incoming"
)

// Navigation paging caps. MaxNavigateLimit mirrors the 200 ceiling the tool
// JSON schemas advertise; it is re-enforced in the shared input projection
// because the CLI and in-process dispatch paths call Invoke directly, bypassing
// the MCP schema validator. NameSuggestLimit bounds the "did you mean"
// substring scan used only for suggestions after an exact display-name lookup
// has already failed — it never gates the exact match itself.
const (
	maxNavigateLimit = 200
	nameSuggestLimit = 25
)

// parseResourceName splits an AIP resource name "{collection}/{uid}" into its
// parts, validating the collection is one navigate serves and the uid is
// non-empty. Returns an E_VALIDATION ToolError on a malformed name so callers
// can return it verbatim.
func parseResourceName(name string) (collection, uid string, err error) {
	parts := strings.SplitN(name, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", mcp.NewValidationError(
			fmt.Sprintf("name %q must be a resource name of the form entities/{uid}, episodes/{uid}, relations/{uid}, facts/{uid}, insights/{uid}, or consolidatedMemories/{uid}", name), nil)
	}
	if !knownCollection(parts[0]) {
		return "", "", mcp.NewValidationError(
			fmt.Sprintf("unknown collection %q in name %q; expected entities, episodes, relations, facts, insights, or consolidatedMemories", parts[0], name), nil)
	}
	return parts[0], parts[1], nil
}

// knownCollection reports whether c is a recognized AIP collection segment.
func knownCollection(c string) bool {
	switch c {
	case collectionEntities, collectionEpisodes, collectionRelations, collectionFacts,
		collectionInsights, collectionConsolidatedMemories:
		return true
	default:
		return false
	}
}

// looksLikeAIPResourceName reports whether arg should be parsed as an AIP
// resource name ("{collection}/{uid}") rather than resolved as a human display
// name. A bare presence of "/" is NOT sufficient: display names legitimately
// contain slashes (e.g. "Ring Mk 4000 / RFC 1149 Integration", "HQ / home base"),
// so only a string whose segment before the first "/" is a RECOGNIZED collection
// is treated as a resource name. Without this distinction every slashed display
// name short-circuited into parseResourceName and failed with "unknown
// collection", making such entities unreachable by name across get/relate/fact.
func looksLikeAIPResourceName(arg string) bool {
	collection, _, found := strings.Cut(arg, "/")
	return found && knownCollection(collection)
}

// entityUIDFromArg accepts either an entity resource name ("entities/{uid}") —
// the form recall hands back in hit.entities[].name — or a bare uid, and
// returns the uid. A name carrying a non-entity collection is rejected so an
// agent that passes an episode/relation name to a node-walking tool gets a
// clear error instead of a silent empty result.
func entityUIDFromArg(arg string) (string, error) {
	if arg == "" {
		return "", mcp.NewValidationError("entity is required (entities/{uid} or a bare uid)", nil)
	}
	if !strings.Contains(arg, "/") {
		return arg, nil // bare uid
	}
	collection, uid, err := parseResourceName(arg)
	if err != nil {
		return "", err
	}
	if collection != collectionEntities {
		return "", mcp.NewValidationError(
			fmt.Sprintf("expected an entities/{uid} name, got %q", arg), nil)
	}
	return uid, nil
}

// resolveEntityArg turns the entity/seed argument the node-walking tools accept
// into a concrete entity UID. It is the forgiving entry point for
// anchor-then-traverse: it accepts an entities/{uid} resource name (what recall
// returns in hit.entities[]), a bare uid, OR a human display name. A bare token
// that matches no existing uid is resolved case-insensitively against display
// names, so an agent that types "Merry Brandybuck" rather than pasting her uid still
// reaches her graph; an unknown or ambiguous name returns a pointed ToolError
// instead of the silent empty result a name-treated-as-uid used to yield.
//
// This is the version without a kind hint; it delegates to resolveEntityArgKind
// with an empty kind. Used by all callers that do not expose a kind field
// (neighbors, relations, canvas, unrelate, fact, relate).
func resolveEntityArg(ctx context.Context, nav Navigator, binder selfBinder, arg string) (string, error) {
	return resolveEntityArgKind(ctx, nav, binder, arg, "")
}

// resolveEntityArgKind is resolveEntityArg extended with an optional kind hint.
// When kindHint is non-empty the display-name resolution lane narrows its query
// to entities of that kind, so `get --kind document "Mithrilmail"` selects the
// document even when a product with the same name exists.
//
// This is the (uid, error) wrapper for all callers except the get tool; use
// resolveEntityArgKindShadow when shadow annotation is needed.
func resolveEntityArgKind(ctx context.Context, nav Navigator, binder selfBinder, arg, kindHint string) (string, error) {
	uid, _, err := resolveEntityArgKindShadow(ctx, nav, binder, arg, kindHint)
	return uid, err
}

// resolveEntityArgKindShadow is the shadow-returning variant of resolveEntityArgKind.
// It returns the resolved UID plus any document entities that were present under
// the same display name but passed over in favour of a non-document winner. The
// shadow slice is non-nil only when a display-name lookup found such documents;
// it is nil for resource-name / bare-uid lookups where no shadow signal exists.
// Called exclusively by the get tool so it can annotate the response.
func resolveEntityArgKindShadow(ctx context.Context, nav Navigator, binder selfBinder, arg, kindHint string) (string, []shadowedDocRef, error) {
	// Resource names and the empty arg go through the strict parser:
	// entities/{uid} → uid; a non-entity collection or "" → E_VALIDATION. A
	// slashed DISPLAY name (prefix is not a known collection) is NOT a resource
	// name and falls through to name resolution below.
	if arg == "" || looksLikeAIPResourceName(arg) {
		uid, err := entityUIDFromArg(arg)
		if err != nil {
			return "", nil, err
		}
		// Read-side self-rebind: after a principal is bound, first-person data
		// lives under the principal UID, so a read for entities/self must follow
		// the binding (the seeded "self" entity is left behind, factless). nil
		// binder / unbound device → "self" passes through unchanged.
		return rebindSelf(ctx, binder, uid), nil, nil
	}
	// Bare token: rebind a literal "self" to the bound principal first, so the
	// existence check below finds the principal entity rather than the stale
	// seeded sentinel.
	arg = rebindSelf(ctx, binder, arg)
	// The reader's own principal (self) always resolves, even when the requested
	// scope set excludes the scope it lives in (personal): it is the reader's
	// identity, not scoped project/team data, and personal is the device-local
	// private scope — always the reader's own, never a cross-scope leak. Edges on
	// it stay filtered to the requested scope by the caller's scoped nav, so
	// `relations self --scope team` traverses only team edges. Mirrors the get
	// tool's self-identity node widening.
	if isOwnPrincipalUID(ctx, binder, arg) {
		return arg, nil, nil
	}
	// An existing uid wins (works for any uid format — ULID in prod, opaque in
	// tests — and never makes a real uid pay for a name scan).
	if _, err := nav.GetEntity(ctx, arg); err == nil {
		return arg, nil, nil
	} else if !errors.Is(err, navigate.ErrNotFound) {
		return "", nil, readErr(fmt.Sprintf("resolve entity %q", arg), err)
	}
	// Not a known uid — treat the token as a display name.
	return resolveEntityByNameResult(ctx, nav, arg, kindHint)
}

// isOwnPrincipalUID reports whether uid is the reader's own bound principal —
// the "self" identity. Only this entity is resolvable outside the requested
// scope set (see resolveEntityArg / the get tool's node widening); every other
// uid stays strictly inside the requested scope, so the fail-closed read
// boundary is unchanged for all non-self reads. A nil/unbound binder → false.
func isOwnPrincipalUID(ctx context.Context, binder selfBinder, uid string) bool {
	if binder == nil || uid == "" {
		return false
	}
	principal, ok := binder.SelfPrincipal(ctx)
	return ok && principal == uid
}

// shadowedDocRef is a document entity that exists under the same display name
// as a non-document resolution winner but was not returned — it was shadowed.
// Surfaced in the get tool output so an agent knows a document with that name
// also exists and can fetch it via get(name).
type shadowedDocRef struct {
	// Name is the entities/{uid} resource name — ready to pass to get.
	Name string `json:"name"`
	// UID is the document entity UID.
	UID string `json:"uid"`
}

// resolveEntityByName maps a display name to exactly one entity UID using the
// shared kind-partitioned exact-name resolver. When exactly one non-document
// entity matches (and zero or more document namesakes exist), that entity wins
// and its UID is returned — this is the `get("Mithrilmail")` → product fix. When
// multiple non-document entities share the name an E_VALIDATION error is
// returned with structured AIP-193 candidate details so the agent can
// disambiguate by UID. An optional kindHint narrows the query to that kind.
//
// For callers that need the shadow identity (get.go), use resolveEntityByNameResult
// which also returns the shadowed-document slice.
func resolveEntityByName(ctx context.Context, nav Navigator, name, kindHint string) (string, error) {
	uid, _, err := resolveEntityByNameResult(ctx, nav, name, kindHint)
	return uid, err
}

// resolveEntityByNameResult is the shadow-returning variant of resolveEntityByName.
// It returns the resolved UID plus any document entities that were present under
// the same display name but passed over in favour of a non-document winner.
// The shadow slice is non-nil only when the displayNameResolver path fires and
// ShadowedDocument is true; it is nil for the fallback (list-based) path and
// when no documents share the name.
func resolveEntityByNameResult(ctx context.Context, nav Navigator, name, kindHint string) (string, []shadowedDocRef, error) {
	// Exact resolution via the shared kind-partitioned resolver. The navigator
	// is the concrete *navigate.Navigator so we type-assert to reach
	// ResolveDisplayName; the Navigator interface exposes ListEntities which the
	// concrete method uses internally.
	type displayNameResolver interface {
		ResolveDisplayName(ctx context.Context, name string, mode navigate.ResolveMode, kindHint string) navigate.ResolveResult
	}
	if dnr, ok := nav.(displayNameResolver); ok {
		r := dnr.ResolveDisplayName(ctx, name, navigate.ResolveStrict, kindHint)
		switch r.Status {
		case navigate.ResolvedOK:
			var shadowed []shadowedDocRef
			if r.ShadowedDocument && len(r.ShadowedDocuments) > 0 {
				shadowed = make([]shadowedDocRef, 0, len(r.ShadowedDocuments))
				for _, d := range r.ShadowedDocuments {
					shadowed = append(shadowed, shadowedDocRef{
						Name: collectionEntities + "/" + d.UID,
						UID:  d.UID,
					})
				}
			}
			return r.UID, shadowed, nil
		case navigate.ResolvedAmbiguous:
			details := buildAmbiguityDetails(name, r.Candidates)
			return "", nil, mcp.NewValidationError(
				fmt.Sprintf("%q matches %d entities: %s; pass the specific entities/{uid}",
					name, len(r.Candidates), navigate.CandidateRefs(r.Candidates)),
				details)
		case navigate.ResolvedError:
			// The resolution query itself failed (e.g. a transient DB error).
			// Propagate it rather than silently degrading to the suggestion scan
			// as though the name simply did not exist. readErr maps a transient
			// lock to a retryable E_UNAVAILABLE.
			return "", nil, readErr(fmt.Sprintf("resolve name %q", name), r.Err)
		case navigate.ResolvedNotFound:
			// Fall through to the suggestion scan below.
		}
	} else {
		// Fallback for test fakes that implement Navigator but not
		// displayNameResolver: use the original list-based path. No shadow info
		// is available on this path.
		exact, err := nav.ListEntities(ctx, navigate.EntityFilter{NameExact: name, Kind: kindHint})
		if err != nil {
			return "", nil, mcp.NewInternalError(fmt.Errorf("resolve name %q: %w", name, err))
		}
		switch {
		case len(exact) == 1:
			return exact[0].UID, nil, nil
		case len(exact) > 1:
			details := buildAmbiguityDetails(name, exact)
			return "", nil, mcp.NewValidationError(
				fmt.Sprintf("%q matches %d entities: %s; pass the specific entities/{uid}",
					name, len(exact), navigate.CandidateRefs(exact)),
				details)
		}
	}

	// No exact match — a capped substring scan offers "did you mean" suggestions,
	// or a clean not-found when nothing is even close.
	cands, err := nav.ListEntities(ctx, navigate.EntityFilter{NameLike: name, Limit: nameSuggestLimit})
	if err != nil {
		return "", nil, readErr(fmt.Sprintf("resolve name %q", name), err)
	}
	if len(cands) == 0 {
		return "", nil, mcp.NewNotFoundError(
			fmt.Sprintf("no entity named %q; browse with the entities tool or pass an entities/{uid} from a recall hit's entities[]", name))
	}
	return "", nil, mcp.NewValidationError(
		fmt.Sprintf("no entity exactly named %q; did you mean: %s? pass the specific entities/{uid}", name, navigate.CandidateRefs(cands)), nil)
}

// buildAmbiguityDetails builds an AIP-193 errorInfo-style details payload for
// an ambiguous-name E_VALIDATION error. The shape is:
//
//	{"reason":"AMBIGUOUS_NAME","candidates":[{"name":"entities/<uid>","displayName":"...","kind":"..."},...]}
//
// Candidates are capped at five entries matching candidateRefs display cap.
func buildAmbiguityDetails(_ string, candidates []navigate.Entity) json.RawMessage {
	const maxCandidates = 5
	type candidateJSON struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Kind        string `json:"kind"`
	}
	shown := candidates
	if len(shown) > maxCandidates {
		shown = shown[:maxCandidates]
	}
	cs := make([]candidateJSON, 0, len(shown))
	for _, e := range shown {
		cs = append(cs, candidateJSON{
			Name:        "entities/" + e.UID,
			DisplayName: e.DisplayName,
			Kind:        e.Kind,
		})
	}
	details := struct {
		Reason     string          `json:"reason"`
		Candidates []candidateJSON `json:"candidates"`
	}{
		Reason:     "AMBIGUOUS_NAME",
		Candidates: cs,
	}
	raw, err := json.Marshal(details)
	if err != nil {
		// Marshal of a struct with only string fields cannot fail; this branch
		// is unreachable but satisfies errcheck.
		return nil
	}
	return raw
}

// relationFilterInput is the shared on-the-wire shape for the validity +
// predicate + paging knobs that neighbors and relations both accept. Pointers
// distinguish absent from zero so an explicit includeInactive:false is honored.
type relationFilterInput struct {
	Predicates      []string `json:"predicates,omitempty"`
	At              *string  `json:"at,omitempty"`
	AsOf            *string  `json:"asOf,omitempty"`
	IncludeInactive *bool    `json:"includeInactive,omitempty"`
	Limit           *int     `json:"limit,omitempty"`
}

// toFilter projects the wire input into a navigate.RelationFilter, parsing the
// RFC 3339 time anchors. A bad timestamp is an E_VALIDATION error.
func (in relationFilterInput) toFilter() (navigate.RelationFilter, error) {
	f := navigate.RelationFilter{Predicates: in.Predicates}
	if in.At != nil && *in.At != "" {
		t, err := time.Parse(time.RFC3339Nano, *in.At)
		if err != nil {
			return navigate.RelationFilter{}, mcp.NewValidationError(fmt.Sprintf("parse 'at': %v", err), nil)
		}
		t = t.UTC()
		f.At = &t
	}
	if in.AsOf != nil && *in.AsOf != "" {
		t, err := time.Parse(time.RFC3339Nano, *in.AsOf)
		if err != nil {
			return navigate.RelationFilter{}, mcp.NewValidationError(fmt.Sprintf("parse 'asOf': %v", err), nil)
		}
		t = t.UTC()
		f.AsOf = &t
	}
	if in.IncludeInactive != nil {
		f.IncludeInactive = *in.IncludeInactive
	}
	if in.Limit != nil {
		f.Limit = *in.Limit
		// Re-enforce the schema's [1,200] bounds: the CLI and in-process callers
		// reach this conversion outside the MCP schema validator. A value over
		// the ceiling pulls far more than the contract; a negative value is
		// worse — the navigator reads it as the "no cap" sentinel and returns
		// every row — so normalize anything < 1 to 0, the navigator's default cap.
		switch {
		case f.Limit < 1:
			f.Limit = 0
		case f.Limit > maxNavigateLimit:
			f.Limit = maxNavigateLimit
		}
	}
	return f, nil
}

// --- wire projections: navigate.* records + their AIP resource name ---

// entityWire is a navigate.Entity plus its resource name, so an agent can pipe
// the name straight into a follow-up get/neighbors/relations call.
type entityWire struct {
	Name string `json:"name"`
	navigate.Entity
}

func projectEntity(e navigate.Entity) entityWire {
	return entityWire{Name: collectionEntities + "/" + e.UID, Entity: e}
}

// episodeWire is a navigate.Episode plus its resource name.
type episodeWire struct {
	Name string `json:"name"`
	navigate.Episode
}

func projectEpisode(ep navigate.Episode) episodeWire {
	return episodeWire{Name: collectionEpisodes + "/" + ep.UID, Episode: ep}
}

// relationWire is a navigate.Relation plus its own resource name and the
// resource names of its endpoints, so traversal needs no string surgery.
type relationWire struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Object  string `json:"object"`
	// Direction tags the edge "outgoing" or "incoming" relative to the entity
	// whose relations were listed: outgoing when that entity is the subject,
	// incoming when it is the object. Set only by an entity-anchored listing
	// (projectRelations, used by the relations tool); left empty — and dropped
	// by omitempty — for get(relations/{uid}) and a neighbor's via-edge, which
	// carry no single viewpoint to be relative to. Mirrors the per-hop direction
	// neighbors already exposes so an agent reads who-points-at-whom for a
	// directional predicate (reports_to, manages) without diffing subjectUid
	// against the queried uid itself.
	Direction string `json:"direction,omitempty"`
	navigate.Relation
}

func projectRelation(r navigate.Relation) relationWire {
	return relationWire{
		Name:     collectionRelations + "/" + r.UID,
		Subject:  collectionEntities + "/" + r.SubjectUID,
		Object:   collectionEntities + "/" + r.ObjectUID,
		Relation: r,
	}
}

// projectRelations projects an entity's relation listing into wire shape,
// stamping each row's Direction relative to anchorUID — the entity whose
// relations were requested — so directional predicates are unambiguous without
// the caller comparing subjectUid/objectUid by hand. AnchorUID is the subject
// of an outgoing edge and the object of an incoming one; a row matching neither
// (not expected from EntityRelations) is left untagged.
func projectRelations(anchorUID string, rs []navigate.Relation) []relationWire {
	out := make([]relationWire, 0, len(rs))
	for _, r := range rs {
		w := projectRelation(r)
		switch anchorUID {
		case r.SubjectUID:
			w.Direction = directionOutgoing
		case r.ObjectUID:
			w.Direction = directionIncoming
		}
		out = append(out, w)
	}
	return out
}

// neighborWire is one hydrated hop: the reached entity, the edge it was reached
// through, the direction relative to the seed, and the hop distance.
type neighborWire struct {
	Entity    entityWire   `json:"entity"`
	Via       relationWire `json:"via"`
	Direction string       `json:"direction"`
	Hops      int          `json:"hops"`
}

func projectNeighbors(ns []navigate.Neighbor) []neighborWire {
	out := make([]neighborWire, 0, len(ns))
	for _, nb := range ns {
		out = append(out, neighborWire{
			Entity:    projectEntity(nb.Entity),
			Via:       projectRelation(nb.Via),
			Direction: nb.Direction,
			Hops:      nb.Hops,
		})
	}
	return out
}
