package contextbrief

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Default budget + per-section caps. All overridable on Request; chosen so a
// default brief lands around a few thousand tokens — a working set, not a dump.
const (
	defaultBudgetTokens = 4000
	defaultPinnedLimit  = 20
	defaultRecentLimit  = 10

	// DefaultEntityLimit is the maximum number of entities shown in the "Key
	// entities" brief section. The ranker (when wired) orders by recall count;
	// the fallback reader path orders by mention count (SortByMentions) so the
	// most-referenced entities appear first even on a cold store with no usage
	// data. 15 is generous enough to cover a typical working brain while keeping
	// the section well within the token budget.
	defaultEntityLimit = 15
	defaultRelevant    = 8
	// Caps the "Referenced externally" section (#208).
	defaultReferencedLimit = 10

	// A document's body excerpt is capped at maxDocExcerptBytes so one large
	// pinned note cannot starve every lower-priority section. The full body is
	// one readDocument away; the brief is a pointer-rich working set, not a
	// mirror.
	maxDocExcerptBytes = 1600
	// A relevant-hit excerpt is capped at maxHitTextBytes.
	maxHitTextBytes = 400

	// The truncationEllipsis marker is appended to a byte-capped body, excerpt,
	// or hit that was cut.
	truncationEllipsis = " …"
)

// Section titles, in priority order (highest first).
const (
	sectionOperating  = "Operating instructions"
	sectionPinned     = "Pinned"
	sectionRecent     = "Recently updated"
	sectionEntities   = "Key entities"
	sectionReferenced = "Referenced externally"
	sectionRelevant   = "Relevant"
)

// operatingBudgetFraction caps the "Operating instructions" section at 1/N of
// the global token budget so untrusted preset bodies can never starve the
// pinned/recent/entity sections, even within the global budget. Expressed as a
// fraction (not a fixed token count) so it scales with any requested budget.
//
// 1/3 (not 1/4): at the default 4000-token budget this is ~1333 tokens, enough
// for a couple of full ~2 KiB playbooks (the shipped builtins) to render
// together rather than the lowest-priority one being dropped whole by
// clampSections, while still leaving the majority of the budget for the
// pinned/recent/entity sections.
const operatingBudgetFraction = 3

// operatingInstructionsBudgetTokens caps the active-preset section composed into
// the MCP `initialize` instructions (Channel A, #214). It reuses the SAME
// per-section budget #210 applies to a default getContext brief
// (defaultBudgetTokens / operatingBudgetFraction) so a single source governs the
// "Operating instructions" overflow behavior across both sinks rather than
// re-deriving it. The per-preset maxPresetBodyBytes cap already bounds each body;
// this bounds the aggregate. A full shipped builtin (plus a second compact one)
// renders; a runaway preset stack is clamped — the complete set is always one
// getContext call away. It must exceed a single capped+fenced preset block, or
// activating one preset would drop it whole and surface no operating section.
const operatingInstructionsBudgetTokens = defaultBudgetTokens / operatingBudgetFraction

// Reader is the document/entity read surface the assembler consumes. Satisfied
// by *navigate.Navigator.
type Reader interface {
	ListDocuments(ctx context.Context, opts navigate.DocumentFilter) ([]navigate.Document, error)
	ListEntities(ctx context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error)
}

// Recaller is the optional query lane: when a Request carries a Query, the
// assembler folds in the top recall hits. Satisfied by *kernel.HybridQuery
// (kernel.Query). Nil disables the relevant section.
type Recaller interface {
	Recall(ctx context.Context, req kernel.RecallRequest) iter.Seq2[kernel.Hit, error]
}

// ContextPinReader is the optional steering source (#15): the user's free-text
// pins and corrections for the cold-start brief, read live from the context_pins
// table. Satisfied by *navigate.Navigator (which the per-request scope-bounded
// reader already is in production, so the read is scope-isolated for free).
// Nil disables the steering overlay; the brief renders unchanged.
type ContextPinReader interface {
	ContextPins(ctx context.Context) ([]navigate.ContextPin, error)
}

// Context-pin discriminator values, mirroring the dispatcher's context_pins
// pin_type column (a 'pin' is a steering note; a 'correction' is an overlay).
const (
	pinTypePin        = "pin"
	pinTypeCorrection = "correction"
)

// Request configures one brief build. The zero value is valid: it produces a
// query-less brief at the default budget and caps.
type Request struct {
	Query           string   // optional; enables the Relevant section when non-empty
	BudgetTokens    int      // 0 → defaultBudgetTokens
	PinnedLimit     int      // 0 → defaultPinnedLimit
	RecentLimit     int      // 0 → defaultRecentLimit
	EntityLimit     int      // 0 → defaultEntityLimit
	RelevantLimit   int      // 0 → defaultRelevant
	ReferencedLimit int      // 0 → defaultReferencedLimit
	Scopes          []string // scope span to read; empty → {project,personal} (#206). Unknown scope → error.
	// Since, when non-zero, filters the brief to only surface entities whose
	// createTime is at or after the instant, and documents whose updateTime
	// (or createTime when updateTime is absent) is at or after the instant.
	// The pinned and relevant sections are unaffected. Zero means no filter.
	Since time.Time
}

// SectionStat reports how many items of a section made it into the brief.
type SectionStat struct {
	Section string `json:"section"`
	Items   int    `json:"items"`
}

// Brief is the assembled context brief.
type Brief struct {
	Markdown string        `json:"markdown"`
	Tokens   int           `json:"tokens"`  // estimated tokens, always <= budget
	Budget   int           `json:"budget"`  // the budget that was applied
	Dropped  int           `json:"dropped"` // lower-priority blocks omitted
	Sections []SectionStat `json:"sections"`
}

// selfRelationLookup is the optional interface the Assembler type-asserts the
// reader against to auto-detect the self/principal same_as pair. Satisfied by
// *navigate.Navigator so the production path collapses the pair without an
// explicit WithSelfUIDs call; fakeReader in tests does not implement it.
type selfRelationLookup interface {
	EntityRelations(ctx context.Context, uid string, opts navigate.RelationFilter) ([]navigate.Relation, error)
}

// selfEntityAnchorUID is the fixed, well-known UID of the local self anchor
// entity seeded by migration 0012. Mirrors resolver.SelfEntityUID and
// cli/self.go's selfEntityAnchorUID as a local literal to avoid a cross-package
// import for one constant.
const selfEntityAnchorUID = "self"

// HitAuthorLookup resolves a batch of event UIDs to their author's display
// name for annotating the "Relevant hits" section of a context brief.
// Satisfied by *tools.SQLiteAuthorLookup; declared here so contextbrief does
// not import the tools package. The daemon wires the same concrete type for
// both the recall tool and the brief assembler.
type HitAuthorLookup interface {
	// BatchEventAuthors returns a map from eventUID to the author's display
	// name. Missing keys mean "unknown author". Errors are best-effort: the
	// caller logs and continues with no author annotation rather than failing.
	BatchEventAuthors(ctx context.Context, eventUIDs []string) (map[string]string, error)
}

// Assembler builds context briefs from the unified graph.
type Assembler struct {
	reader  Reader
	recall  Recaller         // optional
	ranker  EntityRanker     // optional; nil → recency ordering for Key entities
	presets PresetReader     // optional; nil → no Operating instructions section
	pins    ContextPinReader // optional; nil → no steering pins/corrections (#15)
	nowText string           // injected generation label; empty → omitted (keeps output deterministic in tests)
	// scopeFor derives a scope-bounded (reader, ranker) for a build. Set by the
	// getContext wiring (WithScopeResolver) so per-request scopes bound the
	// pinned/recent/entity sections; nil for tests / unscoped callers, which use
	// reader/ranker as-is (#206).
	scopeFor func(kernel.ScopeSet) (Reader, EntityRanker)
	// logger surfaces observability signals that have no place in the returned
	// value — notably OperatingInstructions dropping a capped preset block.
	// Nil → zap.NewNop() at use; tests and unscoped callers stay quiet.
	logger *zap.Logger
	// selfAnchorUID and selfPrincipalUID identify the two same_as nodes that
	// represent one person: the fixed local anchor (uid="self") and the portable
	// principal ULID. When both are set and both appear in the "Key entities"
	// candidate list, the anchor row is dropped so the pair renders as ONE entry
	// — exactly as `maz self` collapses them. The principal wins (canonical,
	// synced node). Set by WithSelfUIDs; production auto-detects via a reader
	// type assertion when this field is empty.
	selfAnchorUID    string
	selfPrincipalUID string
	// authorLookup resolves citation event UIDs to their author's display name
	// for hit bullets in the "Relevant" section. Nil means no annotation.
	authorLookup HitAuthorLookup
}

// New constructs an Assembler. Recall may be nil to disable the query lane.
func New(reader Reader, recall Recaller) *Assembler {
	return &Assembler{reader: reader, recall: recall}
}

// WithScopeResolver installs a factory that derives a scope-bounded reader and
// ranker for a build's scope span. The getContext wiring sets it so a brief's
// document/entity sections honor the request's scopes; when unset, Build reads
// through the base reader/ranker unchanged (the test/unscoped path). Returns the
// Assembler for chaining.
func (a *Assembler) WithScopeResolver(f func(kernel.ScopeSet) (Reader, EntityRanker)) *Assembler {
	a.scopeFor = f
	return a
}

// WithEntityRanker installs an EntityRanker that orders the "Key entities"
// section by recall usage rather than creation time. When the ranker is nil
// (or this method is not called) the brief falls back to the existing
// recency ordering (create_time DESC) so a cold store never produces an
// empty or broken entities section. Returns a to allow method chaining.
func (a *Assembler) WithEntityRanker(r EntityRanker) *Assembler {
	a.ranker = r
	return a
}

// WithAuthorLookup installs the author-lookup service used to annotate
// "Relevant hits" bullets with "by <name>" attribution. Returns a for
// chaining. Nil is a no-op (author annotation silently omitted). The daemon
// wires the same SQLiteAuthorLookup it gives to the recall tool.
func (a *Assembler) WithAuthorLookup(l HitAuthorLookup) *Assembler {
	if l != nil {
		a.authorLookup = l
	}
	return a
}

// WithPresetReader installs the active-preset source for the top-priority
// "Operating instructions" section. Nil (or unset) omits the section, keeping
// tests and unscoped callers working unchanged. Returns a for chaining.
func (a *Assembler) WithPresetReader(p PresetReader) *Assembler {
	a.presets = p
	return a
}

// WithContextPinReader installs the steering source (#15) for user pins and
// corrections. Nil (or unset) omits the overlay, keeping tests and unscoped
// callers unchanged. In production the per-request scope-bounded reader already
// implements ContextPinReader, so Build prefers it (scope-isolated reads) and
// this explicit reader is the test/unscoped fallback. Returns a for chaining.
func (a *Assembler) WithContextPinReader(p ContextPinReader) *Assembler {
	a.pins = p
	return a
}

// WithSelfUIDs records the two entity UIDs that represent one person: the fixed
// local self anchor (uid="self") and the portable principal ULID. When both
// appear in the "Key entities" candidate list the anchor row is dropped so the
// pair renders as a single entry — mirroring the collapse `maz self` performs.
// Call this when the caller has already resolved the binding (e.g. via
// config.SelfBinder). When left unset the Assembler auto-detects the pair via a
// reader type assertion on the production navigator. Returns a for chaining.
func (a *Assembler) WithSelfUIDs(anchor, principal string) *Assembler {
	a.selfAnchorUID = anchor
	a.selfPrincipalUID = principal
	return a
}

// WithGeneratedLabel sets a human label rendered under the title (e.g. a
// timestamp). Left empty by default so the output is deterministic for tests and
// byte-stable for the index.md document; callers that want a stamp pass it in.
func (a *Assembler) WithGeneratedLabel(label string) *Assembler {
	a.nowText = label
	return a
}

// WithLogger installs the logger used for assembler observability signals (e.g.
// OperatingInstructions dropping a capped preset block). Nil or unset stays
// silent. Returns a for chaining.
func (a *Assembler) WithLogger(l *zap.Logger) *Assembler {
	a.logger = l
	return a
}

// Build assembles the brief for req, packing sections by priority into the token
// budget and dropping the lowest-priority tail behind a marker.
//
//nolint:gocritic // hugeParam: Request is a value config DTO; one ~110-byte copy per getContext call is immaterial, and a value signature keeps the public Build API and its briefBuilder interface simple.
func (a *Assembler) Build(ctx context.Context, req Request) (Brief, error) {
	budget := req.BudgetTokens
	if budget <= 0 {
		budget = defaultBudgetTokens
	}

	// Resolve the read's scope span: caller's scopes when given (validated),
	// {project,personal} when omitted (#206). The factory (when set) narrows the
	// document/entity sections; the recall lane is bounded via RecallRequest.Scopes.
	scopes := kernel.DefaultReadSet()
	if len(req.Scopes) > 0 {
		parsed, perr := kernel.ParseScopeSet(req.Scopes)
		if perr != nil {
			return Brief{}, fmt.Errorf("contextbrief: %w", perr)
		}
		scopes = parsed
	}
	reader, ranker := a.reader, a.ranker
	if a.scopeFor != nil {
		reader, ranker = a.scopeFor(scopes)
	}

	blocks, err := a.collectBlocks(ctx, &req, reader, ranker, scopes)
	if err != nil {
		return Brief{}, err
	}

	// Clamp per-section budgets before the global greedy pack so a single
	// section (notably the untrusted Operating instructions) cannot monopolize
	// the budget. clampSections only removes blocks, so the global never-exceed
	// invariant in pack stays intact; the two drop counts compose additively.
	kept, sectionDropped := clampSections(blocks, sectionBudgets{
		sectionOperating: budget / operatingBudgetFraction,
	})

	res := pack(a.title(req.Query, req.Since), kept, budget)
	return Brief{
		Markdown: res.markdown,
		Tokens:   res.tokens,
		Budget:   budget,
		Dropped:  res.dropped + sectionDropped,
		Sections: sectionStats(kept[:res.included]),
	}, nil
}

// collectBlocks gathers the prioritized section blocks: pinned → recent →
// entities → relevant.
func (a *Assembler) collectBlocks(ctx context.Context, req *Request, reader Reader, ranker EntityRanker, scopes kernel.ScopeSet) ([]block, error) {
	var blocks []block
	seen := map[string]struct{}{}

	ops, err := a.operatingBlocks(ctx, scopes)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, ops...)

	// Steering pins (#15) lead the Pinned section, winning a budget slot before
	// auto-surfaced documents and entities. corrections is the latest-overlay-per-
	// target map the entity section reads to override a surfaced item's text.
	steerPins, corrections, err := a.contextPinBlocks(ctx, reader, limitOr(req.PinnedLimit, defaultPinnedLimit))
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, steerPins...)

	pinned, err := a.pinnedBlocks(ctx, reader, limitOr(req.PinnedLimit, defaultPinnedLimit), seen)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, pinned...)

	recent, err := a.recentBlocks(ctx, reader, limitOr(req.RecentLimit, defaultRecentLimit), seen, req.Since)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, recent...)

	ents, err := a.entityBlocks(ctx, reader, ranker, limitOr(req.EntityLimit, defaultEntityLimit), req.Since, corrections)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, ents...)

	refs, err := a.referencedBlocks(ctx, reader, limitOr(req.ReferencedLimit, defaultReferencedLimit))
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, refs...)

	rel, err := a.relevantBlocks(ctx, req.Query, scopes, limitOr(req.RelevantLimit, defaultRelevant))
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, rel...)

	return blocks, nil
}

// operatingBlocks gathers the top-priority "Operating instructions" blocks from
// the active-preset reader, one fenced block per active preset (deterministically
// ordered). Nil reader or no active presets → no blocks (the section is absent).
//
// PERSISTENCE NOTE (#210): this section becomes part of Build()'s markdown, and
// `maz context index` persists that markdown to the vault document hot.md, which
// the renderer DOES write to the git-tracked tree. So unlike the presets
// projection lockfile (PresetRow carries no body) and the privacy marker (keyed
// on the presetInstalled event), the verbatim body reaches hot.md. This is an
// accepted deviation from the "body absent from every rendered file" goal: the
// section is scope-gated to personal-inclusive reads (see the daemon's
// presetBriefReader), hot.md is written at personal scope where the operator's
// own data dir / personal remote is the trust boundary, and the body is the
// operator's own installed config. If a stricter invariant is wanted later, omit
// this section from the persisted brief while keeping it in the live getContext
// response.
func (a *Assembler) operatingBlocks(ctx context.Context, scopes kernel.ScopeSet) ([]block, error) {
	if a.presets == nil {
		return nil, nil
	}
	presets, err := a.presets.ActivePresets(ctx, scopes)
	if err != nil {
		return nil, fmt.Errorf("contextbrief: list active presets: %w", err)
	}
	if len(presets) == 0 {
		return nil, nil
	}
	sortPresets(presets)
	blocks := make([]block, 0, len(presets))
	for i := range presets {
		blocks = append(blocks, block{
			section: sectionOperating,
			text:    fenceOperatingInstructions(&presets[i]),
		})
	}
	return blocks, nil
}

// OperatingInstructions renders ONLY the bounded, fenced "Operating instructions"
// preset section for the given scope span — no title, no other brief sections. It
// is the single-source projection that backs Channel A's per-session MCP
// instructions (#214): the same operatingBlocks (sanitize + provenance-fence per
// active preset) the brief uses, clamped by the instructions-sink budget.
//
// Unlike Build, it does NOT default an empty ScopeSet to {project,personal}: the
// scopes are forwarded verbatim to the preset reader, which fails closed unless
// the span includes personal. So an empty/unwired ScopeSet yields "" (fail-closed,
// not full) — keep Channel A's composition fail-closed by TYPE. A nil preset
// reader or no active preset likewise yields "" with a nil error (the degraded
// path when #210 is absent or nothing is installed).
func (a *Assembler) OperatingInstructions(ctx context.Context, scopes kernel.ScopeSet) (string, error) {
	if a.presets == nil {
		return "", nil
	}
	blocks, err := a.operatingBlocks(ctx, scopes)
	if err != nil {
		return "", err
	}
	if len(blocks) == 0 {
		return "", nil
	}
	// Reuse the brief's per-section clamp so the aggregate budget and tail-drop
	// behavior match #210 exactly (the per-preset maxPresetBodyBytes cap already
	// applies inside operatingBlocks). clampSections counts a "## <section>" header
	// once; OperatingInstructions emits no header, so this slightly under-fills the
	// nominal budget — conservative for a hard cap into the system-prompt sink.
	kept, dropped := clampSections(blocks, sectionBudgets{
		sectionOperating: operatingInstructionsBudgetTokens,
	})
	// Surface the silent drop: when the cap cuts a lower-priority active preset an
	// operator who installed N presets sees fewer in the system prompt with no
	// other signal. Logged only when dropped > 0 so the common single-preset path
	// stays quiet. The complete set is always one getContext call away.
	if dropped > 0 {
		a.log().Debug("contextbrief: operating-instructions preset block(s) dropped by budget cap",
			zap.Int("dropped", dropped),
			zap.Int("kept", len(kept)),
			zap.Int("budgetTokens", operatingInstructionsBudgetTokens),
		)
	}
	if len(kept) == 0 {
		return "", nil
	}
	var b strings.Builder
	for i := range kept {
		b.WriteString(kept[i].text)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (a *Assembler) pinnedBlocks(ctx context.Context, reader Reader, limit int, seen map[string]struct{}) ([]block, error) {
	docs, err := reader.ListDocuments(ctx, navigate.DocumentFilter{PinnedOnly: true, IncludeBody: true, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("contextbrief: list pinned: %w", err)
	}
	return docBlocks(sectionPinned, docs, seen), nil
}

func (a *Assembler) recentBlocks(ctx context.Context, reader Reader, limit int, seen map[string]struct{}, since time.Time) ([]block, error) {
	// ListDocuments applies its SQL LIMIT before we dedupe the already-shown
	// pinned documents out, so fetching exactly `limit` rows would let a vault
	// whose newest documents are all pinned yield an empty recent section even
	// when older unpinned documents exist. Over-fetch by the number of docs
	// already seen (the pinned set) so at least `limit` unpinned candidates can
	// survive the dedupe, then cap the rendered blocks back to `limit`.
	docs, err := reader.ListDocuments(ctx, navigate.DocumentFilter{IncludeBody: true, Limit: limit + len(seen)})
	if err != nil {
		return nil, fmt.Errorf("contextbrief: list recent: %w", err)
	}
	if !since.IsZero() {
		docs = filterDocsByUpdateTime(docs, since)
	}
	blocks := docBlocks(sectionRecent, docs, seen)
	if len(blocks) > limit {
		blocks = blocks[:limit]
	}
	return blocks, nil
}

func (a *Assembler) entityBlocks(ctx context.Context, reader Reader, ranker EntityRanker, limit int, since time.Time, corrections map[string]string) ([]block, error) {
	var (
		ents []navigate.Entity
		err  error
	)
	if ranker != nil {
		// Rank by recall usage (recallCount DESC, createTime DESC, UID ASC).
		// StatsRanker falls back to recency automatically on a cold store.
		ents, err = ranker.TopEntities(ctx, limit)
		if err != nil {
			return nil, fmt.Errorf("contextbrief: rank entities: %w", err)
		}
		// A usage ranker only knows entities that have been recalled, so on a
		// brain with little recall history it returns far fewer than `limit`,
		// leaving the orientation brief sparse with most of the token budget
		// unused. Top up with the most-referenced entities (mention-count
		// centrality proxy), keeping the usage-ranked ones first.
		if len(ents) < limit {
			ents, err = a.backfillByMentions(ctx, reader, ents, limit)
			if err != nil {
				return nil, err
			}
		}
	} else {
		// No ranker wired (cold store or stats not available). Order by mention
		// count DESC so the most-referenced entities appear first — a cold brain
		// that has never been recalled still has mention rows from extraction, and
		// they are a reliable proxy for graph centrality. Falls back to
		// create_time DESC when an entity has no mentions (new entity, tie-break).
		ents, err = reader.ListEntities(ctx, navigate.EntityFilter{Limit: limit, SortByMentions: true})
		if err != nil {
			return nil, fmt.Errorf("contextbrief: list entities: %w", err)
		}
	}
	if !since.IsZero() {
		ents = filterEntitiesByCreateTime(ents, since)
	}
	return renderEntityBlocks(ctx, a, reader, ents, corrections), nil
}

// renderEntityBlocks converts a ranked entity slice into brief blocks, skipping
// external_ref entities (handled by referencedBlocks) and collapsing the
// self/principal same_as pair so the anchor never appears as a separate row
// when the principal is also present.
func renderEntityBlocks(ctx context.Context, a *Assembler, reader Reader, ents []navigate.Entity, corrections map[string]string) []block {
	// Collapse the self/principal same_as pair: the local anchor (uid="self")
	// and the portable principal ULID are ONE person joined by a same_as
	// relation. When both appear in the candidate list, drop the anchor so the
	// principal is the sole representative — exactly as `maz self` does.
	anchorUID, _ := a.resolveSelfPair(ctx, reader, ents)

	blocks := make([]block, 0, len(ents))
	for i := range ents {
		if ents[i].Kind == string(ontology.EntityKindExternalRef) {
			// external_ref entities have their own "Referenced externally"
			// section; skip them here so a reference is never listed twice.
			continue
		}
		if anchorUID != "" && ents[i].UID == anchorUID {
			// Self-anchor collapsed into the principal row; skip the duplicate.
			continue
		}
		name := ents[i].DisplayName
		if name == "" {
			name = ents[i].UID
		}
		blocks = append(blocks, block{
			section: sectionEntities,
			text:    entityLine(name, ents[i].Kind, ents[i].UID, corrections),
		})
	}
	return blocks
}

// resolveSelfPair returns the anchor UID and principal UID of the self/same_as
// pair when BOTH appear in the ents slice. It prefers an explicitly injected
// pair (set via WithSelfUIDs) and falls back to a same_as relation lookup on
// the production navigator (type-asserted from reader). Returns ("", "") when
// the pair is not present or only one node appears in ents.
func (a *Assembler) resolveSelfPair(ctx context.Context, reader Reader, ents []navigate.Entity) (anchorUID, principalUID string) {
	present := entityUIDSet(ents)

	anchor := a.selfAnchorUID
	if anchor == "" {
		anchor = selfEntityAnchorUID
	}
	if !present[anchor] {
		return "", ""
	}

	principal := a.selfPrincipalUID
	if principal == "" {
		principal = lookupSameAsPrincipal(ctx, reader, anchor, present)
	}
	if principal == "" || !present[principal] {
		return "", ""
	}
	return anchor, principal
}

// entityUIDSet builds a presence map from the candidate entity slice.
func entityUIDSet(ents []navigate.Entity) map[string]bool {
	m := make(map[string]bool, len(ents))
	for i := range ents {
		m[ents[i].UID] = true
	}
	return m
}

// lookupSameAsPrincipal auto-detects the principal UID by querying the
// same_as relations of the anchor via a selfRelationLookup type assertion.
// Returns "" when the reader does not implement the interface, the query
// fails, or no same_as neighbour appears in present.
func lookupSameAsPrincipal(ctx context.Context, reader Reader, anchor string, present map[string]bool) string {
	rl, ok := reader.(selfRelationLookup)
	if !ok {
		return ""
	}
	rels, err := rl.EntityRelations(ctx, anchor, navigate.RelationFilter{
		Predicates: []string{"same_as"},
		Limit:      -1,
	})
	if err != nil {
		return ""
	}
	for i := range rels {
		other := rels[i].ObjectUID
		if other == anchor {
			other = rels[i].SubjectUID
		}
		if other != anchor && present[other] {
			return other
		}
	}
	return ""
}

// entityLine renders one "Key entities" bullet, applying a user correction
// overlay when one targets this entity's resource name ("entities/{uid}"). The
// overlay (✏️) replaces the surfaced description with the corrected text while
// keeping the entity anchor for traceability; the corrected text is neutralized
// + inline-escaped like every other dynamic value the brief renders.
func entityLine(name, kind, uid string, corrections map[string]string) string {
	if c, ok := corrections["entities/"+uid]; ok {
		return fmt.Sprintf("- **%s** (%s) — `entities/%s` — ✏️ %s\n",
			name, kind, uid, escapeMarkdownText(neutralizeUntrustedBlock(c)))
	}
	return fmt.Sprintf("- **%s** (%s) — `entities/%s`\n", name, kind, uid)
}

// contextPinBlocks reads the user's live steering pins + corrections (#15) and
// returns the pin blocks (for the TOP of the Pinned section) plus the
// latest-overlay-per-target correction map the entity section consumes. The
// read prefers the per-request scope-bounded reader (scope-isolated in
// production) and falls back to an explicitly-injected ContextPinReader; a nil
// reader yields no blocks and an empty map, leaving the brief unchanged.
//
// A correction targeting a pin (TargetRef == pin.UID) overrides that pin's
// rendered text in place — the pin keeps its slot, the text is the latest
// correction — so steering a wrong pin needs no unpin-then-repin.
func (a *Assembler) contextPinBlocks(ctx context.Context, reader Reader, limit int) ([]block, map[string]string, error) {
	// Prefer the per-request reader when it implements ContextPinReader
	// (production — already scope-bounded by Build's scope resolver), else the
	// explicitly-injected reader (tests / unscoped callers), else no overlay.
	cpr := a.pins
	if r, ok := reader.(ContextPinReader); ok {
		cpr = r
	}
	if cpr == nil {
		return nil, nil, nil
	}
	pins, err := cpr.ContextPins(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("contextbrief: list context pins: %w", err)
	}
	corrections := contextCorrections(pins)
	blocks := make([]block, 0, len(pins))
	count := 0
	for i := range pins {
		if pins[i].Type != pinTypePin {
			continue
		}
		if count >= limit {
			break
		}
		text := pins[i].Text
		corrected := false
		if c, ok := corrections[pins[i].UID]; ok {
			text, corrected = c, true
		}
		blocks = append(blocks, block{section: sectionPinned, text: pinBullet(text, corrected)})
		count++
	}
	return blocks, corrections, nil
}

// contextCorrections folds a pin/correction list into a map of TargetRef →
// corrected text, keeping the LATEST correction per target. ContextPins returns
// rows newest-first, so the first occurrence per TargetRef wins; corrections
// with an empty TargetRef (no item to overlay) are skipped.
func contextCorrections(pins []navigate.ContextPin) map[string]string {
	m := make(map[string]string, len(pins))
	for i := range pins {
		if pins[i].Type != pinTypeCorrection || pins[i].TargetRef == "" {
			continue
		}
		if _, seen := m[pins[i].TargetRef]; seen {
			continue
		}
		m[pins[i].TargetRef] = pins[i].Text
	}
	return m
}

// pinBullet renders one steering pin as a 📌 bullet. The user-authored text is
// neutralized + inline-escaped like every other dynamic value the brief renders
// (it cannot forge brief structure), and a corrected pin is tagged so a reader
// knows the text is an override rather than the original note.
func pinBullet(text string, corrected bool) string {
	safe := escapeMarkdownText(neutralizeUntrustedBlock(text))
	if corrected {
		return fmt.Sprintf("- 📌 %s _(corrected)_\n", safe)
	}
	return fmt.Sprintf("- 📌 %s\n", safe)
}

// referencedBlocks gathers the "Referenced externally" section: external_ref
// entities — first-class local pointers to information held in other systems
// (#208) — visible in the brief's scope span. Build narrows the reader to the
// request's scopes, so a personal-private reference never surfaces in a
// project-only brief. Ordered by ListEntities' default (create_time DESC).
func (a *Assembler) referencedBlocks(ctx context.Context, reader Reader, limit int) ([]block, error) {
	ents, err := reader.ListEntities(ctx, navigate.EntityFilter{
		Kind:  string(ontology.EntityKindExternalRef),
		Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("contextbrief: list external references: %w", err)
	}
	blocks := make([]block, 0, len(ents))
	for i := range ents {
		name := ents[i].DisplayName
		if name == "" {
			name = ents[i].UID
		}
		blocks = append(blocks, block{
			section: sectionReferenced,
			text:    fmt.Sprintf("- **%s** — `entities/%s`\n", name, ents[i].UID),
		})
	}
	return blocks, nil
}

// backfillByMentions tops up a short usage-ranked entity list with the
// most-referenced entities (SortByMentions), skipping UIDs already present, so
// the Key-entities brief fills its budget instead of surfacing only the handful
// of entities that happen to carry recall history. The usage-ranked entities
// keep their leading positions; mention-ranked centrality fills the remainder.
func (a *Assembler) backfillByMentions(ctx context.Context, reader Reader, ents []navigate.Entity, limit int) ([]navigate.Entity, error) {
	seen := make(map[string]struct{}, len(ents))
	for i := range ents {
		seen[ents[i].UID] = struct{}{}
	}
	more, err := reader.ListEntities(ctx, navigate.EntityFilter{Limit: limit, SortByMentions: true})
	if err != nil {
		return nil, fmt.Errorf("contextbrief: backfill entities: %w", err)
	}
	for i := range more {
		if len(ents) >= limit {
			break
		}
		if _, dup := seen[more[i].UID]; dup {
			continue
		}
		ents = append(ents, more[i])
		seen[more[i].UID] = struct{}{}
	}
	return ents, nil
}

// scopeStrings renders a ScopeSet as the []string shape RecallRequest.Scopes
// expects. An empty set yields nil, which Recall resolves to its own default —
// but Build always passes a resolved (non-empty) set here.
func scopeStrings(s kernel.ScopeSet) []string {
	scopes := s.Scopes()
	if len(scopes) == 0 {
		return nil
	}
	out := make([]string, len(scopes))
	for i := range scopes {
		out[i] = string(scopes[i])
	}
	return out
}

func (a *Assembler) relevantBlocks(ctx context.Context, query string, scopes kernel.ScopeSet, limit int) ([]block, error) {
	if query == "" || a.recall == nil {
		return nil, nil
	}
	// Drain hits first so we can batch-lookup authors in one round-trip.
	hits := make([]kernel.Hit, 0, limit)
	for hit, err := range a.recall.Recall(ctx, kernel.RecallRequest{Query: query, Limit: limit, Scopes: scopeStrings(scopes)}) {
		if err != nil {
			return nil, fmt.Errorf("contextbrief: recall %q: %w", query, err)
		}
		hits = append(hits, hit)
		if len(hits) >= limit {
			break
		}
	}

	// Batch-resolve authors for the collected hits. Best-effort: a lookup
	// failure degrades to unannotated bullets rather than a broken brief.
	var authors map[string]string
	if a.authorLookup != nil {
		uids := make([]string, 0, len(hits))
		for i := range hits {
			if len(hits[i].Citations) > 0 {
				uids = append(uids, hits[i].Citations[0])
			}
		}
		if len(uids) > 0 {
			authors, _ = a.authorLookup.BatchEventAuthors(ctx, uids) //nolint:errcheck // best-effort; annotation degrades gracefully on error
		}
	}

	section := relevantSection(query)
	blocks := make([]block, 0, len(hits))
	for i := range hits {
		blocks = append(blocks, block{section: section, text: hitBullet(&hits[i], authors)})
	}
	return blocks, nil
}

// docBlocks renders one bullet-plus-excerpt block per document, deduping by UID
// against seen so a pinned document is not repeated in the recent section.
func docBlocks(section string, docs []navigate.Document, seen map[string]struct{}) []block {
	blocks := make([]block, 0, len(docs))
	for i := range docs {
		d := &docs[i]
		if _, dup := seen[d.DocumentUID]; dup {
			continue
		}
		seen[d.DocumentUID] = struct{}{}
		blocks = append(blocks, block{section: section, text: docBullet(d)})
	}
	return blocks
}

// docBullet renders a document as a linked bullet with a capped body excerpt.
func docBullet(d *navigate.Document) string {
	title := d.Title
	if title == "" {
		title = d.Path
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- [%s](%s)", title, d.Path)
	if d.Pinned {
		b.WriteString(" ★")
	}
	b.WriteByte('\n')
	if excerpt := excerptBody(d.Body); excerpt != "" {
		b.WriteString(excerpt)
		b.WriteByte('\n')
	}
	return b.String()
}

// excerptBody returns a fenced, byte-capped, neutralized excerpt of an UNTRUSTED
// document body, or "". The body is byte-capped (rune-safe) on the raw bytes,
// has its per-line structural markers neutralized via the shared
// neutralizeUntrustedBlock codepath, and is blockquote-contained ("  > ") so it
// cannot forge brief structure or break out of the fence — a blockquote has no
// closing delimiter to spoof.
func excerptBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	truncated := false
	if len(trimmed) > maxDocExcerptBytes {
		trimmed = trimmed[:clampRune(trimmed, maxDocExcerptBytes)]
		truncated = true
	}
	// neutralizeUntrustedBlock only rewrites leading line markers and rejoins, so
	// a non-empty input (guaranteed here by the TrimSpace above; the byte cap never
	// empties a non-empty slice) always yields a non-empty result — no post-call
	// empty guard needed.
	trimmed = neutralizeUntrustedBlock(trimmed)
	out := "  > " + strings.ReplaceAll(trimmed, "\n", "\n  > ")
	if truncated {
		out += truncationEllipsis
	}
	return out
}

// hitBullet renders an UNTRUSTED recall hit as an inline bullet with a capped,
// fenced excerpt, source, and optional author attribution. The text is
// byte-capped (rune-safe) on the raw bytes, has its leading structural markers
// neutralized (neutralizeUntrustedBlock), then is inline-escaped
// (escapeMarkdownText escapes ` * _ [ ] \ and collapses newlines to spaces)
// so inline link/emphasis/code injection cannot corrupt the bullet. Hits are
// single-line, so the inline-escape — not a blockquote — is the fence here.
// Authors maps citation-event UID → principal display name; nil is a no-op.
func hitBullet(hit *kernel.Hit, authors map[string]string) string {
	text := strings.TrimSpace(hit.Text)
	truncated := false
	if len(text) > maxHitTextBytes {
		text = text[:clampRune(text, maxHitTextBytes)]
		truncated = true
	}
	text = escapeMarkdownText(neutralizeUntrustedBlock(text))
	if truncated {
		text += truncationEllipsis
	}
	src := hitSource(hit)
	// Resolve author from the first citation UID.
	var authorName string
	if len(authors) > 0 && len(hit.Citations) > 0 {
		authorName = authors[hit.Citations[0]]
	}
	switch {
	case src != "" && authorName != "":
		return fmt.Sprintf("- %s — %s · by %s\n", text, src, escapeMarkdownText(authorName))
	case src != "":
		return fmt.Sprintf("- %s — %s\n", text, src)
	case authorName != "":
		return fmt.Sprintf("- %s · by %s\n", text, escapeMarkdownText(authorName))
	default:
		return fmt.Sprintf("- %s\n", text)
	}
}

// hitSource labels a hit's origin: a vault path›heading for document hits, else
// the source event UID. The path and heading are derived from the user's own
// document body (findNearestHeading) and are inline-escaped so a heading or path
// containing markdown metacharacters cannot corrupt the bullet.
func hitSource(hit *kernel.Hit) string {
	if hit.Source != nil && hit.Source.Path != "" {
		if hit.Source.Heading != "" {
			return escapeMarkdownText(hit.Source.Path) + " › " + escapeMarkdownText(hit.Source.Heading)
		}
		return escapeMarkdownText(hit.Source.Path)
	}
	if len(hit.Citations) > 0 {
		return escapeMarkdownText(hit.Citations[0])
	}
	return ""
}

// relevantSection builds the query-specific heading.
func relevantSection(query string) string {
	q := query
	if len(q) > 40 {
		q = q[:clampRune(q, 40)] + "…"
	}
	return sectionRelevant + " to \"" + q + "\""
}

// title renders the brief's top heading.
func (a *Assembler) title(query string, since time.Time) string {
	var b strings.Builder
	b.WriteString("# Context\n")
	if !since.IsZero() {
		fmt.Fprintf(&b, "\n_Showing entities created and documents updated since %s._\n", since.UTC().Format(time.RFC3339))
	}
	if a.nowText != "" {
		fmt.Fprintf(&b, "\n_Working-set brief generated %s._\n", a.nowText)
	}
	if query != "" {
		fmt.Fprintf(&b, "\n_Query anchor: %q._\n", query)
	}
	return b.String()
}

// sectionStats tallies how many included blocks fall in each section, preserving
// first-seen section order.
func sectionStats(included []block) []SectionStat {
	order := make([]string, 0, 4)
	counts := map[string]int{}
	for i := range included {
		s := included[i].section
		if _, ok := counts[s]; !ok {
			order = append(order, s)
		}
		counts[s]++
	}
	out := make([]SectionStat, 0, len(order))
	for _, s := range order {
		out = append(out, SectionStat{Section: s, Items: counts[s]})
	}
	return out
}

// log returns the assembler's logger, or a nop logger when none was installed,
// so observability call sites never nil-check.
func (a *Assembler) log() *zap.Logger {
	if a.logger == nil {
		return zap.NewNop()
	}
	return a.logger
}

// limitOr returns v when positive, else def.
func limitOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// clampRune returns the largest index <= n that lands on a UTF-8 rune boundary,
// so a byte-cap never splits a multibyte rune.
func clampRune(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return n
}

// parseRFC3339Time parses s as RFC 3339 (with or without sub-second precision),
// returning the zero time when s is empty or unparseable. Used to compare
// entity/document timestamps against a Request.Since cutoff.
func parseRFC3339Time(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// filterEntitiesByCreateTime returns a new slice containing only entities whose
// createTime is at or after since. Entities with an empty or unparseable
// createTime are kept (permissive: when the timestamp is unknown we assume
// the entity may be new).
func filterEntitiesByCreateTime(ents []navigate.Entity, since time.Time) []navigate.Entity {
	out := ents[:0:0]
	for i := range ents {
		ct := parseRFC3339Time(ents[i].CreateTime)
		if ct.IsZero() || !ct.Before(since) {
			out = append(out, ents[i])
		}
	}
	return out
}

// filterDocsByUpdateTime returns a new slice containing only documents whose
// updateTime (or createTime when updateTime is absent) is at or after since.
// Documents with no parseable timestamp are kept (permissive).
func filterDocsByUpdateTime(docs []navigate.Document, since time.Time) []navigate.Document {
	out := docs[:0:0]
	for i := range docs {
		ut := parseRFC3339Time(docs[i].UpdateTime)
		if ut.IsZero() {
			ut = parseRFC3339Time(docs[i].CreateTime)
		}
		if ut.IsZero() || !ut.Before(since) {
			out = append(out, docs[i])
		}
	}
	return out
}
