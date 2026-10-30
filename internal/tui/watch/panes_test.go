package watch

import (
	"strings"
	"testing"
)

func TestRenderExtractionJob(t *testing.T) {
	t.Run("indeterminate gauge row shows detail and no percentage", func(t *testing.T) {
		out := renderExtractionJob(ExtractionJob{
			ID:       "enrichment",
			Pipeline: "epi→ent",
			Progress: -1,
			Detail:   "3 in flight",
		}, 60, false)
		if !strings.Contains(out, "enrichment") || !strings.Contains(out, "epi→ent") {
			t.Errorf("gauge row missing identity: %q", out)
		}
		if !strings.Contains(out, "3 in flight") {
			t.Errorf("gauge row missing detail: %q", out)
		}
		if strings.Contains(out, "%") {
			t.Errorf("gauge row must not render a percentage: %q", out)
		}
	})

	t.Run("running job renders a percentage and trailing detail", func(t *testing.T) {
		out := renderExtractionJob(ExtractionJob{
			ID:       "j-run",
			Pipeline: "ingest",
			Progress: 0.4,
			Detail:   "resolving",
		}, 60, false)
		if !strings.Contains(out, "40%") {
			t.Errorf("want 40%% in %q", out)
		}
		if !strings.Contains(out, "resolving") {
			t.Errorf("want detail in %q", out)
		}
	})
}
