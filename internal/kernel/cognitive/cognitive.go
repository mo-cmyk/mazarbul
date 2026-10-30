// Package cognitive implements the autonomous LLM jobs of the cognitive memory
// lifecycle (issue #231): reflection (insight synthesis over entity clusters)
// and consolidation (compression of recent episodes). Both append typed,
// auditable events that cite their sources (sourceUids) and degrade gracefully
// when the LLM backend is unavailable — a tripped circuit breaker is a clean
// skip, never a daemon crash (#236).
package cognitive

import (
	"context"
	"errors"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/provider/resilience"
)

// Appender is the event-log surface the cognitive jobs append derived events
// to. Satisfied by *kernel.FileEventLog (orch.Log).
type Appender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// Deduper checks a candidate text against a vector domain for a near-duplicate
// at or above the given cosine threshold. Satisfied by
// *dispatcher.Dispatcher.CheckDuplicate. Used by consolidation to avoid emitting
// a near-duplicate consolidated memory (no orphan derived facts).
type Deduper interface {
	CheckDuplicate(ctx context.Context, domain, candidateUID, text string, threshold float64) (string, []float32, error)
}

// Result summarizes one job pass for the manual-trigger MCP tools and the Jobs
// panel. AppendedEventUIDs lists the events the pass appended; Skipped carries a
// non-empty reason when the pass produced nothing (breaker-open, no-candidates).
type Result struct {
	AppendedEventUIDs []string `json:"appendedEventUids"`
	Skipped           string   `json:"skipped,omitempty"`
}

// consolidationDedupThreshold is the cosine-similarity floor above which a
// candidate consolidated memory is treated as a duplicate of an existing one
// and skipped. Matches the dispatcher's episode dedup default register.
const consolidationDedupThreshold = 0.92

// isBreakerOpen reports whether err is the resilience circuit-breaker "open"
// signal — the LLM backend is unavailable. The jobs treat it as a clean skip
// (retry next tick), never a propagated failure that could crash the daemon.
func isBreakerOpen(err error) bool {
	return errors.Is(err, resilience.ErrOpen)
}

// clampConfidence pins a model-supplied confidence into [0, 1].
func clampConfidence(c float64) float64 {
	switch {
	case c < 0:
		return 0
	case c > 1:
		return 1
	default:
		return c
	}
}

// newULID mints a fresh random ULID for a derived record's own identity.
func newULID() string { return ulid.Make().String() }

// firstNonEmpty returns s when non-blank, else fallback. Used to default an
// empty model-supplied type field to a sane taxonomy value.
func firstNonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// filterToCandidates keeps only the UIDs the model cited that were actually
// offered in the candidate set — rejecting hallucinated provenance. This is the
// auditability guarantee: a synthesized fact may only cite sources it was shown.
func filterToCandidates(cited []string, candidates map[string]struct{}) []string {
	out := make([]string, 0, len(cited))
	seen := make(map[string]struct{}, len(cited))
	for _, uid := range cited {
		if uid == "" {
			continue
		}
		if _, ok := candidates[uid]; !ok {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	return out
}
