package ingest

import (
	"testing"
	"time"
)

// TestClassifyWatermark pins the freshness thresholds shared by listSources,
// `maz source list`, and `maz doctor`: live ≤24h, stale >24h, down >7d or never.
func TestClassifyWatermark(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		advance    string
		wantStatus string
		wantNeg    bool // ageSeconds should be -1 (unknown)
	}{
		{"just now", now.Add(-time.Minute).Format(time.RFC3339Nano), WatermarkLive, false},
		{"under a day", now.Add(-23 * time.Hour).Format(time.RFC3339Nano), WatermarkLive, false},
		{"exactly a day", now.Add(-24 * time.Hour).Format(time.RFC3339Nano), WatermarkLive, false},
		{"over a day", now.Add(-25 * time.Hour).Format(time.RFC3339Nano), WatermarkStale, false},
		{"three days", now.Add(-3 * 24 * time.Hour).Format(time.RFC3339Nano), WatermarkStale, false},
		{"over a week", now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano), WatermarkDown, false},
		{"never advanced", "", WatermarkDown, true},
		{"unparseable", "not-a-time", WatermarkDown, true},
		{"future clamps to live", now.Add(time.Hour).Format(time.RFC3339Nano), WatermarkLive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, age := ClassifyWatermark(tc.advance, now)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if tc.wantNeg && age != -1 {
				t.Errorf("ageSeconds = %d, want -1", age)
			}
			if !tc.wantNeg && age < 0 {
				t.Errorf("ageSeconds = %d, want >= 0", age)
			}
		})
	}
}
