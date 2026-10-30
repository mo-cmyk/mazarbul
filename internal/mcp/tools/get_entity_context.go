package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// cardBuilder is the entity-dossier assembly surface getEntityContext consumes.
// Satisfied by *contextbrief.Assembler. Declared as an interface so the tool test
// can drive the plumbing without a live navigator/recall stack.
type cardBuilder interface {
	EntityCard(ctx context.Context, reader contextbrief.CardReader, req contextbrief.CardRequest) (contextbrief.Brief, error)
}

const getEntityContextInputSchema = `{
  "type": "object",
  "properties": {
    "entity":      {"type": "string", "minLength": 1, "description": "Which entity to profile: an entities/{uid} resource name (feed one straight from a recall hit's entities[] / name), a bare uid, a human display name (resolved case-insensitively), or 'self'. An ambiguous name returns E_VALIDATION naming the candidates; an unknown one returns E_NOT_FOUND."},
    "tokenBudget": {"type": "integer", "minimum": 1, "maximum": 100000, "description": "Token budget for the dossier (default 4000). The card never exceeds it; the lowest-priority bullets are dropped behind a marker, then a counts footer is appended."},
    "include":     {"type": "array", "items": {"type": "string", "enum": ["connections", "facts", "mentions"]}, "description": "Optional section filter. Omit for all three sections; pass a subset (e.g. [\"facts\"]) to narrow the card. The entity header and footer always render."},
    "at":          {"type": "string", "format": "date-time", "description": "Valid-time anchor (RFC 3339 UTC): return the Connections and What-I-know rows as they were live at this instant. Defaults to now. Per ADR-0001 the anchor binds the relation + fact sub-queries only; the entity header (identity) is unchanged."},
    "scopes":      {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "required": ["entity"],
  "additionalProperties": false
}`

const getEntityContextOutputSchema = `{
  "type": "object",
  "properties": {
    "markdown": {"type": "string", "description": "The entity dossier as markdown: a header (name · kind · entities/{uid} · resolution), Connections (typed relations as direction-aware predicate fragments), What I know (the entity's scalar facts), Mentioned in (recall hits naming it), and a counts footer '_N relations, M facts · budget X · used Y_'. Load this verbatim instead of issuing get + relations + neighbors + recall + facts separately."},
    "tokens":   {"type": "integer", "description": "Estimated token count of the dossier (always <= budget)."},
    "budget":   {"type": "integer", "description": "The token budget that was applied."},
    "dropped":  {"type": "integer", "description": "How many lower-priority bullets were omitted to fit the budget."},
    "sections": {"type": "array", "description": "Per-section counts of what made it into the dossier.", "items": {"type": "object"}}
  },
  "required": ["markdown", "tokens", "budget"],
  "additionalProperties": false
}`

// entityConflictChecker reports whether a given entity UID is a subject in any
// open supersede conflict (functional slot with more than one open arm). When
// conflicts are present the tool annotates the card with a provenance note so
// agents know the relation data may be inconsistent pending `maz review conflicts`.
// Nil disables the check (test wiring or daemon without a live SQLite handle).
type entityConflictChecker interface {
	HasOpenConflicts(ctx context.Context, entityUID string) (bool, error)
}

// GetEntityContext assembles a single token-budgeted markdown dossier for ONE
// entity — collapsing get + relations + neighbors + recall + facts into one card.
// Deterministic apart from the optional recall (Mentioned-in) lane.
type GetEntityContext struct {
	builder         cardBuilder
	nav             Navigator
	binder          selfBinder
	waiter          BackgroundWaiter
	conflictChecker entityConflictChecker
}

// NewGetEntityContext constructs a GetEntityContext over an entity-card builder
// (the brief Assembler) and the navigator used to resolve the entity argument and
// read its node/edges/facts.
func NewGetEntityContext(builder cardBuilder, nav Navigator) *GetEntityContext {
	return &GetEntityContext{builder: builder, nav: nav}
}

// SetSelfBinder injects the self->principal binder so `getEntityContext self`
// resolves the bound principal after binding (mirrors get/relate/fact). Nil = none.
func (g *GetEntityContext) SetSelfBinder(b selfBinder) { g.binder = b }

// WithBackgroundWaiter installs a BackgroundWaiter consulted before a card whose
// Mentioned-in section is requested: that lane runs the same HybridQuery as
// recall, so without this drain a card built right after a write burst would
// search the derived tables before extraction + resolution have populated them.
// Returns g for chaining; a nil waiter is a silent no-op.
func (g *GetEntityContext) WithBackgroundWaiter(w BackgroundWaiter) *GetEntityContext {
	if w != nil {
		g.waiter = w
	}
	return g
}

// WithConflictChecker installs the checker consulted after the card is built: if
// the entity is a subject in any open supersede conflict a provenance note is
// prepended to the card body so agents know relation data may be inconsistent
// pending `maz review conflicts`. Returns g for chaining; a nil checker is a
// silent no-op.
func (g *GetEntityContext) WithConflictChecker(c entityConflictChecker) *GetEntityContext {
	if c != nil {
		g.conflictChecker = c
	}
	return g
}

// Name implements mcp.Tool.
func (*GetEntityContext) Name() string { return "getEntityContext" }

// Description implements mcp.Tool.
func (*GetEntityContext) Description() string {
	return "Assemble a single token-budgeted markdown dossier for ONE entity, collapsing what otherwise takes five calls (get + relations + neighbors + recall + facts) into one card. The dossier carries a header (name, kind, entities/{uid}, resolution status), Connections (the entity's typed relations as direction-aware predicate fragments), What I know (its scalar facts), Mentioned in (recall hits naming it), and a counts footer. Pass `entity` as an entities/{uid} resource name, a bare uid, a display name (resolved case-insensitively), or `self`. Use `at` (RFC 3339) to time-slice the relations + facts to a past instant; the header is unchanged. The card never exceeds `tokenBudget`; lower-priority bullets are dropped behind a marker. An unknown entity returns E_NOT_FOUND; an ambiguous display name returns E_VALIDATION naming the candidates. Prefer this over many small reads when you need the full picture of one entity."
}

// InputSchema implements mcp.Tool.
func (*GetEntityContext) InputSchema() json.RawMessage {
	return json.RawMessage(getEntityContextInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*GetEntityContext) OutputSchema() json.RawMessage {
	return json.RawMessage(getEntityContextOutputSchema)
}

type getEntityContextInput struct {
	Entity      string   `json:"entity"`
	TokenBudget int      `json:"tokenBudget"`
	Include     []string `json:"include,omitempty"`
	At          *string  `json:"at,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
}

// Invoke implements mcp.Tool. A malformed argument is E_VALIDATION; an unknown
// entity is E_NOT_FOUND; an ambiguous display name is E_VALIDATION — the latter
// two via resolveEntityArg.
func (g *GetEntityContext) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in getEntityContextInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Entity == "" {
		return nil, mcp.NewValidationError(
			"entity is required (entities/{uid}, a bare uid, a display name, or self)", nil)
	}
	// Validate include[] section keys up front, before any read, so an unknown
	// section is a pointed error rather than a silently-empty card.
	if err := validateIncludeSections(in.Include); err != nil {
		return nil, err
	}
	// Parse the valid-time anchor via the shared relations-filter parser so a bad
	// 'at' is the same E_VALIDATION the relations/neighbors/get tools return.
	rf, ferr := relationFilterInput{At: in.At}.toFilter()
	if ferr != nil {
		return nil, ferr
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	// The Mentioned-in lane runs the same HybridQuery as recall, so drain async
	// extraction + resolution first — exactly as getContext/recall do. Gated on
	// mentions being requested; the connections/facts lanes read projections the
	// dispatcher keeps current and need no wait. ctx-respecting: an expired
	// deadline builds a partial card rather than erroring.
	if g.waiter != nil && wantsMentions(in.Include) {
		if werr := g.waiter.WaitForBackgroundWork(ctx); werr != nil {
			logging.FromContext(ctx).Debug(
				"getEntityContext: background drain incomplete, building partial card",
				zap.Error(werr))
		}
	}
	// Resolve the entity arg (entities/{uid} | bare uid | display name | self) to a
	// uid: unknown → E_NOT_FOUND, ambiguous → E_VALIDATION (resolveEntityArg).
	uid, err := resolveEntityArg(ctx, navWithScopes(g.nav, scopeSet), g.binder, in.Entity)
	if err != nil {
		return nil, err
	}
	// Self-identity widening: the reader's own principal node lives in personal
	// (the device-local private scope — always the reader's own, never a
	// cross-scope leak). Widen the card reader to include personal when the
	// resolved entity is that principal so `getEntityContext self --scopes team`
	// never 404s on the header. Gated on the own-principal check, so no other
	// entity's read widens and the fail-closed scope boundary holds for every
	// non-self card. (Mirrors the get tool's self-identity node widening.)
	cardScopes := scopeSet
	if !scopeSet.Contains(kernel.ScopePersonal) && isOwnPrincipalUID(ctx, g.binder, uid) {
		cardScopes = kernel.NewScopeSet(append(scopeSet.Scopes(), kernel.ScopePersonal)...)
	}
	brief, err := g.builder.EntityCard(ctx, navWithScopes(g.nav, cardScopes), contextbrief.CardRequest{
		UID:          uid,
		BudgetTokens: in.TokenBudget,
		Include:      in.Include,
		At:           rf.At,
		Scopes:       scopeSet,
	})
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return nil, mcp.NewNotFoundError(in.Entity + " not found")
		}
		return nil, mcp.NewInternalError(fmt.Errorf("getEntityContext: build card: %w", err))
	}
	// Conflict provenance note: if the entity is involved in an open supersede
	// conflict, prepend a brief warning block so agents see it prominently before
	// the relation bullets.
	g.applyConflictNote(ctx, uid, &brief)
	return marshalTool(brief)
}

// validateIncludeSections returns a validation error if any element of sections
// is not a recognised card section key. Called before any I/O so an unknown
// key surfaces as a pointed E_VALIDATION rather than a silently-empty card.
func validateIncludeSections(sections []string) error {
	for _, s := range sections {
		if !contextbrief.IsCardSection(s) {
			return mcp.NewValidationError(
				fmt.Sprintf("unknown include section %q (allowed: connections, facts, mentions)", s), nil)
		}
	}
	return nil
}

// applyConflictNote prepends a provenance warning to brief.Markdown when uid has
// open supersede conflicts. Best-effort: a checker failure is debug-logged and
// ignored so the card is still returned without the conflict marker.
func (g *GetEntityContext) applyConflictNote(ctx context.Context, uid string, brief *contextbrief.Brief) {
	if g.conflictChecker == nil {
		return
	}
	hasConflict, cherr := g.conflictChecker.HasOpenConflicts(ctx, uid)
	if cherr != nil {
		logging.FromContext(ctx).Debug("getEntityContext: conflict check failed (ignored)",
			zap.String("entity_uid", uid), zap.Error(cherr))
		return
	}
	if hasConflict {
		brief.Markdown = "> **Provenance note:** This entity has open supersede conflicts on one or more functional relations. Relation data may be inconsistent. Run `maz review conflicts` to inspect and resolve.\n\n" + brief.Markdown
	}
}

// wantsMentions reports whether the Mentioned-in (recall) section is in play for
// this request: an empty include[] means all sections, otherwise it must list
// the mentions key. Gates the background drain so a connections/facts-only card
// does not pay the recall wait.
func wantsMentions(include []string) bool {
	if len(include) == 0 {
		return true
	}
	for _, s := range include {
		if s == contextbrief.CardSectionMentions {
			return true
		}
	}
	return false
}
