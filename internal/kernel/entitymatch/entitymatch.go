// Package entitymatch is the shared entity-deduplication primitive used by
// both entity-creation paths: the extraction resolver (internal/kernel/resolver)
// and the direct-mint createEntity MCP tool. Centralising the exact-name,
// name-embedding, and identifier lookups here guarantees both paths apply
// identical matching rules, so a name that the resolver would have merged is
// the same name the direct path refuses to duplicate.
package entitymatch

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Default cosine thresholds for create-time dedup. ReuseDefault mirrors the
// resolver's NameEmbThreshold (resolver/config.go) so the two paths treat a
// name-vector match as "the same entity" at the same point. AdvisoryDefault is
// the softer band below which a near-match is surfaced as a "did you mean"
// candidate without claiming identity.
const (
	ReuseDefault    = 0.95
	AdvisoryDefault = 0.88
)

// StrongIdentifierMinLength is the minimum identifier length for the non-alias
// strong-identifier lane in IdentifierMatch. Domain identifiers (hrb, lei, vin,
// emails, usernames) are distinctive strings; the floor stops a degenerate
// short value like "12" from anchoring every mention that shares it.
const StrongIdentifierMinLength = 4

// Candidate is a possible duplicate entity surfaced by a Lookup.
type Candidate struct {
	UID         string  `json:"uid"`
	DisplayName string  `json:"displayName"`
	Kind        string  `json:"kind"`
	Similarity  float64 `json:"similarity"` // cosine; 1.0 for exact-name / identifier hits
	// Reason names the lane that surfaced this candidate: "exact_name",
	// "name_emb", or "identifier".
	Reason string `json:"reason"`
	// RelationCount is filled in by callers that hold a relation counter (e.g.
	// createEntity via the navigator); the matching primitives leave it 0.
	RelationCount int `json:"relationCount"`
}

// Lookup resolves whether an entity already exists, via exact name,
// name-vector nearest-neighbour, or identifier. It is safe for concurrent use;
// all state is read-only after construction.
type Lookup struct {
	reader   *sql.DB
	vec      kernel.VectorConn
	embedder kernel.Embedder

	// ReuseThreshold is the cosine similarity at or above which a name-vector
	// match is treated as the same entity. AdvisoryThreshold is the lower bound
	// for surfacing a near-match as a softer candidate. Callers own the band
	// policy; the matching methods return raw cosine similarities and never
	// filter on these.
	ReuseThreshold    float64
	AdvisoryThreshold float64
}

// NewLookup constructs a Lookup. Reader, vec, and embedder are required; a nil
// vec or embedder makes the name-vector lane a no-op (callers degrade to
// exact-name + identifier matching only). Reuse/advisory <= 0 fall back to the
// package defaults.
func NewLookup(reader *sql.DB, vec kernel.VectorConn, embedder kernel.Embedder, reuse, advisory float64) *Lookup {
	if reuse <= 0 {
		reuse = ReuseDefault
	}
	if advisory <= 0 {
		advisory = AdvisoryDefault
	}
	return &Lookup{
		reader:            reader,
		vec:               vec,
		embedder:          embedder,
		ReuseThreshold:    reuse,
		AdvisoryThreshold: advisory,
	}
}

// ExactNameMatch attempts a case-sensitive then case-insensitive exact lookup
// by (display_name, kind) in the entities table. Returns the entity UID on a
// hit, or empty string on a miss.
func (l *Lookup) ExactNameMatch(ctx context.Context, name, kind string) (string, error) {
	var uid string

	err := l.reader.QueryRowContext(ctx,
		`SELECT uid FROM entities WHERE display_name = ? AND kind = ? LIMIT 1`,
		name, kind,
	).Scan(&uid)
	if err == nil {
		return uid, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("exact name (case-sensitive): %w", err)
	}

	err = l.reader.QueryRowContext(ctx,
		`SELECT uid FROM entities WHERE LOWER(display_name) = LOWER(?) AND kind = ? LIMIT 1`,
		name, kind,
	).Scan(&uid)
	if err == nil {
		return uid, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("exact name (case-insensitive): %w", err)
	}
	return "", nil
}

// IdentifierMatch resolves a value against entity_identifiers in two lanes:
// (1) display-name aliases (kind="alias"), gated on the candidate's kind
// matching kind to prevent cross-type collisions; (2) strong typed identifiers
// (email, username, phone, domain lanes), NOT kind-gated but guarded by a
// minimum length, excluding the alias and catch-all "id" kinds. Returns the
// matched entity's UID AND its stored kind on a hit, or two empty strings on a
// miss. The stored kind matters for the strong-id lane: it is kind-blind, so a
// caller that declared one kind can match an entity of another and must learn
// the real kind rather than echo its own. A nil/empty kind disables the kind
// gate on lane 1 (matches any kind) — used by the direct-mint path where an
// identifier anchor is authoritative regardless of declared kind.
func (l *Lookup) IdentifierMatch(ctx context.Context, value, kind string) (uid, matchedKind string, _ error) {
	err := l.reader.QueryRowContext(ctx,
		`SELECT ei.entity_uid, e.kind
		   FROM entity_identifiers AS ei
		   JOIN entities          AS e ON e.uid = ei.entity_uid
		  WHERE LOWER(ei.identifier) = LOWER(?)
		    AND ei.kind = ?
		    AND (? = '' OR e.kind = ?)
		  ORDER BY ei.added_at ASC
		  LIMIT 1`,
		value, string(ontology.IdentifierKindAlias), kind, kind,
	).Scan(&uid, &matchedKind)
	if err == nil {
		return uid, matchedKind, nil
	}
	if err != sql.ErrNoRows {
		return "", "", fmt.Errorf("identifier lookup: %w", err)
	}

	err = l.reader.QueryRowContext(ctx,
		`SELECT ei.entity_uid, e.kind
		   FROM entity_identifiers AS ei
		   JOIN entities          AS e ON e.uid = ei.entity_uid
		  WHERE LOWER(ei.identifier) = LOWER(?)
		    AND ei.kind NOT IN (?, ?)
		    AND LENGTH(ei.identifier) >= ?
		  ORDER BY ei.added_at ASC
		  LIMIT 1`,
		value, string(ontology.IdentifierKindAlias), string(ontology.IdentifierKindID), StrongIdentifierMinLength,
	).Scan(&uid, &matchedKind)
	if err == nil {
		return uid, matchedKind, nil
	}
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return "", "", fmt.Errorf("strong identifier lookup: %w", err)
}

// NameKNN embeds name and returns up to k kind-matching nearest-neighbour
// candidates ranked by cosine similarity (highest first). Returns nil when the
// embedder or vec lane is unavailable, so callers degrade gracefully.
func (l *Lookup) NameKNN(ctx context.Context, name, kind string, k int) ([]Candidate, error) {
	if l.embedder == nil || l.vec == nil {
		return nil, nil
	}
	vec, err := l.embedder.Embed(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("entitymatch: embed name %q: %w", name, err)
	}
	return l.NameKNNByVector(ctx, vec, kind, k)
}

// NameKNNByVector runs a nearest-neighbour search over entities_vec
// (vector_type="name") for a pre-computed name embedding and returns up to k
// kind-matching candidates with cosine similarities. Hits whose entity row is
// missing (a write-then-read race) or whose kind differs from kind are skipped.
// Reusing a caller-supplied vector lets the resolver keep its per-episode
// pre-embed batch while sharing the search + kind-gate logic.
func (l *Lookup) NameKNNByVector(ctx context.Context, vec []float32, kind string, k int) ([]Candidate, error) {
	if l.vec == nil {
		return nil, nil
	}
	hits, err := l.vec.Search(ctx, vec, k, kernel.VectorFilter{
		Kinds:      []kernel.EventKind{kernel.EventKind(ontology.KindEntity)},
		VectorType: "name",
	})
	if err != nil {
		return nil, fmt.Errorf("entitymatch: name emb search: %w", err)
	}

	candidates := make([]Candidate, 0, len(hits))
	for _, hit := range hits {
		gotKind, gotName, ok, lerr := l.loadKindName(ctx, hit.ID)
		if lerr != nil {
			return nil, lerr
		}
		// Missing row: a write-then-read race — treat as a miss, not a crash.
		if !ok {
			continue
		}
		// Kind gate: blocks (Person "Berlin") colliding with (Place "Berlin").
		if gotKind != kind {
			continue
		}
		candidates = append(candidates, Candidate{
			UID:         hit.ID,
			DisplayName: gotName,
			Kind:        gotKind,
			Similarity:  L2DistanceToCosine(float64(hit.Score)),
			Reason:      "name_emb",
		})
	}
	return candidates, nil
}

// Describe loads a candidate's display name, kind, and active-relation count by
// UID, for surfacing an existing entity to a caller (e.g. createEntity's
// reused / duplicate_candidates result). Ok is false when no row exists.
// Similarity and Reason are left to the caller to stamp.
func (l *Lookup) Describe(ctx context.Context, uid string) (Candidate, bool, error) {
	kind, name, ok, err := l.loadKindName(ctx, uid)
	if err != nil || !ok {
		return Candidate{}, ok, err
	}
	count, err := l.CountActiveRelations(ctx, uid)
	if err != nil {
		return Candidate{}, false, err
	}
	return Candidate{UID: uid, DisplayName: name, Kind: kind, RelationCount: count}, true, nil
}

// CountActiveRelations returns the number of non-superseded relations touching
// uid as subject or object. Callers use it to rank duplicate candidates by
// connectedness so the agent can pick the canonical entity to keep.
func (l *Lookup) CountActiveRelations(ctx context.Context, uid string) (int, error) {
	var n int
	err := l.reader.QueryRowContext(ctx,
		`SELECT count(*) FROM relations
		  WHERE superseded_by_uid IS NULL AND (subject_uid = ? OR object_uid = ?)`,
		uid, uid,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("entitymatch: count relations for %s: %w", uid, err)
	}
	return n, nil
}

// loadKindName reads an entity's kind and display name by UID. Ok is false when
// no row exists (write-then-read race), which callers treat as a miss.
func (l *Lookup) loadKindName(ctx context.Context, uid string) (kind, name string, ok bool, err error) {
	err = l.reader.QueryRowContext(ctx,
		`SELECT kind, display_name FROM entities WHERE uid = ? LIMIT 1`, uid,
	).Scan(&kind, &name)
	switch err {
	case nil:
		return kind, name, true, nil
	case sql.ErrNoRows:
		return "", "", false, nil
	default:
		return "", "", false, fmt.Errorf("entitymatch: load entity %s: %w", uid, err)
	}
}

// L2DistanceToCosine inverts cos = 1 - d^2/2, valid for L2-normalised vectors.
// The vec0 default metric is L2 distance; the M1 default embedder emits
// unit-norm vectors. The function is monotonic, so threshold gates stated in
// cosine read the same after conversion.
func L2DistanceToCosine(d float64) float64 {
	return 1.0 - (d*d)/2.0
}
