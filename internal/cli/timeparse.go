package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var durationPattern = regexp.MustCompile(`^(\d+)([smhdw])$`)

// durationUnit maps a single-letter suffix to its duration. Go's
// time.ParseDuration tops out at hours, so day/week are handled here — those are
// the units humans actually type for a memory window (7d, 2w).
func durationUnit(suffix string) time.Duration {
	switch suffix {
	case "s":
		return time.Second
	case "m":
		return time.Minute
	case "h":
		return time.Hour
	case "d":
		return 24 * time.Hour
	case "w":
		return 7 * 24 * time.Hour
	default:
		return 0
	}
}

// ParseSince interprets a CLI time-window value as an absolute UTC instant. It
// accepts a relative Go-style duration with a day/week extension (e.g. "7d",
// "2h", "90m", "1w") resolved against now, then falls back to RFC 3339 (with or
// without sub-second precision) and a bare calendar date ("2006-01-02"). The
// returned time is always UTC so it composes with the event log's createTime.
func ParseSince(s string) (time.Time, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return time.Time{}, errors.New("empty time value")
	}
	if m := durationPattern.FindStringSubmatch(trimmed); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid duration %q: %w", trimmed, err)
		}
		return time.Now().UTC().Add(-time.Duration(n) * durationUnit(m[2])), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, trimmed); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (want a duration like 7d/2h or an RFC 3339 timestamp)", s)
}

// RelativeTime renders t as a compact human delta from now ("just now", "2m
// ago", "3h ago", "5d ago") and falls back to a calendar date for anything
// older than a week. A zero or future time renders as a plain RFC 3339 date so
// the caller never prints a misleading "0s ago".
func RelativeTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return t.UTC().Format("2006-01-02")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.UTC().Format("2006-01-02")
	}
}
