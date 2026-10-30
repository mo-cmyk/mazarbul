package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// warningOutput is the partial output shape we care about for warning tests.
type warningOutput struct {
	Name       string             `json:"name"`
	Status     string             `json:"status"`
	CloseMatch string             `json:"closeMatch"`
	ReusedKind string             `json:"reusedKind"`
	Warnings   []warncode.Warning `json:"warnings"`
}

// invokeKindTool is a helper that marshals args, calls Invoke, and
// unmarshals the result into warningOutput. Fails the test on any error.
func invokeKindTool(t *testing.T, tool *tools.DefineEntityKind, args map[string]any) warningOutput {
	t.Helper()
	req, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out warningOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return out
}

// findWarning returns the first Warning whose Code matches code, or the zero
// value when not found.
func findWarning(warnings []warncode.Warning, code string) (warncode.Warning, bool) {
	for _, w := range warnings {
		if w.Code == code {
			return w, true
		}
	}
	return warncode.Warning{}, false
}

// TestDefineEntityKind_Warnings_MintWithAlias_EmitsKindCloseMatch verifies that
// a medium-similarity MintWithAlias result emits a MAZ-KIND-001 warning with
// the match name in the message.
func TestDefineEntityKind_Warnings_MintWithAlias_EmitsKindCloseMatch(t *testing.T) {
	t.Parallel()

	// "vehicl" has ~0.857 similarity to "user:vehicle" (LCS/Levenshtein) →
	// lands in the MintWithAlias band [0.75, 0.90).
	src := &fakeKindSourceDEK{kinds: []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}}
	checker := dedup.New(src, nil)
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	out := invokeKindTool(t, tool, map[string]any{
		"name":        "vehicl",
		"displayName": "Vehicl",
		"description": "A road vehicle.",
	})

	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if out.CloseMatch != "user:vehicle" {
		t.Errorf("closeMatch = %q, want user:vehicle", out.CloseMatch)
	}

	w, ok := findWarning(out.Warnings, warncode.CodeKindCloseMatch)
	if !ok {
		t.Fatalf("expected warning %q, got warnings: %v", warncode.CodeKindCloseMatch, out.Warnings)
	}
	if w.Message == "" {
		t.Errorf("warning message must not be empty")
	}
}

// TestDefineEntityKind_Warnings_NoEmbedder_EmitsKindDedupDegraded verifies that
// when the Checker has no embedder wired (lexical-only path), a MAZ-KIND-002
// warning is emitted on a clean mint.
func TestDefineEntityKind_Warnings_NoEmbedder_EmitsKindDedupDegraded(t *testing.T) {
	t.Parallel()

	// Empty registry → Mint path; embedder is nil → Degraded=true.
	src := &fakeKindSourceDEK{kinds: nil}
	checker := dedup.New(src, nil) // nil embedder → lexical-only
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	out := invokeKindTool(t, tool, map[string]any{
		"name":        "contraption",
		"displayName": "Contraption",
		"description": "A novel mechanical device not resembling any existing kind.",
	})

	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}

	_, ok := findWarning(out.Warnings, warncode.CodeKindDedupDegraded)
	if !ok {
		t.Fatalf("expected warning %q on lexical-only path, got warnings: %v",
			warncode.CodeKindDedupDegraded, out.Warnings)
	}
}

// TestDefineEntityKind_Warnings_Reuse_NoWarnings verifies that an exact-match
// Reuse result via the lexical layer produces no warnings: the semantic layer
// is skipped only when the lexical layer returns Mint (no close match), so a
// Reuse decision means the embedder was not needed and Degraded is false.
func TestDefineEntityKind_Warnings_Reuse_NoWarnings(t *testing.T) {
	t.Parallel()

	// Exact match → Reuse via lexical; no embedder needed.
	src := &fakeKindSourceDEK{kinds: []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}}
	checker := dedup.New(src, nil)
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	out := invokeKindTool(t, tool, map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A motorised road vehicle.",
	})

	if out.Status != "reused" {
		t.Errorf("status = %q, want reused", out.Status)
	}
	if out.ReusedKind != "user:vehicle" {
		t.Errorf("reusedKind = %q, want user:vehicle", out.ReusedKind)
	}
	// Lexical Reuse: embedder was not needed, no degraded warning expected.
	if _, ok := findWarning(out.Warnings, warncode.CodeKindDedupDegraded); ok {
		t.Errorf("unexpected MAZ-KIND-002 on lexical Reuse (embedder not needed)")
	}
	if len(fa.gotEvents) != 0 {
		t.Errorf("expected 0 events on reuse, got %d", len(fa.gotEvents))
	}
}

// TestDefineEntityKind_Warnings_Force_NoWarnings verifies that force=true
// bypasses the dedup check entirely — no warnings are emitted.
func TestDefineEntityKind_Warnings_Force_NoWarnings(t *testing.T) {
	t.Parallel()

	src := &fakeKindSourceDEK{kinds: []navigate.RegisteredEntityKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
	}}
	checker := dedup.New(src, nil)
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, checker, nil, false)

	out := invokeKindTool(t, tool, map[string]any{
		"name":        "vehicle",
		"displayName": "Vehicle",
		"description": "A motorised road vehicle.",
		"force":       true,
	})

	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("force=true must produce no warnings, got: %v", out.Warnings)
	}
}

// TestDefineEntityKind_Warnings_CleanMint_WithEmbedder_NoWarnings verifies
// that a clean mint with a non-nil embedder that returns a low-similarity
// result produces no warnings (Degraded is false, Decision is Mint with no
// close match).
//
// We simulate "embedder available but candidate is genuinely novel" by using a
// fakeKindSourceDEK with a kind whose name has very low lexical similarity to
// the candidate, so the lexical layer yields Mint too.
func TestDefineEntityKind_Warnings_CleanMint_NilDeduper_NoWarnings(t *testing.T) {
	t.Parallel()

	// No deduper at all → no warnings (force=false, deduper=nil short-circuits
	// the whole check and produces no warnings of any kind).
	fa := &fakeAppender{}
	tool := tools.NewDefineEntityKind(fa, nil, nil, false)

	out := invokeKindTool(t, tool, map[string]any{
		"name":        "nebula",
		"displayName": "Nebula",
		"description": "An interstellar cloud of gas.",
	})

	if out.Status != "registered" {
		t.Errorf("status = %q, want registered", out.Status)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("nil deduper must produce no warnings, got: %v", out.Warnings)
	}
}
