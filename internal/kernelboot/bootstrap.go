// Package kernelboot wires the kernel's event-log writer to the ontology
// registry that validates payloads against the seed schemas. It lives in
// its own package because internal/ontology imports internal/kernel, so
// kernel itself cannot perform the registration without an import cycle.
//
// The package is intentionally small. Production callers (the init wizard
// today, daemon and mcp serve next) call OpenLog to obtain a writer that
// is ready to accept the M1 event kinds.
package kernelboot

import (
	"context"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// OpenLog constructs an event-log writer ready to accept Episode and
// prompt.rendered events. It builds a fresh MemoryOntology, registers all
// seed kinds (Episode + prompt.rendered for M1), and returns the writer
// alongside a closer the caller invokes at shutdown. The closer wraps
// FileEventLog.Close, which is itself idempotent, so a deferred call is
// safe even when the caller has explicitly closed earlier.
//
// Single-writer invariant: FileEventLog takes an exclusive OS lock on
// events/index.bolt under dataDir. Only one process may hold an open log
// per dataDir at a time. The init wizard owns the lock for the duration
// of init; the daemon must not be running concurrently.
//
// The ctx is forwarded to the ontology registration calls. The current
// MemoryOntology implementation does not honour cancellation during
// Register, but plumbing the context through keeps the seam contract-
// complete so future ontology backends (e.g., a disk-backed registry in
// M2) can cancel a long-running registration cleanly.
func OpenLog(ctx context.Context, dataDir string) (*kernel.FileEventLog, func() error, error) {
	onto := kernel.NewMemoryOntology()
	if err := registerCoreKinds(ctx, onto); err != nil {
		return nil, nil, err
	}
	log := kernel.NewFileEventLog(dataDir, onto)
	return log, log.Close, nil
}

// registerCoreKinds registers every event-kind schema the write path validates
// against onto. The set is the single source of truth shared by OpenLog and
// any other caller that needs a fully-populated MemoryOntology. Each entry is
// (name, register) so a failure names the offending kind.
func registerCoreKinds(ctx context.Context, onto kernel.Ontology) error {
	kinds := []struct {
		name     string
		register func(context.Context, kernel.Ontology) error
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
		{"factRetracted", ontology.RegisterFactRetracted},
		{"factForgotten", ontology.RegisterFactForgotten},
		{"relationPromoted", ontology.RegisterRelationPromoted},
		{"relationSupersedeConflict", ontology.RegisterRelationSupersedeConflict},
		{"predicateRegistered", ontology.RegisterPredicateRegistered},
		{"relationCandidatesProposed", ontology.RegisterRelationCandidatesProposed},
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
		{"scopePromoted", ontology.RegisterScopePromoted},
		{"scopeDiscarded", ontology.RegisterScopeDiscarded},
		{"insightDismissed", ontology.RegisterInsightDismissed},
		{"contextPinned", ontology.RegisterContextPinned},
		{"contextUnpinned", ontology.RegisterContextUnpinned},
		{"contextCorrected", ontology.RegisterContextCorrected},
		{"prompt.rendered", prompts.RegisterKind},
	}
	for _, k := range kinds {
		if err := k.register(ctx, onto); err != nil {
			return fmt.Errorf("kernelboot: register %s: %w", k.name, err)
		}
	}
	return nil
}
