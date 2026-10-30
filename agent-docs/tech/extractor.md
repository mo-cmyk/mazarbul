# Extractor: LangExtract-mirror extraction pipeline

**Package:** `internal/kernel/extractor`
**Phase:** P2 of the memory-layer bootstrap (zazzy-newell plan)
**Status:** shipped in `feat/memory-layer-bootstrap`

---

## Overview

The extractor runs after every Episode Append and turns raw text into structured
`Mention` events and a `RelationCandidatesProposed` event. It is the first layer
of the memory pipeline: extraction -> resolver (P3) -> query extensions (P5).

Design is a Go-native mirror of Google LangExtract (Aug 2025): verbatim-text
grounding, ExampleData few-shot prompting, multi-pass with adaptive skip, and
sentence-boundary chunking for long episodes.

---

## Grounding contract (`ground.go`)

Every extracted Mention carries an `extractionText` field that must appear in
the source Episode text. The grounding pass locates it and sets `SourceSpan`
(byte offsets) and `GroundingScore`:

| Match type | Score | Detail |
|---|---|---|
| Exact substring | 1.0 | `strings.Index` in O(n) |
| Case-insensitive exact | 0.95 | `strings.ToLower` + `Index` |
| Levenshtein dist 1 | ~0.86 | Sliding-window DP over runes |
| Levenshtein dist 2 | ~0.71 | Same |
| No match | 0.0 | Confabulation; span = {0,0} |

Mentions with `groundingScore < threshold` (default 0.6, configurable via
`Config.GroundingThreshold`) are **dropped before event emission**. Score-0
confabulations are debug-logged so the LongMemEval harness can count them.

This is the single biggest quality signal that LangExtract demonstrated over
mem0: verbatim grounding rejects hallucinated entities before they pollute the
knowledge graph.

---

## ExampleData few-shot prompting (`prompt.go`)

The `extract-v1` template is registered in `internal/prompts` with version 1.
The system prompt contains:

- A grounding contract section explaining the verbatim-text requirement.
- An entity-kind taxonomy (person, organization, place, project, event, document).
- A seed relation predicate list (works_at, member_of, located_in, ...).
- Five richly-annotated examples (single person, person+org+place, project
  membership, event attendance, relation supersession with temporal hint).

The tool schema passed via `ChatProvider.ToolUse` constrains the model output to
a JSON object with `mentions[]` and `relations[]` arrays. The extractor's
`parse.go` validates and normalises the raw JSON before grounding.

---

## Multi-pass with adaptive skip (`passes.go`)

```
Pass 1: full extraction
  -> if allWellGrounded (all scores >= 0.95): DONE (adaptive skip)
  -> else: compute lowConfidenceKinds, run focused pass 2
Pass 2: focused on low-confidence entity kinds
  -> if allWellGrounded or !needsSecondPass: DONE
  -> else: pass 3 (hard cap)
Pass 3: same as pass 2
DONE
```

`needsSecondPass` fires when:
1. Any surviving Mention has `groundingScore < 0.95`, OR
2. `len(relations)/len(mentions) < 0.3` on episodes with >= 2 mentions (model
   skipped obvious edges in pass 1).

The focused pass appends a short hint to the episode text before rendering the
prompt so the model re-examines specific entity kinds without re-reading the
full prompt preamble.

---

## Chunking (`chunk.go`)

Episodes longer than `Config.ChunkThreshold` (default 6000 bytes) are split at
sentence boundaries (`. `, `! `, `? `, `\n`) with `Config.ChunkOverlap`
(default 200 bytes) overlap. Each chunk is extracted independently; results are
merged with:

1. `AdjustSpans` -- translates chunk-relative byte offsets back to global
   episode coordinates.
2. `DeduplicateMentions` -- removes duplicates by `(displayName, kind, sourceSpan)`,
   keeping the higher-scored mention in a tie.

---

## Wire-up (`file_eventlog.go`)

```go
// After storage.Apply succeeds:
l.applyExtraction(ctx, prepared)
```

`applyExtraction` runs under `context.WithoutCancel + 90s timeout` so a slow
LLM call never cancels on behalf of the original `Append` caller and never pins
the goroutine indefinitely.

For each episode event, it calls `extractor.ExtractFrom`, appends the resulting
events (`mention` + `relationCandidatesProposed`) via the same `FileEventLog`,
and warn-logs any failures. **Failures never fail the original Append.**

The extractor is injected via `kernel.WithExtractor(e)`. The default is nil
(no extraction), so all existing tests that do not supply a ChatProvider
continue to pass unchanged.

---

## Output events

| Event kind | Payload | Emitted |
|---|---|---|
| `mention` | `ontology.Mention` (grounded, schema-valid) | One per surviving Mention |
| `relationCandidatesProposed` | `ontology.RelationCandidatesProposedEvent` | One per Episode (carries all candidates) |

The dispatcher handles both kinds:
- `mention` -> `applyMention` (relational + vector, as in P4)
- `relationCandidatesProposed` -> debug log only in P2; P3 resolver extends this

---

## Testdata fixtures

| Dir | Episode type |
|---|---|
| `01_single_person` | Single person mention |
| `02_multi_person` | Multi-person co-mention |
| `03_person_org_place` | Person + organization + place triad |
| `04_project_membership` | Project membership relation |
| `05_event_attendance` | Event attendance with location |
| `06_supersession` | Temporal relation (works_at supersession) |
| `07_confabulation_trap` | No entities; a naive model may invent |
| `08_short_episode` | < 200 chars |
| `09_long_episode` | > 6000 bytes (triggers chunking) |

---

## Configuration

| Field | Default | Description |
|---|---|---|
| `Config.Model` | (required) | Provider model name |
| `Config.GroundingThreshold` | 0.6 | Minimum score to survive filter |
| `Config.ChunkThreshold` | 6000 | Bytes before chunking kicks in |
| `Config.ChunkOverlap` | 200 | Overlap between consecutive chunks |
| `Config.MaxTokens` | 2048 | Per-call token cap |

---

## References

- LangExtract (Google, Aug 2025): ExampleData few-shot, verbatim grounding
- Graphiti (Zep): incremental episode ingestion, bi-temporal validity
- LongMemEval benchmark: eval driver for threshold calibration
- `agent-docs/designs/storage.md`: four-layer storage and event-log contract
- `we-want-to-do-zazzy-newell.md`: full memory-layer bootstrap plan
