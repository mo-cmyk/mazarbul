package tools

import (
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// TestNeighborsSchemaDepthMatchesMaxHops guards the hand-written depth ceiling in
// neighborsInputSchema against navigate.MaxHops, so raising the kernel cap without
// updating the agent-facing schema fails loudly here rather than silently letting
// the tool advertise a stale maximum.
func TestNeighborsSchemaDepthMatchesMaxHops(t *testing.T) {
	t.Parallel()
	var schema struct {
		Properties struct {
			Depth struct {
				Maximum int `json:"maximum"`
			} `json:"depth"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(neighborsInputSchema), &schema); err != nil {
		t.Fatalf("unmarshal neighborsInputSchema: %v", err)
	}
	if schema.Properties.Depth.Maximum != navigate.MaxHops {
		t.Fatalf("neighbors schema depth.maximum = %d, want navigate.MaxHops = %d",
			schema.Properties.Depth.Maximum, navigate.MaxHops)
	}
}
