package daemon

import (
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// defaultRecallEmbedDeadline bounds the per-recall query-embedding call when the
// user sets no embed_deadline_ms. A warm embed returns in well under a second;
// this only bites when the model must cold-load (Ollama just started, or the
// model was evicted after keep_alive). Without a bound the embed blocks on the
// 300s tool backstop and overruns a client's ~240s deadline, so recall hangs
// for minutes instead of degrading to FTS-only within seconds. Boot-time
// pre-warm (wireAndBuildServer) makes a cold first call rare; this caps the
// worst case when the model is not resident.
const defaultRecallEmbedDeadline = 20 * time.Second

// applyRecallTuning maps a config.RecallTable onto a constructed HybridQuery.
// Knobs whose zero value is meaningful — graph_weight, name_emb_weight,
// two_hop_floor — are *pointers* in the table, so an explicit 0 is applied
// (0 disables the lane / 2-hop expansion) and only an absent key falls
// through to the kernel default. The remaining knobs treat their zero value
// as "disabled", which already equals the kernel default, so a value gate is
// unambiguous for them. HybridOption is a func(*HybridQuery) so the setters
// apply in place; no rebuild needed.
//
// The helper logs each applied knob at Info because LongMemEval tuning loops
// edit the config between runs and operators want to confirm in the daemon
// log that the new value took effect. Negative weights are still rejected by
// the kernel setters; an explicit 0 now disables the lane as written rather
// than being silently swallowed.
//
//nolint:gocritic // hugeParam: RecallTable is intentionally passed by value as an immutable config snapshot across the wiring boundary.
func applyRecallTuning(h *kernel.HybridQuery, cfg config.RecallTable, logger *zap.Logger) {
	if h == nil {
		return
	}
	applied := false
	if cfg.TwoHopFloor != nil {
		kernel.WithTwoHopFloor(*cfg.TwoHopFloor)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "two_hop_floor"), zap.Int("value", *cfg.TwoHopFloor))
		applied = true
	}
	if cfg.GraphWeight != nil {
		kernel.WithGraphWeight(*cfg.GraphWeight)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "graph_weight"), zap.Float64("value", *cfg.GraphWeight), zap.String("note", "rrf-only; not consulted under the default weighted fusion"))
		applied = true
	}
	if cfg.NameEmbWeight != nil {
		kernel.WithNameEmbWeight(*cfg.NameEmbWeight)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "name_emb_weight"), zap.Float64("value", *cfg.NameEmbWeight), zap.String("note", "rrf-only; not consulted under the default weighted fusion"))
		applied = true
	}
	if cfg.RRFMinScore != nil {
		kernel.WithRRFMinScore(*cfg.RRFMinScore)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "rrf_min_score"), zap.Float32("value", *cfg.RRFMinScore))
		applied = true
	}
	if len(cfg.EdgeAllowlist) > 0 {
		kernel.WithEdgeAllowlist(cfg.EdgeAllowlist...)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "edge_allowlist"), zap.Strings("value", cfg.EdgeAllowlist))
		applied = true
	}
	if cfg.MMRLambda != nil {
		kernel.WithMMRLambda(*cfg.MMRLambda)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "mmr_lambda"), zap.Float64("value", *cfg.MMRLambda))
		applied = true
	}
	if applyRecallDecayKnobs(h, cfg, logger) {
		applied = true
	}
	if cfg.FusionMode != "" {
		kernel.WithFusionMode(cfg.FusionMode)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "fusion_mode"), zap.String("value", cfg.FusionMode))
		applied = true
	}
	if cfg.SemanticGateThreshold > 0 {
		kernel.WithSemanticGateThreshold(cfg.SemanticGateThreshold)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "semantic_gate_threshold"), zap.Float64("value", cfg.SemanticGateThreshold))
		applied = true
	}
	if len(cfg.TypeWeights) > 0 {
		kernel.WithTypeWeights(cfg.TypeWeights)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "type_weights"), zap.Any("value", cfg.TypeWeights))
		applied = true
	}
	if cfg.RerankEnabled {
		kernel.WithRerankEnabled(true)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "rerank"), zap.Bool("value", true))
		applied = true
	}
	// Lane weights (cfg.SemanticWeight / BM25Weight / EntityWeight) are
	// retained in the TOML schema for backward compatibility but no
	// longer wired: WeightedComponents now derives weights from mem0
	// v3's adaptive sum/max_possible normalization rather than a fixed
	// (0.5, 0.3, 0.2) convex blend. Weighted fusion is now the DEFAULT,
	// so these weights stay ignored; graph_weight / name_emb_weight
	// (applied above) are RRF-only knobs too — under weighted fusion the
	// graph and name-embed ranks feed the entity-boost term directly.
	if cfg.EmbedDeadlineMs > 0 {
		d := time.Duration(cfg.EmbedDeadlineMs) * time.Millisecond
		kernel.WithEmbedDeadline(d)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "embed_deadline_ms"), zap.Int("value", cfg.EmbedDeadlineMs))
		applied = true
	} else {
		// No explicit deadline configured: bound the embed so a cold/evicted
		// model degrades recall to FTS-only within seconds instead of hanging
		// on the tool backstop (feedback #3). Not counted as user "tuning".
		kernel.WithEmbedDeadline(defaultRecallEmbedDeadline)(h)
		logger.Debug("daemon: recall embed deadline defaulted", zap.Duration("value", defaultRecallEmbedDeadline))
	}
	if !applied {
		logger.Debug("daemon: no recall tuning in user config; using kernel defaults")
	}
}

// applyRecallDecayKnobs maps the post-fusion time-/usage-decay knobs — the
// Gaussian recency boost and the passive-reinforcement (memory-strength) boost
// of the cognitive lifecycle (issue #231) — onto h, returning whether any were
// applied. Extracted from applyRecallTuning to keep that function's cyclomatic
// complexity within budget. Each knob treats its zero value as "absent → kernel
// default"; strength_boost defaults on (0.15) so the cognitive ranking is live
// without config, and a positive override only retunes it.
//
//nolint:gocritic // hugeParam: RecallTable is an immutable config snapshot passed by value across the wiring boundary.
func applyRecallDecayKnobs(h *kernel.HybridQuery, cfg config.RecallTable, logger *zap.Logger) bool {
	applied := false
	if cfg.RecencyBoost != nil {
		kernel.WithRecencyBoost(*cfg.RecencyBoost)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "recency_boost"), zap.Float64("value", *cfg.RecencyBoost))
		applied = true
	}
	if cfg.RecencyHalflifeDays > 0 {
		kernel.WithRecencyHalflife(cfg.RecencyHalflifeDays)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "recency_halflife_days"), zap.Float64("value", cfg.RecencyHalflifeDays))
		applied = true
	}
	if cfg.StrengthBoost > 0 {
		kernel.WithStrengthBoost(cfg.StrengthBoost)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "strength_boost"), zap.Float64("value", cfg.StrengthBoost))
		applied = true
	}
	if cfg.StrengthHalflifeDays > 0 {
		kernel.WithStrengthHalflife(cfg.StrengthHalflifeDays)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "strength_halflife_days"), zap.Float64("value", cfg.StrengthHalflifeDays))
		applied = true
	}
	if cfg.SalienceBase > 0 {
		kernel.WithSalienceBase(cfg.SalienceBase)(h)
		logger.Info("daemon: recall tuning applied", zap.String("knob", "salience_base"), zap.Float64("value", cfg.SalienceBase))
		applied = true
	}
	return applied
}
