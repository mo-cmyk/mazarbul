package tools

import (
	"encoding/json"
	"testing"
)

// TestCostBreakdownEnumValuesMapToDistinctWindows pins the contract
// that every window enum value the cost-breakdown schema advertises
// maps to a non-fallback parseWindow result. The cost.go schema used
// to list "monthToDate" but parseWindow had no case for it, so a
// client calling with that window silently received today's spend —
// see Greptile review on PR #140.
//
// If you add a new window to the schema, either add a parseWindow
// case for it or remove it from the schema; do not let the silent
// today-fallback hide the gap.
func TestCostBreakdownEnumValuesMapToDistinctWindows(t *testing.T) {
	var schema struct {
		Properties struct {
			Window struct {
				Enum []string `json:"enum"`
			} `json:"window"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(costBreakdownInputSchema), &schema); err != nil {
		t.Fatalf("decode costBreakdownInputSchema: %v", err)
	}

	values := schema.Properties.Window.Enum
	if len(values) == 0 {
		t.Fatalf("schema enum is empty; expected at least one window")
	}

	for _, v := range values {
		if v == windowToday {
			// today is the documented default for the fallback
			// branch; it's fine to be the parseWindow miss.
			continue
		}
		_, label := parseWindow(v)
		if label == windowToday {
			t.Errorf("schema advertises window %q but parseWindow falls back to %q; "+
				"either implement the window or remove it from the schema",
				v, label)
		}
	}
}
