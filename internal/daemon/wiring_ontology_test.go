package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestBuildOntology_RegistersIdentityKinds guards the #209 genesis path: the
// daemon's buildOntology must register the principalRegistered + deviceEnrolled
// schemas, or seedIdentityOrWarn fails at Append with "schema not registered"
// and the entire signed-identity substrate silently never initialises (genesis
// is best-effort/non-fatal, so the daemon still boots — just unsigned). This
// registration list drifted from kernelboot.registerCoreKinds once already; the
// gap was invisible to the unit tests because they build their log via
// kernelboot.OpenLog (which DID register the kinds) and only surfaced when the
// real binary ran genesis through OpenWiring.
func TestBuildOntology_RegistersIdentityKinds(t *testing.T) {
	t.Parallel()
	ont, err := buildOntology(context.Background())
	if err != nil {
		t.Fatalf("buildOntology: %v", err)
	}
	for _, kind := range []string{"principalRegistered", "deviceEnrolled"} {
		// An empty payload fails schema validation, but a REGISTERED kind never
		// returns ErrSchemaNotRegistered — that sentinel is the regression we
		// guard against (an unregistered kind short-circuits to it).
		if verr := ont.Validate(context.Background(), kind, []byte(`{}`)); errors.Is(verr, kernel.ErrSchemaNotRegistered) {
			t.Errorf("kind %q not registered in buildOntology — genesis Append would fail: %v", kind, verr)
		}
	}
}

// TestBuildOntology_RegistersFeedbackKinds guards the dynamic feedback loop's
// audit trail. All three agentFeedback* event kinds must be registered —
// agentFeedbackPromoted especially: its registration was once missing, so the
// promotion audit event was silently rejected at Append (the error was
// swallowed as best-effort) and the agent_feedback_promotions projection stayed
// permanently empty even though `feedback gotchas` still worked off row counts.
func TestBuildOntology_RegistersFeedbackKinds(t *testing.T) {
	t.Parallel()
	ont, err := buildOntology(context.Background())
	if err != nil {
		t.Fatalf("buildOntology: %v", err)
	}
	for _, kind := range []string{"agentFeedbackRecorded", "agentFeedbackReviewed", "agentFeedbackPromoted"} {
		if verr := ont.Validate(context.Background(), kind, []byte(`{}`)); errors.Is(verr, kernel.ErrSchemaNotRegistered) {
			t.Errorf("feedback kind %q not registered in buildOntology — its events would be rejected at Append: %v", kind, verr)
		}
	}
}

// TestBuildOntology_RegistersFactRehomed guards the doc-entity wave: the
// daemon's buildOntology must register the factRehomed schema, or every
// observation re-home Append is rejected with ErrSchemaNotRegistered
// (E_VALIDATION) on the live daemon — the feature builds and its dispatcher
// handler exists, but no factRehomed event can ever be written. The unit tests
// for applyFactRehomed mask this because they validate against a noop ontology;
// only the real wiring table gates the live path. (Same drift class as
// TestBuildOntology_RegistersIdentityKinds.)
func TestBuildOntology_RegistersFactRehomed(t *testing.T) {
	t.Parallel()
	ont, err := buildOntology(context.Background())
	if err != nil {
		t.Fatalf("buildOntology: %v", err)
	}
	if verr := ont.Validate(context.Background(), "factRehomed", []byte(`{}`)); errors.Is(verr, kernel.ErrSchemaNotRegistered) {
		t.Errorf("kind \"factRehomed\" not registered in buildOntology — every observation re-home Append would fail: %v", verr)
	}
}
