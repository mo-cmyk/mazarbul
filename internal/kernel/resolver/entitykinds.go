package resolver

// entitykinds.go implements the entity-kind vocabulary auto-registration
// pipeline. It mirrors the proven autoRegisterPredicate / predicateLexicon
// pattern: a curated static lexicon handles the common cases with zero LLM
// calls, and genuinely novel kinds fall through to the entityKindClassify-v1
// prompt.
//
// Routing matrix by mode × confidence:
//   - permissive + high-confidence  → EntityKindRegisteredEvent, Status=""  (active immediately)
//   - permissive + low-confidence   → EntityKindRegisteredEvent, Status="proposed" (review queue)
//   - governed   + any confidence   → EntityKindRegisteredEvent, Status="proposed" (review queue)
//
// The entity's kind string is preserved in all paths so nothing blocks
// ingestion. Proposed kinds are NOT added to the KindRegistry cache and do not
// enter the active taxonomy until explicitly approved via `maz schema review`.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// maxKindMintsPerIngest caps how many novel entity kinds the resolver may
// register in a single Resolve call. Beyond this cap the kind string is
// left as-is without registration. This prevents a runaway extractor from
// minting hundreds of junk kinds in one burst.
const maxKindMintsPerIngest = 5

// kindClassifyConfidenceThreshold is the minimum LLM confidence for a "mint"
// action to be auto-registered. Below this value the kind is enqueued for
// human review and the entity still writes with its novel kind string.
// Picked to match the conservative spirit of the entity resolution thresholds
// (SummaryGrayLow = 0.78 is the lower bound there; 0.6 gives the LLM more
// room on a taxonomy question while still filtering low-confidence mints).
const kindClassifyConfidenceThreshold = 0.6

// kindLexiconEntryType distinguishes alias-map entries (which rewrite the
// incoming kind to a canonical core kind without registration) from new-kind
// entries (which register an EntityKindRegisteredEvent on first use).
type kindLexiconEntryType int

const (
	// KindLexiconAlias means the incoming kind should be rewritten to the
	// canonical target with no event appended.
	kindLexiconAlias kindLexiconEntryType = iota

	// KindLexiconNew means the incoming kind is worth registering immediately
	// without an LLM call, using the bundled displayName and description.
	kindLexiconNew
)

// kindLexiconEntry is one curated entry in entityKindLexicon.
type kindLexiconEntry struct {
	// entryType distinguishes alias entries from new-kind entries.
	entryType kindLexiconEntryType

	// target is the canonical kind name for alias entries, the normalised
	// snake_case name for new-kind entries.
	target string

	// displayName is the human-readable label, used only for new-kind entries.
	displayName string

	// description is the one-line explanation, used only for new-kind entries.
	description string
}

// entityKindLexicon maps common kind strings to curated entries. Consulted by
// autoRegisterEntityKind BEFORE the LLM, so these classify deterministically
// with zero LLM round-trips.
//
// Alias entries (kindLexiconAlias) map common surface forms onto the six core
// seed kinds. New-kind entries (kindLexiconNew) identify well-understood kinds
// worth registering immediately.
//
//nolint:gochecknoglobals // small static catalog, mirrors the predicate lexicon pattern
var entityKindLexicon = map[string]kindLexiconEntry{
	// Aliases onto person.
	"human":      {entryType: kindLexiconAlias, target: "person"},
	"individual": {entryType: kindLexiconAlias, target: "person"},
	"people":     {entryType: kindLexiconAlias, target: "person"},

	// Aliases onto organization.
	"company":      {entryType: kindLexiconAlias, target: "organization"},
	"firm":         {entryType: kindLexiconAlias, target: "organization"},
	"organisation": {entryType: kindLexiconAlias, target: "organization"},
	"corp":         {entryType: kindLexiconAlias, target: "organization"},
	"corporation":  {entryType: kindLexiconAlias, target: "organization"},
	"startup":      {entryType: kindLexiconAlias, target: "organization"},
	"institution":  {entryType: kindLexiconAlias, target: "organization"},

	// Aliases onto place.
	"location": {entryType: kindLexiconAlias, target: "place"},
	"city":     {entryType: kindLexiconAlias, target: "place"},
	"country":  {entryType: kindLexiconAlias, target: "place"},
	"region":   {entryType: kindLexiconAlias, target: "place"},
	"venue":    {entryType: kindLexiconAlias, target: "place"},

	// Document-ish surface forms collapse onto the topic catch-all, NOT the
	// structural `document` kind. `document` is reserved for vault notes created
	// through the document write path (they own a documents-table row + body +
	// path and render to the vault); an extracted "report"/"file"/"paper" is a
	// concept reference, not a note, so routing it to `document` would mint a
	// body-less "ghost document" that breaks every doc-oriented surface (recall
	// doc views, `maz doc show`, the explorer). See reservedExtractionKinds and
	// remapReservedExtractionKind for the hard backstop on a raw `document` kind.
	"file":    {entryType: kindLexiconAlias, target: "topic"},
	"report":  {entryType: kindLexiconAlias, target: "topic"},
	"article": {entryType: kindLexiconAlias, target: "topic"},
	"paper":   {entryType: kindLexiconAlias, target: "topic"},
	"email":   {entryType: kindLexiconAlias, target: "topic"},
	"note":    {entryType: kindLexiconAlias, target: "topic"},

	// Aliases onto event.
	"meeting":     {entryType: kindLexiconAlias, target: "event"},
	"conference":  {entryType: kindLexiconAlias, target: "event"},
	"appointment": {entryType: kindLexiconAlias, target: "event"},
	"call":        {entryType: kindLexiconAlias, target: "event"},
	"occurrence":  {entryType: kindLexiconAlias, target: "event"},

	// Aliases onto project.
	"initiative": {entryType: kindLexiconAlias, target: "project"},
	"program":    {entryType: kindLexiconAlias, target: "project"},
	"programme":  {entryType: kindLexiconAlias, target: "project"},
	"codebase":   {entryType: kindLexiconAlias, target: "project"},

	// Aliases onto tool. The local extractor (qwen2.5:7b) routinely mis-kinds
	// software / libraries / databases as "organization" or invents a fresh
	// "technology"/"library" kind; collapse the unambiguous technical surface
	// forms onto the curated "tool" kind so a dependency like FTS5 or
	// sqlite-vec is not modelled as a company. (The prompt also steers tech to
	// "tool"; this catches the residue.)
	"technology":           {entryType: kindLexiconAlias, target: "tool"},
	"library":              {entryType: kindLexiconAlias, target: "tool"},
	"framework":            {entryType: kindLexiconAlias, target: "tool"},
	"database":             {entryType: kindLexiconAlias, target: "tool"},
	"software":             {entryType: kindLexiconAlias, target: "tool"},
	"sdk":                  {entryType: kindLexiconAlias, target: "tool"},
	"protocol":             {entryType: kindLexiconAlias, target: "tool"},
	"programming_language": {entryType: kindLexiconAlias, target: "tool"},

	// Aliases onto topic. Small models occasionally emit a primitive/value or
	// workflow-state pseudo-kind (the daemon saw "integer", "tick", "count",
	// "status", "phase") — these are attribute VALUES, never entities. Coerce
	// them onto the "topic" catch-all so they neither mint junk taxonomy nor
	// flood the eventlog warn-accept path, rather than registering "integer" as
	// a first-class entity kind. These mirror the prompt's forbidden-kind list,
	// giving a hard backstop when the model ignores the soft prompt steer.
	"integer":    {entryType: kindLexiconAlias, target: "topic"},
	"number":     {entryType: kindLexiconAlias, target: "topic"},
	"count":      {entryType: kindLexiconAlias, target: "topic"},
	"boolean":    {entryType: kindLexiconAlias, target: "topic"},
	"value":      {entryType: kindLexiconAlias, target: "topic"},
	"metric":     {entryType: kindLexiconAlias, target: "topic"},
	"timestamp":  {entryType: kindLexiconAlias, target: "topic"},
	"percentage": {entryType: kindLexiconAlias, target: "topic"},
	"tick":       {entryType: kindLexiconAlias, target: "topic"},
	"status":     {entryType: kindLexiconAlias, target: "topic"},
	"phase":      {entryType: kindLexiconAlias, target: "topic"},

	// Well-known new kinds worth registering without an LLM call.
	"product": {
		entryType:   kindLexiconNew,
		target:      "product",
		displayName: "Product",
		description: "A commercial or open-source product, software application, or physical good.",
	},
	"deal": {
		entryType:   kindLexiconNew,
		target:      "deal",
		displayName: "Deal",
		description: "A business transaction, investment, acquisition, or commercial agreement.",
	},
	"task": {
		entryType:   kindLexiconNew,
		target:      "task",
		displayName: "Task",
		description: "A discrete unit of work, action item, or to-do with a clear completion state.",
	},
	"vehicle": {
		entryType:   kindLexiconNew,
		target:      "vehicle",
		displayName: "Vehicle",
		description: "A land vehicle such as a car, truck, motorcycle, or bicycle.",
	},
	"vessel": {
		entryType:   kindLexiconNew,
		target:      "vessel",
		displayName: "Vessel",
		description: "A watercraft or aircraft such as a ship, yacht, or aeroplane.",
	},
	"team": {
		entryType:   kindLexiconNew,
		target:      "team",
		displayName: "Team",
		description: "A named group of people working together within an organisation or project.",
	},
	"skill": {
		entryType:   kindLexiconNew,
		target:      "skill",
		displayName: "Skill",
		description: "A technical or professional competency, programming language, or domain expertise.",
	},
	"tool": {
		entryType:   kindLexiconNew,
		target:      "tool",
		displayName: "Tool",
		description: "A software tool, framework, service, or instrument used in a workflow.",
	},
	"topic": {
		entryType:   kindLexiconNew,
		target:      "topic",
		displayName: "Topic",
		description: "A subject area, theme, or concept that structures discourse or knowledge.",
	},
	"animal": {
		entryType:   kindLexiconNew,
		target:      "animal",
		displayName: "Animal",
		description: "A non-human living creature, including pets, livestock, and wildlife.",
	},
}

// coreKinds is the set of the six genesis entity kinds that are always
// registered at boot. AutoRegisterEntityKind is a no-op for these.
//
//nolint:gochecknoglobals // fixed set of core kinds, never mutated
var coreKinds = map[string]struct{}{
	"person":       {},
	"organization": {},
	"place":        {},
	"project":      {},
	"event":        {},
	"document":     {},
}

// reservedExtractionKinds maps a structural entity kind that the extraction
// pipeline must NEVER assign onto the semantic kind it is remapped to.
//
// `document` is the only such kind today. A document entity is a vault note
// created through the document write path: it owns a documents-table row, a
// verbatim body, and a vault path, and it renders to the git tree. Letting the
// extractor stamp `document` on a bare mention (a published spec, a "report", a
// concept) mints a body-less "ghost document" that resolves as a doc-to-doc
// links_to target yet 404s in every doc-oriented surface (recall doc views,
// `maz doc show`, the explorer). Such mentions are concept references, so they
// are remapped to the `topic` catch-all.
//
//nolint:gochecknoglobals // fixed policy table, mirrors coreKinds / entityKindLexicon
var reservedExtractionKinds = map[string]string{
	"document": "topic",
}

// remapReservedExtractionKind enforces reservedExtractionKinds on the
// extraction path. When kind is reserved it returns the remap target and
// ensures that target is registered (so the rewritten kind is never an
// unregistered warn-accept downstream); otherwise it returns kind unchanged.
// The document write path does not flow through the resolver, so authored vault
// notes keep their `document` kind — only extraction-sourced mentions are
// rewritten.
func (r *Resolver) remapReservedExtractionKind(ctx context.Context, kind string, counter *ingestMintCounter) string {
	target, reserved := reservedExtractionKinds[normaliseKindName(kind)]
	if !reserved {
		return kind
	}
	r.logger.Debug("Resolver: remapped reserved extraction kind",
		zap.String("from", kind),
		zap.String("to", target),
	)
	r.ensureAliasTargetRegistered(ctx, target, counter)
	return target
}

// kindLexiconLookup returns the curated entry for kind, if any. Lookup is
// case-insensitive on the trimmed kind string.
func kindLexiconLookup(kind string) (kindLexiconEntry, bool) {
	e, ok := entityKindLexicon[strings.ToLower(strings.TrimSpace(kind))]
	return e, ok
}

// snakeCaseKindPattern validates that a minted kind name is a valid bare
// snake_case identifier (lowercase letters, digits, underscores, starting
// with a letter). Namespaced kinds (user:foo) are NOT auto-minted by the
// resolver; the user provenance prefix is not added here.
//

var snakeCaseKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// normaliseKindName lowercases and trims a kind string. Does not namespace it.
func normaliseKindName(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

// KindRegistry is an in-memory cache of registered entity kinds backed by
// the entity_kinds table. It mirrors the predicate Registry pattern.
// Concurrent reads are safe; Register holds the write lock for the full
// operation.
type KindRegistry struct {
	mu    sync.RWMutex
	cache map[string]struct{} // registered kind names
	db    *sql.DB             // read-only pool
}

// NewKindRegistry opens a KindRegistry and loads all registered kind names
// from the entity_kinds table.
func NewKindRegistry(ctx context.Context, db *sql.DB) (*KindRegistry, error) {
	kr := &KindRegistry{
		cache: make(map[string]struct{}),
		db:    db,
	}
	if err := kr.load(ctx); err != nil {
		return nil, fmt.Errorf("kind registry: load: %w", err)
	}
	return kr, nil
}

// IsRegistered reports whether kind is present in the registry cache.
func (kr *KindRegistry) IsRegistered(kind string) bool {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	_, ok := kr.cache[kind]
	return ok
}

// MarkRegistered adds kind to the in-memory cache. Called after a successful
// EntityKindRegisteredEvent append to keep the cache consistent with the
// event log without a round-trip.
func (kr *KindRegistry) MarkRegistered(kind string) {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	kr.cache[kind] = struct{}{}
}

// load queries entity_kinds and populates the cache with active kinds only.
// Proposed and rejected kinds are excluded so the resolver does not treat them
// as already registered. Called only from NewKindRegistry while the lock is
// not held.
func (kr *KindRegistry) load(ctx context.Context) error {
	// COALESCE(status,'active') normalises pre-migration rows to active.
	rows, err := kr.db.QueryContext(ctx,
		`SELECT name FROM entity_kinds WHERE COALESCE(status,'active') = 'active'`)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		kr.cache[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate: %w", err)
	}
	return nil
}

// ingestMintCounter is a small per-Resolve-call counter that enforces
// maxKindMintsPerIngest. Construct one per Resolve invocation and pass it
// into autoRegisterEntityKind.
type ingestMintCounter struct {
	n atomic.Int32
}

// tryIncrement returns true and increments if the cap has not been reached.
func (c *ingestMintCounter) tryIncrement() bool {
	for {
		v := c.n.Load()
		if int(v) >= maxKindMintsPerIngest {
			return false
		}
		if c.n.CompareAndSwap(v, v+1) {
			return true
		}
	}
}

// autoRegisterEntityKind classifies an unknown entity kind and either rewrites
// it to a canonical kind, registers a new EntityKindRegisteredEvent, or
// enqueues a pending-review item. Returns the canonical kind to use (may
// differ from the input when the lexicon finds an alias) and an error only
// when a hard failure (e.g. event-log append) justifies surfacing it. LLM
// errors and low-confidence results are handled gracefully: the returned kind
// is the (possibly normalised) input kind and the error is nil.
//
// The caller MUST rewrite m.Kind to the returned kind before storing the
// entity when the returned kind differs from m.Kind.
func (r *Resolver) autoRegisterEntityKind(ctx context.Context, kind string, contextSnippet string, counter *ingestMintCounter) (string, error) {
	norm := normaliseKindName(kind)

	// Core kinds are always registered; no work to do.
	if _, isCore := coreKinds[norm]; isCore {
		return norm, nil
	}

	// Already in the registry (core or previously minted)?
	if r.kindRegistry.IsRegistered(norm) {
		return norm, nil
	}

	// Consult the curated lexicon before reaching for the LLM.
	if entry, ok := kindLexiconLookup(norm); ok {
		switch entry.entryType {
		case kindLexiconAlias:
			// Rewrite to canonical kind. Most aliases target a core kind (always
			// registered, no event needed), but some target a curated non-core
			// kind (tech forms -> "tool", value forms -> "topic"); ensure that
			// target is registered so the rewritten kind never lands as an
			// unregistered warn-accept downstream.
			r.logger.Debug("Resolver: entity kind aliased to canonical",
				zap.String("inputKind", kind),
				zap.String("canonical", entry.target),
			)
			r.ensureAliasTargetRegistered(ctx, entry.target, counter)
			return entry.target, nil

		case kindLexiconNew:
			// Lexicon-curated new kinds are deterministic and bounded, but still
			// count against the per-ingest mint cap so a pathological extractor
			// that emits many curated kinds in one burst does not bypass the
			// runaway-taxonomy guard.
			if !counter.tryIncrement() {
				r.logger.Warn("Resolver: per-ingest entity kind mint cap reached; skipping lexicon registration",
					zap.Int("cap", maxKindMintsPerIngest),
					zap.String("kind", entry.target),
				)
				return entry.target, nil
			}
			// Register the new kind immediately with zero LLM calls.
			if err := r.appendEntityKindRegistered(ctx, entry.target, entry.displayName, entry.description); err != nil {
				r.logger.Warn("Resolver: entity kind registration failed; using kind as-is",
					zap.String("kind", entry.target),
					zap.Error(err),
				)
				return entry.target, nil
			}
			return entry.target, nil
		}
	}

	// Novel kind not in the lexicon: check the mint cap before calling the LLM.
	if !counter.tryIncrement() {
		r.logger.Warn("Resolver: per-ingest entity kind mint cap reached; skipping registration",
			zap.Int("cap", maxKindMintsPerIngest),
			zap.String("kind", norm),
		)
		return norm, nil
	}

	// LLM fallback: classify-or-mint via the entityKindClassify-v1 prompt.
	return r.llmClassifyEntityKind(ctx, norm, contextSnippet)
}

// ensureAliasTargetRegistered registers a non-core alias target on first use,
// reusing its curated kindLexiconNew metadata for the displayName/description.
// It is a no-op for core kinds (always registered) and for targets already in
// the registry; appendEntityKindRegistered is itself idempotent. This lets an
// alias safely point at a curated-but-non-core kind (e.g. "tool", "topic")
// without the rewritten kind landing as an unregistered warn-accept.
func (r *Resolver) ensureAliasTargetRegistered(ctx context.Context, target string, counter *ingestMintCounter) {
	if _, isCore := coreKinds[target]; isCore {
		return
	}
	if r.kindRegistry.IsRegistered(target) {
		return
	}
	entry, ok := kindLexiconLookup(target)
	if !ok || entry.entryType != kindLexiconNew {
		return // no curated metadata; leave to the normal mint/cap path
	}
	if !counter.tryIncrement() {
		r.logger.Warn("Resolver: per-ingest entity kind mint cap reached; alias target left unregistered",
			zap.Int("cap", maxKindMintsPerIngest),
			zap.String("target", target),
		)
		return
	}
	if err := r.appendEntityKindRegistered(ctx, entry.target, entry.displayName, entry.description); err != nil {
		r.logger.Warn("Resolver: alias target kind registration failed; using kind as-is",
			zap.String("target", target),
			zap.Error(err),
		)
	}
}

// llmClassifyEntityKind calls ChatProvider.ToolUse with the
// entityKindClassify-v1 prompt to determine whether a novel kind should reuse
// an existing registered kind or be minted as new. Handles LLM errors by
// degrading to leaving the kind as-is. Low-confidence mints enqueue a
// review item rather than auto-registering.
func (r *Resolver) llmClassifyEntityKind(ctx context.Context, kind, contextSnippet string) (string, error) {
	system, err := r.promptReg.Render(ctx, entityKindClassifyTemplateName, entityKindClassifyTemplateVersion, prompts.RenderVars{
		"kind":           kind,
		"contextSnippet": contextSnippet,
	})
	if err != nil {
		r.logger.Debug("Resolver: entity kind classify prompt render failed; leaving kind as-is",
			zap.String("kind", kind),
			zap.Error(err),
		)
		return kind, nil
	}

	schema := json.RawMessage(`{
		"type": "object",
		"required": ["action", "kind", "confidence"],
		"properties": {
			"action":      {"type": "string", "enum": ["reuse", "mint"]},
			"kind":        {"type": "string"},
			"displayName": {"type": "string"},
			"description": {"type": "string"},
			"confidence":  {"type": "number", "minimum": 0, "maximum": 1}
		}
	}`)

	resp, err := r.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:  r.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: "Classify the entity kind: " + kind},
		},
		Tool: kernel.ToolSchema{
			Name:        "classify_entity_kind",
			Description: "Classify a novel entity kind: reuse an existing kind or mint a new one.",
			InputSchema: schema,
		},
		MaxTokens: r.cfg.KindClassifyMaxTokens,
	})
	if err != nil {
		r.logger.Info("Resolver: entity kind classify LLM call failed; leaving kind as-is",
			zap.String("kind", kind),
			zap.Error(err),
		)
		return kind, nil
	}
	if resp.ToolUse == nil {
		r.logger.Debug("Resolver: entity kind classify returned no tool-use output; leaving kind as-is",
			zap.String("kind", kind),
		)
		return kind, nil
	}

	var out struct {
		Action      string  `json:"action"`
		Kind        string  `json:"kind"`
		DisplayName string  `json:"displayName"`
		Description string  `json:"description"`
		Confidence  float64 `json:"confidence"`
	}
	if err := json.Unmarshal(resp.ToolUse.Input, &out); err != nil {
		r.logger.Debug("Resolver: entity kind classify parse failed; leaving kind as-is",
			zap.String("kind", kind),
			zap.Error(err),
		)
		return kind, nil
	}

	switch out.Action {
	case "reuse":
		target := normaliseKindName(out.Kind)
		if target == "" {
			target = kind
		}
		// Validate the reuse target before accepting it. The classifier may
		// return an unregistered or syntactically invalid kind (e.g. "vehicles"
		// or "transport mode"). Fall back to the input kind when the target is
		// not a known registered kind so the entity still writes without
		// persisting a garbage kind value.
		if !r.isRegisteredEntityKind(target) {
			r.logger.Debug("Resolver: entity kind classifier reuse target not registered; leaving kind as-is",
				zap.String("inputKind", kind),
				zap.String("rejectTarget", target),
			)
			return kind, nil
		}
		r.logger.Debug("Resolver: entity kind classifier recommends reuse",
			zap.String("inputKind", kind),
			zap.String("reuseKind", target),
			zap.Float64("confidence", out.Confidence),
		)
		return target, nil

	case "mint":
		if out.Confidence < kindClassifyConfidenceThreshold {
			// Low-confidence: enqueue for human review, entity still writes.
			r.enqueueKindReview(ctx, kind, contextSnippet, out.Confidence)
			r.logger.Info("Resolver: entity kind mint confidence below threshold; enqueued for review",
				zap.String("kind", kind),
				zap.Float64("confidence", out.Confidence),
				zap.Float64("threshold", kindClassifyConfidenceThreshold),
			)
			return kind, nil
		}

		// High-confidence mint: normalise and register.
		mintName := normaliseKindName(out.Kind)
		if mintName == "" || !snakeCaseKindPattern.MatchString(mintName) {
			mintName = kind // fall back to the original if the LLM gave a bad name
		}
		displayName := out.DisplayName
		if displayName == "" {
			displayName = strings.Title(strings.ReplaceAll(mintName, "_", " ")) //nolint:staticcheck
		}
		description := out.Description
		if description == "" {
			description = "Novel entity kind minted from extraction context."
		}

		if err := r.appendEntityKindRegistered(ctx, mintName, displayName, description); err != nil {
			r.logger.Warn("Resolver: entity kind registration failed after LLM classify; using kind as-is",
				zap.String("kind", mintName),
				zap.Error(err),
			)
			return mintName, nil
		}
		return mintName, nil

	default:
		r.logger.Debug("Resolver: entity kind classify returned unknown action; leaving kind as-is",
			zap.String("kind", kind),
			zap.String("action", out.Action),
		)
		return kind, nil
	}
}

// isKindActiveOrProposed reports whether name already exists in the
// entity_kinds SQLite table with status 'active' or 'proposed'. It guards both
// the permissive and governed resolver paths against duplicate proposal rows for
// a kind that is already live or already pending review — the KindRegistry
// in-memory cache does not track proposed kinds, so this query is the only
// authoritative check.
//
// Rows with status 'rejected' deliberately do NOT block: the lifecycle allows a
// rejected kind to be re-proposed with a corrected, higher-confidence
// classification (a fresh entityKindRegistered event flips the row back to
// 'proposed' via the dispatcher upsert). Returns false on any query error
// (degrading to allow the append rather than silently blocking).
func (r *Resolver) isKindActiveOrProposed(ctx context.Context, name string) bool {
	var count int
	err := r.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_kinds WHERE name = ? AND status IN ('active', 'proposed')`, name,
	).Scan(&count)
	if err != nil {
		return false // degrade: allow the append rather than silently blocking
	}
	return count > 0
}

// appendEntityKindRegistered appends an EntityKindRegisteredEvent to the event
// log and updates the in-memory KindRegistry cache. Callers must supply the
// normalised snake_case kind name, a non-empty displayName, and a one-line
// description. Provenance is always "user" (the kind was auto-minted during
// ingestion, not shipped with the core); addedBy is EntityKindAddedByLLM.
//
// In governed mode (r.cfg.Governed == true) the event is persisted with
// Status="proposed". Proposed kinds are NOT added to the KindRegistry cache so
// they are not treated as registered by subsequent Resolve calls — the kind
// only becomes active after an explicit `maz schema review approve`.
func (r *Resolver) appendEntityKindRegistered(ctx context.Context, name, displayName, description string) error {
	// Guard: skip if already registered (idempotency for concurrent callers).
	// In governed mode the KindRegistry cache does NOT contain proposed kinds,
	// so we must also check the SQLite table to avoid duplicate proposal rows
	// across multiple ingestion passes for the same unknown kind.
	if r.kindRegistry.IsRegistered(name) {
		return nil
	}

	if r.cfg.Governed {
		// Governed mode: persist as proposed rather than active, and coalesce
		// concurrent proposals for the same kind through singleflight. The
		// status check runs INSIDE the coalesced execution so that (a) two
		// ingestion passes racing the pre-projection window yield exactly one
		// append, and (b) a sequential caller after the flight clears re-checks
		// the freshly-projected 'proposed' row and skips. Because the flight key
		// auto-clears, a rejected kind can still be re-proposed later in the same
		// process — the status-aware check lets 'rejected' through.
		_, err, _ := r.proposedFlight.Do(name, func() (any, error) {
			if r.isKindActiveOrProposed(ctx, name) {
				return false, nil // already live or pending review — skip
			}
			if err := r.writeEntityKindEvent(ctx, name, displayName, description, ontology.EntityKindStatusProposed, 1.0); err != nil {
				return false, err
			}
			r.logger.Info("Resolver: entity kind proposed (governed mode; awaiting review)",
				zap.String("name", name),
				zap.String("displayName", displayName),
				zap.String("provenance", "user"),
				zap.String("addedBy", string(ontology.EntityKindAddedByLLM)),
			)
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("append entity kind registered: %w", err)
		}
		return nil
	}

	// Permissive mode: persist as active (Status="") and update the in-memory
	// cache so subsequent calls within this process see the new kind without a
	// round-trip to the entity_kinds table.
	if err := r.writeEntityKindEvent(ctx, name, displayName, description, "", 1.0); err != nil {
		return err
	}
	r.kindRegistry.MarkRegistered(name)
	r.logger.Info("Resolver: auto-registered entity kind",
		zap.String("name", name),
		zap.String("displayName", displayName),
		zap.String("provenance", "user"),
		zap.String("addedBy", string(ontology.EntityKindAddedByLLM)),
	)
	return nil
}

// writeEntityKindEvent marshals and appends an EntityKindRegisteredEvent with
// the given status ("" = active; "proposed" in governed mode). Provenance is
// always "user" and addedBy is EntityKindAddedByLLM (auto-minted on ingest).
func (r *Resolver) writeEntityKindEvent(ctx context.Context, name, displayName, description, status string, confidence float64) error {
	payload := ontology.EntityKindRegisteredEvent{
		Name:        name,
		DisplayName: displayName,
		Description: description,
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByLLM,
		AddedAt:     time.Now().UTC(),
		Status:      status,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal entity kind registered event: %w", err)
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
		Source:     "resolver/v1",
		Confidence: confidence,
		Payload:    raw,
	}
	if _, err := r.eventLog.Append(ctx, []kernel.Event{ev}); err != nil {
		return fmt.Errorf("append entity kind registered: %w", err)
	}
	return nil
}

// enqueueKindReview records a low-confidence kind classification as a durable
// proposed EntityKindRegisteredEvent in both permissive and governed modes.
// The kind is NOT added to the KindRegistry cache so it is not treated as
// active until explicitly approved via `maz schema review approve`.
//
// The mode distinction is upstream: llmClassifyEntityKind routes here whenever
// confidence is below kindClassifyConfidenceThreshold, regardless of mode.
// High-confidence novel kinds in permissive mode go through
// appendEntityKindRegistered and land active immediately; all novel kinds in
// governed mode go through appendEntityKindRegistered and land proposed.
func (r *Resolver) enqueueKindReview(ctx context.Context, kind, contextSnippet string, confidence float64) {
	// Both permissive and governed modes persist the candidate as proposed.
	// proposedFlight is shared with appendEntityKindRegistered (keyed by kind
	// name) so a kind proposed via either path is not duplicated by the other,
	// and the status check runs inside the coalesced execution. A 'rejected' row
	// does not block — the status-aware check allows a corrected remint, and the
	// flight key auto-clears so this holds in the same long-running process.
	_, err, _ := r.proposedFlight.Do(kind, func() (any, error) {
		if r.isKindActiveOrProposed(ctx, kind) {
			r.logger.Info("Resolver: entity kind already active or proposed; skipping duplicate enqueue",
				zap.String("kind", kind),
			)
			return false, nil
		}
		// Use the kind string itself as displayName and a default description so
		// the review surface has something useful to show.
		displayName := strings.Title(strings.ReplaceAll(kind, "_", " ")) //nolint:staticcheck
		if displayName == "" {
			displayName = kind
		}
		description := fmt.Sprintf("Low-confidence entity kind proposed by ingestion (confidence=%.2f). Context: %s", confidence, contextSnippet)
		if err := r.writeEntityKindEvent(ctx, kind, displayName, description, ontology.EntityKindStatusProposed, confidence); err != nil {
			return false, err
		}
		r.logger.Info("Resolver: entity kind proposed via review queue",
			zap.String("kind", kind),
			zap.Float64("confidence", confidence),
			zap.Bool("governed", r.cfg.Governed),
		)
		return true, nil
	})
	if err != nil {
		r.logger.Warn("Resolver: enqueueKindReview append failed; falling back to log-only",
			zap.String("kind", kind),
			zap.Error(err),
		)
	}
}

// isRegisteredEntityKind reports whether kind is in the entity_kinds table.
// Called from the resolver's new-entity creation path to decide whether
// autoRegisterEntityKind needs to run. Uses the KindRegistry cache.
func (r *Resolver) isRegisteredEntityKind(kind string) bool {
	norm := normaliseKindName(kind)
	if _, isCore := coreKinds[norm]; isCore {
		return true
	}
	return r.kindRegistry.IsRegistered(norm)
}
