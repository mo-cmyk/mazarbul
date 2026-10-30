package extractor

import (
	"regexp"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// syntheticSelfCandidate describes a self-statement RelationCandidate
// produced by the lexical detector. The subject is always the canonical
// self entity (resolved downstream by ID, not by mention index); the
// object is the index into the grounded Mention slice that the LLM
// extractor produced for the same episode.
//
// We intentionally do NOT modify ontology.RelationCandidate's schema
// or use a -1 sentinel for the subject. The pipeline treats self
// candidates as a separate stream that the resolver picks up via a
// dedicated path (subject pre-bound to "self"), so the wire shape on
// the relationCandidatesProposed event stays unchanged.
type syntheticSelfCandidate struct {
	// ObjectMentionIdx is the index into the same []ontology.Mention
	// slice the LLM extractor returned. The detector only emits a
	// candidate when this points to a real grounded mention.
	ObjectMentionIdx int

	// Predicate is one of has_name / lives_in / works_at — drawn from
	// the existing functional-predicate seed list so the resolver's
	// supersede pipeline closes the prior open row automatically.
	Predicate string
}

// selfStatementRule pairs a regex that triggers on first-person
// assertion patterns with the predicate it implies and the entity
// kinds the object mention must have. The kind gate prevents
// nonsense pairings like "my name is Berlin" (place mention typed
// as a person name) or "I work at Lucas" (person mention typed as
// an employer).
type selfStatementRule struct {
	re        *regexp.Regexp
	predicate string
	objKinds  []ontology.EntityKind
}

// selfStatementRules is the canonical lexical-pattern set. Order
// matters only for documentation; the detector evaluates every rule
// independently and emits one candidate per (rule, matching mention)
// pair so a single episode can produce multiple self relations
// (e.g. "My name is Arwen and I live in Berlin").
//
// All patterns are case-insensitive and anchored on word boundaries
// or sentence starts so a substring like "innovation" doesn't trip
// "novation" by accident. Each rule was tuned against the user-
// reported failure modes:
//   - has_name covers TEST 6 (names chain): "I changed my name to X",
//     "my name is X", "i'm called X", "i go by X".
//   - lives_in covers TEST 4 (Berlin pollution): "I live in X",
//     "i'm based in X", "i'm from X originally".
//   - works_at covers TEST 5 (Erebor duration): "I work at X",
//     "I joined X", "i'm employed by X".
//
//nolint:gochecknoglobals // immutable lookup table; constructed once.
var selfStatementRules = []selfStatementRule{
	{
		re: regexp.MustCompile(`(?i)\b(?:i\s+changed\s+my\s+name\s+to|my\s+name\s+is|i'?m\s+called|i\s+go\s+by)\s+([A-Z][A-Za-z'\- ]{1,})`),
		// has_name is not in the canonical 10-predicate seed list yet
		// — migration 0012 adds it as functional alongside lives_in.
		predicate: "has_name",
		objKinds:  []ontology.EntityKind{ontology.EntityKindPerson},
	},
	{
		re:        regexp.MustCompile(`(?i)\b(?:i\s+live\s+in|i\s+lived\s+in|i'?m\s+based\s+in|i'?m\s+from|i\s+moved\s+to)\s+([A-Z][A-Za-z'\- ]{1,})`),
		predicate: "lives_in",
		objKinds:  []ontology.EntityKind{ontology.EntityKindPlace},
	},
	{
		re:        regexp.MustCompile(`(?i)\b(?:i\s+work\s+at|i\s+worked\s+at|i\s+joined|i\s+started\s+at|i'?m\s+employed\s+by|i'?ve\s+joined)\s+([A-Z][A-Za-z'\- ]{1,})`),
		predicate: "works_at",
		objKinds:  []ontology.EntityKind{ontology.EntityKindOrganization},
	},
	{
		// studies_at: school/university enrollment is functional in the
		// same sense as works_at (one active institution per subject in
		// the common case). When the user enrolls elsewhere the
		// supersede pipeline closes the prior row. studies_at is NOT in
		// the 0006 seed list; migration 0014 below adds it as functional.
		re:        regexp.MustCompile(`(?i)\b(?:i\s+study\s+at|i\s+studied\s+at|i\s+attend|i'?m\s+enrolled\s+at)\s+([A-Z][A-Za-z'\- ]{1,})`),
		predicate: "studies_at",
		objKinds:  []ontology.EntityKind{ontology.EntityKindOrganization},
	},
}

// detectSelfStatements scans episodeText for first-person assertion
// patterns. For each match it looks up the captured object string
// among the grounded mentions; when an exact (case-insensitive,
// whitespace-collapsed) DisplayName match exists with a compatible
// EntityKind, it emits one syntheticSelfCandidate.
//
// The function does NOT mutate the input mentions. It returns nil
// when the text contains no first-person triggers or none of the
// captured objects match a grounded mention — both are common cases
// for third-person episodes.
//
// Design note: we deliberately gate on EntityKind to avoid pairing
// "I live in Berlin" with a `Berlin` mention that the extractor
// (mis)classified as a person. The cost is missing edge cases where
// the extractor disagrees with the lexical context; the benefit is
// zero false positives, which matters because every emitted candidate
// becomes a Relation event that the supersede pipeline may then
// auto-close on the next conflicting append.
func detectSelfStatements(episodeText string, mentions []ontology.Mention) []syntheticSelfCandidate {
	if episodeText == "" || len(mentions) == 0 {
		return nil
	}

	var out []syntheticSelfCandidate
	for _, rule := range selfStatementRules {
		matches := rule.re.FindAllStringSubmatch(episodeText, -1)
		if len(matches) == 0 {
			continue
		}
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			object := normaliseForSelfMatch(m[1])
			idx := findMatchingMention(mentions, object, rule.objKinds)
			if idx < 0 {
				continue
			}
			out = append(out, syntheticSelfCandidate{
				ObjectMentionIdx: idx,
				Predicate:        rule.predicate,
			})
		}
	}
	return out
}

// findMatchingMention returns the first mention index whose
// DisplayName matches the captured object string (case-insensitive,
// whitespace-collapsed) AND whose Kind is in allowedKinds. -1 means
// no match.
func findMatchingMention(mentions []ontology.Mention, object string, allowedKinds []ontology.EntityKind) int {
	if object == "" {
		return -1
	}
	for i, m := range mentions {
		if !kindAllowed(m.Kind, allowedKinds) {
			continue
		}
		if normaliseForSelfMatch(m.DisplayName) == object {
			return i
		}
		// Prefix match handles cases where the regex captured a
		// longer span than the grounded mention ("Arwen yesterday"
		// captured by the regex but mention is "Arwen"). Strict
		// to avoid false positives.
		mention := normaliseForSelfMatch(m.DisplayName)
		if mention != "" && strings.HasPrefix(object, mention+" ") {
			return i
		}
	}
	return -1
}

// kindAllowed reports whether k is in the allowed set. Empty set
// allows any kind (currently unused but kept for symmetry — every
// production rule pins a specific kind).
func kindAllowed(k ontology.EntityKind, allowed []ontology.EntityKind) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if k == a {
			return true
		}
	}
	return false
}

// normaliseForSelfMatch lowercases, collapses runs of whitespace, and
// trims trailing punctuation so the regex capture matches the
// grounded mention's DisplayName.
func normaliseForSelfMatch(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimRight(s, ".,;:!?")
	return strings.Join(strings.Fields(s), " ")
}
