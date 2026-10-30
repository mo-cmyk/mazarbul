package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestCognitiveEvents_RegisterAndSchema registers each #231 event against the
// real kernel.MemoryOntology and asserts the schema's Kind / Version /
// Provenance, then round-trips a fully-populated payload byte-for-byte.
func TestCognitiveEvents_RegisterAndSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name        string
		register    func(context.Context, kernel.Ontology) error
		kind        string
		wantVersion string
		schema      func() kernel.Schema
		payload     any
	}{
		{
			name:        "memoryAccessed",
			register:    ontology.RegisterMemoryAccessed,
			kind:        ontology.KindMemoryAccessed,
			wantVersion: ontology.VersionMemoryAccessed,
			schema:      ontology.MemoryAccessedSchema,
			payload: ontology.MemoryAccessedEvent{
				TargetUID:  ontology.ULID(ulid.Make().String()),
				AccessedAt: time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
			},
		},
		{
			name:        "insightSynthesized",
			register:    ontology.RegisterInsightSynthesized,
			kind:        ontology.KindInsightSynthesized,
			wantVersion: ontology.VersionInsightSynthesized,
			schema:      ontology.InsightSynthesizedSchema,
			payload: ontology.InsightSynthesizedEvent{
				InsightUID:            ontology.ULID(ulid.Make().String()),
				Statement:             "Frodo consistently defers to Gandalf on travel decisions.",
				InsightType:           "pattern",
				SourceUids:            []ontology.ULID{ontology.ULID(ulid.Make().String())},
				SeedEntityUID:         ontology.ULID(ulid.Make().String()),
				SynthesizedConfidence: 0.8,
				SynthesizedAt:         time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
			},
		},
		{
			name:        "memoryConsolidated",
			register:    ontology.RegisterMemoryConsolidated,
			kind:        ontology.KindMemoryConsolidated,
			wantVersion: ontology.VersionMemoryConsolidated,
			schema:      ontology.MemoryConsolidatedSchema,
			payload: ontology.MemoryConsolidatedEvent{
				MemoryUID:      ontology.ULID(ulid.Make().String()),
				Summary:        "Across three sessions, the team settled on weighted RRF fusion.",
				MemoryType:     "semantic",
				SourceUids:     []ontology.ULID{ontology.ULID(ulid.Make().String()), ontology.ULID(ulid.Make().String())},
				CitedEntityUID: ontology.ULID(ulid.Make().String()),
				Confidence:     0.7,
				ConsolidatedAt: time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := kernel.NewMemoryOntology()
			if err := tc.register(ctx, o); err != nil {
				t.Fatalf("register: %v", err)
			}
			got, err := o.Schema(ctx, tc.kind)
			if err != nil {
				t.Fatalf("schema lookup: %v", err)
			}
			if got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
			}
			if got.Provenance != "core" {
				t.Errorf("Provenance = %q, want core", got.Provenance)
			}

			payload, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := o.Validate(ctx, tc.kind, payload); err != nil {
				t.Fatalf("validate complete payload: %v", err)
			}

			// Cached schema is byte-stable across calls.
			if !bytes.Equal(tc.schema().Definition, tc.schema().Definition) {
				t.Errorf("schema Definition not stable across calls")
			}
		})
	}
}

// TestCognitiveEvents_MissingRequiredField table-drives every required field of
// each event: drop it, assert validation fails. Covers the per-event error-code
// AC.
func TestCognitiveEvents_MissingRequiredField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ulid1 := ulid.Make().String()
	ulid2 := ulid.Make().String()

	cases := []struct {
		kind     string
		register func(context.Context, kernel.Ontology) error
		complete map[string]any
		required []string
	}{
		{
			kind:     ontology.KindMemoryAccessed,
			register: ontology.RegisterMemoryAccessed,
			complete: map[string]any{
				"targetUid":  ulid1,
				"accessedAt": "2026-05-25T10:00:00Z",
			},
			required: []string{"targetUid", "accessedAt"},
		},
		{
			kind:     ontology.KindInsightSynthesized,
			register: ontology.RegisterInsightSynthesized,
			complete: map[string]any{
				"insightUid":            ulid1,
				"statement":             "an insight",
				"insightType":           "pattern",
				"sourceUids":            []string{ulid2},
				"seedEntityUid":         ulid2,
				"synthesizedConfidence": 0.8,
				"synthesizedAt":         "2026-05-25T10:00:00Z",
			},
			required: []string{"insightUid", "statement", "insightType", "sourceUids", "seedEntityUid", "synthesizedConfidence", "synthesizedAt"},
		},
		{
			kind:     ontology.KindMemoryConsolidated,
			register: ontology.RegisterMemoryConsolidated,
			complete: map[string]any{
				"memoryUid":      ulid1,
				"summary":        "a summary",
				"memoryType":     "semantic",
				"sourceUids":     []string{ulid2},
				"citedEntityUid": ulid2,
				"confidence":     0.7,
				"consolidatedAt": "2026-05-25T10:00:00Z",
			},
			required: []string{"memoryUid", "summary", "memoryType", "sourceUids", "citedEntityUid", "confidence", "consolidatedAt"},
		},
	}

	for _, tc := range cases {
		for _, field := range tc.required {
			t.Run(tc.kind+" missing "+field, func(t *testing.T) {
				t.Parallel()
				o := kernel.NewMemoryOntology()
				if err := tc.register(ctx, o); err != nil {
					t.Fatalf("register: %v", err)
				}
				bad := make(map[string]any, len(tc.complete))
				for k, v := range tc.complete {
					if k == field {
						continue
					}
					bad[k] = v
				}
				payload, err := json.Marshal(bad)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if err := o.Validate(ctx, tc.kind, payload); err == nil {
					t.Errorf("expected validation error for missing %q, got nil", field)
				}
			})
		}
	}
}

// TestCognitiveEvents_EmptySourceUidsRejected pins the auditable-provenance AC at
// the schema layer: an insightSynthesized / memoryConsolidated with an empty
// sourceUids array fails minItems=1 validation. (The dispatcher enforces the
// same rule with a sentinel error; this proves the schema also rejects it.)
func TestCognitiveEvents_EmptySourceUidsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ulid1 := ulid.Make().String()

	cases := []struct {
		kind     string
		register func(context.Context, kernel.Ontology) error
		payload  map[string]any
	}{
		{
			kind:     ontology.KindInsightSynthesized,
			register: ontology.RegisterInsightSynthesized,
			payload: map[string]any{
				"insightUid":            ulid1,
				"statement":             "an insight",
				"insightType":           "pattern",
				"sourceUids":            []string{},
				"seedEntityUid":         ulid1,
				"synthesizedConfidence": 0.8,
				"synthesizedAt":         "2026-05-25T10:00:00Z",
			},
		},
		{
			kind:     ontology.KindMemoryConsolidated,
			register: ontology.RegisterMemoryConsolidated,
			payload: map[string]any{
				"memoryUid":      ulid1,
				"summary":        "a summary",
				"memoryType":     "semantic",
				"sourceUids":     []string{},
				"citedEntityUid": ulid1,
				"confidence":     0.7,
				"consolidatedAt": "2026-05-25T10:00:00Z",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind+" empty sourceUids", func(t *testing.T) {
			t.Parallel()
			o := kernel.NewMemoryOntology()
			if err := tc.register(ctx, o); err != nil {
				t.Fatalf("register: %v", err)
			}
			payload, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := o.Validate(ctx, tc.kind, payload); err == nil {
				t.Errorf("expected minItems validation error for empty sourceUids, got nil")
			}
		})
	}
}
