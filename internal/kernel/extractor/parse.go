package extractor

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// rawMention is the JSON shape the LLM returns inside the tool-use response.
// Field names are chosen to be unambiguous for the model while mapping cleanly
// to ontology.Mention after grounding. All fields are strings so the JSON
// unmarshaler never rejects a slightly-off model response.
type rawMention struct {
	Kind              string          `json:"kind"`
	DisplayName       string          `json:"displayName"`
	ExtractionText    string          `json:"extractionText"`
	DisambiguationCtx string          `json:"disambiguationCtx,omitempty"`
	Identifiers       json.RawMessage `json:"identifiers,omitempty"`
}

// rawIdentifier mirrors the schema shape inside Identifiers. Kept separate
// from rawMention so json.RawMessage can absorb the shape variants small
// LLMs occasionally emit (object instead of array, or null).
type rawIdentifier struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// rawRelation is the JSON shape for one relation inside the tool-use response.
type rawRelation struct {
	SubjectMentionIdx int               `json:"subjectMentionIdx"`
	Predicate         string            `json:"predicate"`
	ObjectMentionIdx  int               `json:"objectMentionIdx"`
	ValidFrom         string            `json:"validFrom,omitempty"`  // RFC 3339 or empty
	ValidUntil        string            `json:"validUntil,omitempty"` // RFC 3339 or empty
	Attributes        map[string]string `json:"attributes,omitempty"`
}

// rawToolOutput is the top-level shape of the tool-use response payload.
// Mentions and Relations are json.RawMessage to accommodate llama3.2's
// occasional habit of returning nested JSON as a string literal under
// format:"json" mode. unwrapJSONField handles both shapes uniformly.
type rawToolOutput struct {
	Mentions  json.RawMessage `json:"mentions"`
	Relations json.RawMessage `json:"relations"`
}

// ParseToolUseResponse unmarshals the raw JSON from a ChatProvider.ToolUse
// call into typed Mention and RelationCandidate slices. It does NOT perform
// grounding (that is ground.go's job). Caller-supplied sourceEventUID is
// copied onto every Mention so they are ready for event emission.
//
// Validation errors (unknown kind, empty displayName, out-of-range indices)
// produce a non-nil error. Partial results are not returned on error; the
// caller should discard and retry or log.
func ParseToolUseResponse(raw json.RawMessage, sourceEventUID string) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("extractor: parse: empty tool-use response")
	}
	var out rawToolOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, fmt.Errorf("extractor: parse: unmarshal tool output: %w", err)
	}

	var rawMentions []rawMention
	if err := unmarshalArrayOrStringifiedArray(out.Mentions, &rawMentions); err != nil {
		return nil, nil, fmt.Errorf("extractor: parse: mentions: %w", err)
	}
	var rawRelations []rawRelation
	if err := unmarshalArrayOrStringifiedArray(out.Relations, &rawRelations); err != nil {
		return nil, nil, fmt.Errorf("extractor: parse: relations: %w", err)
	}

	mentions, err := parseMentions(rawMentions, sourceEventUID)
	if err != nil {
		return nil, nil, err
	}
	candidates, err := parseRelations(rawRelations, len(mentions))
	if err != nil {
		return nil, nil, err
	}
	return mentions, candidates, nil
}

// unmarshalArrayOrStringifiedArray accepts both a JSON array and a JSON
// string whose contents are themselves a JSON array. The second shape is a
// known quirk of llama3.2 under format:"json" where the model wraps nested
// arrays in quotes; rather than fail the whole extraction we unwrap once.
// Small models also occasionally emit Python-style single-quoted JSON
// inside the wrapper; normaliseSingleQuotedJSON takes a best-effort pass
// at converting that to valid double-quoted JSON before the second decode.
// Empty input or JSON null produces an empty slice (treated as no error).
func unmarshalArrayOrStringifiedArray(raw json.RawMessage, dst interface{}) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err == nil {
		return nil
	}
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		return fmt.Errorf("unwrap: %w", err)
	}
	if inner == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(inner), dst); err == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(normaliseSingleQuotedJSON(inner)), dst); err != nil {
		return fmt.Errorf("inner: %w", err)
	}
	return nil
}

// normaliseSingleQuotedJSON converts naively single-quoted JSON to a
// double-quoted form parseable by encoding/json. It is intentionally
// limited: outside of string literals, it swaps ' for ", and inside a
// double-quoted literal it preserves single quotes verbatim. Escaped
// quotes (\' and \") survive the transformation unchanged. This is a
// pragmatic best-effort pass for small-model JSON-mode output; if the
// input is sufficiently malformed the caller still gets a parse error.
func normaliseSingleQuotedJSON(s string) string {
	out := make([]byte, 0, len(s))
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			out = append(out, c, s[i+1])
			i++
		case c == '"' && !inDouble:
			inDouble = true
			out = append(out, c)
		case c == '"' && inDouble:
			inDouble = false
			out = append(out, c)
		case c == '\'' && !inDouble:
			out = append(out, '"')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// parseMentions converts the raw mention slice into typed ontology.Mention values.
// Skip-and-log behaviour: a malformed individual mention (unknown kind, empty
// displayName/extractionText) is dropped from the output rather than failing the
// whole batch. Small LLMs occasionally emit a single bad row alongside several
// good ones; losing one mention is better than losing the whole episode's
// extraction.
func parseMentions(rms []rawMention, sourceEventUID string) ([]ontology.Mention, error) {
	out := make([]ontology.Mention, 0, len(rms))
	for i, rm := range rms {
		m, err := parseOneMention(rm, sourceEventUID, i)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// parseOneMention converts a single rawMention into an ontology.Mention.
func parseOneMention(rm rawMention, sourceEventUID string, idx int) (ontology.Mention, error) {
	kind, err := parseEntityKind(rm.Kind)
	if err != nil {
		return ontology.Mention{}, fmt.Errorf("extractor: parse: mention[%d]: %w", idx, err)
	}
	if rm.DisplayName == "" {
		return ontology.Mention{}, fmt.Errorf("extractor: parse: mention[%d]: empty displayName", idx)
	}
	if rm.ExtractionText == "" {
		return ontology.Mention{}, fmt.Errorf("extractor: parse: mention[%d]: empty extractionText", idx)
	}
	m := ontology.Mention{
		Kind:              kind,
		DisplayName:       rm.DisplayName,
		ExtractionText:    rm.ExtractionText,
		DisambiguationCtx: rm.DisambiguationCtx,
		SourceEventUID:    ontology.ULID(sourceEventUID),
	}
	for _, id := range decodeRawIdentifiers(rm.Identifiers) {
		if id.Type != "" && id.Value != "" {
			m.Identifiers = append(m.Identifiers, ontology.Identifier{
				Type:  id.Type,
				Value: id.Value,
			})
		}
	}
	return m, nil
}

// decodeRawIdentifiers extracts a slice of rawIdentifier from the Mention's
// Identifiers field. Tolerates three shapes small LLMs occasionally emit:
//   - JSON array of {type, value} objects (the schema-correct form)
//   - A single {type, value} object (lifted into a one-element slice)
//   - JSON null or absent (returns an empty slice)
//
// Any other shape is silently dropped rather than failing the whole mention.
func decodeRawIdentifiers(raw json.RawMessage) []rawIdentifier {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []rawIdentifier
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var single rawIdentifier
	if err := json.Unmarshal(raw, &single); err == nil {
		return []rawIdentifier{single}
	}
	return nil
}

// parseRelations converts the raw relation slice into RelationCandidate values.
// Skip-and-log: a relation that references out-of-range mention indices or
// carries an empty predicate is dropped from the output rather than failing
// the whole batch. Small LLMs sometimes invent indices that exceed the
// mention array length; losing the bad relation is better than losing the
// whole episode's extraction including the valid mentions.
func parseRelations(rrs []rawRelation, nMentions int) ([]ontology.RelationCandidate, error) {
	out := make([]ontology.RelationCandidate, 0, len(rrs))
	for i, rr := range rrs {
		rc, err := parseOneRelation(rr, nMentions, i)
		if err != nil {
			continue
		}
		out = append(out, rc)
	}
	return out, nil
}

// parseOneRelation validates and converts a single rawRelation.
func parseOneRelation(rr rawRelation, nMentions, idx int) (ontology.RelationCandidate, error) {
	if rr.SubjectMentionIdx < 0 || rr.SubjectMentionIdx >= nMentions {
		return ontology.RelationCandidate{}, fmt.Errorf(
			"extractor: parse: relation[%d]: subjectMentionIdx %d out of range [0,%d)",
			idx, rr.SubjectMentionIdx, nMentions,
		)
	}
	if rr.ObjectMentionIdx < 0 || rr.ObjectMentionIdx >= nMentions {
		return ontology.RelationCandidate{}, fmt.Errorf(
			"extractor: parse: relation[%d]: objectMentionIdx %d out of range [0,%d)",
			idx, rr.ObjectMentionIdx, nMentions,
		)
	}
	if rr.Predicate == "" {
		return ontology.RelationCandidate{}, fmt.Errorf("extractor: parse: relation[%d]: empty predicate", idx)
	}
	rc := ontology.RelationCandidate{
		SubjectMentionIdx: rr.SubjectMentionIdx,
		Predicate:         rr.Predicate,
		ObjectMentionIdx:  rr.ObjectMentionIdx,
	}
	if len(rr.Attributes) > 0 {
		rc.Attributes = make(map[string]string, len(rr.Attributes))
		for k, v := range rr.Attributes {
			if k != "" && v != "" {
				rc.Attributes[k] = v
			}
		}
		if len(rc.Attributes) == 0 {
			rc.Attributes = nil
		}
	}
	if rr.ValidFrom != "" {
		t, err := time.Parse(time.RFC3339, rr.ValidFrom)
		if err == nil {
			utc := t.UTC()
			rc.ValidFrom = &utc
		}
		// Non-fatal: malformed dates (e.g. "2022-01") are dropped silently.
	}
	if rr.ValidUntil != "" {
		t, err := time.Parse(time.RFC3339, rr.ValidUntil)
		if err == nil {
			utc := t.UTC()
			rc.ValidUntil = &utc
		}
		// Non-fatal: malformed dates (e.g. "2024") are dropped silently. A
		// known shape we have to tolerate is the model emitting a bare year
		// or YYYY-MM; rather than expand the parser surface area, the
		// extractor leaves the bound unset and relies on supersede-on-
		// contradict to close the relation when a current alternative
		// arrives later.
	}
	return rc, nil
}

// entityKindRe is the compiled form of the open-enum pattern for entity
// kinds. It matches the six core bare snake_case kinds and namespaced
// extension kinds (e.g. "user:deal", "extension:schema_org:Person").
// Compiled once at package init.
//

var entityKindRe = regexp.MustCompile(`^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$`)

// parseEntityKind normalises the model's raw kind string and returns a
// typed EntityKind. Normalisation applies only to the first (namespace)
// segment: trim surrounding whitespace, lowercase, collapse interior spaces
// to underscores (common model drift for bare core kinds like "Place" or
// "living space"). Segments after the first colon are preserved verbatim
// because vocabulary imports like "extension:schema_org:Person" use
// PascalCase for the local name. The six core kinds are fast-pathed to
// their typed constants; any other string that matches the open-enum pattern
// is accepted as a custom kind. Empty or pattern-violating strings are
// rejected.
//
// The prompt (prompt.go) presents the six core kinds as a seed list and
// permits novel kinds with a rationale, so a custom kind reaching here is a
// deliberate model proposal; the resolver's auto-registration then mints or
// routes it to review. Accepting any pattern-valid kind here is what lets
// that downstream path run instead of hard-rejecting at the parser.
func parseEntityKind(s string) (ontology.EntityKind, error) {
	trimmed := strings.TrimSpace(s)
	// Normalise only the first colon-separated segment: lowercase + spaces
	// to underscores. Subsequent segments (vocabulary local names) keep
	// their original casing per the pattern definition.
	var norm string
	if idx := strings.IndexByte(trimmed, ':'); idx >= 0 {
		first := strings.ToLower(strings.ReplaceAll(trimmed[:idx], " ", "_"))
		norm = first + trimmed[idx:]
	} else {
		norm = strings.ToLower(strings.ReplaceAll(trimmed, " ", "_"))
	}

	// Fast-path the seven core constants to preserve the typed values.
	switch ontology.EntityKind(norm) {
	case ontology.EntityKindPerson,
		ontology.EntityKindOrganization,
		ontology.EntityKindPlace,
		ontology.EntityKindProject,
		ontology.EntityKindEvent,
		ontology.EntityKindDocument,
		ontology.EntityKindExternalRef:
		return ontology.EntityKind(norm), nil
	}

	// Accept any other string that satisfies the open-enum pattern.
	if norm != "" && entityKindRe.MatchString(norm) {
		return ontology.EntityKind(norm), nil
	}
	return "", fmt.Errorf("extractor: invalid entity kind %q (normalised: %q)", s, norm)
}
