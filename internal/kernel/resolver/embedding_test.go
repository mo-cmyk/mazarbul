package resolver

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestMentionHasMeaningfulSummary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		m    ontology.Mention
		want bool
	}{
		{
			"both empty",
			ontology.Mention{},
			false,
		},
		{
			"single short extraction",
			ontology.Mention{ExtractionText: "Hi"},
			false,
		},
		{
			"meaningful disambiguation only",
			ontology.Mention{DisambiguationCtx: "software engineer at Erebor"},
			true,
		},
		{
			"meaningful extraction only",
			ontology.Mention{ExtractionText: "lemon balm tea brewing"},
			true,
		},
		{
			"combined just over threshold",
			ontology.Mention{DisambiguationCtx: "a", ExtractionText: "samwise gamgee"},
			true,
		},
		{
			"combined under threshold",
			ontology.Mention{DisambiguationCtx: "a", ExtractionText: "x"},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mentionHasMeaningfulSummary(tc.m); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
