package feedback

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Filter narrows a List/Export read. Empty fields are wildcards.
type Filter struct {
	Category string // exact category match, "" = any
	Severity string // exact severity match, "" = any
	Surface  string // exact surface match, "" = any
	// Since is a UTC time lower bound on create_time. Zero = no bound.
	// Compared as time.Time so sub-second timestamps within the same second
	// are included (avoids the ASCII-sort pitfall where "...49Z" < "...49.852Z").
	Since time.Time
	Limit int // 0 = scanLimit
}

// ParseSince parses a --since string as strict RFC3339 and returns the
// corresponding time.Time. A non-empty value that does not parse as RFC3339
// (including date-only values like "2026-06-24") is an error. Empty returns
// zero time with no error.
func ParseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q is not valid RFC3339 (want e.g. 2026-06-24T00:00:00Z): %w", s, err)
	}
	return t.UTC(), nil
}

// Item is a triage row for `maz feedback list` (local-only; carries the
// cluster key so a reviewer can pin/veto it).
type Item struct {
	UID        string `json:"uid"`
	ClusterKey string `json:"clusterKey"`
	Category   string `json:"category"`
	Severity   string `json:"severity"`
	Surface    string `json:"surface,omitempty"`
	Summary    string `json:"summary"`
	CreateTime string `json:"createTime"`
}

// ExportItem is the ALLOWLIST projection for `maz feedback export`. It is the
// security contract for the diagnostic: ONLY these fields are ever emitted.
// Deliberately absent: scope, cluster_key, source_event_uid, and anything from
// the memory / identity / team tables. The export query reads only the
// agent_feedback table and never joins, so user memory, principal ULIDs, and
// team scope cannot leak into a shared diagnostic.
type ExportItem struct {
	ID           string   `json:"id"`
	Timestamp    string   `json:"ts"`
	Category     string   `json:"category"`
	Severity     string   `json:"severity"`
	Surface      string   `json:"surface,omitempty"`
	Summary      string   `json:"summary"`
	Detail       string   `json:"detail,omitempty"`
	Keywords     []string `json:"keywords,omitempty"`
	Version      string   `json:"version,omitempty"`
	ActivePreset string   `json:"activePreset,omitempty"`
}

// whereClause builds the optional filter predicate + args shared by List/Export.
func (f *Filter) whereClause() (string, []any) {
	var conds []string
	var args []any
	if f.Category != "" {
		conds = append(conds, "category = ?")
		args = append(args, f.Category)
	}
	if f.Severity != "" {
		conds = append(conds, "severity = ?")
		args = append(args, f.Severity)
	}
	if f.Surface != "" {
		conds = append(conds, "surface = ?")
		args = append(args, f.Surface)
	}
	if !f.Since.IsZero() {
		// Rows may be stored with sub-second RFC3339Nano precision
		// (e.g. "2026-06-24T08:18:49.852Z"). In ASCII/lexicographic order
		// "." (46) < "Z" (90), so a bare-second bound like "...49Z" sorts
		// AFTER "...49.852Z" and would exclude sub-second entries at the same
		// second. Force a ".000000000Z" suffix so the bound lexicographically
		// precedes any fractional-second timestamp at that instant.
		conds = append(conds, "create_time >= ?")
		args = append(args, f.Since.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (f *Filter) limit() int {
	if f.Limit <= 0 {
		return scanLimit
	}
	return f.Limit
}

// List returns triage rows newest-first, narrowed by filter.
func List(ctx context.Context, q Querier, f *Filter) ([]Item, error) {
	where, args := f.whereClause()
	args = append(args, f.limit())
	rows, err := q.QueryContext(ctx,
		`SELECT uid, cluster_key, category, severity, surface, summary, create_time
		   FROM agent_feedback`+where+`
		   ORDER BY create_time DESC
		   LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query feedback list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.UID, &it.ClusterKey, &it.Category, &it.Severity,
			&it.Surface, &it.Summary, &it.CreateTime); err != nil {
			return nil, fmt.Errorf("scan feedback list row: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feedback list: %w", err)
	}
	return out, nil
}

// Export returns the allowlist-redacted diagnostic rows. It SELECTs only the
// safe columns by name — never `scope`, `source_event_uid`, or `cluster_key`,
// and never joins another table — so the redaction is structural, not a filter
// applied after the fact.
func Export(ctx context.Context, q Querier, f *Filter) ([]ExportItem, error) {
	where, args := f.whereClause()
	args = append(args, f.limit())
	rows, err := q.QueryContext(ctx,
		`SELECT uid, create_time, category, severity, surface, summary, detail, keywords, version, active_preset
		   FROM agent_feedback`+where+`
		   ORDER BY create_time DESC
		   LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query feedback export: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ExportItem{}
	for rows.Next() {
		var it ExportItem
		var keywords string
		if err := rows.Scan(&it.ID, &it.Timestamp, &it.Category, &it.Severity,
			&it.Surface, &it.Summary, &it.Detail, &keywords, &it.Version, &it.ActivePreset); err != nil {
			return nil, fmt.Errorf("scan feedback export row: %w", err)
		}
		if kw := strings.Fields(keywords); len(kw) > 0 {
			it.Keywords = kw
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feedback export: %w", err)
	}
	return out, nil
}

// RenderExportMarkdown renders export items as a human-readable markdown
// diagnostic (the --format md option).
func RenderExportMarkdown(items []ExportItem) string {
	var b strings.Builder
	b.WriteString("# Mazarbul feedback diagnostic\n\n")
	if len(items) == 0 {
		b.WriteString("_No feedback recorded._\n")
		return b.String()
	}
	for i := range items {
		it := &items[i]
		fmt.Fprintf(&b, "## %s — %s/%s\n", it.Timestamp, it.Category, it.Severity)
		if it.Surface != "" {
			fmt.Fprintf(&b, "- surface: %s\n", it.Surface)
		}
		fmt.Fprintf(&b, "- summary: %s\n", it.Summary)
		if it.Detail != "" {
			fmt.Fprintf(&b, "- detail: %s\n", it.Detail)
		}
		if it.Version != "" {
			fmt.Fprintf(&b, "- version: %s\n", it.Version)
		}
		b.WriteString("\n")
	}
	return b.String()
}
