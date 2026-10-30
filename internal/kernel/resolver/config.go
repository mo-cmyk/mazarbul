package resolver

import "time"

// provenanceRanks maps a coordinatesProvenance string to its integer rank.
// Higher rank wins on conflict. Mirrors the CASE expressions in the dispatcher's
// writeEntitySQL so the resolver can guard before emitting an EntityUpdated event.
//
//nolint:gochecknoglobals // process-lifetime lookup table, never mutated
var provenanceRanks = map[string]int{
	"user_provided":      3,
	"calendar:google":    2,
	"geocoded:nominatim": 1,
}

// ProvenanceRank returns the integer rank for a provenance string. Unknown
// values return 0 (lowest priority), which means any recognised provenance
// beats an unrecognised one.
func ProvenanceRank(prov string) int {
	return provenanceRanks[prov]
}

// Config holds all tunable parameters for a Resolver. Zero values are safe;
// applyDefaults fills them before first use.
type Config struct {
	// Model is the ChatProvider model name, e.g. "llama3.1:8b". Required
	// for disambiguation and predicate classification calls.
	Model string

	// Governed enables governed-mode entity-kind minting. When true,
	// autoRegisterEntityKind and enqueueKindReview persist new kinds with
	// Status="proposed" instead of "active", routing them through the
	// `maz schema review` queue before they enter the live taxonomy.
	// When false (the default), new kinds are registered as active
	// immediately (autonomy-first permissive behaviour).
	Governed bool

	// NameEmbThreshold is the cosine similarity at or above which a
	// name-vector nearest-neighbour match is auto-confirmed. Default 0.95.
	NameEmbThreshold float64

	// SummaryAutoMergeThreshold is the cosine similarity at or above which
	// a summary-vector nearest-neighbour match is auto-confirmed. Default 0.92.
	SummaryAutoMergeThreshold float64

	// SummaryGrayLow is the lower bound of the disambiguation gray zone
	// (inclusive). Below this value a new Entity is created. Default 0.78.
	SummaryGrayLow float64

	// SummaryGrayHigh is the upper bound of the disambiguation gray zone
	// (exclusive). At or above this value the match is auto-confirmed.
	// Must equal SummaryAutoMergeThreshold. Default 0.92.
	SummaryGrayHigh float64

	// SummaryRegen is the number of new Mentions that trigger an Entity
	// summary regeneration. Default 5.
	SummaryRegen int

	// SummaryRegenInterval is the elapsed time since the last summary regen
	// that also triggers regeneration. Default 24h.
	SummaryRegenInterval time.Duration

	// SummaryMentionWindow is the number of recent Mentions fed to the
	// summary-v1 prompt. Default 20.
	SummaryMentionWindow int

	// DisambigMaxTokens caps the ToolUse response for disambiguation calls.
	// Default 256.
	DisambigMaxTokens int

	// PredicateCanonMaxTokens caps the ToolUse response for predicate
	// classification calls. Default 256.
	PredicateCanonMaxTokens int

	// ContradictionMaxTokens caps the ToolUse response for the
	// contradiction-v1 call on non-functional predicates. Default 128.
	ContradictionMaxTokens int

	// KindClassifyMaxTokens caps the ToolUse response for entity-kind
	// classification calls. Default 256.
	KindClassifyMaxTokens int

	// SummaryMaxTokens caps the Complete response for summary regeneration.
	// Default 1024.
	SummaryMaxTokens int
}

// ApplyDefaults fills any zero-valued field with its documented default.
// Exported so tests can inspect the result without constructing a Resolver.
func (c *Config) ApplyDefaults() {
	if c.NameEmbThreshold <= 0 {
		c.NameEmbThreshold = 0.95
	}
	if c.SummaryAutoMergeThreshold <= 0 {
		c.SummaryAutoMergeThreshold = 0.92
	}
	if c.SummaryGrayLow <= 0 {
		c.SummaryGrayLow = 0.78
	}
	if c.SummaryGrayHigh <= 0 {
		c.SummaryGrayHigh = 0.92
	}
	if c.SummaryRegen <= 0 {
		c.SummaryRegen = 5
	}
	if c.SummaryRegenInterval <= 0 {
		c.SummaryRegenInterval = 24 * time.Hour
	}
	if c.SummaryMentionWindow <= 0 {
		c.SummaryMentionWindow = 20
	}
	if c.DisambigMaxTokens <= 0 {
		c.DisambigMaxTokens = 256
	}
	if c.PredicateCanonMaxTokens <= 0 {
		c.PredicateCanonMaxTokens = 256
	}
	if c.ContradictionMaxTokens <= 0 {
		c.ContradictionMaxTokens = 128
	}
	if c.KindClassifyMaxTokens <= 0 {
		c.KindClassifyMaxTokens = 256
	}
	if c.SummaryMaxTokens <= 0 {
		c.SummaryMaxTokens = 1024
	}
}
