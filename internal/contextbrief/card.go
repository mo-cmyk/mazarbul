package contextbrief

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// Card section titles, in priority order (highest first). The entity header is
// the title region (rendered by pack), so the packable sections are
// connections → what-I-know → mentioned-in.
const (
	sectionConnections = "Connections"
	sectionKnown       = "What I know"
	sectionMentioned   = "Mentioned in"
)

// Card section keys: the stable wire vocabulary the include[] filter accepts.
// Distinct from the human-facing section titles so the title text can change
// without breaking the filter contract.
const (
	CardSectionConnections = "connections"
	CardSectionFacts       = "facts"
	CardSectionMentions    = "mentions"
)

// Default per-section fetch caps for an entity card. The token budget governs
// the final rendered size; these only bound how much is fetched before packing.
const (
	defaultCardRelationLimit = 50
	defaultCardMentionLimit  = 8
	// A fact value is byte-capped at maxFactValueBytes. Facts render as
	// single-line bullets, so the inline-escape (not a blockquote) is the fence —
	// the same cap+fence shape hitBullet applies to a recall hit.
	maxFactValueBytes = 400
)

// CardReader is the node/edge/fact read surface a single-entity dossier
// consumes — the structured-navigation subset of *navigate.Navigator. The
// caller passes a SCOPE-NARROWED reader (nav.WithScopes) so the dossier's
// Connections and What-I-know sections honor the request's scope span; the
// recall (Mentioned-in) lane is bounded separately via CardRequest.Scopes,
// mirroring how Build splits the document/entity reader from RecallRequest.Scopes.
type CardReader interface {
	GetEntity(ctx context.Context, uid string) (navigate.Entity, error)
	EntityRelations(ctx context.Context, uid string, opts navigate.RelationFilter) ([]navigate.Relation, error)
	FactsForSubject(ctx context.Context, subjectUID string, opts navigate.FactFilter) ([]navigate.Fact, error)
}

// CardRequest configures one entity-dossier build. UID is required (the resolved
// seed); the rest default. The At/AsOf anchors bind the relation + fact
// sub-queries per ADR-0001 — never the timeless entity-identity header row.
type CardRequest struct {
	UID           string          // required: the resolved entity UID (the dossier's seed)
	BudgetTokens  int             // 0 → defaultBudgetTokens
	Include       []string        // optional section filter (CardSection* keys); empty → all sections
	At            *time.Time      // valid-time anchor folded into the relation + fact sub-queries; nil → current truth
	AsOf          *time.Time      // transaction-time anchor folded into the sub-queries; nil → no recording bound
	Scopes        kernel.ScopeSet // scope span bounding the recall (Mentioned-in) lane
	RelationLimit int             // 0 → defaultCardRelationLimit
	MentionLimit  int             // 0 → defaultCardMentionLimit
}

// EntityCard builds a single-entity dossier: an entity header, its Connections
// (typed relations as direction-aware predicate fragments), What I know (its
// open scalar facts), and Mentioned in (recall hits naming it), greedily packed
// by priority into the token budget with a counts footer. It is the dossier
// counterpart of Build — seeded by one entity UID rather than a query — and
// REUSES the brief's budgeting (pack), the shared markdown-injection defenses
// (neutralizeUntrustedBlock / escapeMarkdownText), and sectionStats, so neither
// the packing nor the fence logic is duplicated.
//
// Reader is the SCOPE-NARROWED node/edge/fact surface (the tool passes
// nav.WithScopes); the mentions lane is bounded by req.Scopes. A nil recall lane
// (Assembler built without a Recaller) simply omits the Mentioned-in section,
// exactly as relevantBlocks omits the query lane. The returned Brief never
// exceeds the budget: the body is packed within budget minus a worst-case footer
// reservation, then the (smaller-or-equal) real footer is appended.
//
//nolint:gocritic // hugeParam: CardRequest is a value config DTO (one copy per getEntityContext call), kept by value to mirror Build's Request and keep the cardBuilder interface simple.
func (a *Assembler) EntityCard(ctx context.Context, reader CardReader, req CardRequest) (Brief, error) {
	budget := req.BudgetTokens
	if budget <= 0 {
		budget = defaultBudgetTokens
	}

	entity, err := reader.GetEntity(ctx, req.UID)
	if err != nil {
		// ErrNotFound surfaces verbatim so the tool maps it to E_NOT_FOUND.
		return Brief{}, err
	}

	want := cardSectionFilter(req.Include)
	var (
		blocks    []block
		relations []navigate.Relation
		facts     []navigate.Fact
	)

	if want(CardSectionConnections) {
		relations, err = reader.EntityRelations(ctx, req.UID, navigate.RelationFilter{
			At:    req.At,
			AsOf:  req.AsOf,
			Limit: limitOr(req.RelationLimit, defaultCardRelationLimit),
		})
		if err != nil {
			return Brief{}, fmt.Errorf("contextbrief: card relations: %w", err)
		}
		blocks = append(blocks, connectionBlocks(req.UID, relations)...)
	}

	if want(CardSectionFacts) {
		facts, err = reader.FactsForSubject(ctx, req.UID, navigate.FactFilter{At: req.At, AsOf: req.AsOf})
		if err != nil {
			return Brief{}, fmt.Errorf("contextbrief: card facts: %w", err)
		}
		blocks = append(blocks, factBlocks(facts)...)
	}

	if want(CardSectionMentions) && a.recall != nil && entity.DisplayName != "" {
		mentions, merr := a.cardMentionBlocks(ctx, &entity, &req)
		if merr != nil {
			return Brief{}, merr
		}
		blocks = append(blocks, mentions...)
	}

	header := cardHeader(&entity)
	// Reserve the worst-case footer up front: relation/fact counts and budget are
	// known, and the only varying field (used) is <= budget, so footerText with
	// used == budget is the longest possible footer. Packing the body within
	// budget-footerReserve then appending the (<=reserve) real footer keeps the
	// never-exceed-budget invariant absolute.
	footerReserve := tokensFor(cardFooter(len(relations), len(facts), budget, budget))
	bodyBudget := budget - footerReserve
	if bodyBudget < 0 {
		bodyBudget = 0
	}
	res := pack(header, blocks, bodyBudget)

	// The footer reports its own enclosing total ("used Y"), so its byte length
	// feeds the number it prints — a fixed point. Iterate to convergence (reached
	// in <=2 passes for any realistic budget); the loop bound is a hard backstop.
	used := res.tokens
	footer := ""
	for range 4 {
		footer = cardFooter(len(relations), len(facts), budget, used)
		next := res.tokens + tokensFor(footer)
		if next == used {
			break
		}
		used = next
	}

	return Brief{
		Markdown: res.markdown + footer,
		Tokens:   res.tokens + tokensFor(footer),
		Budget:   budget,
		Dropped:  res.dropped,
		Sections: sectionStats(blocks[:res.included]),
	}, nil
}

// cardHeader renders the dossier's title region: the entity's display name as an
// H1 plus a metadata line (kind · resource name · resolution status). The display
// name is untrusted user data and is inline-escaped; the kind/uid/status are
// controlled vocab. This is the timeless identity row — ADR-0001 exempts it from
// the At/AsOf anchors, so a time-sliced card keeps the same header.
func cardHeader(e *navigate.Entity) string {
	name := e.DisplayName
	if name == "" {
		name = e.UID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", escapeMarkdownText(name))
	meta := make([]string, 0, 3)
	if e.Kind != "" {
		meta = append(meta, escapeMarkdownText(e.Kind))
	}
	// uids are ULIDs / the "self" sentinel / a principal ULID — no markdown
	// metacharacters — so they ride inside the code span verbatim.
	meta = append(meta, "`entities/"+e.UID+"`")
	if e.ResolutionStatus != "" {
		meta = append(meta, escapeMarkdownText(e.ResolutionStatus))
	}
	fmt.Fprintf(&b, "\n_%s_\n", strings.Join(meta, " · "))
	return b.String()
}

// connectionBlocks renders an entity's relations as Connections bullets, each a
// direction-aware predicate fragment pointing at the other endpoint. Relations
// arrive newest-first (EntityRelations ORDER BY create_time DESC); that priority
// order is preserved by the packer.
func connectionBlocks(anchorUID string, rels []navigate.Relation) []block {
	blocks := make([]block, 0, len(rels))
	for i := range rels {
		blocks = append(blocks, block{section: sectionConnections, text: connectionBullet(anchorUID, &rels[i])})
	}
	return blocks
}

// connectionBullet renders one relation relative to the card's entity. The
// predicate is a controlled snake_case identifier rendered as inline code so its
// underscores display verbatim without escaping noise (and markdown
// metacharacters inside a code span are inert); the endpoint display names are
// untrusted user data and are inline-escaped so a name carrying markdown
// metacharacters cannot corrupt the bullet.
func connectionBullet(anchorUID string, r *navigate.Relation) string {
	pred := relPredicate(r.Predicate)
	switch anchorUID {
	case r.SubjectUID:
		// Outgoing: this entity --predicate--> object.
		return fmt.Sprintf("- %s → %s `entities/%s`\n", pred, endpointLabel(r.ObjectName, r.ObjectUID), r.ObjectUID)
	case r.ObjectUID:
		// Incoming: subject --predicate--> this entity.
		return fmt.Sprintf("- %s %s → (this) `entities/%s`\n", endpointLabel(r.SubjectName, r.SubjectUID), pred, r.SubjectUID)
	default:
		// Anchor matches neither endpoint (not expected from EntityRelations);
		// render a neutral subject-predicate-object fragment so no edge is lost.
		return fmt.Sprintf("- %s %s %s\n", endpointLabel(r.SubjectName, r.SubjectUID), pred, endpointLabel(r.ObjectName, r.ObjectUID))
	}
}

// relPredicate renders a relation predicate as an inline code span. Any backtick
// is stripped so a pathological minted predicate cannot break out of the span;
// controlled snake_case predicates render verbatim (works_at, reports_to).
func relPredicate(p string) string {
	return "`" + strings.ReplaceAll(p, "`", "") + "`"
}

// endpointLabel renders a relation endpoint as a bold escaped display name, or
// its resource name when the name is unhydrated (out-of-scope endpoint, or a
// nameless node), so traversal always has a name to follow.
func endpointLabel(name, uid string) string {
	if name == "" {
		return fmt.Sprintf("`entities/%s`", uid)
	}
	return "**" + escapeMarkdownText(name) + "**"
}

// factBlocks renders an entity's scalar facts as What-I-know bullets. Facts
// arrive ordered by state_key (FactsForSubject); that order is preserved.
func factBlocks(facts []navigate.Fact) []block {
	blocks := make([]block, 0, len(facts))
	for i := range facts {
		blocks = append(blocks, block{section: sectionKnown, text: factBullet(&facts[i])})
	}
	return blocks
}

// factBullet renders one fact as "- state_key: value". The state_key is a
// controlled hierarchical slot name and the value is untrusted user data; both
// are neutralized (neutralizeUntrustedBlock) then inline-escaped and the value is
// byte-capped, exactly as hitBullet fences a recall hit — facts are single-line
// bullets, so the inline-escape is the fence.
func factBullet(f *navigate.Fact) string {
	key := escapeMarkdownText(neutralizeUntrustedBlock(f.StateKey))
	val := strings.TrimSpace(f.Value)
	truncated := false
	if len(val) > maxFactValueBytes {
		val = val[:clampRune(val, maxFactValueBytes)]
		truncated = true
	}
	val = escapeMarkdownText(neutralizeUntrustedBlock(val))
	if truncated {
		val += truncationEllipsis
	}
	return fmt.Sprintf("- %s: %s\n", key, val)
}

// cardMentionBlocks runs the recall lane for the entity's display name and
// renders the top hits as Mentioned-in bullets via the shared (injection-safe)
// hitBullet. The lane is bounded to req.Scopes (mirrors relevantBlocks /
// RecallRequest.Scopes). The entity's own bare-entity hit is skipped so the
// section shows where it is TALKED ABOUT, not the node itself.
func (a *Assembler) cardMentionBlocks(ctx context.Context, e *navigate.Entity, req *CardRequest) ([]block, error) {
	limit := limitOr(req.MentionLimit, defaultCardMentionLimit)
	// Drain hits first (with +1 headroom for self-skip) so we can
	// batch-lookup authors in a single round-trip before rendering.
	raw := make([]kernel.Hit, 0, limit+1)
	for hit, err := range a.recall.Recall(ctx, kernel.RecallRequest{
		Query:  e.DisplayName,
		Limit:  limit + 1,
		Scopes: scopeStrings(req.Scopes),
	}) {
		if err != nil {
			return nil, fmt.Errorf("contextbrief: card mentions %q: %w", e.DisplayName, err)
		}
		if mentionIsSelf(&hit, e.UID) {
			continue
		}
		raw = append(raw, hit)
		if len(raw) >= limit {
			break
		}
	}

	// Batch-resolve authors. Best-effort: annotation degrades gracefully.
	var authors map[string]string
	if a.authorLookup != nil {
		uids := make([]string, 0, len(raw))
		for i := range raw {
			if len(raw[i].Citations) > 0 {
				uids = append(uids, raw[i].Citations[0])
			}
		}
		if len(uids) > 0 {
			authors, _ = a.authorLookup.BatchEventAuthors(ctx, uids) //nolint:errcheck // best-effort
		}
	}

	blocks := make([]block, 0, len(raw))
	for i := range raw {
		blocks = append(blocks, block{section: sectionMentioned, text: hitBullet(&raw[i], authors)})
	}
	return blocks, nil
}

// mentionIsSelf reports whether a recall hit is the bare-entity hit for the
// card's own entity (HitTypeEntity whose anchors include the entity), so the
// Mentioned-in section never lists the node as a mention of itself.
func mentionIsSelf(hit *kernel.Hit, uid string) bool {
	if hit.HitType != kernel.HitTypeEntity {
		return false
	}
	for i := range hit.Entities {
		if hit.Entities[i].UID == uid {
			return true
		}
	}
	return false
}

// cardFooter renders the dossier's counts footer: the true relation + fact counts
// (the full fetched sets, independent of any budget trimming, so the agent learns
// the real graph shape even when the rendered bullets were clamped) and the
// budget vs used token accounting. Used is self-referential and is stabilized by
// EntityCard's fixed-point loop.
func cardFooter(relations, facts, budget, used int) string {
	return fmt.Sprintf("\n_%d relations, %d facts · budget %d · used %d_\n", relations, facts, budget, used)
}

// cardSectionFilter returns a predicate reporting whether a card section key is
// requested. An empty include list means all sections; otherwise only the listed
// keys are collected. Unknown keys are rejected at the tool layer (IsCardSection)
// before any read, so this predicate never sees one.
func cardSectionFilter(include []string) func(string) bool {
	if len(include) == 0 {
		return func(string) bool { return true }
	}
	set := make(map[string]struct{}, len(include))
	for _, s := range include {
		set[s] = struct{}{}
	}
	return func(key string) bool {
		_, ok := set[key]
		return ok
	}
}

// IsCardSection reports whether key is a recognized include[] filter value. The
// getEntityContext tool validates each include value against this so an unknown
// section is a pointed E_VALIDATION before the dossier reads anything.
func IsCardSection(key string) bool {
	switch key {
	case CardSectionConnections, CardSectionFacts, CardSectionMentions:
		return true
	default:
		return false
	}
}
