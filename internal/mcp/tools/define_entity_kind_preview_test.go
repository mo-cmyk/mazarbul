package tools_test

// define_entity_kind_preview_test.go verifies the preview=true path for
// DefineEntityKind: the call computes the plan and returns status="preview"
// WITHOUT appending any event to the event log.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// TestDefineEntityKind_Preview_NoEvents verifies that preview=true never
// appends any event and returns status="preview".
func TestDefineEntityKind_Preview_NoEvents(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "nebula",
		"displayName": "Nebula",
		"description": "An interstellar gas cloud.",
		"preview":     true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 0 {
		t.Fatalf("preview=true appended %d events, want 0", len(fa.gotEvents))
	}

	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Plan   *struct {
			Action string `json:"action"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "preview" {
		t.Errorf("status = %q, want preview", out.Status)
	}
	if out.Plan == nil {
		t.Fatal("plan is nil, want non-nil for preview=true")
	}
	if out.Plan.Action != "mint" {
		t.Errorf("plan.action = %q, want mint for a novel kind", out.Plan.Action)
	}
}

// TestDefineEntityKind_Preview_Collapse_NoEvents verifies that preview=true
// with an existing kind returns action="collapse" and appends no event.
func TestDefineEntityKind_Preview_Collapse_NoEvents(t *testing.T) {
	t.Parallel()

	existing := []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}
	src := &fakeKindSourceDEK{kinds: existing}

	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, src, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A road vehicle.",
		"preview":     true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 0 {
		t.Fatalf("preview=true appended %d events, want 0", len(fa.gotEvents))
	}

	var out struct {
		Status string `json:"status"`
		Plan   *struct {
			Action        string `json:"action"`
			CollapsesInto string `json:"collapsesInto"`
		} `json:"plan"`
		Warnings []warncode.Warning `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Status != "preview" {
		t.Errorf("status = %q, want preview", out.Status)
	}
	if out.Plan == nil {
		t.Fatal("plan is nil")
	}
	if out.Plan.Action != "collapse" {
		t.Errorf("plan.action = %q, want collapse", out.Plan.Action)
	}
	if out.Plan.CollapsesInto != "user:vehicle" {
		t.Errorf("plan.collapsesInto = %q, want user:vehicle", out.Plan.CollapsesInto)
	}
	// Collapse must carry MAZ-SCHEMA-003.
	hasCollapse := false
	for _, w := range out.Warnings {
		if w.Code == warncode.CodeSchemaCollapse {
			hasCollapse = true
		}
	}
	if !hasCollapse {
		t.Errorf("expected MAZ-SCHEMA-003 warning, got: %v", out.Warnings)
	}
}

// TestDefineEntityKind_Preview_ForceIgnored verifies that force=true is
// irrelevant on the preview path: the plan is still computed without writing.
func TestDefineEntityKind_Preview_ForceIgnored(t *testing.T) {
	t.Parallel()
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "gadget",
		"displayName": "Gadget",
		"description": "A small electronic device.",
		"force":       true,
		"preview":     true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 0 {
		t.Fatalf("preview=true (force=true) appended %d events, want 0", len(fa.gotEvents))
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "preview" {
		t.Errorf("status = %q, want preview", out.Status)
	}
}

// TestDefineEntityKind_Preview_NearDuplicate_SurfacesNearestAndChecksSkipped
// verifies that previewing a brand-new kind name that is a lexical
// near-duplicate of an existing kind surfaces nearestKinds + closeMatch
// (MAZ-KIND-001) and, with no embedder wired, checksSkipped=["semantic"]
// (MAZ-KIND-002) — all without appending any event.
func TestDefineEntityKind_Preview_NearDuplicate_SurfacesNearestAndChecksSkipped(t *testing.T) {
	t.Parallel()

	// "vehicles" (plural) has ~0.875 lexical similarity to "vehicle" — a
	// near-duplicate above MintAliasThreshold with no exact/normalized match.
	src := &fakeKindSourceDEK{kinds: []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}}
	checker := dedup.New(src, nil) // nil embedder → semantic pass not possible
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, src, false)

	req, _ := json.Marshal(map[string]any{
		"name":        "vehicles",
		"displayName": "Vehicles",
		"description": "A road vehicle.",
		"preview":     true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fa.gotEvents) != 0 {
		t.Fatalf("preview=true appended %d events, want 0", len(fa.gotEvents))
	}

	var out struct {
		Status string `json:"status"`
		Plan   *struct {
			Action       string `json:"action"`
			CloseMatch   string `json:"closeMatch"`
			NearestKinds []struct {
				Kind       string  `json:"kind"`
				Similarity float64 `json:"similarity"`
			} `json:"nearestKinds"`
			ChecksSkipped []string `json:"checksSkipped"`
		} `json:"plan"`
		Warnings []warncode.Warning `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Plan == nil {
		t.Fatal("plan is nil")
	}
	if out.Plan.Action != "mint" {
		t.Errorf("plan.action = %q, want mint", out.Plan.Action)
	}
	if out.Plan.CloseMatch != "user:vehicle" {
		t.Errorf("plan.closeMatch = %q, want user:vehicle", out.Plan.CloseMatch)
	}
	if len(out.Plan.NearestKinds) == 0 || out.Plan.NearestKinds[0].Kind != "user:vehicle" {
		t.Errorf("plan.nearestKinds = %v, want user:vehicle ranked first", out.Plan.NearestKinds)
	}
	if len(out.Plan.ChecksSkipped) != 1 || out.Plan.ChecksSkipped[0] != "semantic" {
		t.Errorf("plan.checksSkipped = %v, want [semantic]", out.Plan.ChecksSkipped)
	}

	hasClose, hasDegraded := false, false
	for _, w := range out.Warnings {
		switch w.Code {
		case warncode.CodeKindCloseMatch:
			hasClose = true
		case warncode.CodeKindDedupDegraded:
			hasDegraded = true
		}
	}
	if !hasClose {
		t.Errorf("expected MAZ-KIND-001 (close match), got: %v", out.Warnings)
	}
	if !hasDegraded {
		t.Errorf("expected MAZ-KIND-002 (semantic skipped), got: %v", out.Warnings)
	}
}
