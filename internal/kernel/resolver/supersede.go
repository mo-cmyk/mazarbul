package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// contradictionToolSchema is the JSON Schema for the contradiction-v1 ToolUse call.
//
//nolint:gochecknoglobals // process-lifetime constant, never mutated
var contradictionToolSchema = json.RawMessage(`{
	"type": "object",
	"required": ["contradictory", "rationale"],
	"properties": {
		"contradictory": {"type": "boolean"},
		"rationale":     {"type": "string"}
	}
}`)

// openRelationExists checks for an existing open Relation of the given trust
// status with the exact (subject_uid, predicate, object_uid) triple. Used by
// processCandidate to short-circuit duplicate appends. Returns the existing
// relation UID when found. A row is "open" when valid_until IS NULL: a
// superseded row is no longer authoritative for the triple, so a fresh
// extraction of the same fact should create a new live row rather than dedup
// against a closed one.
//
// The status filter (#211) keeps dedup within a trust tier: a proposed
// re-ingest dedups against prior proposed rows (idempotent), but a proposed
// claim never dedups against — and so never silently suppresses — a
// user-asserted fact, and an asserted fact is never dropped because a proposed
// row for the same triple already exists.
func (r *Resolver) openRelationExists(ctx context.Context, subjectUID, predicate, objectUID, status string) (string, bool, error) {
	var uid string
	err := r.reader.QueryRowContext(ctx,
		`SELECT uid FROM relations
		  WHERE subject_uid = ? AND predicate = ? AND object_uid = ?
		    AND valid_until IS NULL AND status = ?
		  LIMIT 1`,
		subjectUID, predicate, objectUID, status,
	).Scan(&uid)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("fact dedup query: %w", err)
	}
	return uid, true, nil
}

// supersedeCheck inspects whether a new RelationCandidate conflicts with an
// existing Relation in the SQLite relations table. Returns the UID of the
// Relation to supersede (if any), or empty string when no supersede is required.
//
// For functional predicates (per the predicate registry): any existing Relation
// with the same (subject_uid, predicate) and valid_until IS NULL is
// unconditionally superseded.
//
// For non-functional predicates: an LLM contradiction check via the
// contradiction-v1 prompt decides; the function returns the existing relation
// UID only when the LLM says contradictory.
func (r *Resolver) supersedeCheck(ctx context.Context, subjectUID, predicate string, candidate ontology.RelationCandidate) (string, error) {
	if r.predicates.IsFunctional(predicate) {
		return r.functionalSupersede(ctx, subjectUID, predicate)
	}
	return r.nonFunctionalSupersede(ctx, subjectUID, predicate, candidate)
}

// functionalSupersede returns the UID of the existing open Relation for the
// (subject_uid, predicate) pair, or empty string if none exists.
//
// ORDER BY uid makes the pick deterministic across devices: when a prior sync
// left more than one open row in the slot (the relationSupersedeConflict case),
// every device closes the same lowest-ULID arm. Note the limitation this does
// NOT solve: only one arm is closed here, so a slot that synced two open arms
// still surfaces a conflict via DetectConcurrentSupersede until an operator
// resolves it — closing all open arms is a deliberate non-goal for #205.
func (r *Resolver) functionalSupersede(ctx context.Context, subjectUID, predicate string) (string, error) {
	var uid string
	err := r.reader.QueryRowContext(ctx,
		// status = 'asserted' (#211): a supersede only ever closes an
		// authoritative edge. A proposed connector edge is never a supersession
		// target, so it coexists with the user fact rather than displacing it.
		`SELECT uid FROM relations
		  WHERE subject_uid = ? AND predicate = ? AND valid_until IS NULL
		    AND status = 'asserted'
		  ORDER BY uid
		  LIMIT 1`,
		subjectUID, predicate,
	).Scan(&uid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("functional supersede query: %w", err)
	}
	return uid, nil
}

// nonFunctionalSupersedeMaxOpen caps the number of open relations consulted
// during a non-functional contradiction check. With more than ~8 active facts
// for the same (subject, predicate) pair, every additional check costs an
// LLM call but adds little signal; the first match is usually the
// authoritative one (a real "supports_team Liverpool" buried under a stack of
// stale entries is rare in practice). 8 keeps worst-case latency bounded
// without dropping common-case coverage.
const nonFunctionalSupersedeMaxOpen = 8

// nonFunctionalSupersede queries up to nonFunctionalSupersedeMaxOpen open
// Relations for (subject_uid, predicate) and asks the LLM whether the new
// candidate contradicts any of them. Returns the UID of the FIRST relation
// the LLM flags as contradictory, or empty string when none match. The v1
// implementation only consulted the most-recent open relation, so a
// "supports_team Liverpool" that arrived after several stale entries
// silently coexisted with all of them despite the underlying allegiance
// being mutually exclusive.
func (r *Resolver) nonFunctionalSupersede(ctx context.Context, subjectUID, predicate string, candidate ontology.RelationCandidate) (string, error) {
	rows, err := r.reader.QueryContext(ctx,
		// status = 'asserted' (#211): a proposed connector edge is never a
		// supersession target (see functionalSupersede).
		`SELECT uid, object_uid FROM relations
		  WHERE subject_uid = ? AND predicate = ? AND valid_until IS NULL
		    AND status = 'asserted'
		  ORDER BY create_time DESC
		  LIMIT ?`,
		subjectUID, predicate, nonFunctionalSupersedeMaxOpen,
	)
	if err != nil {
		return "", fmt.Errorf("non-functional supersede query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type openRel struct {
		uid, objectUID string
	}
	var open []openRel
	for rows.Next() {
		var uid, objectUID string
		if err := rows.Scan(&uid, &objectUID); err != nil {
			return "", fmt.Errorf("non-functional supersede scan row: %w", err)
		}
		open = append(open, openRel{uid: uid, objectUID: objectUID})
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("non-functional supersede iterate: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("non-functional supersede close rows: %w", err)
	}
	if len(open) == 0 {
		return "", nil
	}

	for _, existing := range open {
		contradictory, err := r.llmContradictionCheck(ctx, predicate, existing.objectUID, candidate)
		if err != nil {
			// Conservative: on error do not supersede. Log at Warn so operators see
			// the degraded mode; a silent swallow was masking provider timeouts and
			// silently disabling non-functional supersede in production.
			r.logger.Warn("Resolver: contradiction check failed, treating as no-supersede",
				zap.String("predicate", predicate),
				zap.String("subjectUid", subjectUID),
				zap.String("existingRelationUid", existing.uid),
				zap.Error(err))
			continue
		}
		if contradictory {
			return existing.uid, nil
		}
	}
	return "", nil
}

// llmContradictionCheck calls the contradiction-v1 prompt to decide whether
// a new RelationCandidate contradicts an existing Relation.
func (r *Resolver) llmContradictionCheck(ctx context.Context, predicate, existingObjectUID string, candidate ontology.RelationCandidate) (bool, error) {
	existingJSON, err := json.Marshal(map[string]string{
		"predicate": predicate,
		"objectUid": existingObjectUID,
	})
	if err != nil {
		return false, fmt.Errorf("marshal existing relation: %w", err)
	}

	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		return false, fmt.Errorf("marshal candidate: %w", err)
	}

	system, err := r.promptReg.Render(ctx, contradictionTemplateName, contradictionTemplateVersion, prompts.RenderVars{
		"existing":  strings.TrimSpace(string(existingJSON)),
		"candidate": strings.TrimSpace(string(candidateJSON)),
	})
	if err != nil {
		return false, fmt.Errorf("render contradiction prompt: %w", err)
	}

	resp, err := r.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:  r.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: "Check whether the new candidate contradicts the existing relation."},
		},
		Tool: kernel.ToolSchema{
			Name:        "check_contradiction",
			Description: "Return whether the new relation candidate contradicts the existing relation.",
			InputSchema: contradictionToolSchema,
		},
		MaxTokens: r.cfg.ContradictionMaxTokens,
	})
	if err != nil {
		return false, fmt.Errorf("contradiction tool-use: %w", err)
	}

	if resp.ToolUse == nil {
		return false, nil
	}

	var out struct {
		Contradictory bool   `json:"contradictory"`
		Rationale     string `json:"rationale"`
	}
	if err := json.Unmarshal(resp.ToolUse.Input, &out); err != nil {
		return false, fmt.Errorf("contradiction: parse tool output: %w", err)
	}
	return out.Contradictory, nil
}

// crossSubjectRoleSupersede returns the UIDs of OPEN relations that share
// the same role-at-org slot as the candidate but a DIFFERENT subject. The
// classic case: "Rosie is CEO of Erebor" exists; "Frodo is now CEO of
// Erebor" arrives; the slot (object=Erebor, predicate=works_at, role=CEO)
// is functional and Rosie's row must be closed even though the subjects
// differ. The regular functionalSupersede only matches on (subject,
// predicate) and silently skips this case.
//
// The function is opt-in via isSingularRole: roles like "engineer" or
// "member" or "lead" are filled by many people simultaneously and are
// NOT auto-superseded across subjects. Only singular executive roles
// (CEO, CTO, CFO, COO, founder, president, chair) trigger the close.
//
// Role strings are normalised before comparison so "CEO" matches "ceo"
// matches "Chief Executive Officer" via the seeded alias map. Returns
// an empty slice (no error) when no rows match.
func (r *Resolver) crossSubjectRoleSupersede(ctx context.Context, subjectUID, predicate, objectUID, role string) ([]string, error) {
	normalised := normalizeRole(role)
	if normalised == "" {
		return nil, nil
	}
	if _, ok := singularRoles[normalised]; !ok {
		return nil, nil
	}

	rows, err := r.reader.QueryContext(ctx,
		// status = 'asserted' (#211): only authoritative incumbents are closed by
		// a cross-subject role supersede; a proposed connector edge is neither a
		// target here (this query) nor a trigger (processCandidate skips this
		// path entirely for untrusted input).
		`SELECT uid, attributes FROM relations
		  WHERE object_uid = ? AND predicate = ?
		    AND subject_uid <> ?
		    AND valid_until IS NULL
		    AND status = 'asserted'
		    AND attributes != '{}' AND attributes IS NOT NULL`,
		objectUID, predicate, subjectUID,
	)
	if err != nil {
		return nil, fmt.Errorf("cross-subject role supersede query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []string
	for rows.Next() {
		var uid, attrsJSON string
		if err := rows.Scan(&uid, &attrsJSON); err != nil {
			return nil, fmt.Errorf("cross-subject role supersede scan: %w", err)
		}
		var attrs map[string]string
		if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
			// Malformed attributes is non-fatal: skip the row, log, and
			// continue. The supersede pipeline must not crash on one bad
			// row in the projection.
			r.logger.Warn("Resolver: malformed attributes JSON on existing relation",
				zap.String("relationUid", uid),
				zap.String("attributes", attrsJSON),
				zap.Error(err),
			)
			continue
		}
		if normalizeRole(attrs["role"]) == normalised {
			hits = append(hits, uid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cross-subject role supersede iterate: %w", err)
	}
	return hits, nil
}

// singularRoles enumerates canonical role tokens that uniquely identify
// one incumbent at an organisation. A new relation whose role normalises
// into this set closes any open relation with the same (object,
// predicate, role) slot but a different subject. The list is intentionally
// conservative: only executive-style "there is exactly one" roles.
//
// Roles like "engineer", "member", "lead", "manager", "developer",
// "contributor" are filled by many people at once and must NOT trigger
// cross-subject supersession; they are absent from this set by design.
//
//nolint:gochecknoglobals // process-lifetime lookup table, never mutated
var singularRoles = map[string]struct{}{
	"ceo":               {},
	"cto":               {},
	"cfo":               {},
	"coo":               {},
	"cmo":               {},
	"cpo":               {},
	"cio":               {},
	"cso":               {},
	"president":         {},
	"chair":             {},
	"chairman":          {},
	"chairwoman":        {},
	"founder":           {},
	"managing director": {},
}

// roleAliases maps lowercase verbose forms to the canonical short token
// in singularRoles. After applying alias lookup the result is checked
// against singularRoles, so "Chief Executive Officer" -> "ceo" -> singular.
// Lowercase keys only; normalizeRole lowercases the input before lookup.
//
//nolint:gochecknoglobals // process-lifetime alias map, never mutated
var roleAliases = map[string]string{
	"chief executive officer":   "ceo",
	"chief executive":           "ceo",
	"chief technology officer":  "cto",
	"chief technologist":        "cto",
	"chief financial officer":   "cfo",
	"chief operating officer":   "coo",
	"chief marketing officer":   "cmo",
	"chief product officer":     "cpo",
	"chief information officer": "cio",
	"chief security officer":    "cso",
	"chair of the board":        "chair",
	"chairman of the board":     "chairman",
	"chairwoman of the board":   "chairwoman",
	"co-founder":                "founder",
	"cofounder":                 "founder",
}

// normalizeRole canonicalises a role string for slot comparison. Lowercase,
// trim whitespace, strip leading articles ("the", "a", "an") and trailing
// punctuation, then apply the alias map. "  the CEO " and "Chief Executive
// Officer" both normalise to "ceo".
//
// Returns the lowercase trimmed token when no alias matches; callers that
// also need the singular check should use isSingularRole on the original
// string (which composes normalisation + allowlist lookup).
func normalizeRole(role string) string {
	r := strings.TrimRight(strings.ToLower(strings.TrimSpace(role)), ".,!?")
	for _, prefix := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(r, prefix) {
			r = r[len(prefix):]
			break
		}
	}
	if canon, ok := roleAliases[r]; ok {
		return canon
	}
	return r
}

// isSingularRole reports whether the role string identifies a unique
// incumbent at an organisation (CEO, CTO, founder, ...). Composes
// normalisation + the singularRoles allowlist; safe to call on raw
// extraction output.
func isSingularRole(role string) bool {
	_, ok := singularRoles[normalizeRole(role)]
	return ok
}
