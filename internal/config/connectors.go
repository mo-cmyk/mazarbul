package config

import (
	"strings"
	"time"
)

// Freshness status tokens surfaced on recall hits for connector-sourced
// (captureExternal) episodes. They are a stable wire contract (recall hit
// envelope, agent decision rule). "fresh" = within the connector's TTL, trust
// the cached summary; "stale" = past TTL, re-fetch live and re-capture;
// "access-unverified" = the snapshot was tombstoned (404/403 reported by the
// agent), re-verify access before relying on it; "unknown" = no freshness
// signal (no SourceRef, unparseable, or no policy).
const (
	FreshnessFresh            = "fresh"
	FreshnessStale            = "stale"
	FreshnessAccessUnverified = "access-unverified"
	FreshnessUnknown          = "unknown"
)

// defaultConnectorTTL is the fallback max-age for a connector with no explicit
// policy entry: a captured snapshot older than this is "stale" and the agent
// should re-fetch before relying on it.
const defaultConnectorTTL = 24 * time.Hour

// ConnectorsTable configures agent-mediated external capture (captureExternal)
// per connector. It is the [connectors] section of config.toml.
type ConnectorsTable struct {
	// Freshness maps a connector name (notion, linear, gmail, …) to its
	// freshness budget + write-gate. An absent connector inherits the built-in
	// defaults (see DefaultConnectorFreshness); an absent table leaves the
	// defaults wholly in force.
	Freshness map[string]ConnectorFreshness `json:"freshness,omitempty" toml:"freshness,omitempty"`
}

// ConnectorFreshness is one connector's freshness policy.
type ConnectorFreshness struct {
	// TTL is the max-age after which a captured snapshot of this connector's
	// items reads as stale in recall. A Go duration string ("24h", "2h", "720h").
	// Empty inherits the default (24h).
	TTL string `json:"ttl,omitempty" toml:"ttl,omitempty"`

	// Ephemeral, when true, refuses captureExternal for this connector: its items
	// are real-time (calendar availability, a fast-moving Slack thread) and a
	// cached summary would actively mislead, so the agent reads them live each
	// time instead of caching them in maz.
	Ephemeral bool `json:"ephemeral,omitempty" toml:"ephemeral,omitempty"`
}

// DefaultConnectorFreshness returns the built-in per-connector policy. Returned
// from a function (not a package var) so the table can be merged with user
// overrides without a shared-global lint exception, and so each caller gets an
// independent copy. Durable artifacts (Notion specs, Linear issues, Granola
// notes, Drive docs) get a cache budget; real-time surfaces (Gmail, Calendar,
// Slack) are ephemeral and write-gated.
func DefaultConnectorFreshness() map[string]ConnectorFreshness {
	return map[string]ConnectorFreshness{
		"notion":  {TTL: "24h"},
		"linear":  {TTL: "2h"},
		"granola": {TTL: "720h"}, // meeting notes do not change after the meeting
		"drive":   {TTL: "24h"},
		"github":  {TTL: "24h"},
		"gmail":   {Ephemeral: true},
		"gcal":    {Ephemeral: true},
		"slack":   {Ephemeral: true},
	}
}

// FreshnessPolicy is the resolved (defaults merged with user overrides)
// connector freshness policy. The daemon builds one from UserConfig.Connectors
// and injects it into the recall tool (to classify hits) and the captureExternal
// tool (to enforce the ephemeral write-gate and stamp the per-connector TTL).
type FreshnessPolicy struct {
	ttl       map[string]time.Duration
	ephemeral map[string]bool
}

// ResolveFreshnessPolicy merges the user's [connectors.freshness] overrides over
// the built-in defaults into a ready-to-query policy. An unparseable TTL string
// falls back to the default TTL (the policy never fails closed on a typo).
func ResolveFreshnessPolicy(t ConnectorsTable) FreshnessPolicy {
	merged := DefaultConnectorFreshness()
	for name, cf := range t.Freshness {
		merged[strings.ToLower(strings.TrimSpace(name))] = cf
	}
	p := FreshnessPolicy{
		ttl:       make(map[string]time.Duration, len(merged)),
		ephemeral: make(map[string]bool, len(merged)),
	}
	for name, cf := range merged {
		if cf.Ephemeral {
			p.ephemeral[name] = true
		}
		d := defaultConnectorTTL
		if cf.TTL != "" {
			if parsed, err := time.ParseDuration(cf.TTL); err == nil && parsed > 0 {
				d = parsed
			}
		}
		p.ttl[name] = d
	}
	return p
}

// Ephemeral reports whether a connector is write-gated (captureExternal refuses
// it). Comparison is case-insensitive on the connector name.
func (p FreshnessPolicy) Ephemeral(connector string) bool {
	return p.ephemeral[strings.ToLower(strings.TrimSpace(connector))]
}

// TTL returns the freshness budget for a connector, falling back to the default
// when the connector has no explicit entry.
func (p FreshnessPolicy) TTL(connector string) time.Duration {
	if d, ok := p.ttl[strings.ToLower(strings.TrimSpace(connector))]; ok {
		return d
	}
	return defaultConnectorTTL
}

// Classify derives the freshness token for a captured snapshot from its
// connector and fetch time. A zero fetchTime is "unknown" (no signal); within
// the connector's TTL is "fresh"; past it is "stale". Tombstone handling
// (access-unverified) is applied by the caller before Classify, since it is a
// provenance fact, not a TTL computation.
func (p FreshnessPolicy) Classify(connector string, fetchTime, now time.Time) string {
	if fetchTime.IsZero() {
		return FreshnessUnknown
	}
	age := now.Sub(fetchTime)
	if age < 0 {
		age = 0
	}
	if age <= p.TTL(connector) {
		return FreshnessFresh
	}
	return FreshnessStale
}
