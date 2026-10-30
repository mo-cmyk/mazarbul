package document

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// SemanticNameResolver is an optional capability the Importer uses as the
// third lane in wikilink resolution (after exact display-name match and vault
// path match). It resolves a bare display-name string — e.g. "Rosie" in
// [[Rosie]] — to a canonical entity UID by searching the entities_vec name
// vectors with an embedding of the target text.
//
// The lane is kind-agnostic: a wikilink may name an entity of any kind
// (person, organisation, place, project, document), unlike
// resolver.matchMention which is kind-gated.
//
// Implementations must be:
//   - Safe for concurrent use (the vault watcher drives multiple documents
//     concurrently).
//   - Conservative: only return a result when the top candidate is both above
//     the similarity threshold AND unambiguous (clearly beats the runner-up by
//     the configured margin). Otherwise return ok=false so the link stays
//     dangling and joins the reconcile snapshot.
//   - Non-fatal: embedder errors or vector-search errors must cause the
//     implementation to return ok=false (log at warn), never propagate.
//
// Nil is the documented "semantic resolution disabled" sentinel: the importer
// and all resolve helpers guard every call site against a nil receiver.
type SemanticNameResolver interface {
	// ResolveNameSemantically tries to map target to a canonical entity. When
	// ok is true, uid is the entity UID and kind is its entity kind string
	// (e.g. "person", "document") — the caller passes kind to predicateForKind
	// to classify the edge. selfUID is the importing document's own entity UID;
	// the implementation suppresses any match that would produce a self-edge.
	ResolveNameSemantically(ctx context.Context, target, selfUID string) (uid, kind string, ok bool)
}

// nameEmbeddingSearcher is the subset of kernel.VectorConn that the
// NameEmbeddingResolver needs. It is satisfied by *sqlite.Vector via its
// SearchNameVectors method. Keeping it as a narrow interface lets tests inject
// a fake without building a full VectorConn.
type nameEmbeddingSearcher interface {
	// SearchNameVectors runs a top-k approximate-nearest-neighbour search over
	// entities_vec WHERE vector_type='name'. Returns hits in ascending L2
	// distance order (lower score = closer). k should be 2 so the caller can
	// check the runner-up margin.
	SearchNameVectors(ctx context.Context, query []float32, k int) ([]kernel.VectorHit, error)
}

// nameEmbeddingResolverConfig holds the tunable thresholds for
// NameEmbeddingResolver. Defaults are applied by NewNameEmbeddingResolver.
type nameEmbeddingResolverConfig struct {
	// SimilarityThreshold is the minimum cosine similarity the top hit must
	// reach to be considered a candidate. Default: 0.92. Intentionally set
	// slightly lower than resolver.Config.NameEmbThreshold (0.95) because
	// wikilink resolution is kind-agnostic (no secondary identifier / summary
	// confirmation step); the margin gate compensates for the missing kind
	// filter.
	SimilarityThreshold float64

	// MarginThreshold is the minimum cosine-similarity gap between the top
	// candidate and the runner-up required to accept the top hit as
	// unambiguous. Default: 0.05. When two entities score similarly (e.g.
	// "Rosie" is equally close to both "Rosie Cotton" and "Rosie Smith") the
	// link is left dangling rather than guessing.
	MarginThreshold float64
}

// defaultNameEmbeddingConfig returns the default thresholds.
func defaultNameEmbeddingConfig() nameEmbeddingResolverConfig {
	return nameEmbeddingResolverConfig{
		SimilarityThreshold: 0.92,
		MarginThreshold:     0.05,
	}
}

// NameEmbeddingResolverFunc is a convenience type so the daemon wiring can
// pass a plain function literal rather than constructing a named type. It
// satisfies SemanticNameResolver.
type NameEmbeddingResolverFunc func(ctx context.Context, target, selfUID string) (uid, kind string, ok bool)

// ResolveNameSemantically implements SemanticNameResolver.
func (f NameEmbeddingResolverFunc) ResolveNameSemantically(ctx context.Context, target, selfUID string) (uid, kind string, ok bool) {
	return f(ctx, target, selfUID)
}

// l2DistanceToCosineDoc inverts the relation cos = 1 − d²/2 valid for
// L2-normalised vectors. Mirrors the same function in
// internal/kernel/resolver/match.go (duplicated to avoid a cross-package
// import from document into resolver — the resolver package is heavy and would
// create an import cycle via kernel/navigate).
func l2DistanceToCosineDoc(d float64) float64 {
	return 1.0 - (d*d)/2.0
}

// NewNameEmbeddingResolver builds a SemanticNameResolver backed by the entity
// name-vector index. It is the production implementation wired by
// wireMemoryLayer after the embedder and vector layer are available.
//
// Parameters:
//   - embedder: produces dense vectors for the target string (the same
//     embedder the resolver uses for nameEmbMatch — must be identical or the
//     cosine similarity is meaningless).
//   - searcher: the entity name-vector search surface. In production this is
//     the *sqlite.Vector (via its SearchNameVectors method). The searcher must
//     return hits in ascending L2-distance order.
//   - kindForUID: a closure that maps an entity UID to its kind string. The
//     caller provides this so the document package does not import the
//     navigator package (which is already a transitive dependency through
//     Reader, but formalising the dependency here would require importing
//     navigate.EntityFilter, which is out-of-scope). Nil kindForUID is
//     safe — the resolver logs a warning and treats the match as kind="".
//   - log: logger. May be nil (a nop logger is used).
//
// The returned resolver is safe for concurrent use.
func NewNameEmbeddingResolver(
	embedder kernel.Embedder,
	searcher nameEmbeddingSearcher,
	kindForUID func(ctx context.Context, uid string) string,
	log *zap.Logger,
) NameEmbeddingResolverFunc {
	if log == nil {
		log = zap.NewNop()
	}
	cfg := defaultNameEmbeddingConfig()
	return NameEmbeddingResolverFunc(func(ctx context.Context, target, selfUID string) (string, string, bool) {
		return resolveNameViaEmbedding(ctx, target, selfUID, embedder, searcher, kindForUID, cfg, log)
	})
}

// resolveNameViaEmbedding is the pure resolution logic extracted so
// NewNameEmbeddingResolver stays a thin constructor and the logic is unit-testable
// with injected fakes.
func resolveNameViaEmbedding(
	ctx context.Context,
	target, selfUID string,
	embedder kernel.Embedder,
	searcher nameEmbeddingSearcher,
	kindForUID func(ctx context.Context, uid string) string,
	cfg nameEmbeddingResolverConfig,
	log *zap.Logger,
) (uid, kind string, ok bool) {
	// Embed the target display name.
	vec, err := embedder.Embed(ctx, target)
	if err != nil {
		log.Warn("document: semantic wikilink: embed failed; leaving link dangling",
			zap.String("target", target),
			zap.Error(err),
		)
		return "", "", false
	}

	// Fetch top-2 so we can compute the runner-up margin.
	hits, err := searcher.SearchNameVectors(ctx, vec, 2)
	if err != nil {
		log.Warn("document: semantic wikilink: vector search failed; leaving link dangling",
			zap.String("target", target),
			zap.Error(err),
		)
		return "", "", false
	}
	if len(hits) == 0 {
		return "", "", false
	}

	top := hits[0]
	topSim := l2DistanceToCosineDoc(float64(top.Score))

	// Threshold gate: reject low-confidence candidates outright.
	if topSim < cfg.SimilarityThreshold {
		log.Debug("document: semantic wikilink: top hit below threshold; leaving dangling",
			zap.String("target", target),
			zap.String("topUID", top.ID),
			zap.Float64("similarity", topSim),
			zap.Float64("threshold", cfg.SimilarityThreshold),
		)
		return "", "", false
	}

	// Margin gate: reject when the runner-up is too close (ambiguous).
	if len(hits) >= 2 {
		runnerSim := l2DistanceToCosineDoc(float64(hits[1].Score))
		margin := topSim - runnerSim
		if margin < cfg.MarginThreshold {
			log.Debug("document: semantic wikilink: ambiguous top hit; leaving dangling",
				zap.String("target", target),
				zap.String("topUID", top.ID),
				zap.Float64("topSim", topSim),
				zap.Float64("runnerSim", runnerSim),
				zap.Float64("margin", margin),
				zap.Float64("marginThreshold", cfg.MarginThreshold),
			)
			return "", "", false
		}
	}

	// Self-suppression: never resolve to the importing document itself.
	if top.ID == selfUID {
		return "", "", false
	}

	// Hydrate the entity kind from the UID. The kind determines the edge
	// predicate (mentions vs links_to). Unknown kind ("") degrades to the
	// mentions default via predicateForKind.
	hitKind := ""
	if kindForUID != nil {
		hitKind = kindForUID(ctx, top.ID)
	} else {
		log.Warn("document: semantic wikilink: kindForUID is nil; edge predicate may be wrong",
			zap.String("target", target),
			zap.String("uid", top.ID),
		)
	}

	log.Debug("document: semantic wikilink resolved",
		zap.String("target", target),
		zap.String("uid", top.ID),
		zap.String("kind", hitKind),
		zap.Float64("similarity", topSim),
	)
	return top.ID, hitKind, true
}

// semanticProvenancePrefix marks a relation row as produced by the semantic
// name-embedding wikilink resolution lane. The deterministic forget pass does
// NOT special-case this prefix (unlike proseProvenancePrefix): semantic-lane
// edges ARE wikilink edges — they arise from a [[wikilink]] in the document
// body, just resolved via vector similarity rather than an exact name match.
// When the link is removed from the body and the document is re-imported, the
// edge is correctly forgotten by the same deterministic forget pass that removes
// exact-match-resolved edges.
//
// The prefix is written into the Provenance field of the Relation event so an
// operator can distinguish "resolved by name-embedding" from "resolved by
// exact-name" in the raw event log.
const semanticProvenancePrefix = "wikilink-semantic "

// formatSemanticProvenance returns the provenance label for a semantically
// resolved wikilink edge: e.g. "wikilink-semantic [[Rosie]]".
func formatSemanticProvenance(raw string) string {
	return fmt.Sprintf("%s%s", semanticProvenancePrefix, raw)
}
