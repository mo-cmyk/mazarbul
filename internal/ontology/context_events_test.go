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

// TestContextEvents_RegisterAndSchema registers each new agent-context event
// against the real kernel.MemoryOntology and asserts the schema's Kind /
// Version / Provenance, then round-trips a fully-populated payload through
// Validate.
func TestContextEvents_RegisterAndSchema(t *testing.T) {
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
			name:        "insightDismissed",
			register:    ontology.RegisterInsightDismissed,
			kind:        ontology.KindInsightDismissed,
			wantVersion: ontology.VersionInsightDismissed,
			schema:      ontology.InsightDismissedSchema,
			payload: ontology.InsightDismissedEvent{
				InsightUID:  ontology.ULID(ulid.Make().String()),
				Reason:      "no longer relevant after the migration finished",
				DismissedAt: time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC),
			},
		},
		{
			name:        "contextPinned",
			register:    ontology.RegisterContextPinned,
			kind:        ontology.KindContextPinned,
			wantVersion: ontology.VersionContextPinned,
			schema:      ontology.ContextPinnedSchema,
			payload: ontology.ContextPinnedEvent{
				PinUID:    ontology.ULID(ulid.Make().String()),
				Text:      "Mid-migration off Postgres — do not suggest new Postgres features.",
				TargetRef: "entities/" + ulid.Make().String(),
				PinnedAt:  time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC),
			},
		},
		{
			name:        "contextUnpinned",
			register:    ontology.RegisterContextUnpinned,
			kind:        ontology.KindContextUnpinned,
			wantVersion: ontology.VersionContextUnpinned,
			schema:      ontology.ContextUnpinnedSchema,
			payload: ontology.ContextUnpinnedEvent{
				PinUID:     ontology.ULID(ulid.Make().String()),
				UnpinnedAt: time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC),
			},
		},
		{
			name:        "contextCorrected",
			register:    ontology.RegisterContextCorrected,
			kind:        ontology.KindContextCorrected,
			wantVersion: ontology.VersionContextCorrected,
			schema:      ontology.ContextCorrectedSchema,
			payload: ontology.ContextCorrectedEvent{
				CorrectionUID: ontology.ULID(ulid.Make().String()),
				TargetID:      "entities/" + ulid.Make().String(),
				Text:          "Frodo left the Shire in 3018, not 3019.",
				CorrectedAt:   time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC),
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

// TestContextEvents_MissingRequiredField table-drives every required field of
// each event: drop it, assert validation fails.
func TestContextEvents_MissingRequiredField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	id1 := ulid.Make().String()
	id2 := ulid.Make().String()

	cases := []struct {
		kind     string
		register func(context.Context, kernel.Ontology) error
		complete map[string]any
		required []string
	}{
		{
			kind:     ontology.KindInsightDismissed,
			register: ontology.RegisterInsightDismissed,
			complete: map[string]any{
				"insightUid":  id1,
				"reason":      "stale",
				"dismissedAt": "2026-06-24T09:00:00Z",
			},
			required: []string{"insightUid", "dismissedAt"},
		},
		{
			kind:     ontology.KindContextPinned,
			register: ontology.RegisterContextPinned,
			complete: map[string]any{
				"pinUid":    id1,
				"text":      "a steering note",
				"targetRef": "entities/" + id2,
				"pinnedAt":  "2026-06-24T09:00:00Z",
			},
			required: []string{"pinUid", "text", "pinnedAt"},
		},
		{
			kind:     ontology.KindContextUnpinned,
			register: ontology.RegisterContextUnpinned,
			complete: map[string]any{
				"pinUid":     id1,
				"unpinnedAt": "2026-06-24T09:00:00Z",
			},
			required: []string{"pinUid", "unpinnedAt"},
		},
		{
			kind:     ontology.KindContextCorrected,
			register: ontology.RegisterContextCorrected,
			complete: map[string]any{
				"correctionUid": id1,
				"targetId":      "entities/" + id2,
				"text":          "the corrected text",
				"correctedAt":   "2026-06-24T09:00:00Z",
			},
			required: []string{"correctionUid", "targetId", "text", "correctedAt"},
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

// TestContextEvents_OptionalFieldsOmittable proves the optional fields really
// are optional: a contextPinned with no targetRef and an insightDismissed with
// no reason both validate.
func TestContextEvents_OptionalFieldsOmittable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterContextPinned(ctx, o); err != nil {
		t.Fatalf("register contextPinned: %v", err)
	}
	if err := ontology.RegisterInsightDismissed(ctx, o); err != nil {
		t.Fatalf("register insightDismissed: %v", err)
	}

	pin, _ := json.Marshal(map[string]any{
		"pinUid":   ulid.Make().String(),
		"text":     "a free-floating steering note",
		"pinnedAt": "2026-06-24T09:00:00Z",
	})
	if err := o.Validate(ctx, ontology.KindContextPinned, pin); err != nil {
		t.Errorf("contextPinned without targetRef should validate: %v", err)
	}

	dis, _ := json.Marshal(map[string]any{
		"insightUid":  ulid.Make().String(),
		"dismissedAt": "2026-06-24T09:00:00Z",
	})
	if err := o.Validate(ctx, ontology.KindInsightDismissed, dis); err != nil {
		t.Errorf("insightDismissed without reason should validate: %v", err)
	}
}
