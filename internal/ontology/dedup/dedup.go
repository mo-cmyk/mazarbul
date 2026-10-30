// Package dedup provides the reuse-before-mint deduplication helper for the
// dynamic-ontology authoring tools. Every defineEntityKind call routes through
// Checker.Check before a new kind is registered, so agents never accidentally
// mint a synonym when an equivalent kind already exists.
//
// The check is two-layered:
//
//  1. Lexical: exact or normalized-name match against every registered kind
//     (always available, zero-LLM, O(n) over kinds which number in the dozens).
//     Also runs a lightweight LCS-ratio/Levenshtein distance scan for close-but-not-exact
//     names: similarity ≥ 0.90 → Reuse; 0.75–0.90 → MintWithAlias; < 0.75 → Mint.
//
//  2. Semantic: when an Embedder is available, embeds the candidate
//     "name — description" and computes in-memory cosine similarity against the
//     embedded "name — description" of every registered kind.
//     The same thresholds apply. The semantic check only runs when the lexical
//     check returned Mint (no close name match), to avoid the LLM cost on cases
//     that are already resolved.
//
// When no Embedder is wired, Checker degrades to lexical-only. The force flag
// at the tool layer bypasses both checks.
//
// Decision handling by consumers:
//   - Reuse: consumed by defineEntityKind (internal/mcp/tools/define_entity_kind.go)
//     — returns the existing kind name without appending any event.
//   - MintWithAlias: consumed by defineEntityKind — proceeds with the mint and
//     appends the near-duplicate's internal alias URN (maz:kind:<matchName>) to
//     SameAs, surfacing it to the agent as the closeMatch response field.
//   - Mint: defineEntityKind proceeds with a clean mint, no alias recorded.
package dedup

import (
	"context"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// Decision is the outcome of a dedup check.
type Decision int

const (
	// Mint indicates the candidate is sufficiently novel: register it as a new kind.
	Mint Decision = iota

	// MintWithAlias indicates the candidate is close (but not identical) to an
	// existing kind. Mint the new kind AND record the close match so the caller
	// adds a SKOS closeMatch alias. Consumed by defineEntityKind: the match name
	// is appended to SameAs as maz:kind:<matchName> and surfaced in the response
	// as the closeMatch field.
	MintWithAlias

	// Reuse indicates the candidate is equivalent to an existing kind. Return the
	// existing kind name rather than registering a duplicate.
	Reuse
)

// ReuseThreshold: cosine/name-similarity ≥ this → Reuse.
// MintAliasThreshold: similarity ≥ this AND < ReuseThreshold → MintWithAlias.
// Below MintAliasThreshold → Mint.
const (
	ReuseThreshold     = 0.90
	MintAliasThreshold = 0.75
)

// defaultSemanticTimeout bounds the semantic (embedding) layer of a single
// Check. The lexical layer is in-memory and runs on the caller's ctx; only the
// embedding pass — which issues up to N+1 sequential embedder round trips — is
// capped. Without this bound a cold embedding cache or a slow/cold provider
// lets the pass run all the way to the MCP invoke backstop (300s), occupying
// the session for minutes; that was the defineEntityKind "hang". The persistent
// embedding cache means the per-kind cost is paid once, so in steady state the
// pass finishes well inside this budget; only a genuinely cold/wedged embedder
// hits the deadline, in which case Check degrades to Mint (same as an embed
// error). Overridable via Options.SemanticTimeout.
const defaultSemanticTimeout = 20 * time.Second

// Result carries the dedup decision and supporting metadata.
type Result struct {
	// Decision is the recommended action.
	Decision Decision

	// Match is the name of the nearest existing kind, set when Decision is
	// Reuse or MintWithAlias. Empty for Mint.
	Match string

	// Similarity is the similarity score 0..1 against Match. Zero for Mint.
	Similarity float64

	// Degraded is true when the semantic embedding layer was skipped (no
	// embedder wired, or the embedder timed out / errored). In that case only
	// lexical dedup ran; a semantic synonym may have been missed. Callers
	// surface this as a MAZ-KIND-002 warning in the tool response.
	Degraded bool
}

// KindSource is the read-side dependency the Checker needs: enumerate
// registered entity kinds so it can compare the candidate against them.
// *navigate.Navigator satisfies this when wired through the daemon.
type KindSource interface {
	RegisteredEntityKinds(ctx context.Context) ([]navigate.RegisteredEntityKind, error)
}

// Embedder is the optional embedding dependency. When non-nil, the Checker
// uses it for the semantic layer. Uses the kernel.Embedder interface so the
// daemon's existing embedder chain plugs in directly.
type Embedder = kernel.Embedder

// batcher is the optional fast path: an embedder that serves N texts in a
// single round trip. Both *cache.CachingEmbedder (cache-aware: only misses hit
// the provider) and *ollama.Client (one POST /api/embed) satisfy it, so the
// production chain takes this path. The Checker type-asserts against it and
// falls back to bounded sequential Embed calls when it is not satisfied.
type batcher interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// Options carries optional configuration for Checker. Zero values fall back to
// the package-level defaults (ReuseThreshold and MintAliasThreshold).
type Options struct {
	// ReuseThreshold overrides the similarity floor above which the candidate
	// is considered equivalent to an existing kind and should be reused.
	// Zero uses the package default (ReuseThreshold = 0.90).
	ReuseThreshold float64

	// MintAliasThreshold overrides the similarity floor above which the
	// candidate is considered close-but-distinct and a SKOS closeMatch alias
	// should be recorded. Must be < ReuseThreshold when non-zero.
	// Zero uses the package default (MintAliasThreshold = 0.75).
	MintAliasThreshold float64

	// SemanticTimeout overrides the deadline applied to the semantic
	// (embedding) layer of each Check. Zero uses defaultSemanticTimeout.
	// On timeout the check degrades to Mint, exactly as it does on an
	// embedder error, so a cold/wedged embedder cannot stall the caller.
	SemanticTimeout time.Duration
}

// Checker runs the layered dedup check. Construct with New; share a single
// instance per tool registration. Safe for concurrent use (all state is in
// the dependencies, which are themselves safe for concurrent use).
type Checker struct {
	kinds              KindSource
	embedder           Embedder // may be nil → lexical-only
	reuseThreshold     float64
	mintAliasThreshold float64
	semanticTimeout    time.Duration
}

// New constructs a Checker with default thresholds. KindSource is required;
// embedder may be nil, in which case the semantic layer is skipped and dedup
// falls back to lexical. Use NewWithOptions to override the similarity floors.
func New(kinds KindSource, embedder Embedder) *Checker {
	return NewWithOptions(kinds, embedder, Options{})
}

// NewWithOptions constructs a Checker with custom thresholds. Zero threshold
// values in opts fall back to the package-level defaults (ReuseThreshold and
// MintAliasThreshold). KindSource is required; embedder may be nil.
func NewWithOptions(kinds KindSource, embedder Embedder, opts Options) *Checker {
	rt := opts.ReuseThreshold
	if rt == 0 {
		rt = ReuseThreshold
	}
	mat := opts.MintAliasThreshold
	if mat == 0 {
		mat = MintAliasThreshold
	}
	st := opts.SemanticTimeout
	if st == 0 {
		st = defaultSemanticTimeout
	}
	return &Checker{
		kinds:              kinds,
		embedder:           embedder,
		reuseThreshold:     rt,
		mintAliasThreshold: mat,
		semanticTimeout:    st,
	}
}

// HasEmbedder reports whether a semantic (embedding) layer is wired. The
// preview path (PlanKind) never invokes the embedder, but it uses this to set
// KindPreviewRequest.SemanticChecked so a lexical-only verdict is flagged with
// MAZ-KIND-002 when no embedder is available.
func (c *Checker) HasEmbedder() bool { return c.embedder != nil }

// MintAliasThreshold returns the effective close-match floor for this Checker:
// the similarity at or above which a candidate is a near-duplicate worth a SKOS
// closeMatch alias. The preview path passes this to PlanKind so the lexical
// closeMatch verdict uses the same (possibly user-configured) floor the live
// mint uses.
func (c *Checker) MintAliasThreshold() float64 { return c.mintAliasThreshold }

// Check runs the dedup check for a candidate kind with the given name and
// description. Name should be the normalized snake_case identifier. Description
// is used for the semantic layer when an embedder is available. Returns an
// error only on infrastructure failure (kind listing fails). A listing failure
// is treated as Mint (allow minting) so a transient DB error never blocks
// registration.
func (c *Checker) Check(ctx context.Context, name, description string) (Result, error) {
	kinds, err := c.kinds.RegisteredEntityKinds(ctx)
	if err != nil {
		// Degrade gracefully: allow the mint rather than blocking on a list error.
		return Result{Decision: Mint}, nil
	}

	normName := normalizeName(name)

	// --- Lexical layer ---
	best, bestSim := lexicalBest(normName, kinds)
	if bestSim >= c.reuseThreshold {
		return Result{Decision: Reuse, Match: best, Similarity: bestSim}, nil
	}
	if bestSim >= c.mintAliasThreshold {
		return Result{Decision: MintWithAlias, Match: best, Similarity: bestSim}, nil
	}

	// --- Semantic layer (only when lexical found no close match) ---
	if c.embedder == nil {
		return Result{Decision: Mint, Degraded: true}, nil
	}

	// Bound the embedding pass: it embeds the candidate + every registered kind,
	// so a cold cache or slow/wedged provider would otherwise run to the MCP
	// invoke backstop and stall the session. On timeout (or any embed error) we
	// degrade to Mint — the safe direction (mint now, dedup again later) —
	// rather than deciding on a partial comparison set.
	semCtx, cancel := context.WithTimeout(ctx, c.semanticTimeout)
	defer cancel()

	// Candidate at index 0, then one text per registered kind, embedded in a
	// single batched round trip when the embedder supports it (the production
	// path) — collapsing a cold cache from N+1 sequential HTTP calls to one.
	texts := make([]string, 0, len(kinds)+1)
	texts = append(texts, name+" — "+description)
	for i := range kinds {
		texts = append(texts, kinds[i].Name+" — "+kinds[i].Description)
	}
	vecs, ok := c.embedAll(semCtx, texts)
	if !ok || len(vecs) != len(texts) {
		// Embed failure, budget exhausted, or a short/misaligned result:
		// degrade to Mint rather than deciding on a partial comparison set.
		// Mark Degraded so callers can surface a MAZ-KIND-002 warning.
		return Result{Decision: Mint, Degraded: true}, nil
	}
	candVec := vecs[0]

	// Compare the candidate against each registered kind's vector.
	var semBest string
	var semBestSim float64
	for i := range kinds {
		kVec := vecs[i+1]
		if len(kVec) == 0 {
			continue // tolerated per-kind miss on the sequential fallback path
		}
		sim := cosineSimilarity(candVec, kVec)
		if sim > semBestSim {
			semBestSim = sim
			semBest = kinds[i].Name
		}
	}

	switch {
	case semBestSim >= c.reuseThreshold:
		return Result{Decision: Reuse, Match: semBest, Similarity: semBestSim}, nil
	case semBestSim >= c.mintAliasThreshold:
		return Result{Decision: MintWithAlias, Match: semBest, Similarity: semBestSim}, nil
	default:
		return Result{Decision: Mint}, nil
	}
}

// embedAll embeds every text in one pass, preferring a single batched round
// trip when the embedder supports it (the production path: *cache.CachingEmbedder
// over *ollama.Client). Returns one vector per text, in order, and ok=true on
// success. On the sequential fallback an individual kind embed that fails leaves
// a nil slot (the caller skips it), but a candidate (index 0) failure or a ctx
// deadline returns ok=false so the caller degrades to Mint. The whole pass runs
// under the caller's (lifecycle-bounded) ctx, so the semantic deadline caps it
// either way. The underlying error is never inspected — every failure maps to
// the same Mint degrade — so it is collapsed to a bool here rather than
// propagated and swallowed at the call site.
func (c *Checker) embedAll(ctx context.Context, texts []string) (vecs [][]float32, ok bool) {
	if b, isBatcher := c.embedder.(batcher); isBatcher {
		out, err := b.EmbedBatch(ctx, texts)
		return out, err == nil
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if ctx.Err() != nil {
			return nil, false // budget exhausted mid-scan
		}
		v, err := c.embedder.Embed(ctx, t)
		if err != nil {
			if i == 0 {
				return nil, false // candidate embed is required for any comparison
			}
			continue // tolerate an individual kind miss
		}
		out[i] = v
	}
	return out, true
}

// --- helpers ---

// normalizeName returns the lowercased, trimmed, single-spaced version of a
// kind name with underscores replaced by spaces, for display-agnostic comparison.
// Any namespace prefix (e.g. "user:", "extension:schema_org:") is stripped to its
// local segment first, so a bare candidate like "vehicle" compares equal to a
// stored "user:vehicle" — both reduce to "vehicle".
func normalizeName(s string) string {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", " ")
	// Collapse runs of whitespace.
	fields := strings.FieldsFunc(s, unicode.IsSpace)
	return strings.Join(fields, " ")
}

// lexicalBest finds the registered kind with the highest lexical similarity
// to the candidate name. It checks exact normalized match first (similarity=1.0)
// then falls back to an LCS-ratio/Levenshtein score.
func lexicalBest(normCand string, kinds []navigate.RegisteredEntityKind) (string, float64) {
	var best string
	var bestSim float64

	for i := range kinds {
		k := &kinds[i] // range by index: RegisteredEntityKind is large; avoid a per-iteration copy
		norm := normalizeName(k.Name)
		// Exact match.
		if norm == normCand {
			return k.Name, 1.0
		}
		sim := nameSimilarity(normCand, norm)
		if sim > bestSim {
			bestSim = sim
			best = k.Name
		}
	}
	return best, bestSim
}

// nameSimilarity returns a 0..1 similarity score between two normalized name
// strings using a combination of:
//   - Longest-common-subsequence ratio (for prefix/suffix families).
//   - Levenshtein edit distance normalized by max length (for typos).
//
// The result is the max of the two, so either strong structural similarity or
// close edit distance triggers the alias/reuse threshold.
func nameSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	if a == "" || b == "" {
		return 0.0
	}
	lcs := lcsLength(a, b)
	maxLen := maxInt(len(a), len(b))
	lcsSim := float64(lcs) / float64(maxLen)

	lev := levenshtein(a, b)
	levSim := 1.0 - float64(lev)/float64(maxLen)
	if levSim < 0 {
		levSim = 0
	}

	if lcsSim > levSim {
		return lcsSim
	}
	return levSim
}

// lcsLength returns the length of the longest common subsequence of a and b.
// O(|a|*|b|) but kinds number in the dozens so this is fine.
func lcsLength(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	m, n := len(ra), len(rb)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			switch {
			case ra[i-1] == rb[j-1]:
				dp[i][j] = dp[i-1][j-1] + 1
			case dp[i-1][j] > dp[i][j-1]:
				dp[i][j] = dp[i-1][j]
			default:
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	return dp[m][n]
}

// levenshtein returns the Levenshtein edit distance between a and b.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	m, n := len(ra), len(rb)
	row := make([]int, n+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= m; i++ {
		prev := i
		for j := 1; j <= n; j++ {
			cur := row[j-1] // diagonal
			if ra[i-1] != rb[j-1] {
				del := row[j] + 1
				ins := prev + 1
				sub := cur + 1
				cur = minInt(del, minInt(ins, sub))
			}
			row[j-1] = prev
			prev = cur
		}
		row[n] = prev
	}
	return row[n]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// cosineSimilarity returns the cosine similarity between two float32 vectors.
// Returns 0 when either vector is zero-length or their norms are zero.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
