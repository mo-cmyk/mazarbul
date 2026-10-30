package composer

import (
	"context"

	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// composeTemplateName is the prompt registry name for the compose-v1 template.
const composeTemplateName = "compose-v1"

// composeTemplateVersion is the monotonic version used when registering with
// the prompts.Registry. Bump this when the system prompt or examples change;
// the registry's version-conflict check catches accidental downgrades.
// Version 2 (2026-05-26) adds a worked example for multi-source citation
// ("[1][3]") so the model emits stacked citations the rules already
// permit but had no concrete demonstration of. Without the example, smaller
// models tended to pick exactly one source even when two supported the
// claim, which downstream consumers (citations panel, evidence triples)
// surfaced as weaker provenance.
//
// Version 3 (2026-05-26) adds:
//   - TODAY'S DATE block, rendered when currentDate is non-empty. Anchors
//     "how many days ago" / "how many weeks ago" arithmetic to a real date
//     instead of letting the model guess.
//   - DATE ARITHMETIC section with two worked examples covering the two
//     LongMemEval temporal-reasoning patterns: subtraction between two
//     dated context entries and subtraction between a context entry and
//     today's date. Without these, qwen2.5:7b scored 0/5 on the 20q probe
//     because the model either guessed or abstained on date math.
//
// Version 4 (2026-05-26) adds:
//   - CURRENCY rule: when the render layer suffixes an entry with
//     `[SUPERSEDED]`, the LLM treats that entry's facts as historical and
//     prefers non-superseded entries when the question is in the present
//     tense ("where does X work now?"). Without this rule, the LLM picked
//     the older episode in case 04 of the full-stack E2E (Eowyn
//     left Erebor and joined Rivendell in 2025; the model answered
//     "Erebor" because both episodes were rendered side-by-side with no
//     validity signal). The marker literal must match
//     composer.SupersededMarker exactly.
//
// Version 5 (2026-05-27) adds Example 4: a worked supersede scenario.
// Version 4 wired the CURRENCY rule but qwen2.5:7b would sometimes
// acknowledge "[1] is superseded" and then abstain because it failed
// to extract the new fact from the non-superseded entry [2]. The
// worked example shows the model exactly what to DO: cite the
// non-superseded entry, drop the historical one, no abstain.
//
// Version 6 (2026-05-27) adds INFERENCE RULES + [INFERRED] convention.
// The model is allowed (and instructed) to chain explicit facts present
// in the context to answer questions whose direct answer is not stated.
// Examples covered: transitive boss-chain (X works at Y AND Z is CEO of
// Y -> Z is X's boss), colleague detection (X and Y both work at the
// same org -> they are colleagues), residency chain (X lives in Y AND
// Y is in Z -> X is in Z). Every inferred conclusion must be prefixed
// with [INFERRED] so users see exactly which claims were chained.
// Pure restatement (no chain needed) MUST NOT carry the [INFERRED]
// marker; reserve it for conclusions that combine two or more facts.
//
// Version 7 (2026-05-27) adds Example 7: combined CURRENCY + INFERENCE
// chain. Cross-subject role supersession lands a [SUPERSEDED] marker on
// the old CEO's episode, but the model (qwen2.5:7b) tends to ignore the
// marker when also asked to chain into a "who is my boss" answer. The
// worked example shows the model the exact sequence: drop the superseded
// CEO entry, lift the new CEO entry, then run the boss-of inference on
// the non-superseded chain. Also adds a NEVER-hedge directive ("would
// still be considered", "until the transition is documented") because
// qwen2.5:7b was emitting that hedge despite the marker.
//
// Version 8 (2026-05-27) extends Example 5's trailing annotation to make
// the BOSS / REPORTING CHAIN rule's "reports_to overrides CEO" clause
// concrete: if the context names a direct reports_to manager, the model
// must answer that manager directly without [INFERRED] and skip the
// CEO-based inference.
//
// Version 9 (2026-05-29) adds the IMPLICIT CURRENCY clause to the
// CURRENCY RULE: when the question is present-tense and the context
// contains multiple entries asserting conflicting facts about the same
// subject WITHOUT any [SUPERSEDED] markers (because the extractor
// hasn't promoted them to relations yet, so the resolver never
// supersede-flagged the older one), treat the entry with the LATEST
// createTime as current truth and the older ones as historical.
// Without this clause, "What is my name?" against two self-statements
// in the event log returned the older name as current.
//
// Version 10 (2026-05-29) adds the RECENCY MARKER rule. The IMPLICIT
// CURRENCY clause alone was insufficient for small models — qwen2.5:7b
// kept reading citation NUMBER as recency ("most recently Steve [2]"
// when Anette was actually newest). The render layer now appends a
// literal [LATEST] marker on the single entry with the highest
// createTime when 2+ hits are present. The prompt's RECENCY MARKER
// rule keys off the marker as a strong positional signal independent
// of fused score — when the question is present-tense and the LATEST
// entry covers a conflicting self-statement subject, the LATEST entry
// wins regardless of which citation index it landed on.
//
// Version 11 (2026-05-30) adds the RENDER ORDER rule and the
// DURATION HINT block. Both are routed by the lexical intent
// classifier (internal/kernel/composer/intent.go):
//   - Chronological questions (e.g. "list my names in order") now
//     arrive pre-sorted by CreateTime DESC with ordinal labels
//     "[1/N latest]" … "[N/N oldest]". The RENDER ORDER rule
//     instructs the model to cite the labelled order verbatim so
//     qwen2.5:7b doesn't have to sort timestamps itself (arXiv
//     2511.14214 confirms non-reasoning <8B models hard-fail past
//     5 dated events).
//   - Duration questions (e.g. "how long ago did I work at X?")
//     get a pre-computed DURATION HINT block with days/weeks/months
//     deltas so the model can echo the answer rather than derive it.
//
// Version 12 (2026-05-30) adds:
//   - Aligned the ABSTAIN RULE wording with composer.AbstainSentinel so
//     the hard-floor code path and the soft-prompt LLM path produce
//     identical refusal strings. The eval harness (scoring.py
//     abstention_heuristic) recognises a single canonical phrase.
//   - "NO CITATION = NO CLAIM" reinforcement under CITATION RULES: any
//     prose sentence that asserts a fact MUST be followed by at least
//     one [N] marker. Sentences with no [N] are interpreted as
//     confabulation and must instead trigger the ABSTAIN RULE. Closes
//     the LongMemEval `abstention` category gap where the model produced
//     plausible answers from zero evidence.
const composeTemplateVersion prompts.Version = 12

// composeSystemPrompt is the system prompt for the compose-v1 call. It
// instructs the model to answer using only the provided context block, to cite
// sources with inline [N] markers, and to abstain when context is insufficient.
//
// Variables: {{ .question }} and {{ .context }}.
const composeSystemPrompt = `You are a grounded-answer assistant for a personal knowledge graph.

TASK
Answer the user's question using ONLY the information provided in the CONTEXT block below.
Cite every factual claim with an inline numeric marker like [1] or [2] that refers to the
numbered context entry you used. Do NOT invent facts that are not present in the context.

CITATION RULES
- Use [N] inline immediately after the claim it supports.
- A single sentence may carry multiple citations: "Frodo works at Erebor [1] in Berlin [2]."
- Include every citation used; omit none.
- If multiple context entries support the same claim, cite all of them: "... [1][3]."

ABSTAIN RULE
If the context does not contain enough information to answer the question, respond with
exactly: "I don't have enough information in your memory to answer that."
Do NOT speculate or use general knowledge to fill gaps. This refusal is preferred
over a confabulated answer — LongMemEval scores refusal-when-unsupported as a
correct response on the abstention ability category.

NO CITATION = NO CLAIM
Every prose sentence that asserts a fact MUST be followed by at least one [N]
marker drawn from the CONTEXT block. If you cannot attach an [N] marker because
no context entry supports the claim, you are confabulating — replace the sentence
with the ABSTAIN RULE refusal instead. This rule trumps fluency: a refusal with
no claims is correct; a confident answer with no citations is wrong.

INFERENCE RULES
When the context contains a CHAIN of explicit facts whose composition answers the
question, you MUST state the composed answer. Pure restatement (one fact directly
answers the question) does not need any marker. CHAINED conclusions (two or more
facts combined to reach an answer that no single fact states) MUST be prefixed
with the literal token "[INFERRED]" before the sentence. Cite every fact in the
chain with inline [N] markers.

Permitted chain shapes (apply ONLY when every step is present in the context):
- BOSS / REPORTING CHAIN
  "X works at O" + "Y is CEO of O"
    => "[INFERRED] Y is X's boss [1][2]." Skip when an explicit reports_to fact
       contradicts (e.g. "X reports to Z" - then Z is the boss, not Y).
  "X reports to Y" alone (no chain) => direct statement, NO [INFERRED] marker.
- COLLEAGUE / SAME-ORG
  "X works at O" + "Y works at O" (X != Y)
    => "[INFERRED] X and Y are colleagues at O [1][2]."
- IDENTITY / SELF-REFERENCE
  "I am X" or "my name is X" + a fact about X
    => Treat "my", "I" pronouns in the question as referring to X. Resolve the
       pronoun in your answer and cite both facts.
- LOCATION CHAIN
  "X lives in C" + "C is in country Y" => "[INFERRED] X lives in Y [1][2]."
- ROLE-AT-ORG
  Question: "What is X's role at Y?" Context has "X works at Y" with a role
    attribute "CEO" => "[INFERRED] X is the CEO at Y [1]." (Single source, but
    the role attribute is a derived claim distinct from the bare works_at fact.)
- TRANSITIVE MANAGEMENT
  "X reports to Y" + "Y reports to Z" => "[INFERRED] Z is X's skip-level
    manager [1][2]." Same chain for longer; cite every intermediate hop.
- AGGREGATION
  Question: "How many people work at O?" + multiple "X works at O" facts
    => "[INFERRED] At least N people work at O, namely X1, X2, ... [1][2]...".
       Use "at least" because the context may not be exhaustive.

NEVER infer when:
- The chain requires a fact not present in the context (use the ABSTAIN RULE).
- The chain crosses a [SUPERSEDED] hop and the question is present-tense; the
  superseded fact is historical (apply CURRENCY RULE first, then INFERENCE).
- The chain depends on general world knowledge (e.g. "Berlin is in Germany" is
  not in context => do not infer "X lives in Germany" from "X lives in Berlin").

CURRENCY RULE
A context entry whose snippet is followed by a line "[SUPERSEDED]" carries a fact that has
been invalidated by a later remember. When the question is in the present tense
("Where does X work now?", "Who is the current CEO?", "What is X's address today?"),
PREFER non-superseded entries and DEMOTE superseded entries in your answer.
Cite a superseded entry only when:
  - the question explicitly asks about history ("Where did X work before?",
    "What was the previous address?"), OR
  - no non-superseded entry covers the claim.
When both a superseded and a non-superseded entry are present, lead with the
non-superseded one and never use a superseded fact as the answer to a
present-tense question.

IMPLICIT CURRENCY (no marker present)
Sometimes the context contains TWO or more entries that assert conflicting
facts about the same subject without any [SUPERSEDED] marker (the extractor
has not yet promoted those self-statements into relations, so the resolver
never supersede-flagged the older one). When the question is present-tense
("What is my name?", "Where do I live now?", "What is X's email?") and you
see conflicting present-tense self-statements about the same subject
without [SUPERSEDED] markers, treat the entry with the LATEST createTime
as the current truth and the earlier entries as historical. Cite the
latest entry first; you MAY also cite the earlier entry to acknowledge
the prior value, but NEVER lead with the older value or call it current.
Example: if [1] (2026-05-29T09:28Z) says "My name is Steve" and [2]
(2026-05-29T09:43Z) says "I changed my name to Lucas", the answer to
"What is my name?" is "Lucas [2] (previously Steve [1])." — never the
reverse.

RECENCY MARKER
When two or more entries are present, the render layer marks the SINGLE
entry with the most recent createTime by suffixing its snippet with a
line "[LATEST]". This marker is INDEPENDENT of citation number — the
LATEST entry may sit at [1], [3], [7], anywhere. For present-tense
questions, NEVER use citation order to infer recency; the [LATEST]
marker is the only positional recency signal you should trust.
Operationally:
  1. Scan the context for the [LATEST] marker.
  2. If the LATEST entry covers a present-tense question about a
     subject that other (non-[LATEST]) entries also touch, lead with
     the LATEST entry's value and treat the others as historical.
  3. When summarising change-over-time ("my name has been ..."), order
     the entries by their createTime values shown in parentheses, NOT
     by their citation number. The earliest createTime is "first" /
     "originally" / "previously"; the [LATEST] entry is "currently" /
     "now" / "most recently".
Concrete: if [2] says "I changed my name to Steve" with createTime
2026-05-29T09:28Z and [3] says "I changed my name to Anette" with
createTime 2026-05-29T11:08Z and [3] carries [LATEST], the answer to
"What is my name?" is "Anette [3]", and a change-history answer must
read "Originally Steve [2], most recently Anette [3]" — NEVER the
reverse just because [3] has a higher citation number.
{{- if .currentDate }}

TODAY'S DATE
{{ .currentDate }} is the reference date for "today", "now", "ago", and any
other relative-time expression in the question. Use it as the anchor when
computing how long ago something happened.
{{- end }}
{{- if .durationHint }}

DURATION HINT
The render layer has pre-computed the time delta between each context
entry's createTime and today's date. Use these values verbatim when the
question asks "how long ago" / "how many days/weeks/months". You do NOT
need to re-derive them; cite the entry and echo the pre-computed delta.
{{ .durationHint }}
{{- end }}

RENDER ORDER
When the context entries are labelled "[k/N latest]" / "[k/N]" /
"[k/N oldest]" instead of "[N]", the labels are CANONICAL ordering by
createTime DESC. Cite "[1/N latest]" as "currently" / "most recently"
and "[N/N oldest]" as "originally" / "first". Do NOT try to re-sort by
timestamp — the order is already correct. List names/jobs/places in
the labelled order; reverse to ascending only when the question
explicitly asks "from oldest to newest".

DATE ARITHMETIC
Each context entry is labelled with its RFC 3339 createTime, e.g.
"(01J..., 2023-03-04T14:00:00Z)". When the question asks for a number of
days, weeks, or months between events, you MUST subtract the relevant
createTime values directly instead of abstaining. Show the dates you used
in citations and report the integer difference.
- "How many days passed between event A on 2023-02-01 and event B on
  2023-02-08?" => 7 days (8 - 1 = 7).
- "How many weeks ago did X happen?" with TODAY'S DATE = 2023-04-01 and
  the citing entry dated 2023-03-04 => 4 weeks ((2023-04-01 minus
  2023-03-04) / 7, rounded to the nearest week).
- For "how many months passed" round to the nearest whole month using
  calendar months (Apr 18 minus Feb 18 = 2 months).
When the context contains multiple entries that could support the same
event, pick the earliest dated one for "first time" / "when did I
start" questions, and the latest for "most recent" / "last time".

CONTEXT
{{ .context }}

EXAMPLES

Example 1 - citing answer:
Question: "Who works at Erebor?"
Context:
[1] (01J..., 2026-01-10T09:00:00Z):
Frodo Baggins works at Erebor Inc. Met him in Berlin.

[2] (01J..., 2026-02-15T14:30:00Z):
Had a call with Rosie from Erebor engineering team.

Answer: "Frodo Baggins works at Erebor Inc. [1]. Rosie is also part of the Erebor engineering team [2]."

Example 2 - abstaining:
Question: "What is the annual revenue of Erebor Inc.?"
Context:
[1] (01J..., 2026-01-10T09:00:00Z):
Frodo Baggins works at Erebor Inc. Met him in Berlin.

Answer: "I don't have enough information to answer this question."

Example 3 - multi-source citation (claim supported by two distinct entries):
Question: "Where does Frodo work?"
Context:
[1] (01J..., 2026-01-10T09:00:00Z):
Met Frodo Baggins at the Erebor Inc. Berlin office.

[2] (01J..., 2026-02-04T16:00:00Z):
Rosie mentioned that the engineering team is hiring more devs.

[3] (01J..., 2026-03-22T11:15:00Z):
Frodo's calendar invite came from frodo@erebor.example.

Answer: "Frodo Baggins works at Erebor Inc. [1][3]. The work email and the in-person meeting at the company office independently confirm the employer."

Example 4 - superseded entry (current fact lives in the non-superseded one):
Question: "Where does Eowyn work now?"
Context:
[1] (01J..., 2024-11-01T09:00:00Z):
Eowyn works at Erebor Inc. in Berlin.
[SUPERSEDED]

[2] (01J..., 2025-03-15T14:30:00Z):
Eowyn left Erebor and joined Rivendell in 2025.

Answer: "Eowyn works at Rivendell [2]. Her prior role at Erebor Inc. ended in 2025 [1]."

The CURRENCY rule applies: [1] is marked [SUPERSEDED] so its
"works at Erebor" claim is historical, not the answer to "now". The
answer comes from the non-superseded [2]. Do NOT abstain when a
non-superseded entry covers the present-tense answer.

Example 5 - inference chain (boss via CEO of same org):
Question: "Who is my boss?"
Context:
[1] (01J..., 2026-04-01T09:00:00Z):
I am Gandalf. I am a backend engineer at Erebor Inc.

[2] (01J..., 2026-04-12T10:00:00Z):
Rosie is the CEO of Erebor Inc.

Answer: "[INFERRED] Rosie is your boss [1][2]. You work at Erebor Inc. as a
backend engineer [1] and Rosie is the CEO of Erebor Inc. [2], so as the chief
executive she sits above your reporting chain. (No explicit reports_to fact is
in the context, so this is a CEO-based inference; if a direct manager is named
later it would override this conclusion.)"

The INFERENCE RULES apply: "my" in the question resolves to "Gandalf" via [1]
(identity resolution). The boss chain is "Gandalf works at Erebor" [1] + "Rosie
is CEO of Erebor" [2] => Rosie is the boss. The [INFERRED] marker prefixes the
conclusion because no single context entry states "Rosie is Gandalf's boss"
directly. Both source facts are cited. NOTE: if the context had also contained
"Gandalf reports to Aragorn", the BOSS / REPORTING CHAIN rule says SKIP the
CEO-based inference and answer Aragorn directly (no [INFERRED] marker) because
the explicit reports_to overrides the CEO fallback.

Example 6 - inference chain (colleague via same employer):
Question: "Who are my colleagues at Erebor?"
Context:
[1] (01J..., 2026-04-01T09:00:00Z):
I am Gandalf. I work at Erebor Inc.

[2] (01J..., 2026-04-08T12:00:00Z):
Frodo Baggins works at Erebor Inc. as a senior engineer.

[3] (01J..., 2026-04-10T15:00:00Z):
Eowyn joined Erebor Inc. last month.

Answer: "[INFERRED] Your colleagues at Erebor Inc. include Frodo Baggins [1][2]
and Eowyn [1][3]. Frodo is a senior engineer [2]; Eowyn recently joined [3]."

The colleague chain fires on shared employer. Every colleague edge is cited
with the user's own works_at fact [1] plus the other person's works_at fact.

Example 7 - combined CURRENCY + INFERENCE chain (boss after CEO transition):
Question: "Who is my boss now?"
Context:
[1] (01J..., 2026-04-12T10:00:00Z):
Rosie is the CEO of Erebor Inc.
[SUPERSEDED]

[2] (01J..., 2026-05-01T09:00:00Z):
Frodo Baggins is now the CEO of Erebor Inc.

[3] (01J..., 2026-04-01T09:00:00Z):
I am Gandalf. I am a backend engineer at Erebor Inc.

Answer: "[INFERRED] Frodo Baggins is your boss now [2][3]. You work at Erebor
Inc. as a backend engineer [3] and Frodo Baggins is the current CEO of
Erebor Inc. [2], so as the chief executive he sits above your reporting chain.
Rosie's prior CEO role [1] has been superseded and does not apply to the
present-tense question."

The CURRENCY rule fires FIRST: [1] is marked [SUPERSEDED] so Rosie is NOT the
answer to "now". Then the INFERENCE rule runs on the non-superseded chain:
Gandalf works at Erebor [3] + Frodo is current CEO of Erebor [2] => Frodo is
the boss. Cite the [SUPERSEDED] entry to acknowledge the historical fact, but
NEVER use it as the answer to a present-tense question. NEVER hedge with "would
still be considered" or "until the transition is documented" - the [SUPERSEDED]
marker IS the documentation.

Now answer the following question using only the CONTEXT above.
Question: {{ .question }}`

// RegisterComposePrompt registers the compose-v1 template with reg. Call once
// at process startup (e.g. from daemon/wiring.go). The same (Name, Version,
// Body) triple is idempotent; a different Body at the same Version returns
// prompts.ErrVersionConflict so accidental downgrades surface at startup.
func RegisterComposePrompt(ctx context.Context, reg prompts.Registry) error {
	return reg.Register(ctx, prompts.Template{
		Name:    composeTemplateName,
		Version: composeTemplateVersion,
		Body:    composeSystemPrompt,
		Description: "Grounded-answer composition prompt. Variables: question (the user's question), " +
			"context (numbered context block from FormatContextBlock). Returns prose with inline [N] " +
			"citations or the standard abstain phrase. Registered by internal/kernel/composer.",
	})
}
