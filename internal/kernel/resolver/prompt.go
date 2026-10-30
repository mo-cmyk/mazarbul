package resolver

import (
	"context"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// Prompt template names and versions registered by this package.
const (
	disambiguateTemplateName       = "disambiguate-v1"
	contradictionTemplateName      = "contradiction-v1"
	summaryTemplateName            = "summary-v1"
	predicateCanonTemplateName     = "predicate-canon-v1"
	entityKindClassifyTemplateName = "entity-kind-classify-v1"

	disambiguateTemplateVersion       prompts.Version = 1
	contradictionTemplateVersion      prompts.Version = 2
	summaryTemplateVersion            prompts.Version = 2
	predicateCanonTemplateVersion     prompts.Version = 1
	entityKindClassifyTemplateVersion prompts.Version = 1
)

// contradiction-v2 (2026-05-26): added two real-world contradictory examples
// for non-functional predicates that the v1 set under-served (allegiance and
// citizenship-style flags), and tightened the rationale phrasing to make the
// downstream parser more robust to chatty completions.
//
// summary-v2 (2026-05-26): added three worked examples. v1 had none, which
// meant smaller models (qwen2.5:7b, llama3.1:8b) routinely produced
// first-person narratives or sales-y filler. The examples anchor the
// neutral third-person tone and demonstrate handling sparse vs. dense
// excerpt sets.

// disambiguateSystemPrompt is the system prompt for the disambiguate-v1 call.
// It receives one Mention and one candidate Entity and decides whether they
// refer to the same real-world thing.
const disambiguateSystemPrompt = `You are an entity disambiguation assistant for a personal knowledge graph.

TASK
You receive a newly extracted Mention and an existing Entity that a vector search flagged as a possible match.
Decide whether the Mention and the Entity refer to the same real-world thing.

Call the disambiguate tool with your verdict and a one-sentence rationale.

VERDICT VALUES
- same: the Mention and Entity clearly refer to the same real-world referent
- different: they are clearly distinct real-world referents
- unsure: you cannot determine with confidence whether they match

WORKED EXAMPLES

Example 1 (same)
Mention: {"displayName": "Frodo", "kind": "person", "disambiguationCtx": "Erebor engineer", "extractionText": "Frodo joined the call late."}
Entity:  {"displayName": "Frodo Baggins", "kind": "person", "summary": "Software engineer at Erebor Inc. based in Zurich."}
Output: {"verdict": "same", "rationale": "Both refer to a person named Frodo who works at Erebor; the disambiguation context matches the entity summary."}

Example 2 (different)
Mention: {"displayName": "Berlin", "kind": "place", "disambiguationCtx": "startup scene", "extractionText": "We visited Berlin for the conference."}
Entity:  {"displayName": "Berlin", "kind": "organization", "summary": "Berlin Inc., a Munich-based software consultancy."}
Output: {"verdict": "different", "rationale": "The Mention refers to the city of Berlin while the Entity is a company named Berlin Inc.; different real-world referents."}

Example 3 (unsure)
Mention: {"displayName": "Aragorn", "kind": "person", "disambiguationCtx": "", "extractionText": "Aragorn approved the budget."}
Entity:  {"displayName": "Aragorn", "kind": "person", "summary": "Product manager at a fintech company."}
Output: {"verdict": "unsure", "rationale": "The first name matches and both are persons, but there is insufficient context to confirm or rule out identity."}

Now decide for the following:
Mention: {{ .mention }}
Entity:  {{ .entity }}`

// contradictionSystemPrompt is the system prompt for the contradiction-v1 call.
// It decides whether a new RelationCandidate contradicts an existing Relation for
// a non-functional predicate.
const contradictionSystemPrompt = `You are a knowledge-graph consistency checker for a personal knowledge graph.

TASK
You receive an existing Relation and a proposed new RelationCandidate that share the same subject
entity and predicate. For non-functional predicates (where multiple values are allowed), decide
whether the new candidate CONTRADICTS the existing Relation, meaning both cannot be true
simultaneously in the real world.

Call the check_contradiction tool with your verdict.

VERDICT
- contradictory: true when the two facts cannot both hold at the same time (e.g. two different salaries, two incompatible roles)
- contradictory: false when both facts can coexist (e.g. a person is a member of two different organisations)

WORKED EXAMPLES

Example 1 (contradictory = false, non-functional member_of)
Existing:  {"predicate": "member_of", "objectUid": "ORG_A", "objectName": "Mithril Corp"}
Candidate: {"predicate": "member_of", "objectName": "Beta Ltd."}
Output: {"contradictory": false, "rationale": "A person can be a member of multiple organisations simultaneously; no contradiction."}

Example 2 (contradictory = true, non-functional has_role with conflicting claims)
Existing:  {"predicate": "has_role", "objectUid": "ROLE_CEO", "objectName": "CEO"}
Candidate: {"predicate": "has_role", "objectName": "CFO at the same company"}
Output: {"contradictory": true, "rationale": "A person cannot simultaneously be CEO and CFO of the same company; the roles are mutually exclusive."}

Example 3 (contradictory = false, attended multiple events)
Existing:  {"predicate": "attended", "objectName": "NeurIPS 2024"}
Candidate: {"predicate": "attended", "objectName": "ICML 2024"}
Output: {"contradictory": false, "rationale": "A person can attend multiple conferences; both facts can coexist."}

Example 4 (contradictory = true, supports_team allegiance)
Existing:  {"predicate": "supports_team", "objectUid": "TEAM_A", "objectName": "Manchester United"}
Candidate: {"predicate": "supports_team", "objectName": "Liverpool"}
Output: {"contradictory": true, "rationale": "Football allegiance is single-valued in practice: a fan supporting one Premier League club excludes simultaneous support of a fierce rival."}

Example 5 (contradictory = true, prefers diametric values)
Existing:  {"predicate": "prefers_diet", "objectName": "strict vegetarian"}
Candidate: {"predicate": "prefers_diet", "objectName": "carnivore diet"}
Output: {"contradictory": true, "rationale": "The two diet preferences are mutually exclusive; one excludes the food group the other centres on."}

Example 6 (contradictory = false, owns multiple pets)
Existing:  {"predicate": "owns_pet", "objectName": "a dog named Rex"}
Candidate: {"predicate": "owns_pet", "objectName": "a cat named Mia"}
Output: {"contradictory": false, "rationale": "Pet ownership is non-exclusive; a person can own a dog and a cat."}

Now evaluate:
Existing Relation:  {{ .existing }}
Proposed Candidate: {{ .candidate }}`

// summarySystemPrompt is the system prompt for the summary-v1 call. It
// synthesises a one-paragraph entity summary from recent Mention extraction texts.
const summarySystemPrompt = `You are a personal knowledge-graph assistant that writes concise entity summaries.

TASK
Write a single paragraph (3-5 sentences) that summarises what is known about an entity
based on the provided Mention excerpts. The summary should:
- Identify who or what the entity is (kind, name)
- Capture the most important attributes and relationships visible in the excerpts
- Be written in the third person, present tense (unless clearly historical)
- Omit speculation; only include facts supported by the excerpts
- Be neutral and factual

WORKED EXAMPLES

Example 1 (person, dense excerpts)
Entity display name: Frodo Baggins
Entity kind: person
Excerpts:
- "Frodo Baggins works at Erebor Inc. as a senior backend engineer."
- "Met Frodo at the Berlin office on Tuesday for the roadmap review."
- "Frodo recently transferred from the Munich team to lead the ingestion service."
Summary: "Frodo Baggins is a senior backend engineer at Erebor Inc., leading the ingestion service after a transfer from the Munich team. He is based in the Berlin office, where roadmap reviews and team meetings take place. The excerpts depict a hands-on technical leader with cross-office responsibility."

Example 2 (organization, sparse excerpts)
Entity display name: Rivendell
Entity kind: organization
Excerpts:
- "Frodo now works at Rivendell."
Summary: "Rivendell is an organisation that recently became Frodo's employer. The available excerpts do not yet describe the company's domain, location, or scale; the summary will expand as more mentions accumulate."

Example 3 (place, factual)
Entity display name: Berlin
Entity kind: place
Excerpts:
- "Met him in Berlin last week."
- "We visited Berlin for the conference."
- "Berlin's startup scene has grown since 2020."
Summary: "Berlin is a place that frequently hosts meetings and conferences referenced in these notes. It is described as having a notable startup scene that has grown since 2020. The excerpts treat Berlin as a recurring travel destination rather than a permanent residence."

Now write the summary for:
Entity display name: {{ .displayName }}
Entity kind: {{ .kind }}

Recent mention excerpts (most recent first):
{{ .excerpts }}

Write the summary paragraph now:`

// predicateCanonSystemPrompt is the system prompt for the predicate-canon-v1 call.
// It classifies an unknown predicate as functional or non-functional and produces
// a short description.
const predicateCanonSystemPrompt = `You are a knowledge-graph schema assistant.

TASK
Classify a newly encountered relation predicate as either "functional" or "non-functional",
and provide a one-sentence description.

DEFINITIONS
- functional: a subject entity can have AT MOST ONE current value for this predicate at any time.
  Example: works_at, lives_in, born_in, married_to, has_ceo. When a new value arrives the old one
  is superseded.
- non-functional: a subject entity can have MULTIPLE simultaneous current values for this predicate.
  Example: member_of, knows, attended, owns, collaborated_with. Multiple values coexist.

DECISION RULES
Ask: "Can a person (or entity) have more than one X at the same time?"
- If yes -> non-functional
- If no  -> functional

WORKED EXAMPLES

Example 1
Predicate: "works_at"
Output: {"kind": "functional", "description": "The organisation the subject entity currently works at; only one is valid at a time."}

Example 2
Predicate: "member_of"
Output: {"kind": "non-functional", "description": "An organisation or group the subject entity belongs to; multiple memberships are allowed simultaneously."}

Example 3
Predicate: "lives_in"
Output: {"kind": "functional", "description": "The city or place where the subject entity currently lives; only one residence is primary at a time."}

Example 4
Predicate: "attended"
Output: {"kind": "non-functional", "description": "An event the subject entity attended; a person can attend many events."}

Now classify:
Predicate: {{ .predicate }}`

// entityKindClassifySystemPrompt is the system prompt for the
// entity-kind-classify-v1 call. Given a proposed novel entity kind and a
// short context snippet, the model either maps it to one of the registered
// kinds (action: "reuse") or confirms minting it as a new snake_case kind
// (action: "mint").
const entityKindClassifySystemPrompt = `You are a knowledge-graph schema assistant.

TASK
A novel entity kind has been proposed during ingestion. Decide whether it should
reuse an existing registered kind or be minted as a genuinely new kind.

Call the classify_entity_kind tool with your decision.

REGISTERED CORE KINDS
- person: a human individual
- organization: a company, institution, team, NGO, government body
- place: a city, country, building, region, geographic feature
- project: a named software project, initiative, product, codebase, research programme
- event: a named meeting, conference, workshop, trip, ceremony, occurrence
- document: a named file, report, email thread, contract, specification

DECISION RULES
1. If the proposed kind is a synonym or close variant of a core kind, choose
   action "reuse" and return the canonical core kind name.
2. If the proposed kind represents a genuinely distinct concept not covered by
   any core kind, choose action "mint".
3. A minted kind MUST be a singular snake_case noun (e.g. "product", "vehicle",
   "animal"). Never mint a plural or a verb.
4. Set confidence between 0.0 (very uncertain) and 1.0 (certain). Prefer
   conservative values; a score below 0.6 signals human review is needed.

OUTPUT SHAPE
{
  "action":      "reuse" | "mint",
  "kind":        "<canonical or new snake_case name>",
  "displayName": "<title-case label for new kinds, e.g. Vehicle>",
  "description": "<one sentence describing the new kind>",
  "confidence":  <0.0-1.0>
}

WORKED EXAMPLES

Example 1 (reuse)
Proposed kind: "company"
Context: "Erebor Inc. is a software company based in Zurich."
Output: {"action":"reuse","kind":"organization","displayName":"","description":"","confidence":0.98}

Example 2 (reuse)
Proposed kind: "human"
Context: "Frodo is a human engineer on the team."
Output: {"action":"reuse","kind":"person","displayName":"","description":"","confidence":0.99}

Example 3 (mint)
Proposed kind: "vehicle"
Context: "Frodo drives a Tesla Model 3 to the office."
Output: {"action":"mint","kind":"vehicle","displayName":"Vehicle","description":"A motor vehicle such as a car, truck, or motorcycle.","confidence":0.91}

Example 4 (mint, low confidence)
Proposed kind: "thing"
Context: "There was a thing on the table."
Output: {"action":"mint","kind":"thing","displayName":"Thing","description":"A generic object.","confidence":0.35}

Now classify:
Proposed kind: {{ .kind }}
Context: {{ .contextSnippet }}`

// RegisterPrompts registers the four resolver prompt templates with reg. Call
// once at process startup (or once per test, since the same triple is idempotent).
// Returns the first registration error encountered.
func RegisterPrompts(ctx context.Context, reg prompts.Registry) error {
	templates := []prompts.Template{
		{
			Name:        disambiguateTemplateName,
			Version:     disambiguateTemplateVersion,
			Body:        disambiguateSystemPrompt,
			Description: "Entity disambiguation prompt. Decides whether a new Mention and an existing Entity refer to the same real-world thing. Variables: mention (JSON), entity (JSON).",
		},
		{
			Name:        contradictionTemplateName,
			Version:     contradictionTemplateVersion,
			Body:        contradictionSystemPrompt,
			Description: "Contradiction check prompt for non-functional predicates. Decides whether a new RelationCandidate contradicts an existing Relation. Variables: existing (JSON), candidate (JSON).",
		},
		{
			Name:        summaryTemplateName,
			Version:     summaryTemplateVersion,
			Body:        summarySystemPrompt,
			Description: "Entity summary regeneration prompt. Synthesises a paragraph from recent Mention excerpts. Variables: displayName, kind, excerpts.",
		},
		{
			Name:        predicateCanonTemplateName,
			Version:     predicateCanonTemplateVersion,
			Body:        predicateCanonSystemPrompt,
			Description: "Predicate classification prompt. Classifies an unknown predicate as functional or non-functional and provides a description. Variables: predicate.",
		},
		{
			Name:    namingCanonTemplateName,
			Version: namingCanonTemplateVersion,
			Body:    namingCanonSystemPrompt,
			Description: "Naming canonicalisation prompt. Given an entity Kind and a list of " +
				"surface forms, picks the most canonical form. Variables: kind, names (JSON array).",
		},
		{
			Name:        entityKindClassifyTemplateName,
			Version:     entityKindClassifyTemplateVersion,
			Body:        entityKindClassifySystemPrompt,
			Description: "Entity-kind classification prompt. Given a proposed novel entity kind and a context snippet, decides whether to reuse an existing kind or mint a new one. Variables: kind, contextSnippet.",
		},
	}

	for _, tmpl := range templates {
		if err := reg.Register(ctx, tmpl); err != nil {
			return fmt.Errorf("resolver: register prompt %q: %w", tmpl.Name, err)
		}
	}
	return nil
}
