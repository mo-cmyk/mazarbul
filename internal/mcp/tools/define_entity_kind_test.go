package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
)

// wantCode asserts err is an *mcp.ToolError carrying code. This helper is
// defined here (package tools_test) because the identical helper in
// navigate_test.go lives in package tools (internal) and is not visible to the
// external test package.
func wantCode(t *testing.T, err error, code mcp.ToolErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", code)
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not *mcp.ToolError", err)
	}
	if te.Code != code {
		t.Fatalf("error code = %q, want %q (err: %v)", te.Code, code, err)
	}
}

// fakeKindSourceDEK is a dedup.KindSource stub for defineEntityKind tests.
type fakeKindSourceDEK struct {
	kinds []navigate.RegisteredEntityKind
	err   error
}

func (f *fakeKindSourceDEK) RegisteredEntityKinds(_ context.Context) ([]navigate.RegisteredEntityKind, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.kinds, nil
}

// TestDefineEntityKind_HappyPath_Registered verifies a clean registration
// appends one entityKindRegistered event and returns status=registered.
func TestDefineEntityKind_HappyPath_Registered(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A motorised road vehicle.",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Name != "user:vehicle" {
		t.Errorf("name = %q, want user:vehicle", out.Name)
	}
	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("appended %d events, want 1", len(fa.gotEvents))
	}
	if string(fa.gotEvents[0].Kind) != "entityKindRegistered" {
		t.Errorf("event kind = %q, want entityKindRegistered", fa.gotEvents[0].Kind)
	}
}

// TestDefineEntityKind_CoreKind_Validation ensures core kinds are rejected.
func TestDefineEntityKind_CoreKind_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefineEntityKind(&fakeAppender{}, nil, nil, false)

	for _, core := range []string{"person", "organization", "place", "project", "event", "document"} {
		t.Run(core, func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"name":        core,
				"displayName": "X",
				"description": "x",
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestDefineEntityKind_InvalidName_Validation ensures names with uppercase or
// a leading digit are rejected.
func TestDefineEntityKind_InvalidName_Validation(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefineEntityKind(&fakeAppender{}, nil, nil, false)

	for _, bad := range []string{"Vehicle", "1vehicle", "user:vehicle", "vehicle-type"} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			req, _ := json.Marshal(map[string]any{
				"name":        bad,
				"displayName": "X",
				"description": "x",
			})
			_, err := tool.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestDefineEntityKind_AppendError_Internal verifies appender failure maps to
// E_INTERNAL.
func TestDefineEntityKind_AppendError_Internal(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{err: errors.New("storage unavailable")}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "widget",
		"displayName": "Widget",
		"description": "A generic widget.",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeInternal)
}

// TestDefineEntityKind_Dedup_Reused checks that a near-match via dedup returns
// status=reused without appending an event.
func TestDefineEntityKind_Dedup_Reused(t *testing.T) {
	t.Parallel()

	existing := []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}
	src := &fakeKindSourceDEK{kinds: existing}
	checker := dedup.New(src, nil)

	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A motorised road vehicle.",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status     string `json:"status"`
		ReusedKind string `json:"reusedKind"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "reused" {
		t.Errorf("status = %q, want reused", out.Status)
	}
	if out.ReusedKind == "" {
		t.Error("reusedKind should be non-empty")
	}
	if len(fa.gotEvents) != 0 {
		t.Errorf("expected 0 appended events on reuse, got %d", len(fa.gotEvents))
	}
}

// TestDefineEntityKind_Dedup_ReuseSkipsExtendsValidation pins the Invoke
// ordering: the dedup check runs before extends resolution, so a call that
// resolves to an existing kind returns status=reused even when its extends
// argument names an unknown parent. Nothing is minted on the reuse path, so
// the extends value never persists — failing the call on it would only
// punish a near-duplicate request for a typo in a field that goes unused.
func TestDefineEntityKind_Dedup_ReuseSkipsExtendsValidation(t *testing.T) {
	t.Parallel()

	existing := []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}
	src := &fakeKindSourceDEK{kinds: existing}
	checker := dedup.New(src, nil)

	fa := &fakeAppender{}
	// src doubles as the kind lister: "machne" is not a registered kind, so
	// extends resolution would return E_VALIDATION if it ran before dedup.
	tool := tools.NewDefineEntityKind(fa, checker, src, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A motorised road vehicle.",
		"extends":     "machne",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v (reuse must not validate extends)", err)
	}
	var out struct {
		Status     string `json:"status"`
		ReusedKind string `json:"reusedKind"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "reused" {
		t.Errorf("status = %q, want reused", out.Status)
	}
	if out.ReusedKind != "user:vehicle" {
		t.Errorf("reusedKind = %q, want user:vehicle", out.ReusedKind)
	}
	if len(fa.gotEvents) != 0 {
		t.Errorf("expected 0 appended events on reuse, got %d", len(fa.gotEvents))
	}
}

// TestDefineEntityKind_Force_BypassesDedup ensures force=true registers even
// when dedup would say Reuse.
func TestDefineEntityKind_Force_BypassesDedup(t *testing.T) {
	t.Parallel()

	existing := []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}
	src := &fakeKindSourceDEK{kinds: existing}
	checker := dedup.New(src, nil)

	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A motorised road vehicle.",
		"force":       true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "registered" {
		t.Errorf("status = %q, want registered (force bypasses dedup)", out.Status)
	}
	if len(fa.gotEvents) != 1 {
		t.Errorf("expected 1 event, got %d", len(fa.gotEvents))
	}
}

// TestDefineEntityKind_PropertySchema_Valid verifies a valid JSON Schema object
// is accepted.
func TestDefineEntityKind_PropertySchema_Valid(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "deal",
		"displayName": "Deal",
		"description": "A commercial deal.",
		"propertySchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "number"},
			},
		},
	})
	_, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke with valid propertySchema: %v", err)
	}
}

// TestDefineEntityKind_PropertySchema_Invalid verifies that a non-object
// propertySchema triggers E_VALIDATION.
func TestDefineEntityKind_PropertySchema_Invalid(t *testing.T) {
	t.Parallel()
	tool := tools.NewDefineEntityKind(&fakeAppender{}, nil, nil, false)

	// propertySchema must be a JSON object; a string literal fails
	// json.Unmarshal into map[string]any inside compilePropertySchema.
	req := json.RawMessage(`{"name":"deal","displayName":"Deal","description":"A commercial deal.","propertySchema":"not-an-object"}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestDefineEntityKind_PropertySchema_Uncompilable verifies that a JSON object
// that is not a valid JSON Schema (e.g. "type" must be a string/array, not a
// number) is rejected as E_VALIDATION rather than "registered" then silently
// dropped by the dispatcher.
func TestDefineEntityKind_PropertySchema_Uncompilable(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req := json.RawMessage(`{"name":"deal","displayName":"Deal","description":"A commercial deal.","propertySchema":{"type":12}}`)
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("an uncompilable schema must not append an event, got %d", len(fa.gotEvents))
	}
}

// --- extends validation tests (Greptile fix: kindLister as 3rd ctor arg) ---

// vehicleLister is a one-kind registry used across the extends tests.
func vehicleLister() *fakeKindSourceDEK {
	return &fakeKindSourceDEK{kinds: []navigate.RegisteredEntityKind{{Name: "user:vehicle"}}}
}

// TestDefineEntityKind_Extends_UnknownParent verifies that a typo in the
// extends field (user:vehcile vs user:vehicle) is rejected with E_VALIDATION
// so the dispatcher never persists a dangling parent reference.
func TestDefineEntityKind_Extends_UnknownParent(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, vehicleLister(), false)

	req, _ := json.Marshal(map[string]any{
		"name":        "ecar",
		"displayName": "E-Car",
		"description": "An electric road vehicle.",
		"extends":     "user:vehcile", // deliberate typo
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	if len(fa.gotEvents) != 0 {
		t.Errorf("unknown parent must not append any event, got %d", len(fa.gotEvents))
	}
}

// TestDefineEntityKind_Extends_BareNameResolution verifies that a bare parent
// name (vehicle) resolves to its canonical registered form (user:vehicle) when
// unambiguous, and that the persisted event payload carries the full name.
func TestDefineEntityKind_Extends_BareNameResolution(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, vehicleLister(), false)

	req, _ := json.Marshal(map[string]any{
		"name":        "ecar",
		"displayName": "E-Car",
		"description": "An electric road vehicle.",
		"extends":     "vehicle", // bare name, should resolve to user:vehicle
	})
	_, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fa.gotEvents))
	}
	var payload ontology.EntityKindRegisteredEvent
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload.Extends != "user:vehicle" {
		t.Errorf("payload.Extends = %q, want user:vehicle", payload.Extends)
	}
}

// TestDefineEntityKind_Extends_AmbiguousBareName verifies that a bare parent
// name matching kinds in multiple namespaces returns E_VALIDATION whose message
// names both candidates.
func TestDefineEntityKind_Extends_AmbiguousBareName(t *testing.T) {
	t.Parallel()
	lister := &fakeKindSourceDEK{kinds: []navigate.RegisteredEntityKind{
		{Name: "user:asset"},
		{Name: "extension:business_de:asset"},
	}}
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, lister, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "realestate",
		"displayName": "Real Estate",
		"description": "A real-estate asset.",
		"extends":     "asset",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("expected *mcp.ToolError, got %T", err)
	}
	if !strings.Contains(te.Message, "user:asset") || !strings.Contains(te.Message, "extension:business_de:asset") {
		t.Errorf("ambiguous error message should name both candidates, got: %q", te.Message)
	}
}

// TestDefineEntityKind_Extends_SelfExtension verifies that setting extends to
// the kind's own would-be canonical name is rejected with E_VALIDATION.
func TestDefineEntityKind_Extends_SelfExtension(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, vehicleLister(), false)

	// "vehicle" with extends:"user:vehicle" — after prefixing name becomes
	// "user:vehicle", which equals the resolved parent.
	req, _ := json.Marshal(map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A motorised road vehicle.",
		"extends":     "user:vehicle",
	})
	_, err := tool.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeValidation)
}

// TestDefineEntityKind_Extends_CoreParentPassesWithoutLister verifies that a
// core parent (person) passes through without consulting the registry lister,
// and that the event payload preserves the literal core kind name.
func TestDefineEntityKind_Extends_CoreParentPassesWithoutLister(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	// nil lister — core kinds must not need a DB round-trip.
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "researcher",
		"displayName": "Researcher",
		"description": "A person who conducts research.",
		"extends":     "person",
	})
	_, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fa.gotEvents))
	}
	var payload ontology.EntityKindRegisteredEvent
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload.Extends != "person" {
		t.Errorf("payload.Extends = %q, want person", payload.Extends)
	}
}

// TestDefineEntityKind_Extends_NilListerPassesThrough verifies the
// minimal-boot degrade: when no lister is wired any extends value is
// accepted unchanged (never block a mint on a missing registry).
func TestDefineEntityKind_Extends_NilListerPassesThrough(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "special",
		"displayName": "Special",
		"description": "A special kind.",
		"extends":     "user:whatever",
	})
	_, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("nil lister must pass any extends through, got: %v", err)
	}
	if len(fa.gotEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fa.gotEvents))
	}
}

// --- MintWithAlias (medium-similarity closeMatch) tests ---

// mediumSimilarityLister returns a kind source whose single entry produces a
// lexical similarity score of ~0.857 against the candidate name "vehicl"
// (LCS and Levenshtein both yield 6/7), landing in the MintWithAlias band
// [0.75, 0.90).
func mediumSimilarityLister() *fakeKindSourceDEK {
	return &fakeKindSourceDEK{kinds: []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}}
}

// TestDefineEntityKind_MintWithAlias_CloseMatchRecorded verifies that a
// medium-similarity dedup result (MintWithAlias) proceeds with the mint,
// appends maz:kind:<match> to SameAs in the event payload, and returns
// closeMatch + similarity in the response with status=registered.
func TestDefineEntityKind_MintWithAlias_CloseMatchRecorded(t *testing.T) {
	t.Parallel()

	src := mediumSimilarityLister()
	checker := dedup.New(src, nil)
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicl", // ~0.857 similarity to user:vehicle → MintWithAlias
		"displayName": "Vehicl",
		"description": "A road-going motorised conveyance.",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Name       string   `json:"name"`
		Status     string   `json:"status"`
		CloseMatch string   `json:"closeMatch"`
		Similarity *float64 `json:"similarity"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Name != "user:vehicl" {
		t.Errorf("name = %q, want user:vehicl", out.Name)
	}
	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if out.CloseMatch != "user:vehicle" {
		t.Errorf("closeMatch = %q, want user:vehicle", out.CloseMatch)
	}
	if out.Similarity == nil || *out.Similarity <= 0 {
		t.Errorf("similarity should be positive, got %v", out.Similarity)
	}

	// Event must be appended (it's a real mint).
	if len(fa.gotEvents) != 1 {
		t.Fatalf("expected 1 appended event, got %d", len(fa.gotEvents))
	}

	// SameAs in the event payload must contain maz:kind:user:vehicle.
	var payload ontology.EntityKindRegisteredEvent
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	wantURN := "maz:kind:user:vehicle"
	found := false
	for _, s := range payload.SameAs {
		if s == wantURN {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SameAs = %v, want to contain %q", payload.SameAs, wantURN)
	}
}

// TestDefineEntityKind_MintWithAlias_Force_SkipsCloseMatch verifies that
// force=true bypasses the dedup check entirely, so no closeMatch is recorded
// even when a medium-similarity match exists.
func TestDefineEntityKind_MintWithAlias_Force_SkipsCloseMatch(t *testing.T) {
	t.Parallel()

	src := mediumSimilarityLister()
	checker := dedup.New(src, nil)
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicl",
		"displayName": "Vehicl",
		"description": "A road-going motorised conveyance.",
		"force":       true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var out struct {
		Status     string   `json:"status"`
		CloseMatch string   `json:"closeMatch"`
		Similarity *float64 `json:"similarity"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if out.CloseMatch != "" {
		t.Errorf("closeMatch = %q, want empty (force bypasses dedup)", out.CloseMatch)
	}
	if out.Similarity != nil {
		t.Errorf("similarity = %v, want nil (force bypasses dedup)", out.Similarity)
	}

	// The event SameAs must NOT contain the alias URN.
	if len(fa.gotEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fa.gotEvents))
	}
	var payload ontology.EntityKindRegisteredEvent
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	for _, s := range payload.SameAs {
		if strings.HasPrefix(s, "maz:kind:") {
			t.Errorf("SameAs contains unexpected alias URN %q (force should skip dedup)", s)
		}
	}
}

// TestDefineEntityKind_MintWithAlias_CallerURNNotDoubled verifies that when
// the caller already includes the maz:kind:<match> URN in sameAs, it is not
// duplicated in the event payload.
func TestDefineEntityKind_MintWithAlias_CallerURNNotDoubled(t *testing.T) {
	t.Parallel()

	src := mediumSimilarityLister()
	checker := dedup.New(src, nil)
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicl",
		"displayName": "Vehicl",
		"description": "A road-going motorised conveyance.",
		"sameAs":      []string{"maz:kind:user:vehicle"},
	})
	_, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if len(fa.gotEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fa.gotEvents))
	}
	var payload ontology.EntityKindRegisteredEvent
	if uerr := json.Unmarshal(fa.gotEvents[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	count := 0
	for _, s := range payload.SameAs {
		if s == "maz:kind:user:vehicle" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("maz:kind:user:vehicle appears %d times in SameAs, want exactly 1", count)
	}
}
