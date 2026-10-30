package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// openPredicateTestStore opens a fully-migrated SQLite store in a temp dir and
// registers a cleanup. The predicates table (migration 0006) and its domain/range
// extension (migration 0021) are applied automatically by sqlite.Open.
func openPredicateTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "pred_test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// capturePredicateAppender captures appended events for inspection.
type capturePredicateAppender struct {
	events []kernel.Event
}

func (c *capturePredicateAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	c.events = append(c.events, events...)
	return make([]kernel.Receipt, len(events)), nil
}

// TestDefinePredicate_UpgradesExisting verifies that calling definePredicate on
// an already-registered predicate upgrades its kind, domain/range, and
// description rather than no-oping. The predicate is first seeded as
// NonFunctional with no domain/range (mimicking ensureRelationPredicate), then
// definePredicate is called with kind=functional + subjectKinds/objectKinds.
// Asserts: status="updated", registry reports functional, a second
// predicateRegistered event was appended carrying the new subject/object kinds.
func TestDefinePredicate_UpgradesExisting(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openPredicateTestStore(t)

	reg, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Pre-register as NonFunctional with no domain/range, mimicking the
	// auto-registration that ensureRelationPredicate performs on first relate.
	tx, err := store.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := reg.Register(ctx, tx, resolver.Predicate{
		Name:        "mentors_at",
		Kind:        resolver.NonFunctional,
		Description: "Auto-registered from an asserted relation.",
		AddedAt:     time.Now().UTC(),
		AddedBy:     "llm",
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed Register: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	app := &capturePredicateAppender{}
	tool := tools.NewDefinePredicate(app, store.Writer(), reg, nil)

	req, _ := json.Marshal(map[string]any{
		"predicate":    "mentors_at",
		"kind":         "functional",
		"description":  "Subject mentors the object person at the given organisation.",
		"subjectKinds": []string{"person"},
		"objectKinds":  []string{"organization"},
	})
	raw, err := tool.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Predicate string `json:"predicate"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	// status must be "updated", not "registered" or "exists".
	if out.Status != "updated" {
		t.Errorf("status = %q, want updated", out.Status)
	}
	if out.Predicate != "mentors_at" {
		t.Errorf("predicate = %q, want mentors_at", out.Predicate)
	}

	// Registry must now report functional.
	if !reg.IsFunctional("mentors_at") {
		t.Error("IsFunctional(mentors_at) = false after upgrade, want true")
	}

	// A predicateRegistered event must have been appended.
	if len(app.events) == 0 {
		t.Fatal("no events appended; expected at least one predicateRegistered event")
	}
	// The appended event payload must carry the new subject/object kinds.
	var evPayload struct {
		Predicate    string   `json:"predicate"`
		SubjectKinds []string `json:"subjectKinds"`
		ObjectKinds  []string `json:"objectKinds"`
	}
	if err := json.Unmarshal(app.events[len(app.events)-1].Payload, &evPayload); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	if evPayload.Predicate != "mentors_at" {
		t.Errorf("event predicate = %q, want mentors_at", evPayload.Predicate)
	}
	if len(evPayload.SubjectKinds) != 1 || evPayload.SubjectKinds[0] != "person" {
		t.Errorf("event subjectKinds = %v, want [person]", evPayload.SubjectKinds)
	}
	if len(evPayload.ObjectKinds) != 1 || evPayload.ObjectKinds[0] != "organization" {
		t.Errorf("event objectKinds = %v, want [organization]", evPayload.ObjectKinds)
	}
}

// TestDefinePredicate_NewReturnsRegistered verifies that defining a brand-new
// predicate returns status="registered" (not "updated").
func TestDefinePredicate_NewReturnsRegistered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openPredicateTestStore(t)

	reg, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	app := &capturePredicateAppender{}
	tool := tools.NewDefinePredicate(app, store.Writer(), reg, nil)

	req, _ := json.Marshal(map[string]any{
		"predicate":   "advises",
		"kind":        "non-functional",
		"description": "Subject advises the object person or organisation.",
	})
	raw, err := tool.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if len(app.events) == 0 {
		t.Error("no events appended for new predicate")
	}
}

// TestDefinePredicate_UpgradeBadDomainKind_Validation verifies that the
// domain/range shape guard still fires on an upgrade call. A malformed kind
// (uppercase, spaces, leading dash) must return E_VALIDATION even when the
// predicate already exists in the registry.
func TestDefinePredicate_UpgradeBadDomainKind_Validation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openPredicateTestStore(t)

	reg, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Pre-seed so the predicate is already in the registry.
	tx, err := store.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := reg.Register(ctx, tx, resolver.Predicate{
		Name:        "coaches",
		Kind:        resolver.NonFunctional,
		Description: "Auto-registered from an asserted relation.",
		AddedAt:     time.Now().UTC(),
		AddedBy:     "llm",
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed Register: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	tool := tools.NewDefinePredicate(&capturePredicateAppender{}, store.Writer(), reg, nil)

	req, _ := json.Marshal(map[string]any{
		"predicate":    "coaches",
		"kind":         "functional",
		"description":  "Subject coaches the object person.",
		"subjectKinds": []string{"Bad Kind!"},
	})
	_, err = tool.Invoke(ctx, req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestDefinePredicate_BadName_Validation rejects a non-snake_case predicate.
// Name validation runs before the registry/writer are touched, so nil deps are
// safe here.
func TestDefinePredicate_BadName_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefinePredicate(&fakeAppender{}, nil, nil, nil)
	for _, bad := range []string{"Works At", "works-at", "1works", ""} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"predicate":   bad,
				"kind":        "functional",
				"description": "x",
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestDefinePredicate_BadKind_Validation rejects an out-of-enum kind. The kind
// switch runs after name validation but before registry access.
func TestDefinePredicate_BadKind_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefinePredicate(&fakeAppender{}, nil, nil, nil)
	req, _ := json.Marshal(map[string]any{
		"predicate":   "works_at",
		"kind":        "sometimes",
		"description": "x",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestDefinePredicate_DecodeError_Validation maps malformed arguments to
// E_VALIDATION.
func TestDefinePredicate_DecodeError_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefinePredicate(&fakeAppender{}, nil, nil, nil)
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"predicate":`))
	wantCode(t, err, mcp.CodeValidation)
}

// TestDefinePredicate_NilRegistry_Internal verifies a valid request against an
// unwired tool (nil registry/writer) returns E_INTERNAL instead of panicking on
// the nil registry dereference.
func TestDefinePredicate_NilRegistry_Internal(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefinePredicate(&fakeAppender{}, nil, nil, nil)
	req, _ := json.Marshal(map[string]any{
		"predicate":   "works_at",
		"kind":        "functional",
		"description": "Subject is employed by the object.",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}

// TestDefinePredicate_BadPropertySchema_Validation rejects a propertySchema that
// is not a JSON object (P5 typed relations). The propertySchema check runs after
// name/kind validation but before the registry is touched, so nil deps are safe.
func TestDefinePredicate_BadPropertySchema_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefinePredicate(&fakeAppender{}, nil, nil, nil)
	for _, bad := range []json.RawMessage{
		json.RawMessage(`"a string is not a schema object"`),
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`42`),
	} {
		t.Run(string(bad), func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"predicate":      "employed_by",
				"kind":           "non-functional",
				"description":    "x",
				"propertySchema": bad,
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestDefinePredicate_BadDomainKind_Validation rejects a malformed subject/object
// (domain/range) kind before the registry is touched. ValidateRelation compares
// endpoint kinds by exact string, so a shape-invalid value would register fine
// yet silently void the constraint (or wrongly reject under strict_domain_range).
// Shape validation runs before the nil-deps guard, so nil deps are safe here.
func TestDefinePredicate_BadDomainKind_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefinePredicate(&fakeAppender{}, nil, nil, nil)
	for _, tc := range []struct {
		name string
		kind string
	}{
		{"uppercase", "Person"},
		{"space", "user vehicle"},
		{"leading dash", "-bad"},
		{"empty namespace segment", "user:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"predicate":    "manages",
				"kind":         "functional",
				"description":  "x",
				"subjectKinds": []string{tc.kind},
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestDefinePredicate_ValidDomainKinds_PassShapeCheck verifies the shape check
// does not over-reject: well-formed bare core and namespaced kinds proceed past
// validation to the (nil) registry, surfacing E_INTERNAL rather than E_VALIDATION.
func TestDefinePredicate_ValidDomainKinds_PassShapeCheck(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefinePredicate(&fakeAppender{}, nil, nil, nil)
	req, _ := json.Marshal(map[string]any{
		"predicate":    "manages",
		"kind":         "functional",
		"description":  "x",
		"subjectKinds": []string{"person", "user:vehicle"},
		"objectKinds":  []string{"organization"},
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}
