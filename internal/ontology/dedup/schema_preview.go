package dedup

// schema_preview.go provides pure, side-effect-free computation of what a
// defineEntityKind or defineSchema change would do, used by the preview=true
// path and the `maz schema preview` CLI subcommand.

import (
	"encoding/json"
	"sort"

	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// KindPreviewRequest describes the candidate kind change to preview.
type KindPreviewRequest struct {
	// Name is the bare snake_case kind name (no namespace prefix).
	Name string
	// Description is the one-sentence explanation, used for the collapse check.
	Description string
	// PropertySchema is the proposed JSON Schema for typed attributes. When
	// non-nil, orphan analysis compares its properties against ExistingProperties.
	PropertySchema json.RawMessage
	// CloseMatchThreshold is the lexical-similarity floor (0..1) above which the
	// nearest existing kind is reported as a close match and a "mint" verdict is
	// downgraded with a MAZ-KIND-001 warning. Zero falls back to MintAliasThreshold,
	// the same floor the live defineEntityKind dedup uses for closeMatch.
	CloseMatchThreshold float64
	// SemanticChecked records whether a semantic (embedding) dedup pass was run
	// in addition to the lexical near-duplicate scan. When false, PlanKind reports
	// ChecksSkipped=["semantic"] and emits a MAZ-KIND-002 warning so the caller
	// knows the mint verdict is lexical-only. The preview path itself never runs
	// the embedder; this flag lets the caller declare whether one is wired.
	SemanticChecked bool
}

// NearestKind is one existing kind ranked by lexical similarity to the
// candidate, surfaced on the mint preview so a caller can judge "would this
// collapse into something I already have?".
type NearestKind struct {
	// Kind is the fully-qualified registered kind name, e.g. "user:vehicle".
	Kind string `json:"kind"`
	// Similarity is the lexical name-similarity score (0..1) against Kind.
	Similarity float64 `json:"similarity"`
}

// ExistingKind is a snapshot of a registered kind's current state.
type ExistingKind struct {
	// Name is the fully-qualified registered name, e.g. "user:vehicle".
	Name string
	// Description is the current one-sentence explanation.
	Description string
	// PropertySchema is the current JSON Schema (nil when absent).
	PropertySchema json.RawMessage
	// EntityCount is the number of entities currently using this kind.
	EntityCount int
}

// KindPreviewResult is the output of PlanKind.
type KindPreviewResult struct {
	// Action describes the planned operation: "mint", "supersede", or "collapse".
	Action string

	// CollapsesInto is the fully-qualified name of the kind the candidate would
	// collapse into (only set when Action=="collapse").
	CollapsesInto string

	// SupersedesFrom is the current schema version string when Action=="supersede"
	// and the kind already has a registered propertySchema.
	SupersedesFrom string

	// OrphanCount is the number of entities that rely on a property field that
	// would be removed or renamed by the candidate propertySchema. Zero when the
	// candidate adds fields only (backward-compatible) or when no existing schema
	// is registered. Populated for both "supersede" and "mint" actions when a
	// prior propertySchema exists.
	OrphanCount int

	// CloseMatch is the name of the nearest existing kind when the candidate
	// would mint but its top lexical similarity crosses CloseMatchThreshold. It
	// mirrors the closeMatch field defineEntityKind sets on a live MintWithAlias:
	// the verdict stays "mint", but the caller is warned (MAZ-KIND-001) that an
	// existing kind is close enough to reuse instead. Empty otherwise.
	CloseMatch string

	// NearestKinds lists existing kinds ranked by lexical name-similarity to the
	// candidate, highest first. Populated only for an Action=="mint" verdict (the
	// collapse/supersede branches already name the matched kind), so a caller can
	// answer "would this collapse into something I already have?" before writing.
	NearestKinds []NearestKind

	// ChecksSkipped names the dedup layers that did NOT run. Currently the only
	// member is "semantic", set when SemanticChecked was false (no embedder
	// wired), signalling that the mint verdict is lexical-only. Nil when every
	// layer ran. Mirrors the MAZ-KIND-002 "dedup degraded" semantics.
	ChecksSkipped []string

	// Warnings lists the MAZ-SCHEMA-NNN / MAZ-KIND-NNN advisory codes predicted
	// for this change.
	Warnings []warncode.Warning
}

// SchemaPreviewRequest describes the candidate schema change to preview.
type SchemaPreviewRequest struct {
	// SchemaKind is the schema kind identifier (bare camelCase or colon-namespaced).
	SchemaKind string
	// Version is the proposed semver string, e.g. "v1.2.0".
	Version string
	// Definition is the proposed JSON Schema document.
	Definition json.RawMessage
}

// ExistingSchema is a snapshot of a registered schema's current state.
type ExistingSchema struct {
	// SchemaKind is the schema kind identifier.
	SchemaKind string
	// Version is the currently registered semver string.
	Version string
	// Definition is the current JSON Schema document.
	Definition json.RawMessage
}

// SchemaPreviewResult is the output of PlanSchema.
type SchemaPreviewResult struct {
	// Action describes the planned operation: "register" (first time) or "supersede".
	Action string

	// SupersedesFrom is the currently registered version string when Action=="supersede".
	SupersedesFrom string

	// OrphanCount is the number of entities whose properties reference a field
	// removed or renamed by the candidate definition.
	OrphanCount int

	// Warnings lists the MAZ-SCHEMA-NNN advisory codes predicted for this change.
	Warnings []warncode.Warning
}

// PlanKind computes the preview result for a defineEntityKind call with
// preview=true. It is pure: it reads from existing and candidate without
// touching the event log.
//
// Decision rules (evaluated in order):
//
//  1. Collapse via cross-namespace synonym: any existing kind (with a different
//     canonical name) whose normalized form matches the candidate → action="collapse".
//  2. Collapse via re-registration: the exact canonical name (user:<name>) is
//     already registered AND the candidate does not update the propertySchema
//     → action="collapse" (idempotent re-registration, nothing new would be written).
//  3. Supersede: the exact canonical name is already registered AND the candidate
//     also provides a new propertySchema → action="supersede". Orphan analysis
//     compares the property key sets; removed keys emit MAZ-SCHEMA-002.
//  4. Mint: no matching existing kind → action="mint". The same lexical
//     near-duplicate scan defineEntityKind runs (nameSimilarity over registered
//     kinds) populates NearestKinds; when the top score crosses
//     CloseMatchThreshold the verdict stays "mint" but CloseMatch is set and a
//     MAZ-KIND-001 warning is emitted, so a caller can see "this would collapse
//     into <kind>" before writing. When SemanticChecked is false a MAZ-KIND-002
//     warning records that the verdict is lexical-only.
//
// Orphan detection uses EntityCount as a conservative upper bound (every entity
// of that kind, not just those with the removed property set).
func PlanKind(req KindPreviewRequest, existing []ExistingKind) KindPreviewResult {
	candidate := "user:" + req.Name
	normCand := normalizeName(req.Name)

	// Scan once: collect exact canonical match and cross-namespace normalized match.
	var exactMatch, normMatch *ExistingKind
	for i := range existing {
		ek := &existing[i]
		if ek.Name == candidate {
			exactMatch = ek
		} else if normalizeName(ek.Name) == normCand && normMatch == nil {
			normMatch = ek
		}
	}

	// Rule 1: cross-namespace synonym → collapse into the other kind.
	if normMatch != nil {
		return KindPreviewResult{
			Action:        "collapse",
			CollapsesInto: normMatch.Name,
			Warnings: []warncode.Warning{
				warncode.New(warncode.CodeSchemaCollapse,
					"candidate kind %q collapses into existing kind %q (normalized name match across namespaces); use that kind instead",
					candidate, normMatch.Name),
			},
		}
	}

	// Rule 3: exact canonical match + schema update → supersede.
	if exactMatch != nil && len(exactMatch.PropertySchema) > 0 && len(req.PropertySchema) > 0 {
		result := KindPreviewResult{
			Action:         "supersede",
			SupersedesFrom: "existing schema for " + exactMatch.Name,
		}
		orphans := countOrphanedFields(exactMatch.PropertySchema, req.PropertySchema)
		if orphans > 0 {
			result.OrphanCount = exactMatch.EntityCount
			result.Warnings = append(result.Warnings,
				warncode.New(warncode.CodeSchemaOrphan,
					"proposed propertySchema removes or renames %d field(s) present in the current schema; up to %d entities of kind %q may be affected",
					orphans, exactMatch.EntityCount, exactMatch.Name))
		}
		result.Warnings = append(result.Warnings,
			warncode.New(warncode.CodeSchemaSupersede,
				"change supersedes the existing propertySchema for kind %q; existing entities retain their stored values until re-validated",
				exactMatch.Name))
		return result
	}

	// Rule 2: exact canonical match, no schema change → collapse (already exists).
	if exactMatch != nil {
		return KindPreviewResult{
			Action:        "collapse",
			CollapsesInto: exactMatch.Name,
			Warnings: []warncode.Warning{
				warncode.New(warncode.CodeSchemaCollapse,
					"candidate kind %q is already registered; no new kind would be created",
					candidate),
			},
		}
	}

	// Rule 4: mint. Run the same lexical near-duplicate scan the live
	// defineEntityKind dedup uses (nameSimilarity over registered kind names) so
	// the preview can answer "would this collapse into an existing kind?". The
	// embedder is never invoked here; ChecksSkipped records that the semantic
	// pass did not run when the caller has no embedder wired.
	result := KindPreviewResult{
		Action:       "mint",
		NearestKinds: nearestKinds(normCand, existing),
	}

	threshold := req.CloseMatchThreshold
	if threshold == 0 {
		threshold = MintAliasThreshold
	}
	if len(result.NearestKinds) > 0 && result.NearestKinds[0].Similarity >= threshold {
		top := result.NearestKinds[0]
		result.CloseMatch = top.Kind
		// MAZ-KIND-001: close match found, mirroring the live mint-with-alias path.
		result.Warnings = append(result.Warnings,
			warncode.New(warncode.CodeKindCloseMatch,
				"candidate kind %q is a near-duplicate of existing kind %q (lexical similarity %.2f >= %.2f); consider reusing that kind instead of minting a synonym",
				candidate, top.Kind, top.Similarity, threshold))
	}

	if !req.SemanticChecked {
		result.ChecksSkipped = []string{"semantic"}
		// MAZ-KIND-002: semantic dedup pass was skipped (no embedder wired), so
		// the mint verdict is lexical-only and a semantic synonym may be missed.
		result.Warnings = append(result.Warnings,
			warncode.New(warncode.CodeKindDedupDegraded,
				"semantic dedup pass was skipped (no embedder wired); only the lexical near-duplicate scan ran — a semantic synonym may have slipped through"))
	}

	return result
}

// nearestKinds ranks existing kinds by lexical similarity to the normalized
// candidate name, highest first, reusing the same nameSimilarity scorer the
// live dedup.Checker uses (LCS-ratio / Levenshtein over normalized names). It
// drops zero-similarity entries so the list carries only plausible neighbours,
// and breaks ties by kind name for deterministic output.
func nearestKinds(normCand string, existing []ExistingKind) []NearestKind {
	out := make([]NearestKind, 0, len(existing))
	for i := range existing {
		sim := nameSimilarity(normCand, normalizeName(existing[i].Name))
		if sim <= 0 {
			continue
		}
		out = append(out, NearestKind{Kind: existing[i].Name, Similarity: sim})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// PlanSchema computes the preview result for a defineSchema call with
// preview=true.
func PlanSchema(req SchemaPreviewRequest, existing []ExistingSchema) SchemaPreviewResult {
	// Find any existing registration for this schemaKind.
	var matchedSchema *ExistingSchema
	for i := range existing {
		if existing[i].SchemaKind == req.SchemaKind {
			matchedSchema = &existing[i]
			break
		}
	}

	if matchedSchema == nil {
		return SchemaPreviewResult{Action: "register"}
	}

	result := SchemaPreviewResult{
		Action:         "supersede",
		SupersedesFrom: matchedSchema.Version,
	}

	orphans := countOrphanedFields(matchedSchema.Definition, req.Definition)
	if orphans > 0 {
		result.OrphanCount = orphans
		result.Warnings = append(result.Warnings,
			warncode.New(warncode.CodeSchemaOrphan,
				"proposed definition removes or renames %d field(s) present in the currently registered schema %q@%s; entities validated against the old schema may fail re-validation",
				orphans, req.SchemaKind, matchedSchema.Version))
	}

	result.Warnings = append(result.Warnings,
		warncode.New(warncode.CodeSchemaSupersede,
			"schema %q will be superseded from %s to %s",
			req.SchemaKind, matchedSchema.Version, req.Version))

	return result
}

// countOrphanedFields returns the number of top-level property keys that are
// present in oldSchema but absent from newSchema. It extracts the key set from
// each schema's "properties" object; any other JSON Schema structure is ignored.
// Returns 0 when either schema is nil, malformed, or has no "properties" key.
func countOrphanedFields(oldSchema, newSchema json.RawMessage) int {
	oldKeys := extractPropertyKeys(oldSchema)
	newKeys := extractPropertyKeys(newSchema)
	if len(oldKeys) == 0 {
		return 0
	}
	count := 0
	for _, k := range oldKeys {
		if !containsKey(newKeys, k) {
			count++
		}
	}
	return count
}

// extractPropertyKeys returns the top-level keys from a JSON Schema's
// "properties" object. Returns nil when the schema is absent, not a JSON
// object, or has no "properties" key.
func extractPropertyKeys(schema json.RawMessage) []string {
	if len(schema) == 0 {
		return nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil
	}
	propsRaw, ok := doc["properties"]
	if !ok {
		return nil
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(propsRaw, &props); err != nil {
		return nil
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	return keys
}

// containsKey reports whether key appears in slice using exact (case-sensitive)
// matching. A case-insensitive match that differs in casing is treated as a
// rename (not present) so it contributes to the orphan count.
func containsKey(slice []string, key string) bool {
	for _, s := range slice {
		if s == key {
			return true
		}
	}
	return false
}
