package daemon

import (
	"context"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// buildOntology constructs a SQLiteOntology (unattached) seeded with every
// built-in schema kind the daemon needs. The ontology is constructed before
// the SQLite pool is open (boot-order constraint), so it uses late binding:
// the caller must invoke AttachStore on the returned ontology after the
// dispatcher's SQLite reader is available.
//
// Registration order: Episode first (existing M1 kind), then Entity /
// Mention / Relation / RelationRetracted / PredicateRegistered (P0
// additions), then prompt.rendered. The TODO comment marks the slot
// where the backend teammate's SecretProvider wiring lands.
func buildOntology(ctx context.Context) (*kernel.SQLiteOntology, error) {
	ont := kernel.NewSQLiteOntologyUnattached()
	// Table-driven so each new event kind is one row, not another error-checked
	// branch — keeps the function flat (the registry is keyed by kind, so order
	// is not load-bearing; kept stable for readable introspect output).
	// TODO(backend): SecretProvider chain wires in here.
	registrations := []struct {
		name string
		fn   func(context.Context, kernel.Ontology) error
	}{
		{"episode", ontology.RegisterEpisode},
		{"entity", ontology.RegisterEntity},
		{"mention", ontology.RegisterMention},
		{"relation", ontology.RegisterRelation},
		{"fact", ontology.RegisterFact},
		{"factRehomed", ontology.RegisterFactRehomed},
		{"sourceRefRecorded", ontology.RegisterSourceRefRecorded},
		{"agentFeedbackRecorded", ontology.RegisterAgentFeedbackRecorded},
		{"agentFeedbackReviewed", ontology.RegisterAgentFeedbackReviewed},
		{"agentFeedbackPromoted", ontology.RegisterAgentFeedbackPromoted},
		{"connectorWatermarkAdvanced", ontology.RegisterConnectorWatermarkAdvanced},
		{"relationRetracted", ontology.RegisterRelationRetracted},
		{"relationPromoted", ontology.RegisterRelationPromoted},
		{"relationSupersedeConflict", ontology.RegisterRelationSupersedeConflict},
		{"predicateRegistered", ontology.RegisterPredicateRegistered},
		{"relationCandidatesProposed", ontology.RegisterRelationCandidatesProposed},
		{"entityIdentifierAdded", ontology.RegisterEntityIdentifierAdded},
		{"episodeForgotten", ontology.RegisterEpisodeForgotten},
		{"entityUpdated", ontology.RegisterEntityUpdated},
		{"entityMerged", ontology.RegisterEntityMerged},
		{"entityDeleted", ontology.RegisterEntityDeleted},
		{"relationForgotten", ontology.RegisterRelationForgotten},
		{"factRetracted", ontology.RegisterFactRetracted},
		{"factForgotten", ontology.RegisterFactForgotten},
		{"documentRevision", ontology.RegisterDocumentRevision},
		{"documentDeleted", ontology.RegisterDocumentDeleted},
		{"entityKindRegistered", ontology.RegisterEntityKindRegistered},
		{"entityKindReviewed", ontology.RegisterEntityKindReviewed},
		{"schemaRegistered", ontology.RegisterSchemaRegistered},
		{"principalRegistered", ontology.RegisterPrincipalRegistered},
		{"deviceEnrolled", ontology.RegisterDeviceEnrolled},
		{"deviceRevoked", ontology.RegisterDeviceRevoked},
		{"presetInstalled", ontology.RegisterPresetInstalled},
		{"presetDeactivated", ontology.RegisterPresetDeactivated},
		{"teamMemberGranted", ontology.RegisterTeamMemberGranted},
		{"teamMemberRevoked", ontology.RegisterTeamMemberRevoked},
		{"teamRoleChanged", ontology.RegisterTeamRoleChanged},
		{"teamRegrantRequested", ontology.RegisterTeamRegrantRequested},
		{"memoryAccessed", ontology.RegisterMemoryAccessed},
		{"insightSynthesized", ontology.RegisterInsightSynthesized},
		{"insightDismissed", ontology.RegisterInsightDismissed},
		{"memoryConsolidated", ontology.RegisterMemoryConsolidated},
		{"scopePromoted", ontology.RegisterScopePromoted},
		{"scopeDiscarded", ontology.RegisterScopeDiscarded},
		{"contextPinned", ontology.RegisterContextPinned},
		{"contextUnpinned", ontology.RegisterContextUnpinned},
		{"contextCorrected", ontology.RegisterContextCorrected},
		{"prompt.rendered", prompts.RegisterKind},
	}
	for _, r := range registrations {
		if err := r.fn(ctx, ont); err != nil {
			return nil, fmt.Errorf("daemon: register %s schema: %w", r.name, err)
		}
	}
	return ont, nil
}
