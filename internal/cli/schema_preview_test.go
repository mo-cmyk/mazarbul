package cli

// schema_preview_test.go covers the human-readable rendering of the
// defineEntityKind preview plan, specifically the near-duplicate fields
// (close match, nearest kinds, checks skipped) added so a user can answer
// "would this collapse into an existing kind?" before writing.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// TestRenderKindPreviewCLI_NearDuplicateFields verifies the mint preview table
// surfaces closeMatch, every nearestKinds row, and the checksSkipped line.
func TestRenderKindPreviewCLI_NearDuplicateFields(t *testing.T) {
	t.Parallel()

	resp := &kindPreviewResponse{
		Name:   "user:vehicles",
		Status: "preview",
		Plan: &kindPreviewPlan{
			Action:     "mint",
			CloseMatch: "user:vehicle",
			NearestKinds: []nearestKindEntry{
				{Kind: "user:vehicle", Similarity: 0.88},
				{Kind: "user:deal", Similarity: 0.29},
			},
			ChecksSkipped: []string{"semantic"},
		},
		Warnings: []warncode.Warning{
			{Code: warncode.CodeKindCloseMatch, Message: "near-duplicate kind found"},
			{Code: warncode.CodeKindDedupDegraded, Message: "semantic dedup pass was skipped"},
		},
	}

	var buf bytes.Buffer
	if err := renderKindPreviewCLI(&buf, resp); err != nil {
		t.Fatalf("renderKindPreviewCLI: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"action",
		"mint",
		"close match",
		"user:vehicle",
		"nearest kind",
		"user:deal",
		"checks skipped",
		"semantic",
		warncode.CodeKindCloseMatch,
		warncode.CodeKindDedupDegraded,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestDefaultKindDisplayName pins that omitting --display-name yields a sensible
// titleized label so the documented `schema preview kind <name>` example (which
// carries no --display-name) computes a plan instead of failing validation.
func TestDefaultKindDisplayName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"vehicle":    "Vehicle",
		"sales_deal": "Sales Deal",
		"sales-deal": "Sales Deal",
		"a":          "A",
		"":           "",
	}
	for in, want := range cases {
		if got := defaultKindDisplayName(in); got != want {
			t.Errorf("defaultKindDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderKindPreviewCLI_CleanMint_NoNearDupLines verifies that a clean mint
// with a fully-checked semantic pass renders neither a close-match nor a
// checks-skipped line.
func TestRenderKindPreviewCLI_CleanMint_NoNearDupLines(t *testing.T) {
	t.Parallel()

	resp := &kindPreviewResponse{
		Name:   "user:nebula",
		Status: "preview",
		Plan:   &kindPreviewPlan{Action: "mint"},
	}

	var buf bytes.Buffer
	if err := renderKindPreviewCLI(&buf, resp); err != nil {
		t.Fatalf("renderKindPreviewCLI: %v", err)
	}
	out := buf.String()

	for _, unwanted := range []string{"close match", "nearest kind", "checks skipped"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("clean mint output unexpectedly contains %q\n--- output ---\n%s", unwanted, out)
		}
	}
}
