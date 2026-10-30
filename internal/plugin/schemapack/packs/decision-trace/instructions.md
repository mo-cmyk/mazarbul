# Decision-trace operating instructions

You are maintaining a decision-provenance graph in this knowledge base.
Model the reasoning trail with the `dt:` vocabulary this preset installed,
and keep the graph clean as new decisions, insights, and principles emerge.

## Vocabulary

- **dt:decision** — a discrete choice made at a point in time. Captures
  `status` (active, superseded, reversed), `decisionDate`, `drivers`
  (the key constraints), and `alternatives` (options not taken).
- **dt:insight** — a distilled observation extracted from evidence or
  analysis. Carries `confidence` (0.0–1.0), `sourceKind`, and `validFrom`.
- **dt:principle** — a durable rule or value that guides recurring choices.
  Carries `domain`, `priority`, and `origin`.

Relations:

- `derived_from` — an insight or decision was derived from another entity
  (evidence, document, or prior decision). Non-functional; many sources allowed.
- `supersedes_because` — a decision explicitly replaces exactly one prior
  decision. Functional; use `status=superseded` on the old decision too.
- `rationale_for` — an insight or principle supplies the rationale for a
  decision. Non-functional; a decision may draw on several.

## How to operate

1. **When a choice is described**, create a `dt:decision`. Set `decisionDate`
   to the date it was made (not recorded). Record `drivers` as a brief prose
   summary of constraints, not a restatement of the option chosen. Record
   `alternatives` when the source explicitly mentions rejected options.

2. **When an observation or learning is articulated**, create a `dt:insight`.
   Set `confidence` honestly: 0.8+ means strong evidence, 0.5 means plausible
   but contested, below 0.5 means speculative. Link its evidence sources with
   `derived_from`.

3. **When a standing rule or value is stated**, create a `dt:principle`. Link
   it to any decision it informed with `rationale_for`.

4. **When a decision replaces a prior one**, link the new decision to the old
   with `supersedes_because` and update the old decision's `status` to
   `superseded`. Do not delete the old decision — its history is the point.

5. **Prefer updating an existing entity over creating a duplicate.** Search
   by display name before creating a new decision or principle.

6. **Do not invent rationale.** If the source does not explain why a decision
   was made, leave `drivers` and `rationale_for` empty rather than guessing.

## When asked about a decision's provenance

Traverse from the `dt:decision` entity:
- Follow `rationale_for` edges (reversed) to find supporting insights and principles.
- Follow `supersedes_because` chains to reconstruct the lineage.
- Follow `derived_from` edges on insights to surface primary evidence.

Surface decisions whose `status` is `superseded` with a clear label so the
reader knows they are no longer active.
