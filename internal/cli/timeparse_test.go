package cli

import (
	"testing"
	"time"
)

func TestParseSinceDuration(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cases := map[string]time.Duration{
		"7d":  7 * 24 * time.Hour,
		"2h":  2 * time.Hour,
		"90m": 90 * time.Minute,
		"1w":  7 * 24 * time.Hour,
		"30s": 30 * time.Second,
	}
	for in, want := range cases {
		got, err := ParseSince(in)
		if err != nil {
			t.Errorf("ParseSince(%q): %v", in, err)
			continue
		}
		delta := now.Sub(got)
		if diff := delta - want; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("ParseSince(%q) = %v ago, want ~%v ago", in, delta, want)
		}
	}
}

func TestParseSinceRFC3339AndDate(t *testing.T) {
	t.Parallel()
	got, err := ParseSince("2026-06-15T09:00:00Z")
	if err != nil {
		t.Fatalf("ParseSince RFC3339: %v", err)
	}
	if got.Year() != 2026 || got.Month() != time.June || got.Day() != 15 {
		t.Errorf("RFC3339 parse wrong: %v", got)
	}
	d, err := ParseSince("2026-06-15")
	if err != nil {
		t.Fatalf("ParseSince date: %v", err)
	}
	if d.Year() != 2026 || d.Day() != 15 {
		t.Errorf("date parse wrong: %v", d)
	}
}

func TestParseSinceRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := ParseSince("not-a-time"); err == nil {
		t.Error("expected error for garbage input")
	}
	if _, err := ParseSince(""); err == nil {
		t.Error("expected error for empty input")
	}
}

func TestRelativeTime(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-2 * 24 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := RelativeTime(c.t); got != c.want {
			t.Errorf("RelativeTime(%v) = %q, want %q", c.t, got, c.want)
		}
	}
	if got := RelativeTime(time.Time{}); got != "—" {
		t.Errorf("zero time = %q, want em-dash", got)
	}
}
