// Package extractor implements the LangExtract-mirror entity and relation
// extraction pipeline for Mazarbul's memory layer. It is Phase 2 of the
// memory-layer bootstrap plan (we-want-to-do-zazzy-newell.md).
//
// # Design overview
//
// The extractor mirrors Google's Aug-2025 LangExtract release in Go:
//
//   - Verbatim-text grounding with char-offset alignment (ground.go): every
//     extracted Mention carries an ExtractionText that must appear verbatim (or
//     near-verbatim within Levenshtein distance 2) in the source Episode. A
//     GroundingScore of 1.0 means exact match; lower values mean fuzzy match;
//     0.0 means confabulation. Mentions below the configurable threshold
//     (default 0.6) are silently dropped; confabulations (score == 0) are
//     debug-logged for the eval harness to count.
//
//   - ExampleData few-shot prompting (prompt.go): the system prompt carries at
//     least five hand-curated examples spanning person, organization, place,
//     project, and event entity kinds. Each example specifies verbatim text and
//     explicit char offsets so the model learns the grounding contract. Examples
//     are baked into the template registered under the name "extract-v1" in
//     internal/prompts.
//
//   - Multi-pass with class-conditional adaptive skip (passes.go): a first pass
//     runs full extraction. If any surviving Mention has groundingScore < 0.95,
//     or the relation-to-mention ratio on multi-entity episodes is below 0.3, a
//     second pass runs focused on the low-confidence entity kinds. If first-pass
//     all-grounded >= 0.95 the second pass is skipped entirely. Hard cap at 3
//     passes to bound token cost.
//
//   - Chunking (chunk.go): episodes longer than ~6000 characters are split at
//     sentence boundaries with a 200-character overlap. Each chunk is extracted
//     independently; mentions are deduplicated by (displayName, kind, sourceSpan)
//     and span offsets are adjusted back to global episode coordinates after
//     merging.
//
// # Wire into file_eventlog.go
//
// The Extractor is injected into FileEventLog via kernel.WithExtractor. After
// storage.Apply succeeds for an Append batch, applyExtraction calls
// extractor.ExtractFrom for each event. Episode events produce zero or more
// Mention events and a RelationCandidatesProposed event; non-episode events
// return nil, nil immediately. Extraction failures are warn-logged and never
// propagate to the original Append caller. The inner Append that writes the
// extraction results is itself safe because it runs outside the writer mutex.
//
// # Output shape
//
// Extract returns:
//   - []ontology.Mention: grounded, schema-valid Mention values. GroundingScore
//     >= threshold. SourceSpan set to the first occurrence of ExtractionText in
//     the episode text.
//   - []ontology.RelationCandidate: raw directed edges referencing Mention
//     indices (not UIDs). The resolver (P3) assigns UIDs.
//
// ExtractFrom is the kernel.ExtractorIface adapter. It marshals the above into
// kernel.Event slices ready for l.Append.
//
// # References
//
//   - LangExtract (Google, Aug 2025): verbatim grounding + ExampleData few-shot
//   - Graphiti (Zep): incremental episode-based ingestion; bi-temporal validity
//   - LongMemEval benchmark: the eval harness that drives threshold calibration
package extractor
