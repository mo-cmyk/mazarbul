package composer_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
)

// TestFormatContextBlockWithIntent_PreSortsChronological verifies the
// TEST 6 ("List my names chronologically") fix: when intent is
// Chronological the render layer sorts hits by CreateTime DESC and
// prefixes each entry with "[k/N latest]" / "[k/N]" / "[k/N oldest]"
// so qwen2.5:7b can echo the ordering instead of having to sort
// timestamps itself.
func TestFormatContextBlockWithIntent_PreSortsChronological(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Deliberately shuffled — render must re-sort, not preserve input order.
	hits := []kernel.Hit{
		{UID: "01MID00000000000000000000A", Snippet: "Lucas", CreateTime: t0.AddDate(0, 2, 0)},
		{UID: "01OLD00000000000000000000B", Snippet: "Steve", CreateTime: t0},
		{UID: "01NEW00000000000000000000C", Snippet: "Arwen", CreateTime: t0.AddDate(0, 4, 0)},
	}
	block, citations := composer.FormatContextBlockWithIntent(hits, 200, composer.IntentChronological)

	if len(citations) != 3 {
		t.Fatalf("len(citations)=%d, want 3", len(citations))
	}

	// First entry must be the newest (Arwen).
	if citations[0].EpisodeUID != "01NEW00000000000000000000C" {
		t.Errorf("citations[0]=%q, want 01NEW…C (newest first)", citations[0].EpisodeUID)
	}
	if citations[2].EpisodeUID != "01OLD00000000000000000000B" {
		t.Errorf("citations[2]=%q, want 01OLD…B (oldest last)", citations[2].EpisodeUID)
	}

	// Ordinal labels must appear in the rendered block.
	for _, want := range []string{"[1/3 latest]", "[2/3]", "[3/3 oldest]"} {
		if !strings.Contains(block, want) {
			t.Errorf("rendered block missing %q\nblock:\n%s", want, block)
		}
	}
}

// TestFormatContextBlockWithIntent_AgnosticPreservesInputOrder is the
// safety check: every existing caller passing IntentAgnostic must get
// the same numeric "[N]" prefix and the same input order it always
// had. Regression here breaks every test that pinned the old format.
func TestFormatContextBlockWithIntent_AgnosticPreservesInputOrder(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hits := []kernel.Hit{
		{UID: "01A00000000000000000000001", Snippet: "first", CreateTime: t0.AddDate(0, 2, 0)},
		{UID: "01B00000000000000000000002", Snippet: "second", CreateTime: t0},
	}
	block, citations := composer.FormatContextBlockWithIntent(hits, 200, composer.IntentAgnostic)

	if citations[0].EpisodeUID != "01A00000000000000000000001" {
		t.Errorf("Agnostic must preserve input order; got citations[0]=%q",
			citations[0].EpisodeUID)
	}
	if !strings.Contains(block, "[1] (01A00000000000000000000001") {
		t.Errorf("Agnostic must use plain [N] labels; block=\n%s", block)
	}
	if strings.Contains(block, "latest]") || strings.Contains(block, "oldest]") {
		t.Errorf("Agnostic must not emit ordinal labels; block=\n%s", block)
	}
}

// TestFormatContextBlock_BackCompatWrapper confirms the legacy 2-arg
// signature still works and routes through IntentAgnostic so every
// existing call site keeps its old behaviour without modification.
func TestFormatContextBlock_BackCompatWrapper(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hits := []kernel.Hit{
		{UID: "01A00000000000000000000001", Snippet: "first", CreateTime: t0.AddDate(0, 2, 0)},
		{UID: "01B00000000000000000000002", Snippet: "second", CreateTime: t0},
	}
	legacyBlock, legacyCites := composer.FormatContextBlock(hits, 200)
	intentBlock, intentCites := composer.FormatContextBlockWithIntent(hits, 200, composer.IntentAgnostic)
	if legacyBlock != intentBlock {
		t.Errorf("back-compat wrapper diverged from IntentAgnostic:\nlegacy:\n%s\nintent:\n%s",
			legacyBlock, intentBlock)
	}
	if len(legacyCites) != len(intentCites) {
		t.Errorf("citation count mismatch: legacy=%d intent=%d", len(legacyCites), len(intentCites))
	}
}
