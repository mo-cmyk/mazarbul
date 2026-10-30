package contextbrief

import (
	"strings"
	"testing"
)

func TestTokensFor_CeilDivision(t *testing.T) {
	t.Parallel()
	cases := map[string]int{"": 0, "a": 1, "abcd": 1, "abcde": 2, "12345678": 2, "123456789": 3}
	for in, want := range cases {
		if got := tokensFor(in); got != want {
			t.Errorf("tokensFor(%q) = %d, want %d", in, got, want)
		}
	}
}

func sampleBlocks(n int) []block {
	blocks := make([]block, n)
	for i := range n {
		blocks[i] = block{section: "S", text: strings.Repeat("x", 40) + "\n"} // ~10 tokens each
	}
	return blocks
}

func TestPack_NeverExceedsBudget(t *testing.T) {
	t.Parallel()
	blocks := sampleBlocks(20)
	for budget := 1; budget <= 300; budget++ {
		res := pack("# T\n", blocks, budget)
		// The real invariant: the assembled markdown never exceeds the budget.
		if got := tokensFor(res.markdown); got > budget {
			t.Fatalf("budget=%d: markdown is %d tokens (exceeds)", budget, got)
		}
		if res.tokens > budget {
			t.Fatalf("budget=%d: reported tokens %d exceed budget", budget, res.tokens)
		}
	}
}

func TestPack_PriorityOrderPreserved(t *testing.T) {
	t.Parallel()
	blocks := []block{
		{section: "A", text: "alpha\n"},
		{section: "A", text: "beta\n"},
		{section: "B", text: "gamma\n"},
		{section: "B", text: "delta\n"},
	}
	// A budget that fits roughly the first two blocks plus headers.
	res := pack("# T\n", blocks, 12)
	// Whatever is included must be a prefix of the priority order.
	if res.included < 1 {
		t.Fatalf("expected at least one block included, got %d", res.included)
	}
	if res.included < len(blocks) && res.dropped != len(blocks)-res.included {
		t.Errorf("dropped=%d, want %d (len-included)", res.dropped, len(blocks)-res.included)
	}
	// The first block's text must appear; a later, dropped block must not.
	if !strings.Contains(res.markdown, "alpha") {
		t.Errorf("highest-priority block 'alpha' missing:\n%s", res.markdown)
	}
	if res.included < len(blocks) && strings.Contains(res.markdown, "delta") {
		t.Errorf("lowest-priority block 'delta' present despite truncation:\n%s", res.markdown)
	}
}

func TestPack_TruncationMarked(t *testing.T) {
	t.Parallel()
	blocks := sampleBlocks(20)
	res := pack("# T\n", blocks, 50) // far too small for 20 ~10-token blocks
	if res.dropped == 0 {
		t.Fatalf("expected truncation, dropped=0")
	}
	if !strings.Contains(res.markdown, "omitted to fit") {
		t.Errorf("truncation marker missing:\n%s", res.markdown)
	}
	if res.included+res.dropped != len(blocks) {
		t.Errorf("included(%d)+dropped(%d) != %d", res.included, res.dropped, len(blocks))
	}
	// Marker inclusion must not push the brief over budget.
	if got := tokensFor(res.markdown); got > 50 {
		t.Errorf("brief with marker is %d tokens, exceeds budget 50", got)
	}
}

func TestPack_AllFitNoMarker(t *testing.T) {
	t.Parallel()
	blocks := sampleBlocks(3)
	res := pack("# T\n", blocks, 10_000)
	if res.dropped != 0 {
		t.Errorf("dropped=%d, want 0 with a large budget", res.dropped)
	}
	if res.included != 3 {
		t.Errorf("included=%d, want 3", res.included)
	}
	if strings.Contains(res.markdown, "omitted to fit") {
		t.Errorf("unexpected truncation marker when everything fits:\n%s", res.markdown)
	}
}

func TestPack_SectionHeaderEmittedOncePerRun(t *testing.T) {
	t.Parallel()
	blocks := []block{
		{section: "Pinned", text: "a\n"},
		{section: "Pinned", text: "b\n"},
		{section: "Recent", text: "c\n"},
	}
	res := pack("# T\n", blocks, 10_000)
	if n := strings.Count(res.markdown, "## Pinned"); n != 1 {
		t.Errorf("## Pinned appears %d times, want 1", n)
	}
	if n := strings.Count(res.markdown, "## Recent"); n != 1 {
		t.Errorf("## Recent appears %d times, want 1", n)
	}
}
