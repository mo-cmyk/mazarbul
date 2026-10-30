package ontology_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestAgentFeedbackRecorded_Register_SchemaRoundtrips registers the schema
// against the real MemoryOntology, asserts its identity, and validates a fully
// populated event.
func TestAgentFeedbackRecorded_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterAgentFeedbackRecorded(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindAgentFeedbackRecorded)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindAgentFeedbackRecorded || got.Version != ontology.VersionAgentFeedbackRecorded {
		t.Errorf("schema identity = %q/%q, want %q/%q", got.Kind, got.Version,
			ontology.KindAgentFeedbackRecorded, ontology.VersionAgentFeedbackRecorded)
	}

	payload, err := json.Marshal(ontology.AgentFeedbackRecordedEvent{
		Category:     "confusing",
		Severity:     "medium",
		Surface:      "recall",
		Summary:      "recall returned nothing for an exact display name",
		Detail:       "expected the entity I just created to surface",
		Rule:         "prefer get over recall when you hold the exact name",
		Keywords:     []string{"recall", "exact-name"},
		Version:      "v0.3.0-dev",
		ActivePreset: "agent-memory",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindAgentFeedbackRecorded, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestAgentFeedbackRecorded_RejectsMissingRequired proves the required fields
// (category, severity, summary) are enforced by the schema.
func TestAgentFeedbackRecorded_RejectsMissingRequired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterAgentFeedbackRecorded(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing summary", `{"category":"slow","severity":"low"}`},
		{"missing category", `{"severity":"low","summary":"x"}`},
		{"bad category enum", `{"category":"vibes","severity":"low","summary":"x"}`},
		{"bad severity enum", `{"category":"slow","severity":"critical","summary":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := o.Validate(ctx, ontology.KindAgentFeedbackRecorded, json.RawMessage(tc.body)); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

// TestAgentFeedbackReviewed_Register_SchemaRoundtrips covers the review event.
func TestAgentFeedbackReviewed_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterAgentFeedbackReviewed(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindAgentFeedbackReviewed)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindAgentFeedbackReviewed {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindAgentFeedbackReviewed)
	}
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid pin", `{"clusterKey":"confusing|recall|foo","action":"pin"}`, false},
		{"valid edit", `{"clusterKey":"c|s|t","action":"edit","editedRule":"do X"}`, false},
		{"missing cluster", `{"action":"pin"}`, true},
		{"bad action", `{"clusterKey":"c|s|t","action":"delete"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := o.Validate(ctx, ontology.KindAgentFeedbackReviewed, json.RawMessage(tc.body))
			if tc.wantErr != (err != nil) {
				t.Errorf("validate %s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
			}
		})
	}
}

// TestAgentFeedbackPromoted_Register_SchemaRoundtrips covers the promoted event.
func TestAgentFeedbackPromoted_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterAgentFeedbackPromoted(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindAgentFeedbackPromoted)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindAgentFeedbackPromoted {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindAgentFeedbackPromoted)
	}
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid", `{"clusterKey":"confusing|recall|foo","recurrence":3,"severity":"low"}`, false},
		{"missing cluster", `{"recurrence":3,"severity":"low"}`, true},
		{"missing recurrence", `{"clusterKey":"c|s|t","severity":"low"}`, true},
		{"missing severity", `{"clusterKey":"c|s|t","recurrence":2}`, true},
		{"bad severity", `{"clusterKey":"c|s|t","recurrence":2,"severity":"critical"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := o.Validate(ctx, ontology.KindAgentFeedbackPromoted, json.RawMessage(tc.body))
			if tc.wantErr != (err != nil) {
				t.Errorf("validate %s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
			}
		})
	}
}

// TestLexicalClusterKey_StableAndCollapsesParaphrases proves the embedder-free
// fallback key is deterministic and coarse enough that near-identical reports
// collapse onto one cluster while distinct surfaces stay distinct.
func TestLexicalClusterKey_StableAndCollapsesParaphrases(t *testing.T) {
	t.Parallel()
	a := ontology.LexicalClusterKey("confusing", "recall", "Recall returned nothing for an exact name!")
	b := ontology.LexicalClusterKey("confusing", "recall", "recall returned nothing for an exact name")
	if a != b {
		t.Errorf("punctuation/case should not change the key:\n  a=%q\n  b=%q", a, b)
	}
	if a == "" {
		t.Fatalf("key should not be empty")
	}
	// Distinct surface => distinct cluster.
	c := ontology.LexicalClusterKey("confusing", "get", "Recall returned nothing for an exact name!")
	if a == c {
		t.Errorf("different surface should yield a different cluster key")
	}
}
