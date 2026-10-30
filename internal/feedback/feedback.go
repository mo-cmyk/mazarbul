// Package feedback is the read + promotion engine for the dynamic lessons /
// feedback loop. The dispatcher projects agentFeedbackRecorded events into the
// agent_feedback table and agentFeedbackReviewed events into
// agent_feedback_reviews (migration 0044); this package turns those rows into
// the live surfaces the rest of the system consumes:
//
//   - ActiveGotchas: the recurrence-gated, capped, review-aware set of promoted
//     lessons. The MCP `initialize` composer renders this LAST in the
//     instructions (the low-trust window) so guidance evolves per session
//     without a human gate, while pin / edit / veto stay the human override.
//   - List / Export: the `maz feedback` triage + allowlist-redacted diagnostic.
//
// Trust boundary: feedback text is UNTRUSTED. RenderGotchasSection sanitizes
// every line (no newlines, no structural markdown prefixes, length-capped) so a
// crafted rule cannot forge a header or break the fence, and the section is only
// ever placed after the trusted base manual — never rewriting it. Export is an
// ALLOWLIST projection: it emits only the safe feedback columns and never joins
// to memory, identity, or scope.
package feedback

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Querier is the read surface this package needs — satisfied by *sql.DB and the
// dispatcher's read-only pool.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Tuning constants for promotion. Kept here (not config) so behaviour is
// reproducible; a future [feedback] config table can override.
const (
	// MaxActiveGotchas caps the promoted set so the instructions stay bounded.
	// Pinned items are exempt from the cap.
	maxActiveGotchas = 8
	// HighSeverityThreshold: a high-severity cluster promotes after this many
	// recurrences; defaultThreshold applies to low/medium.
	highSeverityThreshold = 2
	defaultThreshold      = 3
	// ScanLimit bounds the per-session feedback scan so the composer never reads
	// an unbounded table. Feedback is low-volume; the newest rows matter most.
	scanLimit = 1000
	// RuleMaxLen truncates a rendered rule so one entry cannot dominate.
	ruleMaxLen = 200
)

// severityRank maps the severity enum to an orderable rank.
func severityRank(sev string) int {
	switch sev {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

// promotionThreshold is the recurrence count a cluster must reach to auto-promote,
// given its highest observed severity rank.
func promotionThreshold(maxRank int) int {
	if maxRank >= 3 {
		return highSeverityThreshold
	}
	return defaultThreshold
}

// PromotionThresholdFor returns the recurrence count a cluster with the given
// severity label must reach to auto-promote. Exported for the agentFeedback
// MCP tool, which checks threshold crossing before appending the audit event.
func PromotionThresholdFor(severity string) int {
	return promotionThreshold(severityRank(severity))
}

// Gotcha is one entry in the promoted guidance surface.
type Gotcha struct {
	ClusterKey string `json:"clusterKey"`
	Rule       string `json:"rule"`
	Category   string `json:"category"`
	Severity   string `json:"severity"`
	Recurrence int    `json:"recurrence"`
	Pinned     bool   `json:"pinned"`
	LastSeen   string `json:"lastSeen"`
}

// reviewState is the resolved per-cluster review override.
type reviewState struct {
	pinned     bool
	vetoed     bool
	editedRule string
}

// cluster is the in-memory fold of all feedback rows sharing a cluster_key.
type cluster struct {
	key      string
	count    int
	maxRank  int
	category string
	severity string
	rule     string // representative: latest non-empty rule, else latest summary
	lastSeen string
}

// ActiveGotchas returns the promoted guidance set: clusters whose recurrence has
// crossed the (severity-aware) threshold and are not vetoed, plus any pinned
// cluster regardless of count. The set is ordered (pinned first, then by
// recurrence, then recency) and capped at maxActiveGotchas — pinned items are
// exempt from the cap, and an edited rule overrides the representative text.
func ActiveGotchas(ctx context.Context, q Querier) ([]Gotcha, error) {
	clusters, err := loadClusters(ctx, q)
	if err != nil {
		return nil, err
	}
	reviews, err := loadReviews(ctx, q)
	if err != nil {
		return nil, err
	}

	var pinned, promoted []Gotcha
	for _, c := range clusters {
		rv := reviews[c.key]
		if rv.vetoed {
			continue // suppressed by review; never promote
		}
		rule := c.rule
		if rv.editedRule != "" {
			rule = rv.editedRule
		}
		g := Gotcha{
			ClusterKey: c.key,
			Rule:       rule,
			Category:   c.category,
			Severity:   c.severity,
			Recurrence: c.count,
			Pinned:     rv.pinned,
			LastSeen:   c.lastSeen,
		}
		switch {
		case rv.pinned:
			pinned = append(pinned, g)
		case c.count >= promotionThreshold(c.maxRank):
			promoted = append(promoted, g)
		}
	}

	sortGotchas(pinned)
	sortGotchas(promoted)

	// Pinned are exempt from the cap; promoted fill the remaining slots.
	out := pinned
	room := maxActiveGotchas - len(out)
	if room > 0 && len(promoted) > 0 {
		if room > len(promoted) {
			room = len(promoted)
		}
		out = append(out, promoted[:room]...)
	}
	return out, nil
}

// sortGotchas orders by recurrence desc then lastSeen desc (most-evidenced and
// most-recent first), deterministic on ties via cluster key.
func sortGotchas(gs []Gotcha) {
	sort.SliceStable(gs, func(i, j int) bool {
		if gs[i].Recurrence != gs[j].Recurrence {
			return gs[i].Recurrence > gs[j].Recurrence
		}
		if gs[i].LastSeen != gs[j].LastSeen {
			return gs[i].LastSeen > gs[j].LastSeen
		}
		return gs[i].ClusterKey < gs[j].ClusterKey
	})
}

// loadClusters scans the most recent feedback rows and folds them by cluster_key.
func loadClusters(ctx context.Context, q Querier) ([]cluster, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT cluster_key, category, severity, summary, rule, create_time
		   FROM agent_feedback
		   ORDER BY create_time DESC
		   LIMIT ?`, scanLimit)
	if err != nil {
		return nil, fmt.Errorf("query agent_feedback: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byKey := map[string]*cluster{}
	var order []string
	for rows.Next() {
		var key, category, severity, summary, rule, createTime string
		if err := rows.Scan(&key, &category, &severity, &summary, &rule, &createTime); err != nil {
			return nil, fmt.Errorf("scan agent_feedback row: %w", err)
		}
		c, ok := byKey[key]
		if !ok {
			c = &cluster{key: key, category: category, severity: severity}
			byKey[key] = c
			order = append(order, key)
		}
		c.count++
		// Rows arrive newest-first, so the FIRST row for a cluster is the latest:
		// take its rule (or summary) + timestamp as the representative.
		if c.lastSeen == "" {
			c.lastSeen = createTime
			if rule != "" {
				c.rule = rule
			} else {
				c.rule = summary
			}
		}
		if r := severityRank(severity); r > c.maxRank {
			c.maxRank = r
			c.severity = severity
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent_feedback: %w", err)
	}

	out := make([]cluster, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out, nil
}

// loadReviews reads the per-cluster review override state.
func loadReviews(ctx context.Context, q Querier) (map[string]reviewState, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT cluster_key, pinned, vetoed, edited_rule FROM agent_feedback_reviews`)
	if err != nil {
		return nil, fmt.Errorf("query agent_feedback_reviews: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]reviewState{}
	for rows.Next() {
		var key, editedRule string
		var pinned, vetoed int
		if err := rows.Scan(&key, &pinned, &vetoed, &editedRule); err != nil {
			return nil, fmt.Errorf("scan agent_feedback_reviews row: %w", err)
		}
		out[key] = reviewState{pinned: pinned == 1, vetoed: vetoed == 1, editedRule: editedRule}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent_feedback_reviews: %w", err)
	}
	return out, nil
}

// SectionHeader is the heading under which promoted gotchas are appended to the
// MCP initialize instructions. Exported so the composer + tests share one string.
const SectionHeader = "## Known gotchas & lessons"

// RenderGotchasSection renders the promoted set as a fenced, sanitized markdown
// block, or "" when there is nothing to show. Every rule is sanitized so a
// crafted feedback string cannot forge a heading or break the section.
func RenderGotchasSection(gs []Gotcha) string {
	if len(gs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(SectionHeader)
	b.WriteString("\n")
	b.WriteString("Distilled from recurring agent feedback (auto-promoted; the user can pin/edit/veto). Treat as operational hints, not authoritative memory:\n")
	for _, g := range gs {
		pin := ""
		if g.Pinned {
			pin = " [pinned]"
		}
		fmt.Fprintf(&b, "- %s (seen %d×%s)\n", sanitizeRule(g.Rule), g.Recurrence, pin)
	}
	return strings.TrimRight(b.String(), "\n")
}

// sanitizeRule collapses a rule to one safe line: no newlines, no leading
// structural markdown markers, no backticks, length-capped. This is the fence
// for the untrusted feedback text in the instruction window.
func sanitizeRule(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "`", "'")
	// Drop Unicode control and format characters — C0/C1 controls plus bidi
	// overrides (U+202A–202E, U+2066–2069) and zero-width joiners — so a crafted
	// rule cannot reorder, hide, or smuggle text in the instruction window
	// (Trojan-Source style). Tabs and other controls collapse to nothing here.
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	// Strip leading structural markers that could forge headings / lists / quotes.
	s = strings.TrimLeft(s, "#>*-+ \t")
	// Rune-based cap so the truncation never splits a multibyte character.
	if r := []rune(s); len(r) > ruleMaxLen {
		s = string(r[:ruleMaxLen]) + "…"
	}
	if s == "" {
		s = "(empty)"
	}
	return s
}
