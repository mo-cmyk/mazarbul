package stats

// Price is the per-million-token price (USD) for a single
// (provider, model, kind) tuple. Embedded prices are best-effort
// defaults; users override via ~/.config/mazarbul/prices.toml.
type Price struct {
	Provider     string  `toml:"provider"`
	Model        string  `toml:"model"`
	Kind         string  `toml:"kind"`
	USDPerMToken float64 `toml:"usdPerMToken"`
}

// DefaultPrices returns the embedded price table. Numbers reflect
// public pricing as of early 2026; users override via
// ~/.config/mazarbul/prices.toml.
//
// Anthropic prompt-cache pricing follows the published multipliers:
//   - cache write (5m TTL): 1.25x input
//   - cache read:           0.10x input
//
// Returning a slice (rather than exposing a package-level table)
// keeps the value immutable from callers' perspective and avoids a
// long-lived global the linter would flag.
func DefaultPrices() []Price {
	return []Price{
		{Provider: "anthropic", Model: "claude-sonnet-4-6", Kind: "input", USDPerMToken: 3.00},
		{Provider: "anthropic", Model: "claude-sonnet-4-6", Kind: "output", USDPerMToken: 15.00},
		{Provider: "anthropic", Model: "claude-sonnet-4-6", Kind: "cache-read", USDPerMToken: 0.30},
		{Provider: "anthropic", Model: "claude-sonnet-4-6", Kind: "cache-create", USDPerMToken: 3.75},

		{Provider: "anthropic", Model: "claude-haiku-4-5", Kind: "input", USDPerMToken: 0.80},
		{Provider: "anthropic", Model: "claude-haiku-4-5", Kind: "output", USDPerMToken: 4.00},
		{Provider: "anthropic", Model: "claude-haiku-4-5", Kind: "cache-read", USDPerMToken: 0.08},
		{Provider: "anthropic", Model: "claude-haiku-4-5", Kind: "cache-create", USDPerMToken: 1.00},

		{Provider: "anthropic", Model: "claude-opus-4-7", Kind: "input", USDPerMToken: 15.00},
		{Provider: "anthropic", Model: "claude-opus-4-7", Kind: "output", USDPerMToken: 75.00},
		{Provider: "anthropic", Model: "claude-opus-4-7", Kind: "cache-read", USDPerMToken: 1.50},
		{Provider: "anthropic", Model: "claude-opus-4-7", Kind: "cache-create", USDPerMToken: 18.75},

		{Provider: "ollama", Model: "*", Kind: "input", USDPerMToken: 0},
		{Provider: "ollama", Model: "*", Kind: "output", USDPerMToken: 0},
		{Provider: "ollama", Model: "*", Kind: "embed", USDPerMToken: 0},
	}
}

// LookupPrice returns the USD-per-million price for the given key.
// Wildcard "*" model matches any model under a provider, so local
// providers (Ollama) don't need per-model rows. Returns 0 + false when
// no price is known — callers should treat unknown costs as $0 rather
// than refuse to render.
func LookupPrice(table []Price, provider, model string, kind Kind) (float64, bool) {
	wireKind := wireKindOf(kind)
	for _, p := range table {
		if p.Provider != provider {
			continue
		}
		if p.Kind != wireKind {
			continue
		}
		if p.Model == model || p.Model == "*" {
			return p.USDPerMToken, true
		}
	}
	return 0, false
}

// wireKindOf maps the in-memory Kind enum to the price table's string
// kind. Direction is encoded into the kind for input/output regular
// charges — the price table doesn't carry a separate direction column.
func wireKindOf(k Kind) string {
	switch k {
	case KindCacheRead:
		return "cache-read"
	case KindCacheCreate:
		return "cache-create"
	case KindEmbed:
		return "embed"
	default:
		return "regular"
	}
}
